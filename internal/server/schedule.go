// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	identityv1 "github.com/k8shell-io/common/pkg/api/gen/go/identity/v1"
	"github.com/k8shell-io/common/pkg/models"
	"github.com/k8shell-io/provisioner/internal/config"
	"github.com/k8shell-io/provisioner/internal/helm"
	ws "github.com/k8shell-io/provisioner/internal/workspace"
	"github.com/rs/zerolog"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// Notification categories sent through identity's NotifyUser.
const (
	notifyStopWarning = "workspace.stop-warning"
	notifyStopped     = "workspace.stopped"
	notifyTerminated  = "workspace.terminated"
)

// scheduleAction is what the enforcer does to one workspace in one pass.
type scheduleAction string

const (
	scheduleNone      scheduleAction = ""
	scheduleWarn      scheduleAction = "warn"
	scheduleStop      scheduleAction = "stop"
	scheduleTerminate scheduleAction = "terminate"
)

// planSchedule decides what to do to a workspace at now from its schedule:
//
//	now < to                     nothing
//	to <= now < stopsAt          warn the user, once (warned reports it was done)
//	stopsAt <= now               stop the pod, while there is one
//	terminatesAt <= now          terminate (delete) the workspace
//
// Injected workspaces are never acted on: stopping the pod of someone else's
// Deployment would only get it recreated. A workspace whose node is lost
// cannot be stopped, but is terminated once its deadline passes.
func planSchedule(d *models.WorkspaceDetails, warned bool, now time.Time) scheduleAction {
	if d == nil || d.Schedule == nil || d.Schedule.To == nil || d.Schedule.StopsAt == nil ||
		d.WorkspaceType == models.WorkspaceTypeInjected {
		return scheduleNone
	}
	s := d.Schedule

	if d.Status == models.WorkspaceStatusTerminating {
		return scheduleNone
	}
	if s.TerminatesAt != nil && !now.Before(*s.TerminatesAt) {
		return scheduleTerminate
	}

	switch d.Status {
	case models.WorkspaceStatusStopped, models.WorkspaceStatusStopping, models.WorkspaceStatusNodeLost:
		return scheduleNone
	}
	if !now.Before(*s.StopsAt) {
		return scheduleStop
	}
	if !now.Before(*s.To) && !warned {
		return scheduleWarn
	}
	return scheduleNone
}

// scheduleNotice is a message to the owner of a workspace.
type scheduleNotice struct {
	Username  string
	Workspace string
	Category  string
	Title     string
	Body      string
}

// fmtScheduleTime formats t in loc with the zone's abbreviation, e.g.
// "2026-11-14 19:15 CET". A nil loc is UTC.
func fmtScheduleTime(t time.Time, loc *time.Location) string {
	if loc == nil {
		loc = time.UTC
	}
	return t.In(loc).Format("2006-01-02 15:04 MST")
}

func warnNotice(d *models.WorkspaceDetails, loc *time.Location) scheduleNotice {
	s := d.Schedule
	return scheduleNotice{
		Username: d.Username, Workspace: d.Name, Category: notifyStopWarning,
		Title: fmt.Sprintf("Workspace %s will stop at %s", d.Name, fmtScheduleTime(*s.StopsAt, loc)),
		Body: fmt.Sprintf("The scheduled time of your workspace %s (blueprint %s) ended at %s.\n"+
			"It will be stopped at %s. Save your work before then.%s",
			d.Name, d.Blueprint, fmtScheduleTime(*s.To, loc), fmtScheduleTime(*s.StopsAt, loc), terminateSentence(s, loc)),
	}
}

func stoppedNotice(d *models.WorkspaceDetails, loc *time.Location) scheduleNotice {
	s := d.Schedule
	return scheduleNotice{
		Username: d.Username, Workspace: d.Name, Category: notifyStopped,
		Title: fmt.Sprintf("Workspace %s was stopped", d.Name),
		Body: fmt.Sprintf("Your workspace %s (blueprint %s) was stopped because its scheduled time ended at %s.%s "+
			"If you need access to your workspace%s, ask your administrator to extend its schedule.",
			d.Name, d.Blueprint, fmtScheduleTime(*s.To, loc), terminateSentence(s, loc), beforeDeletion(s)),
	}
}

// beforeDeletion is " before it is deleted" when the workspace is going to be
// deleted, and empty when it is kept.
func beforeDeletion(s *models.WorkspaceSchedule) string {
	if s.TerminatesAt == nil {
		return ""
	}
	return " before it is deleted"
}

func terminatedNotice(d *models.WorkspaceDetails, loc *time.Location) scheduleNotice {
	return scheduleNotice{
		Username: d.Username, Workspace: d.Name, Category: notifyTerminated,
		Title: fmt.Sprintf("Workspace %s was deleted", d.Name),
		Body: fmt.Sprintf("Your workspace %s (blueprint %s) and its data were deleted after its scheduled time ended.",
			d.Name, d.Blueprint),
	}
}

func terminateSentence(s *models.WorkspaceSchedule, loc *time.Location) string {
	if s.TerminatesAt == nil {
		return ""
	}
	return fmt.Sprintf(" The workspace and its data will be deleted at %s.", fmtScheduleTime(*s.TerminatesAt, loc))
}

// scheduleBackend is what the enforcer needs from the cluster, so that the
// enforcer itself can be tested without one.
type scheduleBackend interface {
	// List returns the workspaces with the pod of each, aligned by index.
	// It may serve from caches.
	List(ctx context.Context) ([]*models.WorkspaceDetails, []corev1.Pod, error)
	// MarkWarned records that the workspace's user is being warned. It
	// returns false when the workspace was already marked, or marked by
	// another replica first, in which case the caller must not warn.
	MarkWarned(ctx context.Context, pod *corev1.Pod) (bool, error)
	// Stop stops the workspace pod. It returns false when there was nothing
	// to stop (already stopped, or another caller holds the workspace lock).
	Stop(ctx context.Context, d *models.WorkspaceDetails) (bool, error)
	// Terminate deletes the workspace. It returns false when another caller
	// is already deleting it.
	Terminate(ctx context.Context, d *models.WorkspaceDetails, pod *corev1.Pod) (bool, error)
	// Notify sends a message to the workspace's user.
	Notify(ctx context.Context, n scheduleNotice) error
}

// scheduleEnforcer applies workspace schedules: it warns users when a
// workspace's time has ended, stops it after the stop grace period and
// terminates it after the terminate grace period. It works from timestamps
// only, so a restart or a missed pass loses nothing.
type scheduleEnforcer struct {
	backend scheduleBackend
	// mode is one of config.ScheduleEnforcement*. In dry-run mode the
	// enforcer only logs what it would do.
	mode string
	log  *zerolog.Logger
	now  func() time.Time
	// loc is the zone times are shown in, in notices.
	loc *time.Location
}

func newScheduleEnforcer(backend scheduleBackend, mode string, loc *time.Location, log *zerolog.Logger) *scheduleEnforcer {
	return &scheduleEnforcer{backend: backend, mode: mode, loc: loc, log: log, now: time.Now}
}

// Run reconciles every interval until ctx is cancelled.
func (e *scheduleEnforcer) Run(ctx context.Context, interval time.Duration) {
	e.log.Info().Str("mode", e.mode).Dur("interval", interval).Msg("Schedule enforcement started")
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.Reconcile(ctx)
		}
	}
}

// Reconcile runs one pass over all workspaces with a schedule. A failure on
// one workspace is logged and does not stop the pass.
func (e *scheduleEnforcer) Reconcile(ctx context.Context) {
	workspaces, pods, err := e.backend.List(ctx)
	if err != nil {
		e.log.Error().Err(err).Msg("Schedule enforcement: failed to list workspaces")
		return
	}
	now := e.now()
	for i, d := range workspaces {
		if ctx.Err() != nil {
			return
		}
		if d == nil || d.Schedule == nil || i >= len(pods) {
			continue
		}
		pod := &pods[i]
		warned := pod.Annotations[helm.AnnotationScheduleWarned] != ""
		e.apply(ctx, planSchedule(d, warned, now), d, pod)
	}
}

func (e *scheduleEnforcer) apply(ctx context.Context, action scheduleAction, d *models.WorkspaceDetails, pod *corev1.Pod) {
	if action == scheduleNone {
		return
	}
	log := e.log.With().Str("workspace", d.Name).Str("user", d.Username).Str("action", string(action)).Logger()

	if e.mode != config.ScheduleEnforcementEnforce {
		log.Info().Msg("Schedule enforcement (dry run): would act on workspace")
		return
	}

	switch action {
	case scheduleWarn:
		won, err := e.backend.MarkWarned(ctx, pod)
		if err != nil {
			log.Error().Err(err).Msg("Failed to mark workspace as warned")
			return
		}
		if !won {
			return
		}
		e.notify(ctx, &log, warnNotice(d, e.loc))
		log.Info().Msg("Warned user that the workspace is about to stop")

	case scheduleStop:
		stopped, err := e.backend.Stop(ctx, d)
		if err != nil {
			if errors.Is(err, ws.ErrNodeUnreachable) {
				log.Warn().Err(err).Msg("Not stopping workspace")
				return
			}
			log.Error().Err(err).Msg("Failed to stop workspace")
			return
		}
		if !stopped {
			return
		}
		e.notify(ctx, &log, stoppedNotice(d, e.loc))
		log.Info().Msg("Stopped workspace as its schedule ended")

	case scheduleTerminate:
		terminated, err := e.backend.Terminate(ctx, d, pod)
		if err != nil {
			log.Error().Err(err).Msg("Failed to terminate workspace")
			return
		}
		if !terminated {
			return
		}
		e.notify(ctx, &log, terminatedNotice(d, e.loc))
		log.Info().Msg("Terminated workspace as its schedule ended")
	}
}

// notify sends n. A failure is logged and otherwise ignored: the action it
// reports has been taken, and the notice is best effort.
func (e *scheduleEnforcer) notify(ctx context.Context, log *zerolog.Logger, n scheduleNotice) {
	if err := e.backend.Notify(ctx, n); err != nil {
		log.Warn().Err(err).Str("category", n.Category).Msg("Failed to notify user")
	}
}

// provisionerScheduleBackend implements scheduleBackend on the cluster, with
// the same steps the StopWorkspace and DeleteUserWorkspaces RPCs take.
type provisionerScheduleBackend struct {
	p *ProvisionerService
}

func (b *provisionerScheduleBackend) List(ctx context.Context) ([]*models.WorkspaceDetails, []corev1.Pod, error) {
	// Only standalone workspaces: InjectNamespaces is left empty. The listing
	// is served from the caches since it only picks candidates; every action
	// below re-checks the live state.
	res, err := ws.GetWorkspaces(ctx, b.p.server.helm, ws.GetWorkspacesOptions{UseCache: true})
	if err != nil {
		return nil, nil, err
	}
	return res.Workspaces, res.Pods, nil
}

func (b *provisionerScheduleBackend) MarkWarned(ctx context.Context, pod *corev1.Pod) (bool, error) {
	return markPodWarned(ctx, b.p.server.helm.KubeClient(), pod.Namespace, pod.Name, time.Now())
}

// markPodWarned stamps helm.AnnotationScheduleWarned on the pod. It returns
// true only for the caller that stamped it: false when the pod is gone, was
// already stamped, or was stamped by another replica between the read and the
// patch.
func markPodWarned(ctx context.Context, kube kubernetes.Interface, namespace, name string, now time.Time) (bool, error) {
	pods := kube.CoreV1().Pods(namespace)
	live, err := pods.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	if live.Annotations[helm.AnnotationScheduleWarned] != "" {
		return false, nil
	}
	// The resourceVersion makes the patch fail if anything changed the pod
	// since the read, in particular another replica marking it first.
	patch := fmt.Appendf(nil, `{"metadata":{"resourceVersion":%q,"annotations":{%q:%q}}}`,
		live.ResourceVersion, helm.AnnotationScheduleWarned, now.UTC().Format(time.RFC3339))
	if _, err := pods.Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (b *provisionerScheduleBackend) Stop(ctx context.Context, d *models.WorkspaceDetails) (bool, error) {
	w, err := ws.NewWorkspaceFromHelmRelease(ctx, d.Name, b.p.server.helm, b.p.server.Identity, b.p.server.config)
	if err != nil {
		return false, err
	}
	if err := w.EnsureNodeReachable(ctx); err != nil {
		return false, err
	}

	lockCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	lock := w.CreateLock()
	acquired, err := lock.TryAcquire(lockCtx)
	if err != nil {
		if errors.Is(err, ws.ErrLockAlreadyHeld) {
			return false, nil
		}
		return false, fmt.Errorf("acquire lock: %w", err)
	}
	if !acquired {
		return false, nil
	}
	defer func() {
		if err := lock.Release(context.WithoutCancel(ctx)); err != nil {
			b.p.log.Error().Err(err).Msgf("Failed to release lock after scheduled stop of workspace %s", d.Name)
		}
	}()

	// Re-check under the lock: the listing may be cached, and a user or
	// another replica may have stopped the workspace meanwhile.
	st, err := w.GetPodStatus(ctx)
	if err != nil {
		if errors.Is(err, models.ErrWorkspaceNotFound) {
			return false, nil
		}
		return false, err
	}
	switch st.Status {
	case models.WorkspaceStatusStopped, models.WorkspaceStatusStopping, models.WorkspaceStatusTerminating:
		return false, nil
	}

	if err := w.DeletePAT(ctx); err != nil {
		b.p.log.Error().Err(err).Msgf("Failed to delete PAT for workspace %s", d.Name)
	}
	if err := w.StopPod(ctx); err != nil {
		return false, err
	}
	return true, nil
}

func (b *provisionerScheduleBackend) Terminate(ctx context.Context, d *models.WorkspaceDetails, pod *corev1.Pod) (bool, error) {
	// The listing may be cached. Deleting is not undoable, so look at the
	// workspace's schedule as it is now: an administrator may just have
	// force-started it with a new expiry.
	fresh, err := ws.GetWorkspaces(ctx, b.p.server.helm, ws.GetWorkspacesOptions{WorkspaceName: d.Name})
	if err != nil {
		return false, err
	}
	if len(fresh.Workspaces) == 0 || planSchedule(fresh.Workspaces[0], true, time.Now()) != scheduleTerminate {
		return false, nil
	}

	if err := ws.DeleteWorkspacePod(ctx, b.p.server.helm, b.p.server.Identity, pod); err != nil {
		if errors.Is(err, ws.ErrLockAlreadyHeld) {
			return false, nil
		}
		return false, err
	}
	key := pod.Labels[helm.LabelCanonicalId]
	if key == "" {
		key = pod.Name
	}
	b.p.clearThreatDetection(key)
	return true, nil
}

func (b *provisionerScheduleBackend) Notify(ctx context.Context, n scheduleNotice) error {
	if b.p.server.Identity == nil {
		return errors.New("no identity client")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := b.p.server.Identity.NotifyUser(ctx, &identityv1.NotifyUserRequest{
		Username:  n.Username,
		Title:     n.Title,
		Body:      n.Body,
		Category:  n.Category,
		Workspace: n.Workspace,
	})
	return err
}

// startScheduleEnforcement starts the schedule enforcer in the background.
//
// TEMPORARY: enforcement is hard-wired on, in enforce mode and at the default
// interval, whatever schedule.enforcement and schedule.interval say, so it can
// be tested without a config change. Remove this override to make the
// configuration decide again; the commented code below is the real logic.
func (s *Server) startScheduleEnforcement(ctx context.Context) {
	mode, interval := config.ScheduleEnforcementEnforce, config.DefaultScheduleInterval

	// cfg := s.config.Schedule
	// if cfg.Enforcement == "" || cfg.Enforcement == config.ScheduleEnforcementOff {
	// 	return
	// }
	// mode, interval := cfg.Enforcement, cfg.Interval

	backend := &provisionerScheduleBackend{p: NewProvisionerService(s)}
	enforcer := newScheduleEnforcer(backend, mode, s.config.Schedule.Location, s.log)
	go enforcer.Run(ctx, interval)
}
