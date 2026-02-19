package workspace

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/k8shell-io/common/pkg/models"
	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
)

func getNamespace() string {
	data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		return ""
	}
	return string(data)
}

func getPodContainerPort(pod *corev1.Pod, defaultPort int) int {
	preferredNames := map[string]struct{}{
		"grpc":  {},
		"https": {},
		"http":  {},
	}

	for _, c := range pod.Spec.Containers {
		for _, p := range c.Ports {
			if p.ContainerPort <= 0 {
				continue
			}
			if _, ok := preferredNames[strings.ToLower(p.Name)]; ok {
				return int(p.ContainerPort)
			}
		}
	}

	for _, c := range pod.Spec.Containers {
		for _, p := range c.Ports {
			if p.ContainerPort > 0 {
				return int(p.ContainerPort)
			}
		}
	}

	return defaultPort
}

func podMountsSecret(pod *corev1.Pod, secretName string) bool {
	if secretName == "" {
		return false
	}
	for _, v := range pod.Spec.Volumes {
		if v.Secret != nil && v.Secret.SecretName == secretName {
			return true
		}
	}
	return false
}

// workspacePodStatus returns a small set of UI-friendly statuses.
// Keep this stable for UI/API consumers.
func workspacePodStatus(pod *corev1.Pod) models.WorkspacePodStatus {
	if pod == nil {
		return models.WorkspaceStatusUnknown
	}

	// If there's a concrete reason from init/containers, map it.
	reason, _ := podTopReason(pod)
	if reason != "" {
		switch {
		case isFailingReason(reason):
			return models.WorkspaceStatusFailing
		case isProvisioningReason(reason):
			return models.WorkspaceStatusProvisioning
		}
	}

	// Otherwise decide by phase + readiness.
	switch pod.Status.Phase {
	case corev1.PodPending:
		return models.WorkspaceStatusProvisioning
	case corev1.PodRunning:
		if podAllContainersReady(pod) {
			return models.WorkspaceStatusRunning
		}
		return models.WorkspaceStatusProvisioning
	case corev1.PodSucceeded:
		return models.WorkspaceStatusStopped
	case corev1.PodFailed:
		return models.WorkspaceStatusFailing
	default:
		return models.WorkspaceStatusUnknown
	}
}

// workspacePodMessage returns a single message consistent with workspacePodStatus.
func workspacePodMessage(pod *corev1.Pod) string {
	if pod == nil {
		return ""
	}

	// Prefer the most actionable reason/message from init/containers.
	reason, msg := podTopReason(pod)
	if reason != "" {
		if msg != "" {
			return fmt.Sprintf("%s: %s", reason, msg)
		}
		return reason
	}

	// Otherwise provide a short message based on phase/readiness.
	switch pod.Status.Phase {
	case corev1.PodPending:
		// Surface scheduling reason/message if present.
		if pod.Status.Reason != "" && pod.Status.Message != "" {
			return fmt.Sprintf("%s: %s", pod.Status.Reason, pod.Status.Message)
		}
		if pod.Status.Reason != "" {
			return pod.Status.Reason
		}
		return "Pending"
	case corev1.PodRunning:
		if podAllContainersReady(pod) {
			return "Workspace is ready"
		}
		return "Starting"
	case corev1.PodSucceeded:
		return "Completed"
	case corev1.PodFailed:
		if pod.Status.Reason != "" && pod.Status.Message != "" {
			return fmt.Sprintf("%s: %s", pod.Status.Reason, pod.Status.Message)
		}
		if pod.Status.Message != "" {
			return pod.Status.Message
		}
		return "Failed"
	default:
		if pod.Status.Reason != "" {
			return pod.Status.Reason
		}
		return string(pod.Status.Phase)
	}
}

// podTopReason returns the "best" actionable reason/message from init containers first,
// then regular containers. This is what catches CrashLoopBackOff while phase is Running.
func podTopReason(pod *corev1.Pod) (reason string, message string) {
	for _, cs := range pod.Status.InitContainerStatuses {
		if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
			return cs.State.Waiting.Reason, cs.State.Waiting.Message
		}
		if cs.State.Terminated != nil && cs.State.Terminated.ExitCode != 0 {
			if cs.State.Terminated.Reason != "" {
				return cs.State.Terminated.Reason, cs.State.Terminated.Message
			}
			return "InitContainerError", cs.State.Terminated.Message
		}
	}

	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
			return cs.State.Waiting.Reason, cs.State.Waiting.Message
		}
		if cs.State.Terminated != nil && cs.State.Terminated.ExitCode != 0 {
			if cs.State.Terminated.Reason != "" {
				return cs.State.Terminated.Reason, cs.State.Terminated.Message
			}
			return "ContainerError", cs.State.Terminated.Message
		}
	}

	return "", ""
}

func podAllContainersReady(pod *corev1.Pod) bool {
	if pod == nil {
		return false
	}
	if len(pod.Status.ContainerStatuses) == 0 {
		return false
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if !cs.Ready {
			return false
		}
	}
	return true
}

func isFailingReason(reason string) bool {
	switch reason {
	case "CrashLoopBackOff",
		"ImagePullBackOff",
		"ErrImagePull",
		"CreateContainerConfigError",
		"CreateContainerError",
		"RunContainerError",
		"ContainerError",
		"OOMKilled":
		return true
	default:
		return false
	}
}

func isProvisioningReason(reason string) bool {
	switch reason {
	case "ContainerCreating", "PodInitializing":
		return true
	default:
		return false
	}
}

// podRestartCount returns total restarts across init + regular containers.
func podRestartCount(pod *corev1.Pod) int32 {
	if pod == nil {
		return 0
	}

	var total int32
	for _, cs := range pod.Status.InitContainerStatuses {
		total += cs.RestartCount
	}
	for _, cs := range pod.Status.ContainerStatuses {
		total += cs.RestartCount
	}
	return total
}

// podLastFailure returns the most recent non-zero exit termination reason/message.
// It prefers the latest FinishedAt among init/regular containers.
// If no termination is found, it falls back to a failing waiting reason (e.g. CrashLoopBackOff).
func podLastFailure(pod *corev1.Pod) string {
	if pod == nil {
		return ""
	}

	var bestReason, bestMsg string
	var bestAt time.Time
	found := false

	considerTerminated := func(t *corev1.ContainerStateTerminated) {
		if t == nil || t.ExitCode == 0 {
			return
		}
		at := t.FinishedAt.Time
		if !found || at.After(bestAt) {
			found = true
			bestAt = at
			bestReason = t.Reason
			bestMsg = t.Message
			if bestReason == "" {
				bestReason = "ContainerTerminated"
			}
		}
	}

	for _, cs := range pod.Status.InitContainerStatuses {
		considerTerminated(cs.State.Terminated)
		considerTerminated(cs.LastTerminationState.Terminated)
	}
	for _, cs := range pod.Status.ContainerStatuses {
		considerTerminated(cs.State.Terminated)
		considerTerminated(cs.LastTerminationState.Terminated)
	}

	if found {
		return formatLastFailMessage(bestReason, bestMsg)
	}

	r, m := podTopReason(pod)
	if r != "" && isFailingReason(r) {
		return formatLastFailMessage(r, m)
	}

	return ""
}

func formatLastFailMessage(reason, msg string) string {
	reason = strings.TrimSpace(reason)
	msg = strings.TrimSpace(msg)

	if reason == "" && msg == "" {
		return ""
	}
	if reason != "" && msg != "" {
		return reason + ": " + msg
	}
	if reason != "" {
		return reason
	}
	return msg
}

// podHostname returns the Pod hostname if hostname+subdomain are set, otherwise "".
func podHostname(pod *corev1.Pod) string {
	if pod == nil {
		return ""
	}
	hn := strings.TrimSpace(pod.Spec.Hostname)
	sd := strings.TrimSpace(pod.Spec.Subdomain)
	if hn == "" || sd == "" {
		return ""
	}
	return fmt.Sprintf("%s.%s.%s", hn, sd, pod.Namespace)
}

// ToMap converts any struct to a map[string]interface{} representation
func toMap(b any) (map[string]interface{}, error) {
	yamlBytes, err := yaml.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal struct to YAML: %w", err)
	}

	var values map[string]interface{}
	if err := yaml.Unmarshal(yamlBytes, &values); err != nil {
		return nil, fmt.Errorf("failed to unmarshal YAML to map: %w", err)
	}

	return values, nil
}

// GetSelector returns a label selector string from the given labels map
func getSelector(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}

	var selectors []string
	for key, value := range labels {
		selectors = append(selectors, fmt.Sprintf("%s=%s", key, value))
	}

	return strings.Join(selectors, ",")
}
