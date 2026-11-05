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

// scheduleGateError is ErrScheduleNotAllowed with the reason. ended is true
// when the schedule's "to" has passed, false when "from" has not been reached.
type scheduleGateError struct {
	ended  bool
	detail string
}

func (e *scheduleGateError) Error() string { return ErrScheduleNotAllowed.Error() + ": " + e.detail }

// Is makes errors.Is(err, ErrScheduleNotAllowed) true.
func (e *scheduleGateError) Is(target error) bool { return target == ErrScheduleNotAllowed }

// ScheduleEnded reports whether err is an ErrScheduleNotAllowed because the
// schedule's expiry has passed, as opposed to its start not yet being reached.
func ScheduleEnded(err error) bool {
	var g *scheduleGateError
	return errors.As(err, &g) && g.ended
}

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
	parsed, err := models.ParseSchedule(w.blueprint.Schedule, w.scheduleLocation())
	if err != nil {
		w.log.Warn().Err(err).Msgf("Ignoring invalid schedule of workspace %s", w.Name)
		return nil
	}
	return checkParsedSchedule(parsed, now)
}

// scheduleValues is the schedule as it goes into the chart values and the
// stored release: every time resolved to an absolute RFC 3339 timestamp.
func (w *Workspace) scheduleValues(s *models.Schedule) (map[string]interface{}, error) {
	norm, err := s.Normalize(w.scheduleLocation())
	if err != nil {
		return nil, fmt.Errorf("invalid schedule: %w", err)
	}
	values, err := toMap(norm)
	if err != nil {
		return nil, fmt.Errorf("failed to convert schedule to map: %w", err)
	}
	return values, nil
}

// forceStartDuration is the configured run time of a force-started workspace
// without an expiry of its own; zero when not configured.
func (w *Workspace) forceStartDuration() time.Duration {
	if w.config == nil {
		return 0
	}
	return w.config.Schedule.ForceStartDuration
}

// scheduleLocation is the zone schedule times without an offset are read in
// by default: the configured one, or the system's local zone.
func (w *Workspace) scheduleLocation() *time.Location {
	if w.config != nil && w.config.Schedule.Location != nil {
		return w.config.Schedule.Location
	}
	return time.Local
}

// formatScheduleTime shows t in loc, the zone of the schedule, with the zone's
// abbreviation, e.g. "2026-11-05 11:35 CET". A nil loc is UTC.
func formatScheduleTime(t time.Time, loc *time.Location) string {
	if loc == nil {
		loc = time.UTC
	}
	return t.In(loc).Format("2006-01-02 15:04 MST")
}

func checkParsedSchedule(s *models.ParsedSchedule, now time.Time) error {
	if s == nil {
		return nil
	}
	if s.From != nil && now.Before(*s.From) {
		return &scheduleGateError{detail: "the workspace cannot run before " + formatScheduleTime(*s.From, s.Location)}
	}
	if s.To != nil && !now.Before(*s.To) {
		return &scheduleGateError{ended: true, detail: "the workspace schedule ended at " + formatScheduleTime(*s.To, s.Location)}
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
	// The stamped times are absolute, so the zone does not matter.
	parsed, err := models.ParseSchedule(&raw, time.UTC)
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
	// The stamped times are absolute; the zone only decides how they are shown.
	var bpSchedule *models.Schedule
	if w.blueprint != nil {
		bpSchedule = w.blueprint.Schedule
	}
	loc, err := bpSchedule.Location(w.scheduleLocation())
	if err != nil {
		loc = w.scheduleLocation()
	}
	parsed, err := models.ParseSchedule(&raw, loc)
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
// Such a rejection wraps models.ErrInvalidParameters. A "to" without an offset
// is read in the blueprint's timezone, or the default one.
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

	var bpSchedule *models.Schedule
	if w.blueprint != nil {
		bpSchedule = w.blueprint.Schedule
	}
	loc, err := bpSchedule.Location(w.scheduleLocation())
	if err != nil {
		return nil, fmt.Errorf("%w: %v", models.ErrInvalidParameters, err)
	}

	var next models.Schedule
	var override string
	if ch.Revert {
		if bpSchedule != nil {
			norm, err := bpSchedule.Normalize(loc)
			if err != nil {
				return nil, fmt.Errorf("%w: %v", models.ErrInvalidParameters, err)
			}
			next = *norm
			next.Timezone = "" // not stamped on the pod
		}
	} else {
		next = helm.ScheduleFromAnnotations(pod.Annotations)
		if ch.To != nil {
			to := models.Schedule{To: *ch.To}
			norm, err := to.Normalize(loc)
			if err != nil {
				return nil, fmt.Errorf("%w: %v", models.ErrInvalidParameters, err)
			}
			next.To = norm.To
		}
		if ch.StopGrace != nil {
			next.StopGrace = *ch.StopGrace
		}
		if ch.TerminateGrace != nil {
			next.TerminateGrace = *ch.TerminateGrace
		}
		parsed, err := models.ParseSchedule(&next, loc)
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

// ErrForceNeedsExpiry is returned when a workspace whose schedule has ended is
// to be force-started without a new expiry and no default one is configured
// (schedule.forceStartDuration): it would be stopped again at once.
var ErrForceNeedsExpiry = fmt.Errorf("%w: the schedule of this workspace has ended, so a forced start needs a new expiry "+
	"(an empty one removes the expiry)", models.ErrInvalidParameters)

// PrepareForcedSchedule works out the schedule a force-started workspace runs
// with. to is the new expiry: nil keeps the current schedule, "" removes the
// expiry, anything else is a time (absolute, or local in the blueprint's zone
// or the default one) that must lie in the future. Without a new expiry, a
// workspace whose current one has passed gets one the configured
// schedule.forceStartDuration from now, or is rejected with ErrForceNeedsExpiry
// when none is configured. It returns nil when the schedule stays as it is, otherwise the whole
// effective schedule to store as the override. Other rejections wrap
// models.ErrInvalidParameters.
func (w *Workspace) PrepareForcedSchedule(ctx context.Context, to *string, now time.Time) (*models.Schedule, error) {
	rp, err := w.client.DeployedReleasePod(ctx, w.Name)
	if err != nil {
		return nil, err
	}
	cur := helm.ScheduleFromAnnotations(rp.Pod.Annotations)

	var bpSchedule *models.Schedule
	if w.blueprint != nil {
		bpSchedule = w.blueprint.Schedule
	}
	loc, err := bpSchedule.Location(w.scheduleLocation())
	if err != nil {
		return nil, fmt.Errorf("%w: %v", models.ErrInvalidParameters, err)
	}

	if to == nil {
		parsed, err := models.ParseSchedule(&cur, loc)
		if err != nil || parsed.To == nil || now.Before(*parsed.To) {
			return nil, nil // not ended: starts as it is
		}
		d := w.forceStartDuration()
		if d <= 0 {
			return nil, ErrForceNeedsExpiry
		}
		next := cur
		next.To = now.Add(d).In(loc).Format(time.RFC3339)
		return &next, nil
	}

	next := cur
	next.To = ""
	if *to != "" {
		norm, err := (&models.Schedule{To: *to}).Normalize(loc)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", models.ErrInvalidParameters, err)
		}
		next.To = norm.To
	}
	parsed, err := models.ParseSchedule(&next, loc)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", models.ErrInvalidParameters, err)
	}
	if parsed.To != nil && !parsed.To.After(now) {
		return nil, fmt.Errorf("%w: the new expiry %q is not in the future", models.ErrInvalidParameters, *to)
	}
	return &next, nil
}

// applyForcedSchedule stores forced as the workspace's schedule override on its
// release and applies it to pod, which is about to be created from the
// release's manifest. Done under the workspace lock, before the pod exists, so
// that the schedule enforcer never sees the started pod with the old expiry.
func (w *Workspace) applyForcedSchedule(ctx context.Context, pod *corev1.Pod, forced *models.Schedule) error {
	raw, err := helm.ScheduleOverrideJSON(*forced)
	if err != nil {
		return err
	}
	if err := w.client.SetScheduleOverride(ctx, w.Name, raw); err != nil {
		return fmt.Errorf("failed to store the schedule of force-started workspace %s: %w", w.Name, err)
	}
	if err := helm.ApplyScheduleOverride(pod, raw); err != nil {
		return err
	}
	w.log.Warn().Str("to", forced.To).Msgf("Workspace %s is force-started past its schedule", w.Name)
	return nil
}
