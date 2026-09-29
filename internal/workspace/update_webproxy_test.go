// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package workspace

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/k8shell-io/provisioner/internal/helm"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
)

// newWebProxyUpdateFixture creates the workspace pod ws-a, publishing a route
// on port when port > 0, in the fake cluster behind c.
func newWebProxyUpdateFixture(t *testing.T, c *aliasClaims, port string) corev1client.PodInterface {
	t.Helper()
	pod := aliasTestPod("ws-a", "")
	delete(pod.Annotations, helm.AnnotationWebProxyPort)
	if port != "" {
		pod.Annotations[helm.AnnotationWebProxyPort] = port
		pod.Annotations[helm.AnnotationWebProxyRoles] = `["developer"]`
		pod.Labels[helm.LabelWebProxy] = "true"
	}
	pods := c.kube.CoreV1().Pods("test")
	if _, err := pods.Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create pod: %v", err)
	}
	return pods
}

func getPod(t *testing.T, pods corev1client.PodInterface) *corev1.Pod {
	t.Helper()
	pod, err := pods.Get(context.Background(), "ws-a", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	return pod
}

func assertPodMetadataUnchanged(t *testing.T, before, after *corev1.Pod) {
	t.Helper()
	if !reflect.DeepEqual(before.Labels, after.Labels) || !reflect.DeepEqual(before.Annotations, after.Annotations) {
		t.Fatalf("pod metadata changed:\nbefore labels=%v annotations=%v\nafter  labels=%v annotations=%v",
			before.Labels, before.Annotations, after.Labels, after.Annotations)
	}
}

// No route before, and a port and an alias in one request: the new route
// satisfies the alias precondition and both are applied.
func TestWebProxyUpdateRouteAndAliasTogether(t *testing.T) {
	ctx := context.Background()
	c := newTestAliasClaims(time.Now(), "ws-a")
	pods := newWebProxyUpdateFixture(t, c, "")

	u, err := prepareWebProxyUpdate(ctx, pods, c, "ws-a", "ws-a", UpdateOptions{
		ReplaceWebProxy: true, WebProxyPort: 3000, WebProxyRoles: []string{"qa"},
		ChangeWebProxyAlias: true, WebProxyAlias: "nats",
	})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	port, alias, err := u.commit(ctx)
	if err != nil || port != 3000 || alias != "nats" {
		t.Fatalf("commit = (%d, %q, %v), want (3000, nats, nil)", port, alias, err)
	}

	pod := getPod(t, pods)
	if pod.Annotations[helm.AnnotationWebProxyPort] != "3000" || pod.Annotations[helm.AnnotationWebProxyRoles] != `["qa"]` ||
		pod.Labels[helm.LabelWebProxy] != "true" || pod.Labels[helm.LabelWebProxyAlias] != "nats" {
		t.Fatalf("pod not updated: labels=%v annotations=%v", pod.Labels, pod.Annotations)
	}
	if holderOf(t, c, "nats") != "ws-a" {
		t.Fatal("alias not claimed")
	}
}

// Port 0 and an alias in one request: the update would leave no route, so it
// fails with ErrNoWebProxyRoute and the existing route is not cleared.
func TestWebProxyUpdateAliasWithRouteClearRejected(t *testing.T) {
	ctx := context.Background()
	c := newTestAliasClaims(time.Now(), "ws-a")
	pods := newWebProxyUpdateFixture(t, c, "8080")
	before := getPod(t, pods)

	_, err := prepareWebProxyUpdate(ctx, pods, c, "ws-a", "ws-a", UpdateOptions{
		ReplaceWebProxy: true, WebProxyPort: 0,
		ChangeWebProxyAlias: true, WebProxyAlias: "nats",
	})
	if !errors.Is(err, ErrNoWebProxyRoute) {
		t.Fatalf("prepare err = %v, want ErrNoWebProxyRoute", err)
	}
	assertPodMetadataUnchanged(t, before, getPod(t, pods))
	if holderOf(t, c, "nats") != "" {
		t.Fatal("alias claimed despite the rejected update")
	}
}

// A port and an alias that is already taken: the update fails with
// ErrWebProxyAliasHeld and the route is not changed either.
func TestWebProxyUpdateTakenAliasLeavesRouteUnchanged(t *testing.T) {
	ctx := context.Background()
	c := newTestAliasClaims(time.Now(), "ws-a", "ws-b")
	pods := newWebProxyUpdateFixture(t, c, "8080")
	if _, err := c.claim(ctx, testOrg, "nats", "ws-b", "ws-b"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	before := getPod(t, pods)

	_, err := prepareWebProxyUpdate(ctx, pods, c, "ws-a", "ws-a", UpdateOptions{
		ReplaceWebProxy: true, WebProxyPort: 9090,
		ChangeWebProxyAlias: true, WebProxyAlias: "nats",
	})
	if !errors.Is(err, ErrWebProxyAliasHeld) {
		t.Fatalf("prepare err = %v, want ErrWebProxyAliasHeld", err)
	}
	assertPodMetadataUnchanged(t, before, getPod(t, pods))
	if holderOf(t, c, "nats") != "ws-b" {
		t.Fatal("holder of the taken alias changed")
	}
}

// When a later step of the update fails, the alias the update claimed is
// given back and the pod keeps its previous alias and claim.
func TestWebProxyUpdateAbortReleasesNewClaim(t *testing.T) {
	ctx := context.Background()
	c := newTestAliasClaims(time.Now(), "ws-a")
	pods := newWebProxyUpdateFixture(t, c, "8080")
	if _, _, err := c.reconcile(ctx, testOrg, "ws-a", "ws-a", "old"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	pod := getPod(t, pods)
	pod.Labels[helm.LabelWebProxyAlias] = "old"
	if _, err := pods.Update(ctx, pod, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update pod: %v", err)
	}
	before := getPod(t, pods)

	u, err := prepareWebProxyUpdate(ctx, pods, c, "ws-a", "ws-a",
		UpdateOptions{ChangeWebProxyAlias: true, WebProxyAlias: "new"})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	u.abort(ctx)

	assertPodMetadataUnchanged(t, before, getPod(t, pods))
	if holderOf(t, c, "new") != "" || holderOf(t, c, "old") != "ws-a" {
		t.Fatal("abort did not restore the claims")
	}
}

// Clearing the route clears the alias, its message and its claim, in the
// same patch.
func TestWebProxyUpdateRouteClearClearsAlias(t *testing.T) {
	ctx := context.Background()
	c := newTestAliasClaims(time.Now(), "ws-a")
	pods := newWebProxyUpdateFixture(t, c, "8080")
	if _, _, err := c.reconcile(ctx, testOrg, "ws-a", "ws-a", "nats"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	pod := getPod(t, pods)
	pod.Labels[helm.LabelWebProxyAlias] = "nats"
	pod.Annotations[helm.AnnotationWebProxyAliasMessage] = "stale"
	if _, err := pods.Update(ctx, pod, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update pod: %v", err)
	}

	u, err := prepareWebProxyUpdate(ctx, pods, c, "ws-a", "ws-a", UpdateOptions{ReplaceWebProxy: true})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if port, alias, err := u.commit(ctx); err != nil || port != 0 || alias != "" {
		t.Fatalf("commit = (%d, %q, %v), want route and alias cleared", port, alias, err)
	}

	pod = getPod(t, pods)
	for _, k := range []string{helm.LabelWebProxy, helm.LabelWebProxyAlias} {
		if _, ok := pod.Labels[k]; ok {
			t.Fatalf("label %s not cleared", k)
		}
	}
	for _, k := range []string{helm.AnnotationWebProxyPort, helm.AnnotationWebProxyRoles, helm.AnnotationWebProxyAliasMessage} {
		if _, ok := pod.Annotations[k]; ok {
			t.Fatalf("annotation %s not cleared", k)
		}
	}
	if holderOf(t, c, "nats") != "" {
		t.Fatal("claim not released")
	}
}

// A route-only update leaves the alias and its claim alone.
func TestWebProxyUpdateRouteOnlyKeepsAlias(t *testing.T) {
	ctx := context.Background()
	c := newTestAliasClaims(time.Now(), "ws-a")
	pods := newWebProxyUpdateFixture(t, c, "8080")
	if _, _, err := c.reconcile(ctx, testOrg, "ws-a", "ws-a", "nats"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	pod := getPod(t, pods)
	pod.Labels[helm.LabelWebProxyAlias] = "nats"
	if _, err := pods.Update(ctx, pod, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update pod: %v", err)
	}

	u, err := prepareWebProxyUpdate(ctx, pods, c, "ws-a", "ws-a", UpdateOptions{ReplaceWebProxy: true, WebProxyPort: 9090})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if port, alias, err := u.commit(ctx); err != nil || port != 9090 || alias != "nats" {
		t.Fatalf("commit = (%d, %q, %v), want (9090, nats, nil)", port, alias, err)
	}
	if getPod(t, pods).Labels[helm.LabelWebProxyAlias] != "nats" || holderOf(t, c, "nats") != "ws-a" {
		t.Fatal("route-only update touched the alias")
	}
}
