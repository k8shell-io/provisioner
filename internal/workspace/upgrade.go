package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

var ErrInvalidValue = errors.New("invalid value")

// ResizeResources resizes k8shell-main container CPU/memory using the Pod /resize subresource.
// If cpu or memory is empty (or "0"), that resource is left unchanged.
func (w *Workspace) ResizeResources(ctx context.Context, cpu, memory string) error {
	cpu = strings.TrimSpace(cpu)
	memory = strings.TrimSpace(memory)

	// validate inputs (only if provided)
	if cpu != "" && cpu != "0" {
		if err := validateCPUQuantity(cpu); err != nil {
			return fmt.Errorf("%w: invalid cpu %q: %v", ErrInvalidValue, cpu, err)
		}
	}
	if memory != "" && memory != "0" {
		if err := validateMemoryQuantity(memory); err != nil {
			return fmt.Errorf("%w: invalid memory %q: %v", ErrInvalidValue, memory, err)
		}
	}

	if (cpu == "" || cpu == "0") && (memory == "" || memory == "0") {
		return nil
	}

	ns := w.client.TargetNamespace()
	v1 := w.client.KubeClient().CoreV1()
	lim := map[string]string{}

	// Only set the keys that were specified; unspecified ones remain unchanged server-side.
	if cpu != "" && cpu != "0" {
		lim["cpu"] = cpu
	}
	if memory != "" && memory != "0" {
		lim["memory"] = memory
	}

	patchObj := map[string]any{
		"spec": map[string]any{
			"containers": []any{
				map[string]any{
					"name": "k8shell-main",
					"resources": map[string]any{
						"limits": lim,
					},
				},
			},
		},
	}

	patchBytes, err := json.Marshal(patchObj)
	if err != nil {
		return fmt.Errorf("marshal patch: %w", err)
	}

	_, err = v1.Pods(ns).Patch(
		ctx,
		w.Name,
		types.StrategicMergePatchType,
		patchBytes,
		metav1.PatchOptions{},
		"resize",
	)
	if err != nil {
		if strings.Contains(err.Error(), "Invalid value") {
			return fmt.Errorf("%w: %v", ErrInvalidValue, err)
		}
		return fmt.Errorf("patch workspace resources via /resize: %w", err)
	}

	return nil
}

func validateCPUQuantity(v string) error {
	q, err := resource.ParseQuantity(v)
	if err != nil {
		return err
	}
	if q.Sign() <= 0 {
		return fmt.Errorf("must be > 0")
	}
	return nil
}

func validateMemoryQuantity(v string) error {
	q, err := resource.ParseQuantity(v)
	if err != nil {
		return err
	}
	if q.Sign() <= 0 {
		return fmt.Errorf("must be > 0")
	}
	return nil
}
