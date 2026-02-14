package workspace

import (
	"context"
	"encoding/json"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// ResizeResources patches a workspace pod to update CPU/memory requests+limits for the workspace
// k8shell-main container. If cpu or memory is empty, that resource is left unchanged.
func (w *Workspace) ResizeResources(ctx context.Context, cpu, memory string) error {
	if cpu == "" && memory == "" {
		return fmt.Errorf("at least one of cpu or memory must be specified")
	}

	v1 := w.client.KubeClient().CoreV1()

	requests := map[string]string{}
	limits := map[string]string{}

	if cpu != "" {
		requests["cpu"] = cpu
		limits["cpu"] = cpu
	}
	if memory != "" {
		requests["memory"] = memory
		limits["memory"] = memory
	}

	patchObj := map[string]any{
		"spec": map[string]any{
			"containers": []any{
				map[string]any{
					"name": "k8shell-main",
					"resources": map[string]any{
						"requests": requests,
						"limits":   limits,
					},
				},
			},
		},
	}

	patchBytes, err := json.Marshal(patchObj)
	if err != nil {
		return fmt.Errorf("marshal patch: %w", err)
	}

	_, err = v1.Pods(w.client.TargetNamespace()).Patch(ctx, w.Name, types.StrategicMergePatchType,
		patchBytes, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("patch workspace resources: %w", err)
	}

	return nil
}
