// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/k8shell-io/common/pkg/models"
	"github.com/k8shell-io/provisioner/internal/helm"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// ErrScheduleNotAllowed is returned when a workspace's schedule does not allow
// it to be provisioned or started now: before its from time or after its to
// time.
var ErrScheduleNotAllowed = errors.New("workspace schedule does not allow running")

// CheckSchedule returns ErrScheduleNotAllowed when the schedule of the
// workspace's blueprint does not allow the workspace to run at now. It
// applies both to provisioning a new workspace and to starting a stopped one,
// whose blueprint is read back from its Helm release. A workspace without a
// schedule, or with one that cannot be parsed, is always allowed: blueprints
// are validated when they load, so an unparsable schedule should not lock a
// user out of an existing workspace.
func (w *Workspace) CheckSchedule(now time.Time) error {
	if w.blueprint == nil {
		return nil
	}
	parsed, err := models.ParseSchedule(w.blueprint.Schedule)
	if err != nil {
		w.log.Warn().Err(err).Msgf("Ignoring invalid schedule of workspace %s", w.Name)
		return nil
	}
	return checkParsedSchedule(parsed, now)
}

func checkParsedSchedule(s *models.ParsedSchedule, now time.Time) error {
	if s == nil {
		return nil
	}
	if s.From != nil && now.Before(*s.From) {
		return fmt.Errorf("%w: the workspace cannot run before %s",
			ErrScheduleNotAllowed, s.From.UTC().Format(time.RFC3339))
	}
	if s.To != nil && !now.Before(*s.To) {
		return fmt.Errorf("%w: the workspace schedule ended at %s",
			ErrScheduleNotAllowed, s.To.UTC().Format(time.RFC3339))
	}
	return nil
}

// scheduleFromPod reads the run schedule stamped on the pod. It returns nil
// when the pod carries none, or when the stamped values cannot be parsed.
func scheduleFromPod(pod *corev1.Pod) *models.WorkspaceSchedule {
	return workspaceSchedule(helm.ScheduleFromAnnotations(pod.Annotations))
}

// workspaceSchedule resolves raw into the schedule reported in the workspace
// details, with the stop and terminate times worked out. It returns nil for an
// empty or unparsable schedule.
func workspaceSchedule(raw models.Schedule) *models.WorkspaceSchedule {
	if raw == (models.Schedule{}) {
		return nil
	}
	parsed, err := models.ParseSchedule(&raw)
	if err != nil {
		return nil
	}
	out := &models.WorkspaceSchedule{
		From:           parsed.From,
		To:             parsed.To,
		StopGrace:      raw.StopGrace,
		TerminateGrace: raw.TerminateGrace,
	}
	if parsed.To != nil {
		stopsAt := parsed.To.Add(parsed.StopGrace)
		out.StopsAt = &stopsAt
		if parsed.HasTerminateGrace {
			terminatesAt := stopsAt.Add(parsed.TerminateGrace)
			out.TerminatesAt = &terminatesAt
		}
	}
	return out
}

// CheckStartSchedule is CheckSchedule for starting a stopped workspace. It
// goes by the schedule in effect, which a SetWorkspaceSchedule override may
// have changed from the blueprint's, falling back to the blueprint's when the
// release cannot be read.
func (w *Workspace) CheckStartSchedule(ctx context.Context, now time.Time) error {
	rp, err := w.client.DeployedReleasePod(ctx, w.Name)
	if err != nil {
		return w.CheckSchedule(now)
	}
	raw := helm.ScheduleFromAnnotations(rp.Pod.Annotations)
	parsed, err := models.ParseSchedule(&raw)
	if err != nil {
		w.log.Warn().Err(err).Msgf("Ignoring invalid schedule of workspace %s", w.Name)
		return nil
	}
	return checkParsedSchedule(parsed, now)
}

// ErrWorkspaceNotRunning is returned when an operation needs a running
// workspace pod and there is none, or it is being deleted.
var ErrWorkspaceNotRunning = errors.New("workspace is not running")

// ScheduleChange is a change to the schedule of a running workspace. A nil
// field keeps the current value and a pointer to "" clears it. Revert instead
// drops the override and restores the blueprint's schedule.
type ScheduleChange struct {
	To             *string
	StopGrace      *string
	TerminateGrace *string
	Revert         bool
}

// SetSchedule overrides the schedule of the running workspace, or with
// ch.Revert restores the blueprint's, and returns the schedule in effect. The
// override is stored on the workspace's Helm release so that it outlives the
// pod (see helm.AnnotationScheduleOverride) and is written to the pod's
// annotations, which the enforcer and the workspace details read. Clearing the
// stop warning mark lets the user be warned again for the new time. The
// caller holds the workspace lock.
//
// The new "to" must be in the future, and the result must be a valid schedule.
// Such a rejection wraps models.ErrInvalidParameters.
func (w *Workspace) SetSchedule(ctx context.Context, ch ScheduleChange, now time.Time) (*models.WorkspaceSchedule, error) {
	pods := w.client.KubeClient().CoreV1().Pods(w.client.TargetNamespace())
	pod, err := pods.Get(ctx, w.Name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("%w: the schedule can only be changed while the workspace is running", ErrWorkspaceNotRunning)
		}
		return nil, fmt.Errorf("failed to get workspace pod %s: %w", w.Name, err)
	}
	if pod.DeletionTimestamp != nil {
		return nil, fmt.Errorf("%w: the workspace is being stopped", ErrWorkspaceNotRunning)
	}

	var next models.Schedule
	var override string
	if ch.Revert {
		if w.blueprint != nil && w.blueprint.Schedule != nil {
			next = *w.blueprint.Schedule
		}
	} else {
		next = helm.ScheduleFromAnnotations(pod.Annotations)
		if ch.To != nil {
			next.To = *ch.To
		}
		if ch.StopGrace != nil {
			next.StopGrace = *ch.StopGrace
		}
		if ch.TerminateGrace != nil {
			next.TerminateGrace = *ch.TerminateGrace
		}
		parsed, err := models.ParseSchedule(&next)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", models.ErrInvalidParameters, err)
		}
		if parsed != nil && parsed.To != nil && !parsed.To.After(now) {
			return nil, fmt.Errorf("%w: schedule.to %q is not in the future", models.ErrInvalidParameters, next.To)
		}
		if override, err = helm.ScheduleOverrideJSON(next); err != nil {
			return nil, err
		}
	}

	// The release first: it is what outlives the pod. A failure on the pod
	// below leaves the override stored, and repeating the call applies it.
	if err := w.client.SetScheduleOverride(ctx, w.Name, override); err != nil {
		return nil, err
	}

	annotations := make(map[string]any, len(helm.ScheduleAnnotationKeys)+1)
	for _, k := range helm.ScheduleAnnotationKeys {
		annotations[k] = nil // JSON null removes the annotation
	}
	for k, v := range scheduleAnnotationValues(next) {
		if v != "" {
			annotations[k] = v
		}
	}
	annotations[helm.AnnotationScheduleWarned] = nil
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": annotations}})
	if err != nil {
		return nil, err
	}
	if _, err := pods.Patch(ctx, w.Name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("%w: the workspace was stopped meanwhile", ErrWorkspaceNotRunning)
		}
		return nil, fmt.Errorf("failed to patch workspace pod %s: %w", w.Name, err)
	}
	return workspaceSchedule(next), nil
}

func scheduleAnnotationValues(s models.Schedule) map[string]string {
	return map[string]string{
		helm.AnnotationScheduleFrom:           s.From,
		helm.AnnotationScheduleTo:             s.To,
		helm.AnnotationScheduleStopGrace:      s.StopGrace,
		helm.AnnotationScheduleTerminateGrace: s.TerminateGrace,
	}
}
