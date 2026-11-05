// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package workspace

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/k8shell-io/common/pkg/models"
	"github.com/k8shell-io/provisioner/internal/config"
	"github.com/k8shell-io/provisioner/internal/helm"
	"github.com/rs/zerolog"
	"helm.sh/helm/v3/pkg/release"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

func TestCheckSchedule(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	nop := zerolog.Nop()

	tests := []struct {
		name     string
		schedule *models.Schedule
		wantErr  string // substring; "" for allowed
	}{
		{"no schedule", nil, ""},
		{"empty schedule", &models.Schedule{}, ""},
		{"inside from and to", &models.Schedule{From: "2026-11-10T08:00:00Z", To: "2026-11-10T18:00:00Z"}, ""},
		{"only from, passed", &models.Schedule{From: "2026-11-01T00:00:00Z"}, ""},
		{"only to, ahead", &models.Schedule{To: "2026-12-01T00:00:00Z"}, ""},
		{"before from", &models.Schedule{From: "2026-11-11T08:00:00Z"}, "cannot run before 2026-11-11 08:00 UTC"},
		{"at to", &models.Schedule{To: "2026-11-10T12:00:00Z"}, "ended at 2026-11-10 12:00 UTC"},
		{"after to", &models.Schedule{To: "2026-11-10T11:59:59Z"}, "ended at"},
		{"to given with an offset", &models.Schedule{To: "2026-11-10T12:30:00+01:00"}, "ended at 2026-11-10 11:30 UTC"},
		{"unparsable is allowed", &models.Schedule{To: "tomorrow"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// the zone the message shows times in; pinned so the test does not
			// depend on the machine's own
			cfg := &config.Config{Schedule: config.ScheduleConfig{Location: time.UTC}}
			w := &Workspace{Name: "ws", log: &nop, config: cfg, blueprint: &models.Blueprint{Schedule: tt.schedule}}
			err := w.CheckSchedule(now)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("expected allowed, got %v", err)
				}
				return
			}
			if !errors.Is(err, ErrScheduleNotAllowed) {
				t.Fatalf("expected ErrScheduleNotAllowed, got %v", err)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %q should contain %q", err, tt.wantErr)
			}
		})
	}

	t.Run("nil blueprint", func(t *testing.T) {
		w := &Workspace{Name: "ws", log: &nop}
		if err := w.CheckSchedule(now); err != nil {
			t.Fatalf("expected allowed, got %v", err)
		}
	})
}

func podWithAnnotations(a map[string]string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: a}}
}

func TestScheduleFromPod(t *testing.T) {
	t.Run("none", func(t *testing.T) {
		if got := scheduleFromPod(podWithAnnotations(nil)); got != nil {
			t.Fatalf("expected nil, got %+v", got)
		}
	})

	t.Run("full", func(t *testing.T) {
		got := scheduleFromPod(podWithAnnotations(map[string]string{
			helm.AnnotationScheduleFrom:           "2026-11-10T08:00:00Z",
			helm.AnnotationScheduleTo:             "2026-11-10T18:00:00Z",
			helm.AnnotationScheduleStopGrace:      "15m",
			helm.AnnotationScheduleTerminateGrace: "7d",
		}))
		if got == nil {
			t.Fatal("expected a schedule")
		}
		if got.From == nil || !got.From.Equal(time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)) {
			t.Errorf("from = %v", got.From)
		}
		if got.To == nil || !got.To.Equal(time.Date(2026, 11, 10, 18, 0, 0, 0, time.UTC)) {
			t.Errorf("to = %v", got.To)
		}
		if got.StopsAt == nil || !got.StopsAt.Equal(time.Date(2026, 11, 10, 18, 15, 0, 0, time.UTC)) {
			t.Errorf("stopsAt = %v, want to + 15m", got.StopsAt)
		}
		if got.TerminatesAt == nil || !got.TerminatesAt.Equal(time.Date(2026, 11, 17, 18, 15, 0, 0, time.UTC)) {
			t.Errorf("terminatesAt = %v, want stopsAt + 7d", got.TerminatesAt)
		}
		if got.StopGrace != "15m" || got.TerminateGrace != "7d" {
			t.Errorf("graces = %q %q", got.StopGrace, got.TerminateGrace)
		}
	})

	t.Run("without to there is no stopsAt", func(t *testing.T) {
		got := scheduleFromPod(podWithAnnotations(map[string]string{helm.AnnotationScheduleStopGrace: "15m"}))
		if got == nil || got.To != nil || got.StopsAt != nil || got.TerminatesAt != nil {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("without terminate grace there is no terminatesAt", func(t *testing.T) {
		got := scheduleFromPod(podWithAnnotations(map[string]string{
			helm.AnnotationScheduleTo: "2026-11-10T18:00:00Z", helm.AnnotationScheduleStopGrace: "15m",
		}))
		if got == nil || got.StopsAt == nil || got.TerminatesAt != nil {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("unparsable is dropped", func(t *testing.T) {
		got := scheduleFromPod(podWithAnnotations(map[string]string{helm.AnnotationScheduleTo: "soon"}))
		if got != nil {
			t.Fatalf("expected nil, got %+v", got)
		}
	})

	t.Run("reaches workspace details", func(t *testing.T) {
		pod := baseWebProxyPod(t)
		pod.Annotations[helm.AnnotationScheduleTo] = "2026-11-10T18:00:00Z"
		pod.Annotations[helm.AnnotationScheduleStopGrace] = "15m"

		d := workspaceDetailsCore(pod)
		if d == nil {
			t.Fatal("workspaceDetailsCore returned nil")
		}
		if d.Schedule == nil || d.Schedule.StopsAt == nil ||
			!d.Schedule.StopsAt.Equal(time.Date(2026, 11, 10, 18, 15, 0, 0, time.UTC)) {
			t.Fatalf("schedule = %+v", d.Schedule)
		}
	})

	t.Run("workspace without schedule has none in details", func(t *testing.T) {
		d := workspaceDetailsCore(baseWebProxyPod(t))
		if d == nil || d.Schedule != nil {
			t.Fatalf("got %+v", d)
		}
	})
}

const schedTestNS = "ws-ns"

func strp(s string) *string { return &s }

// newScheduleFixture creates a workspace "ws" with a running pod carrying
// podAnnotations, and a deployed release Secret.
func newScheduleFixture(t *testing.T, podAnnotations map[string]string, bp *models.Schedule) (*Workspace, *fake.Clientset) {
	t.Helper()
	ctx := context.Background()
	kube := fake.NewClientset()
	if _, err := kube.CoreV1().Pods(schedTestNS).Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "ws", Namespace: schedTestNS, Annotations: podAnnotations}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := kube.CoreV1().Secrets(schedTestNS).Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: "sh.helm.release.v1.ws.v1", Namespace: schedTestNS,
		Labels: map[string]string{"owner": "helm", "status": "deployed", "name": "ws"}}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	nop := zerolog.Nop()
	return &Workspace{
		Name:      "ws",
		log:       &nop,
		client:    helm.NewClientWithKube(kube, schedTestNS),
		blueprint: &models.Blueprint{Schedule: bp},
	}, kube
}

func schedPod(t *testing.T, kube *fake.Clientset) *corev1.Pod {
	t.Helper()
	pod, err := kube.CoreV1().Pods(schedTestNS).Get(context.Background(), "ws", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return pod
}

func schedOverride(t *testing.T, kube *fake.Clientset) string {
	t.Helper()
	s, err := kube.CoreV1().Secrets(schedTestNS).Get(context.Background(), "sh.helm.release.v1.ws.v1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return s.Annotations[helm.AnnotationScheduleOverride]
}

var schedNow = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

func baseScheduleAnnotations() map[string]string {
	return map[string]string{
		helm.AnnotationScheduleTo:             "2026-11-10T18:00:00Z",
		helm.AnnotationScheduleStopGrace:      "15m",
		helm.AnnotationScheduleTerminateGrace: "7d",
		helm.AnnotationScheduleWarned:         "2026-11-10T18:01:00Z",
		"keep":                                "me",
	}
}

func TestSetScheduleExtendsTo(t *testing.T) {
	w, kube := newScheduleFixture(t, baseScheduleAnnotations(), nil)

	got, err := w.SetSchedule(context.Background(), ScheduleChange{To: strp("2026-11-12T18:00:00Z")}, schedNow)
	if err != nil {
		t.Fatal(err)
	}
	if got.To == nil || !got.To.Equal(time.Date(2026, 11, 12, 18, 0, 0, 0, time.UTC)) ||
		got.StopsAt == nil || !got.StopsAt.Equal(time.Date(2026, 11, 12, 18, 15, 0, 0, time.UTC)) {
		t.Fatalf("returned schedule = %+v", got)
	}

	pod := schedPod(t, kube)
	if pod.Annotations[helm.AnnotationScheduleTo] != "2026-11-12T18:00:00Z" ||
		pod.Annotations[helm.AnnotationScheduleStopGrace] != "15m" ||
		pod.Annotations[helm.AnnotationScheduleTerminateGrace] != "7d" {
		t.Fatalf("pod annotations = %v", pod.Annotations)
	}
	if _, ok := pod.Annotations[helm.AnnotationScheduleWarned]; ok {
		t.Error("the warning mark must be cleared so the user is warned for the new time")
	}
	if pod.Annotations["keep"] != "me" {
		t.Error("unrelated annotation was lost")
	}

	// stored on the release, as the whole effective schedule
	var stored models.Schedule
	if err := json.Unmarshal([]byte(schedOverride(t, kube)), &stored); err != nil {
		t.Fatalf("override: %v", err)
	}
	if stored != (models.Schedule{To: "2026-11-12T18:00:00Z", StopGrace: "15m", TerminateGrace: "7d"}) {
		t.Fatalf("stored override = %+v", stored)
	}
}

func TestSetScheduleClearsAndKeepsFields(t *testing.T) {
	w, kube := newScheduleFixture(t, baseScheduleAnnotations(), nil)

	// no expiry, no terminate grace; stop grace untouched
	got, err := w.SetSchedule(context.Background(), ScheduleChange{To: strp(""), TerminateGrace: strp("")}, schedNow)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.To != nil || got.StopsAt != nil || got.TerminatesAt != nil || got.StopGrace != "15m" {
		t.Fatalf("returned schedule = %+v", got)
	}
	pod := schedPod(t, kube)
	if _, ok := pod.Annotations[helm.AnnotationScheduleTo]; ok {
		t.Error("to should be removed")
	}
	if _, ok := pod.Annotations[helm.AnnotationScheduleTerminateGrace]; ok {
		t.Error("terminate grace should be removed")
	}
	if pod.Annotations[helm.AnnotationScheduleStopGrace] != "15m" {
		t.Error("stop grace should be kept")
	}
}

func TestSetScheduleRejections(t *testing.T) {
	tests := []struct {
		name string
		ch   ScheduleChange
		base map[string]string
	}{
		{"to in the past", ScheduleChange{To: strp("2026-11-10T11:00:00Z")}, nil},
		{"to is now", ScheduleChange{To: strp("2026-11-10T12:00:00Z")}, nil},
		{"to not a timestamp", ScheduleChange{To: strp("tomorrow")}, nil},
		{"to before from", ScheduleChange{To: strp("2026-11-15T12:00:00Z")}, map[string]string{helm.AnnotationScheduleFrom: "2026-11-20T00:00:00Z"}},
		{"bad stop grace", ScheduleChange{StopGrace: strp("soon")}, nil},
		{"negative terminate grace", ScheduleChange{TerminateGrace: strp("-1d")}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ann := baseScheduleAnnotations()
			for k, v := range tt.base {
				ann[k] = v
			}
			w, kube := newScheduleFixture(t, ann, nil)
			before := schedPod(t, kube).Annotations

			_, err := w.SetSchedule(context.Background(), tt.ch, schedNow)
			if !errors.Is(err, models.ErrInvalidParameters) {
				t.Fatalf("expected ErrInvalidParameters, got %v", err)
			}
			if !reflect.DeepEqual(schedPod(t, kube).Annotations, before) {
				t.Error("a rejected change must not touch the pod")
			}
			if schedOverride(t, kube) != "" {
				t.Error("a rejected change must not store an override")
			}
		})
	}
}

func TestSetScheduleNeedsRunningWorkspace(t *testing.T) {
	t.Run("no pod", func(t *testing.T) {
		w, kube := newScheduleFixture(t, nil, nil)
		if err := kube.CoreV1().Pods(schedTestNS).Delete(context.Background(), "ws", metav1.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
		_, err := w.SetSchedule(context.Background(), ScheduleChange{To: strp("2026-12-01T00:00:00Z")}, schedNow)
		if !errors.Is(err, ErrWorkspaceNotRunning) {
			t.Fatalf("got %v", err)
		}
		if schedOverride(t, kube) != "" {
			t.Error("nothing may be stored for a stopped workspace")
		}
	})

	t.Run("pod being deleted", func(t *testing.T) {
		w, kube := newScheduleFixture(t, nil, nil)
		pod := schedPod(t, kube)
		now := metav1.Now()
		pod.DeletionTimestamp = &now
		if _, err := kube.CoreV1().Pods(schedTestNS).Update(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		_, err := w.SetSchedule(context.Background(), ScheduleChange{To: strp("2026-12-01T00:00:00Z")}, schedNow)
		if !errors.Is(err, ErrWorkspaceNotRunning) {
			t.Fatalf("got %v", err)
		}
	})
}

func TestSetScheduleRevert(t *testing.T) {
	bp := &models.Schedule{To: "2026-11-10T18:00:00Z", StopGrace: "15m"}
	ann := map[string]string{
		// as left by an earlier override
		helm.AnnotationScheduleTo:             "2026-11-20T18:00:00Z",
		helm.AnnotationScheduleStopGrace:      "1h",
		helm.AnnotationScheduleTerminateGrace: "1d",
		helm.AnnotationScheduleWarned:         "2026-11-10T18:01:00Z",
	}
	w, kube := newScheduleFixture(t, ann, bp)
	if _, err := kube.CoreV1().Secrets(schedTestNS).Patch(context.Background(), "sh.helm.release.v1.ws.v1",
		types.MergePatchType, []byte(`{"metadata":{"annotations":{"`+helm.AnnotationScheduleOverride+`":"{}"}}}`),
		metav1.PatchOptions{}); err != nil {
		t.Fatal(err)
	}

	got, err := w.SetSchedule(context.Background(), ScheduleChange{Revert: true}, schedNow)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.To == nil || !got.To.Equal(time.Date(2026, 11, 10, 18, 0, 0, 0, time.UTC)) || got.TerminatesAt != nil {
		t.Fatalf("returned schedule = %+v", got)
	}
	pod := schedPod(t, kube)
	if helm.ScheduleFromAnnotations(pod.Annotations) != *bp {
		t.Fatalf("pod schedule = %+v, want the blueprint's", helm.ScheduleFromAnnotations(pod.Annotations))
	}
	if _, ok := pod.Annotations[helm.AnnotationScheduleWarned]; ok {
		t.Error("warning mark should be cleared")
	}
	if schedOverride(t, kube) != "" {
		t.Error("override should be removed from the release")
	}
}

func TestSetScheduleRevertWithoutBlueprintSchedule(t *testing.T) {
	w, kube := newScheduleFixture(t, baseScheduleAnnotations(), nil)
	got, err := w.SetSchedule(context.Background(), ScheduleChange{Revert: true}, schedNow)
	if err != nil || got != nil {
		t.Fatalf("got %+v, %v; want no schedule", got, err)
	}
	if helm.ScheduleFromAnnotations(schedPod(t, kube).Annotations) != (models.Schedule{}) {
		t.Error("the blueprint has no schedule, so the pod should have none")
	}
}

// encodedRelease builds a release Secret the way Helm stores it.
func encodedRelease(t *testing.T, name, manifest, override string) *corev1.Secret {
	t.Helper()
	raw, err := json.Marshal(&release.Release{Name: name, Manifest: manifest})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(raw)
	_ = zw.Close()
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "sh.helm.release.v1." + name + ".v1", Namespace: schedTestNS,
			Labels: map[string]string{"owner": "helm", "status": "deployed", "name": name},
		},
		Data: map[string][]byte{"release": []byte(base64.StdEncoding.EncodeToString(buf.Bytes()))},
	}
	if override != "" {
		s.Annotations = map[string]string{helm.AnnotationScheduleOverride: override}
	}
	return s
}

const expiredReleaseManifest = `---
apiVersion: v1
kind: Pod
metadata:
  name: ws
  annotations:
    k8shell.io/schedule-to: "2026-11-01T18:00:00Z"
`

func TestCheckStartScheduleUsesTheOverride(t *testing.T) {
	nop := zerolog.Nop()
	newWS := func(secret *corev1.Secret, bp *models.Schedule) *Workspace {
		kube := fake.NewClientset()
		if secret != nil {
			if _, err := kube.CoreV1().Secrets(schedTestNS).Create(context.Background(), secret, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
		}
		return &Workspace{Name: "ws", log: &nop, client: helm.NewClientWithKube(kube, schedTestNS),
			blueprint: &models.Blueprint{Schedule: bp}}
	}

	t.Run("expired and not extended", func(t *testing.T) {
		w := newWS(encodedRelease(t, "ws", expiredReleaseManifest, ""), nil)
		if err := w.CheckStartSchedule(context.Background(), schedNow); !errors.Is(err, ErrScheduleNotAllowed) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("the message shows the time in the schedule's zone", func(t *testing.T) {
		w := newWS(encodedRelease(t, "ws", expiredReleaseManifest, ""), &models.Schedule{Timezone: "Europe/Prague"})
		// the manifest's to is 2026-11-01T18:00:00Z = 19:00 in Prague
		err := w.CheckStartSchedule(context.Background(), schedNow)
		if err == nil || !strings.Contains(err.Error(), "ended at 2026-11-01 19:00 CET") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("extended by an override", func(t *testing.T) {
		w := newWS(encodedRelease(t, "ws", expiredReleaseManifest, `{"to":"2026-11-20T18:00:00Z"}`), nil)
		if err := w.CheckStartSchedule(context.Background(), schedNow); err != nil {
			t.Fatalf("an extended workspace must start, got %v", err)
		}
	})

	t.Run("override without expiry", func(t *testing.T) {
		w := newWS(encodedRelease(t, "ws", expiredReleaseManifest, `{}`), nil)
		if err := w.CheckStartSchedule(context.Background(), schedNow); err != nil {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("unreadable release falls back to the blueprint", func(t *testing.T) {
		w := newWS(nil, &models.Schedule{To: "2026-11-01T18:00:00Z"})
		if err := w.CheckStartSchedule(context.Background(), schedNow); !errors.Is(err, ErrScheduleNotAllowed) {
			t.Fatalf("got %v", err)
		}
		w = newWS(nil, nil)
		if err := w.CheckStartSchedule(context.Background(), schedNow); err != nil {
			t.Fatalf("got %v", err)
		}
	})
}

func pragueConfig(t *testing.T) *config.Config {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Prague")
	if err != nil {
		t.Skipf("no time zone data: %v", err)
	}
	return &config.Config{Schedule: config.ScheduleConfig{Location: loc}}
}

func TestCheckScheduleReadsLocalTimesInTheConfiguredZone(t *testing.T) {
	nop := zerolog.Nop()
	cfg := pragueConfig(t)
	// schedNow is 2026-11-10 12:00Z = 13:00 in Prague
	check := func(s *models.Schedule) error {
		w := &Workspace{Name: "ws", log: &nop, config: cfg, blueprint: &models.Blueprint{Schedule: s}}
		return w.CheckSchedule(schedNow)
	}

	if err := check(&models.Schedule{To: "2026-11-10 13:30"}); err != nil { // 12:30Z, still ahead
		t.Errorf("to 13:30 Prague should be ahead: %v", err)
	}
	if err := check(&models.Schedule{To: "2026-11-10 12:30"}); !errors.Is(err, ErrScheduleNotAllowed) { // 11:30Z, passed
		t.Errorf("to 12:30 Prague should have passed: %v", err)
	}
	if err := check(&models.Schedule{From: "2026-11-10 13:30"}); !errors.Is(err, ErrScheduleNotAllowed) { // not yet
		t.Errorf("from 13:30 Prague should not have started: %v", err)
	}
	// the blueprint's own zone wins: 13:30 in Tokyo is 04:30Z, long passed
	if err := check(&models.Schedule{Timezone: "Asia/Tokyo", To: "2026-11-10 13:30"}); !errors.Is(err, ErrScheduleNotAllowed) {
		t.Errorf("to 13:30 Tokyo should have passed: %v", err)
	}
	// an offset wins over both
	if err := check(&models.Schedule{Timezone: "Asia/Tokyo", To: "2026-11-10T13:30:00Z"}); err != nil {
		t.Errorf("to 13:30Z should be ahead: %v", err)
	}
	// the message shows the time in the schedule's zone, as it was written
	if err := check(&models.Schedule{To: "2026-11-10 12:30"}); err == nil || !strings.Contains(err.Error(), "ended at 2026-11-10 12:30 CET") {
		t.Errorf("message = %v", err)
	}
	if err := check(&models.Schedule{From: "2026-11-10 13:30"}); err == nil || !strings.Contains(err.Error(), "cannot run before 2026-11-10 13:30 CET") {
		t.Errorf("message = %v", err)
	}
	// ... in the blueprint's own zone when it sets one
	if err := check(&models.Schedule{Timezone: "Asia/Tokyo", To: "2026-11-10 13:30"}); err == nil || !strings.Contains(err.Error(), "ended at 2026-11-10 13:30 JST") {
		t.Errorf("message = %v", err)
	}
	// ... and an absolute time is shown in the zone too, not as written
	if err := check(&models.Schedule{To: "2026-11-10T11:30:00Z"}); err == nil || !strings.Contains(err.Error(), "ended at 2026-11-10 12:30 CET") {
		t.Errorf("message = %v", err)
	}
	// the message the user sees names no UTC time unless the zone is UTC
	if err := check(&models.Schedule{To: "2026-11-10T11:30:00Z"}); err == nil || strings.Contains(err.Error(), "T11:30") {
		t.Errorf("message = %v", err)
	}
}

func TestScheduleValuesAreAbsolute(t *testing.T) {
	w := &Workspace{Name: "ws", config: pragueConfig(t)}

	got, err := w.scheduleValues(&models.Schedule{
		From: "2026-11-10 09:00", To: "2026-07-10T19:00:00", StopGrace: "15m", TerminateGrace: "7d",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]interface{}{
		"from": "2026-11-10T09:00:00+01:00", "to": "2026-07-10T19:00:00+02:00",
		"stopGrace": "15m", "terminateGrace": "7d",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}

	t.Run("blueprint timezone is kept, times are read in it", func(t *testing.T) {
		got, err := w.scheduleValues(&models.Schedule{Timezone: "Asia/Tokyo", To: "2026-11-10 09:00"})
		if err != nil {
			t.Fatal(err)
		}
		if got["to"] != "2026-11-10T09:00:00+09:00" || got["timezone"] != "Asia/Tokyo" {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("absolute times are left alone", func(t *testing.T) {
		got, err := w.scheduleValues(&models.Schedule{To: "2026-11-10T18:00:00Z"})
		if err != nil || got["to"] != "2026-11-10T18:00:00Z" {
			t.Fatalf("got %v, %v", got, err)
		}
	})

	t.Run("invalid", func(t *testing.T) {
		if _, err := w.scheduleValues(&models.Schedule{To: "later"}); err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("a stored schedule no longer depends on the configured zone", func(t *testing.T) {
		stored, err := w.scheduleValues(&models.Schedule{To: "2026-11-10 09:00"})
		if err != nil {
			t.Fatal(err)
		}
		other := &Workspace{Name: "ws", config: &config.Config{Schedule: config.ScheduleConfig{Location: time.UTC}}}
		again, err := other.scheduleValues(&models.Schedule{To: stored["to"].(string)})
		if err != nil || again["to"] != stored["to"] {
			t.Fatalf("stored %v, after a zone change %v (%v)", stored["to"], again["to"], err)
		}
	})
}

func TestSetScheduleReadsLocalTimes(t *testing.T) {
	t.Run("in the configured zone", func(t *testing.T) {
		w, kube := newScheduleFixture(t, baseScheduleAnnotations(), nil)
		w.config = pragueConfig(t)

		got, err := w.SetSchedule(context.Background(), ScheduleChange{To: strp("2026-11-12 19:00")}, schedNow)
		if err != nil {
			t.Fatal(err)
		}
		if got.To == nil || !got.To.Equal(time.Date(2026, 11, 12, 18, 0, 0, 0, time.UTC)) {
			t.Fatalf("returned to = %v", got.To)
		}
		if a := schedPod(t, kube).Annotations[helm.AnnotationScheduleTo]; a != "2026-11-12T19:00:00+01:00" {
			t.Fatalf("annotation = %q, want the absolute time", a)
		}
		var stored models.Schedule
		if err := json.Unmarshal([]byte(schedOverride(t, kube)), &stored); err != nil || stored.To != "2026-11-12T19:00:00+01:00" {
			t.Fatalf("stored = %+v (%v)", stored, err)
		}
	})

	t.Run("in the blueprint's own zone", func(t *testing.T) {
		w, kube := newScheduleFixture(t, baseScheduleAnnotations(), &models.Schedule{Timezone: "Asia/Tokyo"})
		w.config = pragueConfig(t)

		if _, err := w.SetSchedule(context.Background(), ScheduleChange{To: strp("2026-11-12 19:00")}, schedNow); err != nil {
			t.Fatal(err)
		}
		if a := schedPod(t, kube).Annotations[helm.AnnotationScheduleTo]; a != "2026-11-12T19:00:00+09:00" {
			t.Fatalf("annotation = %q", a)
		}
	})

	t.Run("a local time already in the past is rejected", func(t *testing.T) {
		w, _ := newScheduleFixture(t, baseScheduleAnnotations(), nil)
		w.config = pragueConfig(t)
		// 12:30 Prague is 11:30Z, before schedNow (12:00Z)
		_, err := w.SetSchedule(context.Background(), ScheduleChange{To: strp("2026-11-10 12:30")}, schedNow)
		if !errors.Is(err, models.ErrInvalidParameters) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("an unknown blueprint timezone is rejected", func(t *testing.T) {
		w, _ := newScheduleFixture(t, baseScheduleAnnotations(), &models.Schedule{Timezone: "Mars/Olympus"})
		_, err := w.SetSchedule(context.Background(), ScheduleChange{To: strp("2026-11-12 19:00")}, schedNow)
		if !errors.Is(err, models.ErrInvalidParameters) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("revert resolves the blueprint's local times", func(t *testing.T) {
		w, kube := newScheduleFixture(t, baseScheduleAnnotations(),
			&models.Schedule{Timezone: "Europe/Prague", To: "2026-11-10 19:00", StopGrace: "15m"})
		w.config = pragueConfig(t)

		if _, err := w.SetSchedule(context.Background(), ScheduleChange{Revert: true}, schedNow); err != nil {
			t.Fatal(err)
		}
		ann := schedPod(t, kube).Annotations
		if ann[helm.AnnotationScheduleTo] != "2026-11-10T19:00:00+01:00" || ann[helm.AnnotationScheduleStopGrace] != "15m" {
			t.Fatalf("annotations = %v", ann)
		}
	})
}

func TestScheduleEnded(t *testing.T) {
	nop := zerolog.Nop()
	cfg := &config.Config{Schedule: config.ScheduleConfig{Location: time.UTC}}
	check := func(s *models.Schedule) error {
		w := &Workspace{Name: "ws", log: &nop, config: cfg, blueprint: &models.Blueprint{Schedule: s}}
		return w.CheckSchedule(schedNow)
	}

	ended := check(&models.Schedule{To: "2026-11-10T11:00:00Z"})
	if !errors.Is(ended, ErrScheduleNotAllowed) || !ScheduleEnded(ended) {
		t.Fatalf("an ended schedule: ErrScheduleNotAllowed=%v ScheduleEnded=%v", errors.Is(ended, ErrScheduleNotAllowed), ScheduleEnded(ended))
	}
	notYet := check(&models.Schedule{From: "2026-11-11T11:00:00Z"})
	if !errors.Is(notYet, ErrScheduleNotAllowed) || ScheduleEnded(notYet) {
		t.Fatalf("a schedule not yet started: ErrScheduleNotAllowed=%v ScheduleEnded=%v", errors.Is(notYet, ErrScheduleNotAllowed), ScheduleEnded(notYet))
	}
	if ScheduleEnded(nil) || ScheduleEnded(errors.New("other")) {
		t.Fatal("unrelated errors are not an ended schedule")
	}
}

const stoppedReleaseManifest = `---
apiVersion: v1
kind: Pod
metadata:
  name: ws
  annotations:
    k8shell.io/schedule-from: "2026-10-01T00:00:00Z"
    k8shell.io/schedule-to: "2026-11-10T11:00:00Z"
    k8shell.io/schedule-stop-grace: "15m"
    k8shell.io/schedule-terminate-grace: "7d"
`

const runningReleaseManifest = `---
apiVersion: v1
kind: Pod
metadata:
  name: ws
  annotations:
    k8shell.io/schedule-to: "2026-11-20T11:00:00Z"
    k8shell.io/schedule-stop-grace: "15m"
`

func newForceFixture(t *testing.T, manifest, override string, bp *models.Schedule) (*Workspace, *fake.Clientset) {
	t.Helper()
	kube := fake.NewClientset(encodedRelease(t, "ws", manifest, override))
	nop := zerolog.Nop()
	return &Workspace{
		Name: "ws", log: &nop,
		config:    pragueConfig(t),
		client:    helm.NewClientWithKube(kube, schedTestNS),
		blueprint: &models.Blueprint{Schedule: bp},
	}, kube
}

func TestPrepareForcedSchedule(t *testing.T) {
	ctx := context.Background()
	// schedNow is 2026-11-10 12:00Z; the stopped workspace's "to" was 11:00Z

	t.Run("an ended schedule without a new expiry or a configured default is rejected", func(t *testing.T) {
		w, _ := newForceFixture(t, stoppedReleaseManifest, "", nil)
		_, err := w.PrepareForcedSchedule(ctx, nil, schedNow)
		if !errors.Is(err, ErrForceNeedsExpiry) || !errors.Is(err, models.ErrInvalidParameters) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("an ended schedule without a new expiry gets the configured default", func(t *testing.T) {
		w, _ := newForceFixture(t, stoppedReleaseManifest, "", nil)
		w.config.Schedule.ForceStartDuration = time.Hour
		next, err := w.PrepareForcedSchedule(ctx, nil, schedNow)
		if err != nil {
			t.Fatal(err)
		}
		// now is 12:00Z = 13:00 in Prague; one hour on; everything else is kept
		want := models.Schedule{From: "2026-10-01T00:00:00Z", To: "2026-11-10T14:00:00+01:00", StopGrace: "15m", TerminateGrace: "7d"}
		if next == nil || *next != want {
			t.Fatalf("got %+v, want %+v", next, want)
		}
		// the stop and terminate times follow from it
		sched := workspaceSchedule(*next)
		if sched == nil || sched.StopsAt == nil || !sched.StopsAt.Equal(time.Date(2026, 11, 10, 13, 15, 0, 0, time.UTC)) ||
			sched.TerminatesAt == nil || !sched.TerminatesAt.Equal(time.Date(2026, 11, 17, 13, 15, 0, 0, time.UTC)) {
			t.Fatalf("schedule = %+v", sched)
		}
	})

	t.Run("an explicit expiry wins over the default", func(t *testing.T) {
		w, _ := newForceFixture(t, stoppedReleaseManifest, "", nil)
		w.config.Schedule.ForceStartDuration = time.Hour
		next, err := w.PrepareForcedSchedule(ctx, strp("2026-11-12T18:00:00Z"), schedNow)
		if err != nil || next == nil || next.To != "2026-11-12T18:00:00Z" {
			t.Fatalf("got %+v, %v", next, err)
		}
	})

	t.Run("an empty expiry still removes it, whatever the default", func(t *testing.T) {
		w, _ := newForceFixture(t, stoppedReleaseManifest, "", nil)
		w.config.Schedule.ForceStartDuration = time.Hour
		next, err := w.PrepareForcedSchedule(ctx, strp(""), schedNow)
		if err != nil || next == nil || next.To != "" {
			t.Fatalf("got %+v, %v", next, err)
		}
	})

	t.Run("the default is not applied when the schedule has not ended", func(t *testing.T) {
		w, _ := newForceFixture(t, runningReleaseManifest, "", nil)
		w.config.Schedule.ForceStartDuration = time.Hour
		next, err := w.PrepareForcedSchedule(ctx, nil, schedNow)
		if err != nil || next != nil {
			t.Fatalf("got %+v, %v", next, err)
		}
	})

	t.Run("the default is read from the blueprint's zone", func(t *testing.T) {
		w, _ := newForceFixture(t, stoppedReleaseManifest, "", &models.Schedule{Timezone: "Asia/Tokyo"})
		w.config.Schedule.ForceStartDuration = 2 * time.Hour
		next, err := w.PrepareForcedSchedule(ctx, nil, schedNow)
		if err != nil || next == nil || next.To != "2026-11-10T23:00:00+09:00" { // 14:00Z
			t.Fatalf("got %+v, %v", next, err)
		}
	})

	t.Run("a new expiry replaces to and keeps the rest", func(t *testing.T) {
		w, _ := newForceFixture(t, stoppedReleaseManifest, "", nil)
		next, err := w.PrepareForcedSchedule(ctx, strp("2026-11-12 19:00"), schedNow) // local, Prague
		if err != nil {
			t.Fatal(err)
		}
		want := models.Schedule{From: "2026-10-01T00:00:00Z", To: "2026-11-12T19:00:00+01:00", StopGrace: "15m", TerminateGrace: "7d"}
		if next == nil || *next != want {
			t.Fatalf("got %+v, want %+v", next, want)
		}
	})

	t.Run("an absolute new expiry", func(t *testing.T) {
		w, _ := newForceFixture(t, stoppedReleaseManifest, "", nil)
		next, err := w.PrepareForcedSchedule(ctx, strp("2026-11-12T18:00:00Z"), schedNow)
		if err != nil || next == nil || next.To != "2026-11-12T18:00:00Z" {
			t.Fatalf("got %+v, %v", next, err)
		}
	})

	t.Run("an empty expiry removes it", func(t *testing.T) {
		w, _ := newForceFixture(t, stoppedReleaseManifest, "", nil)
		next, err := w.PrepareForcedSchedule(ctx, strp(""), schedNow)
		if err != nil || next == nil || next.To != "" || next.StopGrace != "15m" {
			t.Fatalf("got %+v, %v", next, err)
		}
	})

	t.Run("a new expiry must be in the future", func(t *testing.T) {
		w, _ := newForceFixture(t, stoppedReleaseManifest, "", nil)
		for _, to := range []string{"2026-11-10 12:30", "2026-11-10T12:00:00Z", "2026-11-09T00:00:00Z"} { // 11:30Z, now, past
			if _, err := w.PrepareForcedSchedule(ctx, strp(to), schedNow); !errors.Is(err, models.ErrInvalidParameters) {
				t.Errorf("to %q: got %v", to, err)
			}
		}
	})

	t.Run("a new expiry must come after from", func(t *testing.T) {
		manifest := strings.Replace(stoppedReleaseManifest, "2026-10-01T00:00:00Z", "2026-12-01T00:00:00Z", 1)
		w, _ := newForceFixture(t, manifest, "", nil)
		if _, err := w.PrepareForcedSchedule(ctx, strp("2026-11-12T18:00:00Z"), schedNow); !errors.Is(err, models.ErrInvalidParameters) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("a bad expiry", func(t *testing.T) {
		w, _ := newForceFixture(t, stoppedReleaseManifest, "", nil)
		if _, err := w.PrepareForcedSchedule(ctx, strp("soon"), schedNow); !errors.Is(err, models.ErrInvalidParameters) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("a schedule that has not ended is left alone without a new expiry", func(t *testing.T) {
		w, _ := newForceFixture(t, runningReleaseManifest, "", nil)
		next, err := w.PrepareForcedSchedule(ctx, nil, schedNow)
		if err != nil || next != nil {
			t.Fatalf("got %+v, %v", next, err)
		}
	})

	t.Run("an earlier override that extended it counts", func(t *testing.T) {
		// the manifest's "to" has passed, the override moved it ahead
		w, _ := newForceFixture(t, stoppedReleaseManifest, `{"to":"2026-11-20T18:00:00Z","stopGrace":"15m"}`, nil)
		next, err := w.PrepareForcedSchedule(ctx, nil, schedNow)
		if err != nil || next != nil {
			t.Fatalf("got %+v, %v", next, err)
		}
	})

	t.Run("no schedule at all", func(t *testing.T) {
		w, _ := newForceFixture(t, "---\napiVersion: v1\nkind: Pod\nmetadata:\n  name: ws\n", "", nil)
		if next, err := w.PrepareForcedSchedule(ctx, nil, schedNow); err != nil || next != nil {
			t.Fatalf("got %+v, %v", next, err)
		}
		// a forced start may still give it an expiry
		if next, err := w.PrepareForcedSchedule(ctx, strp("2026-11-12T18:00:00Z"), schedNow); err != nil || next == nil || next.To == "" {
			t.Fatalf("got %+v, %v", next, err)
		}
	})

	t.Run("the blueprint's own zone reads a local expiry", func(t *testing.T) {
		w, _ := newForceFixture(t, stoppedReleaseManifest, "", &models.Schedule{Timezone: "Asia/Tokyo"})
		next, err := w.PrepareForcedSchedule(ctx, strp("2026-11-12 19:00"), schedNow)
		if err != nil || next == nil || next.To != "2026-11-12T19:00:00+09:00" {
			t.Fatalf("got %+v, %v", next, err)
		}
	})

	t.Run("a missing release", func(t *testing.T) {
		nop := zerolog.Nop()
		w := &Workspace{Name: "ws", log: &nop, config: pragueConfig(t), client: helm.NewClientWithKube(fake.NewClientset(), schedTestNS)}
		if _, err := w.PrepareForcedSchedule(ctx, strp("2026-11-12T18:00:00Z"), schedNow); err == nil {
			t.Fatal("expected an error")
		}
	})
}

func TestApplyForcedScheduleSurvivesAStop(t *testing.T) {
	ctx := context.Background()
	w, _ := newForceFixture(t, stoppedReleaseManifest, "", nil)

	// the pod doStart is about to create, as built from the release manifest
	rp, err := w.client.DeployedReleasePod(ctx, "ws")
	if err != nil {
		t.Fatal(err)
	}
	pod := rp.Pod
	if pod.Annotations[helm.AnnotationScheduleTo] != "2026-11-10T11:00:00Z" {
		t.Fatalf("the manifest should still have the old expiry: %v", pod.Annotations)
	}

	forced := &models.Schedule{From: "2026-10-01T00:00:00Z", To: "2026-11-12T19:00:00+01:00", StopGrace: "15m", TerminateGrace: "7d"}
	if err := w.applyForcedSchedule(ctx, pod, forced); err != nil {
		t.Fatal(err)
	}

	// the pod to be created carries the new expiry
	if pod.Annotations[helm.AnnotationScheduleTo] != "2026-11-12T19:00:00+01:00" {
		t.Fatalf("pod annotations = %v", pod.Annotations)
	}
	// and so does the stopped workspace read back from the release later
	again, err := w.client.DeployedReleasePod(ctx, "ws")
	if err != nil {
		t.Fatal(err)
	}
	if got := helm.ScheduleFromAnnotations(again.Pod.Annotations); got != *forced {
		t.Fatalf("after a stop the schedule reads %+v, want %+v", got, *forced)
	}
	// the start check now lets it run
	if err := w.CheckStartSchedule(ctx, schedNow); err != nil {
		t.Fatalf("a force-started workspace must pass the start check: %v", err)
	}
}

func TestApplyForcedScheduleCanRemoveTheExpiry(t *testing.T) {
	ctx := context.Background()
	w, _ := newForceFixture(t, stoppedReleaseManifest, "", nil)
	rp, err := w.client.DeployedReleasePod(ctx, "ws")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.applyForcedSchedule(ctx, rp.Pod, &models.Schedule{From: "2026-10-01T00:00:00Z", StopGrace: "15m"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := rp.Pod.Annotations[helm.AnnotationScheduleTo]; ok {
		t.Fatalf("the expiry should be gone: %v", rp.Pod.Annotations)
	}
	if got := workspaceSchedule(helm.ScheduleFromAnnotations(rp.Pod.Annotations)); got == nil || got.To != nil || got.StopsAt != nil {
		t.Fatalf("schedule = %+v", got)
	}
}
