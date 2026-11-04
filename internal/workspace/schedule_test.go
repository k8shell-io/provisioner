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
		{"before from", &models.Schedule{From: "2026-11-11T08:00:00Z"}, "cannot run before 2026-11-11T08:00:00Z"},
		{"at to", &models.Schedule{To: "2026-11-10T12:00:00Z"}, "ended at 2026-11-10T12:00:00Z"},
		{"after to", &models.Schedule{To: "2026-11-10T11:59:59Z"}, "ended at"},
		{"to given with an offset", &models.Schedule{To: "2026-11-10T12:30:00+01:00"}, "ended at 2026-11-10T11:30:00Z"},
		{"unparsable is allowed", &models.Schedule{To: "tomorrow"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := &Workspace{Name: "ws", log: &nop, blueprint: &models.Blueprint{Schedule: tt.schedule}}
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
