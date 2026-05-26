package workspace

import (
	"bytes"
	"fmt"
	"os"
	"reflect"
	"strings"
	"time"

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
// If no termination is found, it falls back to a hard-failing waiting reason (e.g. CrashLoopBackOff).
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

	// Fallback: surface a hard-failing waiting reason such as CrashLoopBackOff.
	for _, cs := range pod.Status.InitContainerStatuses {
		if cs.State.Waiting != nil && isHardFailureReason(cs.State.Waiting.Reason) {
			return formatLastFailMessage(cs.State.Waiting.Reason, cs.State.Waiting.Message)
		}
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Waiting != nil && isHardFailureReason(cs.State.Waiting.Reason) {
			return formatLastFailMessage(cs.State.Waiting.Reason, cs.State.Waiting.Message)
		}
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

// toMap converts any struct to a map[string]interface{} representation.
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

// getSelector returns a label selector string from the given labels map.
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
