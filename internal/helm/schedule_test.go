// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package helm

import (
	"context"
	"testing"
	"time"

	"github.com/k8shell-io/common/pkg/models"
	"github.com/rs/zerolog"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestApplyScheduleOverride(t *testing.T) {
	podWith := func(a map[string]string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: a}}
	}

	t.Run("replaces the whole schedule", func(t *testing.T) {
		pod := podWith(map[string]string{
			AnnotationScheduleTo:             "2026-11-10T18:00:00Z",
			AnnotationScheduleStopGrace:      "15m",
			AnnotationScheduleTerminateGrace: "7d",
			"keep":                           "me",
		})
		// stop grace changed, terminate grace cleared (empty), to as before
		err := ApplyScheduleOverride(pod, `{"to":"2026-11-12T18:00:00Z","stopGrace":"30m"}`)
		if err != nil {
			t.Fatal(err)
		}
		got := ScheduleFromAnnotations(pod.Annotations)
		want := models.Schedule{To: "2026-11-12T18:00:00Z", StopGrace: "30m"}
		if got != want {
			t.Fatalf("schedule = %+v, want %+v", got, want)
		}
		if pod.Annotations["keep"] != "me" {
			t.Fatal("unrelated annotation was touched")
		}
	})

	t.Run("creates the annotations on a bare pod", func(t *testing.T) {
		pod := &corev1.Pod{}
		if err := ApplyScheduleOverride(pod, `{"to":"2026-11-12T18:00:00Z"}`); err != nil {
			t.Fatal(err)
		}
		if pod.Annotations[AnnotationScheduleTo] != "2026-11-12T18:00:00Z" {
			t.Fatalf("annotations = %v", pod.Annotations)
		}
	})

	t.Run("empty override changes nothing", func(t *testing.T) {
		pod := podWith(map[string]string{AnnotationScheduleTo: "2026-11-10T18:00:00Z"})
		if err := ApplyScheduleOverride(pod, ""); err != nil {
			t.Fatal(err)
		}
		if pod.Annotations[AnnotationScheduleTo] != "2026-11-10T18:00:00Z" {
			t.Fatalf("annotations = %v", pod.Annotations)
		}
	})

	t.Run("invalid override leaves the pod untouched", func(t *testing.T) {
		pod := podWith(map[string]string{AnnotationScheduleTo: "2026-11-10T18:00:00Z"})
		if err := ApplyScheduleOverride(pod, `{not json`); err == nil {
			t.Fatal("expected an error")
		}
		if pod.Annotations[AnnotationScheduleTo] != "2026-11-10T18:00:00Z" {
			t.Fatalf("annotations = %v", pod.Annotations)
		}
	})

	t.Run("round trip with ScheduleOverrideJSON", func(t *testing.T) {
		s := models.Schedule{From: "2026-11-01T00:00:00Z", To: "2026-11-12T18:00:00Z", TerminateGrace: "1d"}
		raw, err := ScheduleOverrideJSON(s)
		if err != nil {
			t.Fatal(err)
		}
		pod := &corev1.Pod{}
		if err := ApplyScheduleOverride(pod, raw); err != nil {
			t.Fatal(err)
		}
		if got := ScheduleFromAnnotations(pod.Annotations); got != s {
			t.Fatalf("got %+v, want %+v", got, s)
		}
	})
}

const scheduledPodManifest = `---
apiVersion: v1
kind: Pod
metadata:
  name: ws-1
  annotations:
    k8shell.io/schedule-to: "2026-11-10T18:00:00Z"
    k8shell.io/schedule-stop-grace: "15m"
`

func newScheduleTestClient(kube *fake.Clientset) *Client {
	l := zerolog.Nop()
	return &Client{log: &l, kubeClient: kube, targetNamespace: testTargetNamespace,
		releaseCache: newTTLCache[[]*ReleasePod](), podCache: newTTLCache[[]corev1.Pod]()}
}

func TestReleasePodFromSecretAppliesOverride(t *testing.T) {
	secret := testReleaseSecret(t, "ws-1", scheduledPodManifest, time.Now())
	secret.Annotations = map[string]string{AnnotationScheduleOverride: `{"to":"2026-11-12T18:00:00Z","stopGrace":"1h"}`}

	l := zerolog.Nop()
	rp := releasePodFromSecret(&l, secret)
	if rp.Pod == nil {
		t.Fatal("no pod decoded")
	}
	want := models.Schedule{To: "2026-11-12T18:00:00Z", StopGrace: "1h"}
	if got := ScheduleFromAnnotations(rp.Pod.Annotations); got != want {
		t.Fatalf("schedule = %+v, want %+v", got, want)
	}

	// without the annotation the manifest's schedule shows
	plain := releasePodFromSecret(&l, testReleaseSecret(t, "ws-1", scheduledPodManifest, time.Now()))
	if got := ScheduleFromAnnotations(plain.Pod.Annotations); got.To != "2026-11-10T18:00:00Z" {
		t.Fatalf("schedule = %+v", got)
	}

	// an undecodable override is ignored, not fatal
	secret.Annotations[AnnotationScheduleOverride] = "{nope"
	bad := releasePodFromSecret(&l, secret)
	if bad.Pod == nil || ScheduleFromAnnotations(bad.Pod.Annotations).To != "2026-11-10T18:00:00Z" {
		t.Fatalf("bad override should leave the manifest schedule: %+v", bad.Pod)
	}
}

func TestSetScheduleOverrideOnDeployedRevision(t *testing.T) {
	ctx := context.Background()
	v1 := testReleaseSecret(t, "ws-1", scheduledPodManifest, time.Now())
	v1.Name = "sh.helm.release.v1.ws-1.v9"
	v1.Labels["status"] = "superseded"
	v2 := testReleaseSecret(t, "ws-1", scheduledPodManifest, time.Now())
	v2.Name = "sh.helm.release.v1.ws-1.v10"
	other := testReleaseSecret(t, "ws-2", scheduledPodManifest, time.Now())

	kube := fake.NewClientset(v1, v2, other)
	c := newScheduleTestClient(kube)

	if err := c.SetScheduleOverride(ctx, "ws-1", `{"to":"2026-11-12T18:00:00Z"}`); err != nil {
		t.Fatal(err)
	}
	get := func(name string) *corev1.Secret {
		s, err := kube.CoreV1().Secrets(testTargetNamespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	if get("sh.helm.release.v1.ws-1.v10").Annotations[AnnotationScheduleOverride] == "" {
		t.Error("override not stored on the deployed revision")
	}
	if get("sh.helm.release.v1.ws-1.v9").Annotations[AnnotationScheduleOverride] != "" {
		t.Error("superseded revision must not be touched")
	}
	if get("sh.helm.release.v1.ws-2.v1").Annotations[AnnotationScheduleOverride] != "" {
		t.Error("another workspace's release must not be touched")
	}

	// DeployedReleasePod shows the override
	rp, err := c.DeployedReleasePod(ctx, "ws-1")
	if err != nil {
		t.Fatal(err)
	}
	if got := ScheduleFromAnnotations(rp.Pod.Annotations); got.To != "2026-11-12T18:00:00Z" {
		t.Fatalf("DeployedReleasePod schedule = %+v", got)
	}

	// an empty override removes it again
	if err := c.SetScheduleOverride(ctx, "ws-1", ""); err != nil {
		t.Fatal(err)
	}
	if _, ok := get("sh.helm.release.v1.ws-1.v10").Annotations[AnnotationScheduleOverride]; ok {
		t.Error("override not removed")
	}
	rp, err = c.DeployedReleasePod(ctx, "ws-1")
	if err != nil {
		t.Fatal(err)
	}
	if got := ScheduleFromAnnotations(rp.Pod.Annotations); got.To != "2026-11-10T18:00:00Z" {
		t.Fatalf("schedule after removal = %+v", got)
	}
}

func TestSetScheduleOverrideUnknownRelease(t *testing.T) {
	c := newScheduleTestClient(fake.NewClientset())
	err := c.SetScheduleOverride(context.Background(), "nope", "{}")
	if err == nil {
		t.Fatal("expected an error")
	}
}

func TestListReleasePodsBySelectorAppliesOverride(t *testing.T) {
	secret := testReleaseSecret(t, "ws-1", scheduledPodManifest, time.Now())
	secret.Annotations = map[string]string{AnnotationScheduleOverride: `{"to":"2026-11-12T18:00:00Z"}`}
	c := newScheduleTestClient(fake.NewClientset(secret))

	rps, err := c.ListReleasePodsBySelector(context.Background(), testTargetNamespace, "")
	if err != nil || len(rps) != 1 {
		t.Fatalf("got %d, %v", len(rps), err)
	}
	if got := ScheduleFromAnnotations(rps[0].Pod.Annotations); got.To != "2026-11-12T18:00:00Z" || got.StopGrace != "" {
		t.Fatalf("schedule = %+v", got)
	}
}

func TestRevisionOf(t *testing.T) {
	for name, want := range map[string]int{
		"sh.helm.release.v1.ws.v1": 1, "sh.helm.release.v1.ws.v10": 10, "sh.helm.release.v1.ws.v9": 9,
		"sh.helm.release.v1.my.v2.ws.v3": 3, "weird": 0, "sh.helm.release.v1.ws.vx": 0,
	} {
		if got := revisionOf(name); got != want {
			t.Errorf("revisionOf(%q) = %d, want %d", name, got, want)
		}
	}
}
