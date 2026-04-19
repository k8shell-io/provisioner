package workspace

import (
	"bytes"
	"fmt"
	"os"
	"reflect"
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

// podStatusAndMessage analyzes a pod and returns both the workspace status and a corresponding message.
type podStatusInfo struct {
	status          models.WorkspaceStatusMessage
	message         string
	phase           corev1.PodPhase
	reason          string
	detailedMessage string
}

func analyzePodStatus(pod *corev1.Pod) podStatusInfo {
	if pod == nil {
		return podStatusInfo{
			status:  models.WorkspaceStatusUnknown,
			message: "Pod information not available",
		}
	}

	info := podStatusInfo{
		phase: pod.Status.Phase,
	}

	// Pod is being deleted
	if pod.DeletionTimestamp != nil {
		info.status = models.WorkspaceStatusTerminating
		info.message = "Workspace is terminating"
		return info
	}

	// Check container states for specific reasons (init containers first, then regular containers)
	containerReason, containerMsg := podTopReason(pod)
	info.reason = containerReason
	info.detailedMessage = containerMsg

	// Determine status and message based on container reason and pod phase
	if containerReason != "" {
		if isFailingReason(containerReason) {
			info.status = models.WorkspaceStatusFailing
			info.message = formatStatusMessage(info.phase, containerReason, containerMsg)
			return info
		}
		if isProvisioningReason(containerReason) {
			if podAnyContainerImagePending(pod) {
				info.status = models.WorkspaceStatusPulling
				info.message = formatStatusMessage(info.phase, "Pulling", "Downloading container images")
			} else {
				info.status = models.WorkspaceStatusProvisioning
				info.message = formatStatusMessage(info.phase, containerReason, containerMsg)
			}
			return info
		}
	}

	// Fall back to pod phase analysis
	switch pod.Status.Phase {
	case corev1.PodPending:
		if podAnyContainerImagePending(pod) {
			info.status = models.WorkspaceStatusPulling
			info.message = formatStatusMessage(info.phase, "Pulling", "Downloading container images")
		} else {
			info.status = models.WorkspaceStatusProvisioning
			// Check for scheduling issues
			if pod.Status.Reason != "" {
				info.message = formatStatusMessage(info.phase, pod.Status.Reason, pod.Status.Message)
			} else {
				info.message = formatStatusMessage(info.phase, "Pending", "Waiting for resources")
			}
		}

	case corev1.PodRunning:
		if podAllContainersReady(pod) {
			info.status = models.WorkspaceStatusRunning
			info.message = "Workspace is ready"
		} else {
			info.status = models.WorkspaceStatusProvisioning
			info.message = formatStatusMessage(info.phase, "Starting", "Initializing containers")
		}

	case corev1.PodSucceeded:
		info.status = models.WorkspaceStatusStopped
		info.message = formatStatusMessage(info.phase, "Succeeded", "Workspace completed")

	case corev1.PodFailed:
		info.status = models.WorkspaceStatusFailing
		if pod.Status.Reason != "" {
			info.message = formatStatusMessage(info.phase, pod.Status.Reason, pod.Status.Message)
		} else {
			info.message = formatStatusMessage(info.phase, "Failed", pod.Status.Message)
		}

	default:
		info.status = models.WorkspaceStatusUnknown
		if pod.Status.Reason != "" {
			info.message = formatStatusMessage(info.phase, pod.Status.Reason, pod.Status.Message)
		} else {
			info.message = fmt.Sprintf("Phase: %s", info.phase)
		}
	}

	return info
}

// formatStatusMessage creates a consistent, informative message from phase, reason, and details
func formatStatusMessage(phase corev1.PodPhase, reason, detailedMessage string) string {
	reason = strings.TrimSpace(reason)
	detailedMessage = strings.TrimSpace(detailedMessage)

	// For user-friendly messages, we don't need to show the phase if reason is clear
	if reason != "" {
		if detailedMessage != "" {
			return fmt.Sprintf("%s: %s", reason, detailedMessage)
		}
		return reason
	}

	// Fallback: include phase
	if detailedMessage != "" {
		return fmt.Sprintf("Phase %s: %s", phase, detailedMessage)
	}
	return fmt.Sprintf("Phase: %s", phase)
}

// workspacePodStatus returns a small set of UI-friendly statuses
func workspacePodStatus(pod *corev1.Pod) models.WorkspaceStatusMessage {
	return analyzePodStatus(pod).status
}

// workspacePodMessage returns a single message consistent with workspacePodStatus
func workspacePodMessage(pod *corev1.Pod) string {
	return analyzePodStatus(pod).message
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
	case "ContainerCreating",
		"PodInitializing":
		return true
	default:
		return false
	}
}

// podAnyContainerImagePending returns true when at least one regular container
// has not yet had its image pulled (imageID is empty), indicating an active download.
func podAnyContainerImagePending(pod *corev1.Pod) bool {
	if pod == nil {
		return false
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.ImageID == "" {
			return true
		}
	}
	return false
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

// marshalYAML2 marshals v to YAML using 2-space indentation.
func marshalYAML2(v interface{}) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// marshalYAMLAllFields marshals a struct to YAML with 2-space indentation,
// including all fields even if they hold zero values (ignoring omitempty tags).
func marshalYAMLAllFields(v interface{}) ([]byte, error) {
	full, err := toFullValue(reflect.ValueOf(v))
	if err != nil {
		return nil, err
	}
	return marshalYAML2(full)
}

// toFullValue recursively converts a reflect.Value to a plain Go value
// (map, slice, scalar) using yaml tag names and preserving zero values.
func toFullValue(rv reflect.Value) (interface{}, error) {
	for rv.Kind() == reflect.Ptr || rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return nil, nil
		}
		rv = rv.Elem()
	}
	switch rv.Kind() {
	case reflect.Struct:
		return structToFullMap(rv)
	case reflect.Slice:
		if rv.IsNil() {
			return []interface{}{}, nil
		}
		result := make([]interface{}, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			v, err := toFullValue(rv.Index(i))
			if err != nil {
				return nil, err
			}
			result[i] = v
		}
		return result, nil
	case reflect.Map:
		if rv.IsNil() {
			return map[string]interface{}{}, nil
		}
		result := make(map[string]interface{})
		for _, k := range rv.MapKeys() {
			v, err := toFullValue(rv.MapIndex(k))
			if err != nil {
				return nil, err
			}
			result[fmt.Sprintf("%v", k.Interface())] = v
		}
		return result, nil
	default:
		return rv.Interface(), nil
	}
}

// structToFullMap converts a struct reflect.Value to a map[string]interface{}
// keyed by yaml tag names (falling back to lowercase field name), including all
// exported fields regardless of omitempty.
func structToFullMap(rv reflect.Value) (map[string]interface{}, error) {
	rt := rv.Type()
	result := make(map[string]interface{})
	for i := 0; i < rt.NumField(); i++ {
		field := rt.Field(i)
		if !field.IsExported() {
			continue
		}
		fv := rv.Field(i)
		name := strings.ToLower(field.Name)
		inline := false
		if tag := field.Tag.Get("yaml"); tag != "" {
			parts := strings.SplitN(tag, ",", 2)
			if parts[0] == "-" {
				continue
			}
			if parts[0] != "" {
				name = parts[0]
			}
			if len(parts) > 1 && strings.Contains(parts[1], "inline") {
				inline = true
			}
		}
		val, err := toFullValue(fv)
		if err != nil {
			return nil, err
		}
		if inline {
			if m, ok := val.(map[string]interface{}); ok {
				for k, v := range m {
					result[k] = v
				}
			}
		} else {
			result[name] = val
		}
	}
	return result, nil
}
