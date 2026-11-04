// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	provisionerv1 "github.com/k8shell-io/common/pkg/api/gen/go/provisioner/v1"
	"github.com/k8shell-io/common/pkg/models"
	"github.com/k8shell-io/provisioner/internal/config"
	"github.com/k8shell-io/provisioner/internal/helm"
	ws "github.com/k8shell-io/provisioner/internal/workspace"
	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

var (
	schedTo        = time.Date(2026, 11, 10, 18, 0, 0, 0, time.UTC)
	schedStopsAt   = schedTo.Add(15 * time.Minute)
	schedTerminate = schedStopsAt.Add(7 * 24 * time.Hour)
)

func schedWorkspace(name string, status models.WorkspaceStatusMessage) *models.WorkspaceDetails {
	to, stops, term := schedTo, schedStopsAt, schedTerminate
	return &models.WorkspaceDetails{
		WorkspaceStatus: models.WorkspaceStatus{Status: status},
		Name:            name,
		Username:        "alice",
		Blueprint:       "dev",
		WorkspaceType:   models.WorkspaceTypeStandalone,
		Schedule: &models.WorkspaceSchedule{
			To: &to, StopsAt: &stops, TerminatesAt: &term,
			StopGrace: "15m", TerminateGrace: "7d",
		},
	}
}

func TestPlanSchedule(t *testing.T) {
	running, stopped := models.WorkspaceStatusRunning, models.WorkspaceStatusStopped

	tests := []struct {
		name   string
		mutate func(*models.WorkspaceDetails)
		status models.WorkspaceStatusMessage
		warned bool
		now    time.Time
		want   scheduleAction
	}{
		{"before to", nil, running, false, schedTo.Add(-time.Second), scheduleNone},
		{"at to warns", nil, running, false, schedTo, scheduleWarn},
		{"in grace warns", nil, running, false, schedTo.Add(5 * time.Minute), scheduleWarn},
		{"in grace already warned", nil, running, true, schedTo.Add(5 * time.Minute), scheduleNone},
		{"at stopsAt stops", nil, running, true, schedStopsAt, scheduleStop},
		{"after stopsAt stops even if never warned", nil, running, false, schedStopsAt.Add(time.Hour), scheduleStop},
		{"starting workspace is stopped too", nil, models.WorkspaceStatusProvisioning, false, schedStopsAt, scheduleStop},
		{"already stopped waits", nil, stopped, false, schedStopsAt.Add(time.Hour), scheduleNone},
		{"stopped is terminated at deadline", nil, stopped, false, schedTerminate, scheduleTerminate},
		{"running past deadline is terminated", nil, running, false, schedTerminate.Add(time.Hour), scheduleTerminate},
		{"stopping waits", nil, models.WorkspaceStatusStopping, false, schedStopsAt.Add(time.Minute), scheduleNone},
		{"node lost is not stopped", nil, models.WorkspaceStatusNodeLost, false, schedStopsAt.Add(time.Minute), scheduleNone},
		{"node lost is terminated at deadline", nil, models.WorkspaceStatusNodeLost, false, schedTerminate, scheduleTerminate},
		{"terminating is left alone", nil, models.WorkspaceStatusTerminating, false, schedTerminate.Add(time.Hour), scheduleNone},
		{"no terminate grace never terminates", func(d *models.WorkspaceDetails) { d.Schedule.TerminatesAt = nil },
			stopped, false, schedTerminate.Add(1000 * time.Hour), scheduleNone},
		{"no stop grace stops at to without warning", func(d *models.WorkspaceDetails) {
			s := schedTo
			d.Schedule.StopsAt = &s
		}, running, false, schedTo, scheduleStop},
		{"no to means no schedule", func(d *models.WorkspaceDetails) { d.Schedule.To = nil }, running, false, schedTerminate, scheduleNone},
		{"no schedule", func(d *models.WorkspaceDetails) { d.Schedule = nil }, running, false, schedTerminate, scheduleNone},
		{"injected is never acted on", func(d *models.WorkspaceDetails) { d.WorkspaceType = models.WorkspaceTypeInjected },
			running, false, schedTerminate, scheduleNone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := schedWorkspace("ws", tt.status)
			if tt.mutate != nil {
				tt.mutate(d)
			}
			if got := planSchedule(d, tt.warned, tt.now); got != tt.want {
				t.Fatalf("planSchedule = %q, want %q", got, tt.want)
			}
		})
	}

	if got := planSchedule(nil, false, schedTo); got != scheduleNone {
		t.Fatalf("nil workspace: got %q", got)
	}
}

// fakeScheduleBackend records what the enforcer does.
type fakeScheduleBackend struct {
	workspaces []*models.WorkspaceDetails
	warned     map[string]bool // pods already carrying the warned annotation

	listErr      error
	markWon      map[string]bool // workspace -> result of MarkWarned (default true)
	stopResult   map[string]bool // workspace -> Stop's first result (default true)
	stopErr      map[string]error
	terminateErr map[string]error
	notifyErr    error

	marked, stopped, terminated []string
	notices                     []scheduleNotice
}

func (f *fakeScheduleBackend) List(context.Context) ([]*models.WorkspaceDetails, []corev1.Pod, error) {
	if f.listErr != nil {
		return nil, nil, f.listErr
	}
	pods := make([]corev1.Pod, len(f.workspaces))
	for i, d := range f.workspaces {
		if d == nil {
			continue
		}
		pods[i] = corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: d.Name, Annotations: map[string]string{}}}
		if f.warned[d.Name] {
			pods[i].Annotations[helm.AnnotationScheduleWarned] = "2026-11-10T18:00:00Z"
		}
	}
	return f.workspaces, pods, nil
}

func (f *fakeScheduleBackend) MarkWarned(_ context.Context, pod *corev1.Pod) (bool, error) {
	f.marked = append(f.marked, pod.Name)
	if won, ok := f.markWon[pod.Name]; ok {
		return won, nil
	}
	return true, nil
}

func (f *fakeScheduleBackend) Stop(_ context.Context, d *models.WorkspaceDetails) (bool, error) {
	if err := f.stopErr[d.Name]; err != nil {
		return false, err
	}
	f.stopped = append(f.stopped, d.Name)
	if r, ok := f.stopResult[d.Name]; ok {
		return r, nil
	}
	return true, nil
}

func (f *fakeScheduleBackend) Terminate(_ context.Context, d *models.WorkspaceDetails, _ *corev1.Pod) (bool, error) {
	if err := f.terminateErr[d.Name]; err != nil {
		return false, err
	}
	f.terminated = append(f.terminated, d.Name)
	return true, nil
}

func (f *fakeScheduleBackend) Notify(_ context.Context, n scheduleNotice) error {
	f.notices = append(f.notices, n)
	return f.notifyErr
}

func newTestEnforcer(b *fakeScheduleBackend, mode string, now time.Time) *scheduleEnforcer {
	log := zerolog.Nop()
	e := newScheduleEnforcer(b, mode, &log)
	e.now = func() time.Time { return now }
	return e
}

func categories(ns []scheduleNotice) []string {
	var out []string
	for _, n := range ns {
		out = append(out, n.Workspace+":"+n.Category)
	}
	return out
}

func TestEnforcerDryRunDoesNothing(t *testing.T) {
	b := &fakeScheduleBackend{workspaces: []*models.WorkspaceDetails{
		schedWorkspace("warn", models.WorkspaceStatusRunning),
		schedWorkspace("stop", models.WorkspaceStatusRunning),
		schedWorkspace("term", models.WorkspaceStatusStopped),
	}}
	// at terminate time every workspace has something to do
	newTestEnforcer(b, config.ScheduleEnforcementDryRun, schedTerminate).Reconcile(context.Background())

	if len(b.marked)+len(b.stopped)+len(b.terminated)+len(b.notices) != 0 {
		t.Fatalf("dry run acted: %+v", b)
	}
}

func TestEnforcerWarnsOnce(t *testing.T) {
	b := &fakeScheduleBackend{workspaces: []*models.WorkspaceDetails{schedWorkspace("ws", models.WorkspaceStatusRunning)}}
	e := newTestEnforcer(b, config.ScheduleEnforcementEnforce, schedTo.Add(time.Minute))

	e.Reconcile(context.Background())
	if got := categories(b.notices); len(got) != 1 || got[0] != "ws:"+notifyStopWarning {
		t.Fatalf("notices = %v", got)
	}
	n := b.notices[0]
	if n.Username != "alice" || !strings.Contains(n.Title, "ws") || !strings.Contains(n.Body, "2026-11-10 18:15 UTC") {
		t.Fatalf("notice = %+v", n)
	}

	// the next pass sees the annotation the first one stamped
	b.warned = map[string]bool{"ws": true}
	e.Reconcile(context.Background())
	if len(b.notices) != 1 || len(b.marked) != 1 {
		t.Fatalf("warned again: marked %v notices %v", b.marked, categories(b.notices))
	}
}

func TestEnforcerDoesNotWarnWhenAnotherReplicaDid(t *testing.T) {
	b := &fakeScheduleBackend{
		workspaces: []*models.WorkspaceDetails{schedWorkspace("ws", models.WorkspaceStatusRunning)},
		markWon:    map[string]bool{"ws": false},
	}
	newTestEnforcer(b, config.ScheduleEnforcementEnforce, schedTo.Add(time.Minute)).Reconcile(context.Background())
	if len(b.notices) != 0 {
		t.Fatalf("notified despite losing the mark: %v", categories(b.notices))
	}
}

func TestEnforcerStops(t *testing.T) {
	b := &fakeScheduleBackend{workspaces: []*models.WorkspaceDetails{schedWorkspace("ws", models.WorkspaceStatusRunning)}}
	newTestEnforcer(b, config.ScheduleEnforcementEnforce, schedStopsAt).Reconcile(context.Background())

	if len(b.stopped) != 1 || b.stopped[0] != "ws" {
		t.Fatalf("stopped = %v", b.stopped)
	}
	if got := categories(b.notices); len(got) != 1 || got[0] != "ws:"+notifyStopped {
		t.Fatalf("notices = %v", got)
	}
	if !strings.Contains(b.notices[0].Body, "2026-11-17 18:15 UTC") {
		t.Fatalf("stop notice should name the deletion time: %q", b.notices[0].Body)
	}
}

func TestEnforcerNoNoticeWhenNothingWasStopped(t *testing.T) {
	b := &fakeScheduleBackend{
		workspaces: []*models.WorkspaceDetails{schedWorkspace("ws", models.WorkspaceStatusRunning)},
		stopResult: map[string]bool{"ws": false}, // someone else got there first
	}
	newTestEnforcer(b, config.ScheduleEnforcementEnforce, schedStopsAt).Reconcile(context.Background())
	if len(b.notices) != 0 {
		t.Fatalf("notified for a stop it did not do: %v", categories(b.notices))
	}
}

func TestEnforcerTerminates(t *testing.T) {
	b := &fakeScheduleBackend{workspaces: []*models.WorkspaceDetails{schedWorkspace("ws", models.WorkspaceStatusStopped)}}
	newTestEnforcer(b, config.ScheduleEnforcementEnforce, schedTerminate).Reconcile(context.Background())

	if len(b.terminated) != 1 {
		t.Fatalf("terminated = %v", b.terminated)
	}
	if got := categories(b.notices); len(got) != 1 || got[0] != "ws:"+notifyTerminated {
		t.Fatalf("notices = %v", got)
	}
}

func TestEnforcerOneFailureDoesNotStopThePass(t *testing.T) {
	b := &fakeScheduleBackend{
		workspaces: []*models.WorkspaceDetails{
			schedWorkspace("lost", models.WorkspaceStatusRunning),
			schedWorkspace("broken", models.WorkspaceStatusRunning),
			schedWorkspace("fine", models.WorkspaceStatusRunning),
		},
		stopErr: map[string]error{
			"lost":   fmt.Errorf("%w: node gone", ws.ErrNodeUnreachable),
			"broken": errors.New("boom"),
		},
	}
	newTestEnforcer(b, config.ScheduleEnforcementEnforce, schedStopsAt).Reconcile(context.Background())

	if len(b.stopped) != 1 || b.stopped[0] != "fine" {
		t.Fatalf("stopped = %v, want only the healthy workspace", b.stopped)
	}
	if got := categories(b.notices); len(got) != 1 || got[0] != "fine:"+notifyStopped {
		t.Fatalf("notices = %v: failed stops must not notify", got)
	}
}

func TestEnforcerNotifyFailureIsNotFatal(t *testing.T) {
	b := &fakeScheduleBackend{
		workspaces: []*models.WorkspaceDetails{
			schedWorkspace("a", models.WorkspaceStatusRunning),
			schedWorkspace("b", models.WorkspaceStatusRunning),
		},
		notifyErr: errors.New("identity unavailable"),
	}
	newTestEnforcer(b, config.ScheduleEnforcementEnforce, schedStopsAt).Reconcile(context.Background())
	if len(b.stopped) != 2 {
		t.Fatalf("stopped = %v, want both", b.stopped)
	}
}

func TestEnforcerListErrorAndNoSchedule(t *testing.T) {
	e := newTestEnforcer(&fakeScheduleBackend{listErr: errors.New("boom")}, config.ScheduleEnforcementEnforce, schedTerminate)
	e.Reconcile(context.Background()) // must not panic

	plain := &models.WorkspaceDetails{Name: "plain", Username: "bob", WorkspaceType: models.WorkspaceTypeStandalone}
	b := &fakeScheduleBackend{workspaces: []*models.WorkspaceDetails{plain, nil}}
	newTestEnforcer(b, config.ScheduleEnforcementEnforce, schedTerminate).Reconcile(context.Background())
	if len(b.stopped)+len(b.terminated)+len(b.marked) != 0 {
		t.Fatalf("acted on workspaces without a schedule: %+v", b)
	}
}

func TestScheduleNoticesWithoutTerminateGrace(t *testing.T) {
	d := schedWorkspace("ws", models.WorkspaceStatusRunning)
	d.Schedule.TerminatesAt = nil
	for _, n := range []scheduleNotice{warnNotice(d), stoppedNotice(d)} {
		if strings.Contains(n.Body, "will be deleted") {
			t.Errorf("%s notice promises a deletion that is not scheduled: %q", n.Category, n.Body)
		}
	}
}

func TestMarkPodWarned(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 10, 18, 1, 0, 0, time.UTC)
	newPod := func(annotations map[string]string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "ws", Namespace: "ns", Annotations: annotations, ResourceVersion: "7"}}
	}

	t.Run("stamps once", func(t *testing.T) {
		kube := fake.NewClientset(newPod(nil))
		won, err := markPodWarned(ctx, kube, "ns", "ws", now)
		if err != nil || !won {
			t.Fatalf("first call = %v, %v; want true", won, err)
		}
		pod, _ := kube.CoreV1().Pods("ns").Get(ctx, "ws", metav1.GetOptions{})
		if got := pod.Annotations[helm.AnnotationScheduleWarned]; got != "2026-11-10T18:01:00Z" {
			t.Fatalf("annotation = %q", got)
		}
		won, err = markPodWarned(ctx, kube, "ns", "ws", now)
		if err != nil || won {
			t.Fatalf("second call = %v, %v; want false", won, err)
		}
	})

	t.Run("keeps other annotations", func(t *testing.T) {
		kube := fake.NewClientset(newPod(map[string]string{"keep": "me"}))
		if won, err := markPodWarned(ctx, kube, "ns", "ws", now); err != nil || !won {
			t.Fatalf("got %v, %v", won, err)
		}
		pod, _ := kube.CoreV1().Pods("ns").Get(ctx, "ws", metav1.GetOptions{})
		if pod.Annotations["keep"] != "me" {
			t.Fatalf("annotations = %v", pod.Annotations)
		}
	})

	t.Run("pod gone", func(t *testing.T) {
		won, err := markPodWarned(ctx, fake.NewClientset(), "ns", "ws", now)
		if err != nil || won {
			t.Fatalf("got %v, %v; want false, nil", won, err)
		}
	})

	t.Run("lost the race to another replica", func(t *testing.T) {
		kube := fake.NewClientset(newPod(nil))
		kube.PrependReactor("patch", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "pods"}, "ws", errors.New("changed"))
		})
		won, err := markPodWarned(ctx, kube, "ns", "ws", now)
		if err != nil || won {
			t.Fatalf("got %v, %v; want false, nil", won, err)
		}
	})

	t.Run("other errors are returned", func(t *testing.T) {
		kube := fake.NewClientset(newPod(nil))
		kube.PrependReactor("patch", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("api down")
		})
		if _, err := markPodWarned(ctx, kube, "ns", "ws", now); err == nil {
			t.Fatal("expected the error")
		}
	})
}

func TestSetWorkspaceScheduleValidatesTheRequest(t *testing.T) {
	log := zerolog.Nop()
	p := &ProvisionerService{log: &log} // validation fails before anything uses the cluster
	s := func(v string) *string { return &v }

	tests := []struct {
		name string
		req  *provisionerv1.SetWorkspaceScheduleRequest
	}{
		{"no workspace", &provisionerv1.SetWorkspaceScheduleRequest{To: s("2026-12-01T00:00:00Z")}},
		{"nothing to change", &provisionerv1.SetWorkspaceScheduleRequest{Workspace: "ws"}},
		{"revert together with values", &provisionerv1.SetWorkspaceScheduleRequest{Workspace: "ws", Revert: true, StopGrace: s("5m")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := p.SetWorkspaceSchedule(context.Background(), tt.req)
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("code = %v (%v), want InvalidArgument", status.Code(err), err)
			}
		})
	}
}

func TestConvertScheduleErrors(t *testing.T) {
	tests := []struct {
		err  error
		want codes.Code
	}{
		{fmt.Errorf("%w: the workspace schedule ended", ws.ErrScheduleNotAllowed), codes.InvalidArgument},
		{fmt.Errorf("%w: schedule.to is not in the future", models.ErrInvalidParameters), codes.InvalidArgument},
		{fmt.Errorf("%w: the schedule can only be changed while running", ws.ErrWorkspaceNotRunning), codes.FailedPrecondition},
	}
	for _, tt := range tests {
		if got := status.Code(convertToGRPCError(tt.err)); got != tt.want {
			t.Errorf("%v: code = %v, want %v", tt.err, got, tt.want)
		}
	}
}
