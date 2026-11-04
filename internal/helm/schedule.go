// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package helm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/k8shell-io/common/pkg/models"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// scheduleAnnotations maps the fields of models.Schedule to the pod
// annotations the chart stamps them on.
func scheduleAnnotations(s *models.Schedule) map[string]string {
	return map[string]string{
		AnnotationScheduleFrom:           s.From,
		AnnotationScheduleTo:             s.To,
		AnnotationScheduleStopGrace:      s.StopGrace,
		AnnotationScheduleTerminateGrace: s.TerminateGrace,
	}
}

// ScheduleAnnotationKeys are the pod annotations holding a workspace's
// schedule.
var ScheduleAnnotationKeys = []string{
	AnnotationScheduleFrom, AnnotationScheduleTo,
	AnnotationScheduleStopGrace, AnnotationScheduleTerminateGrace,
}

// ScheduleFromAnnotations reads the schedule stamped on a pod. A missing
// annotation is an unset field.
func ScheduleFromAnnotations(a map[string]string) models.Schedule {
	return models.Schedule{
		From:           a[AnnotationScheduleFrom],
		To:             a[AnnotationScheduleTo],
		StopGrace:      a[AnnotationScheduleStopGrace],
		TerminateGrace: a[AnnotationScheduleTerminateGrace],
	}
}

// ApplyScheduleOverride replaces the schedule annotations of pod with the
// override, the JSON held by AnnotationScheduleOverride. An empty override
// leaves the pod untouched. It returns an error, leaving the pod untouched,
// when the override cannot be decoded.
func ApplyScheduleOverride(pod *corev1.Pod, override string) error {
	if override == "" || pod == nil {
		return nil
	}
	var s models.Schedule
	if err := json.Unmarshal([]byte(override), &s); err != nil {
		return fmt.Errorf("decode %s: %w", AnnotationScheduleOverride, err)
	}
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	for k, v := range scheduleAnnotations(&s) {
		if v == "" {
			delete(pod.Annotations, k)
		} else {
			pod.Annotations[k] = v
		}
	}
	return nil
}

// ScheduleOverrideJSON encodes s as the value of AnnotationScheduleOverride.
func ScheduleOverrideJSON(s models.Schedule) (string, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// deployedReleaseSecret returns the Secret of the deployed revision of the
// release name, the latest one if several carry the label.
func (c *Client) deployedReleaseSecret(ctx context.Context, name string) (*corev1.Secret, error) {
	secrets, err := c.kubeClient.CoreV1().Secrets(c.targetNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: releaseSecretSelector("name=" + name),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list release secrets for %s: %w", name, err)
	}
	if len(secrets.Items) == 0 {
		return nil, fmt.Errorf("%w: no deployed release %s", models.ErrWorkspaceNotFound, name)
	}
	// Helm names the Secrets sh.helm.release.v1.<name>.v<revision>; compare
	// the revisions numerically so that v10 is later than v9.
	latest := &secrets.Items[0]
	for i := range secrets.Items {
		if revisionOf(secrets.Items[i].Name) > revisionOf(latest.Name) {
			latest = &secrets.Items[i]
		}
	}
	return latest, nil
}

func revisionOf(secretName string) int {
	i := strings.LastIndex(secretName, ".v")
	if i < 0 {
		return 0
	}
	n := 0
	for _, r := range secretName[i+2:] {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// DeployedReleasePod returns the pod manifest of the deployed release name as
// the release informer would, with a schedule override applied. The
// returned ReleasePod's annotations are those of the release Secret.
func (c *Client) DeployedReleasePod(ctx context.Context, name string) (*ReleasePod, error) {
	secret, err := c.deployedReleaseSecret(ctx, name)
	if err != nil {
		return nil, err
	}
	rp := releasePodFromSecret(c.log, secret)
	if rp.Pod == nil {
		return nil, fmt.Errorf("release %s has no pod manifest", name)
	}
	return rp, nil
}

// ListReleasePodsBySelector lists the deployed releases matching selector as
// ReleasePods, decoded the way the release informer does it and with a
// schedule override applied, but read from the cluster now.
func (c *Client) ListReleasePodsBySelector(ctx context.Context, namespace, selector string) ([]*ReleasePod, error) {
	secrets, err := c.kubeClient.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: releaseSecretSelector(selector),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list release secrets in namespace %s with selector %s: %w", namespace, selector, err)
	}
	out := make([]*ReleasePod, 0, len(secrets.Items))
	for i := range secrets.Items {
		out = append(out, releasePodFromSecret(c.log, &secrets.Items[i]))
	}
	return out, nil
}

// SetScheduleOverride stores override, or removes the stored one when it is
// empty, on the deployed release Secret of the workspace name.
func (c *Client) SetScheduleOverride(ctx context.Context, name, override string) error {
	secret, err := c.deployedReleaseSecret(ctx, name)
	if err != nil {
		return err
	}
	var value any // JSON null deletes the annotation
	if override != "" {
		value = override
	}
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"annotations": map[string]any{AnnotationScheduleOverride: value}},
	})
	if err != nil {
		return err
	}
	if _, err := c.kubeClient.CoreV1().Secrets(c.targetNamespace).Patch(ctx, secret.Name,
		types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("failed to patch release secret %s: %w", secret.Name, err)
	}
	return nil
}
