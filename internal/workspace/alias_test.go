// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package workspace

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/k8shell-io/common/pkg/models"
	"github.com/k8shell-io/provisioner/internal/helm"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

const testOrg = "acme"

// newTestAliasClaims returns an aliasClaims backed by a fake clientset.
// Workspaces in live are reported as existing; the clock is fixed at now.
func newTestAliasClaims(now time.Time, live ...string) *aliasClaims {
	liveSet := map[string]bool{}
	for _, w := range live {
		liveSet[w] = true
	}
	return &aliasClaims{
		kube:      fake.NewSimpleClientset(),
		namespace: "test",
		holderExists: func(_ context.Context, workspace string) (bool, error) {
			return liveSet[workspace], nil
		},
		now: func() time.Time { return now },
	}
}

func holderOf(t *testing.T, c *aliasClaims, alias string) string {
	t.Helper()
	holder, found, err := c.holder(context.Background(), testOrg, alias)
	if err != nil {
		t.Fatalf("holder(%q): %v", alias, err)
	}
	if !found {
		return ""
	}
	return holder
}

// Two workspaces provisioned from one blueprint with a static alias: the
// second is provisioned without the alias and told why, not failed.
func TestReconcileStaticAliasSecondWorkspaceWarned(t *testing.T) {
	ctx := context.Background()
	c := newTestAliasClaims(time.Now(), "ws-a", "ws-b")

	applied, msg, err := c.reconcile(ctx, testOrg, "ws-a", "ws-a", "nats-course")
	if err != nil || applied != "nats-course" || msg != "" {
		t.Fatalf("first reconcile = (%q, %q, %v), want the alias applied", applied, msg, err)
	}

	applied, msg, err = c.reconcile(ctx, testOrg, "ws-b", "ws-b", "nats-course")
	if err != nil {
		t.Fatalf("second reconcile failed: %v", err)
	}
	if applied != "" {
		t.Fatalf("second workspace got alias %q, want none", applied)
	}
	if want := `alias "nats-course" is held by workspace ws-a; not applied`; msg != want {
		t.Fatalf("message = %q, want %q", msg, want)
	}
	if h := holderOf(t, c, "nats-course"); h != "ws-a" {
		t.Fatalf("holder = %q, want ws-a", h)
	}
}

func TestReconcileAliasScopedPerOrganization(t *testing.T) {
	ctx := context.Background()
	c := newTestAliasClaims(time.Now())

	if applied, _, _ := c.reconcile(ctx, "org-1", "ws-a", "ws-a", "nats"); applied != "nats" {
		t.Fatalf("org-1 did not get the alias")
	}
	if applied, msg, _ := c.reconcile(ctx, "org-2", "ws-b", "ws-b", "nats"); applied != "nats" {
		t.Fatalf("org-2 did not get the alias: %s", msg)
	}
}

// An alias that only turns out invalid after CEL evaluation is not applied,
// and doesn't fail provisioning.
func TestReconcileInvalidAliasNotApplied(t *testing.T) {
	c := newTestAliasClaims(time.Now())
	applied, msg, err := c.reconcile(context.Background(), testOrg, "ws-a", "ws-a", "Bad.User-nats")
	if err != nil || applied != "" {
		t.Fatalf("reconcile = (%q, %v), want no alias and no error", applied, err)
	}
	if want := `alias "Bad.User-nats" is not a valid DNS label; not applied`; msg != want {
		t.Fatalf("message = %q, want %q", msg, want)
	}
}

// A CEL alias that evaluates to all digits would be read as the port form
// by the web proxy, so it is not applied.
func TestReconcileAllDigitAliasNotApplied(t *testing.T) {
	c := newTestAliasClaims(time.Now())
	applied, msg, err := c.reconcile(context.Background(), testOrg, "ws-a", "ws-a", "8080")
	if err != nil || applied != "" || msg == "" {
		t.Fatalf("reconcile = (%q, %q, %v), want no alias with a message", applied, msg, err)
	}
	if holderOf(t, c, "8080") != "" {
		t.Fatal("all-digit alias was claimed")
	}
}

// Re-provisioning a workspace that already holds its alias keeps it.
func TestReconcileReprovisionKeepsOwnAlias(t *testing.T) {
	ctx := context.Background()
	c := newTestAliasClaims(time.Now(), "ws-a")
	for i := range 2 {
		applied, msg, err := c.reconcile(ctx, testOrg, "ws-a", "ws-a", "nats")
		if err != nil || applied != "nats" || msg != "" {
			t.Fatalf("reconcile #%d = (%q, %q, %v), want the alias kept", i, applied, msg, err)
		}
	}
}

// An alias set through the update RPC lasts until the next re-provision,
// which reverts to the blueprint's alias and frees the override.
func TestReprovisionRevertsUpdateAlias(t *testing.T) {
	ctx := context.Background()
	c := newTestAliasClaims(time.Now(), "ws-a")

	if applied, _, _ := c.reconcile(ctx, testOrg, "ws-a", "ws-a", "blue"); applied != "blue" {
		t.Fatal("blueprint alias not applied")
	}

	pod := aliasTestPod("ws-a", "blue")
	keep, err := c.claimForUpdate(ctx, pod, UpdateOptions{ChangeWebProxyAlias: true, WebProxyAlias: "override"}, "ws-a")
	if err != nil {
		t.Fatalf("claimForUpdate: %v", err)
	}
	if err := c.release(ctx, "ws-a", keep); err != nil {
		t.Fatalf("release: %v", err)
	}
	if holderOf(t, c, "blue") != "" || holderOf(t, c, "override") != "ws-a" {
		t.Fatal("update did not move the claim from blue to override")
	}

	if applied, _, _ := c.reconcile(ctx, testOrg, "ws-a", "ws-a", "blue"); applied != "blue" {
		t.Fatal("re-provision did not revert to the blueprint alias")
	}
	if holderOf(t, c, "override") != "" {
		t.Fatal("re-provision did not free the override alias")
	}
}

func TestReconcileEmptyAliasReleasesClaims(t *testing.T) {
	ctx := context.Background()
	c := newTestAliasClaims(time.Now(), "ws-a")
	if applied, _, _ := c.reconcile(ctx, testOrg, "ws-a", "ws-a", "nats"); applied != "nats" {
		t.Fatal("alias not applied")
	}
	if _, _, err := c.reconcile(ctx, testOrg, "ws-a", "ws-a", ""); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if holderOf(t, c, "nats") != "" {
		t.Fatal("claim not released")
	}
}

// Race: concurrent claims of one alias must leave exactly one holder, with
// every other claimer told who holds it.
func TestClaimConcurrentSameAlias(t *testing.T) {
	const claimers = 16
	for round := range 20 {
		c := newTestAliasClaims(time.Now())
		var wg sync.WaitGroup
		results := make([]error, claimers)
		for i := range claimers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ws := fmt.Sprintf("ws-%d", i)
				_, results[i] = c.claim(context.Background(), testOrg, "nats", ws, ws)
			}()
		}
		wg.Wait()

		winners := 0
		for i, err := range results {
			switch {
			case err == nil:
				winners++
			case !errors.Is(err, ErrWebProxyAliasHeld):
				t.Fatalf("round %d: claimer %d: unexpected error %v", round, i, err)
			}
		}
		if winners != 1 {
			t.Fatalf("round %d: %d claimers won, want exactly 1", round, winners)
		}
	}
}

// A claim whose holder is gone is taken over once the grace period has
// passed, but not before: the holder may still be installing its release.
func TestClaimStaleTakeover(t *testing.T) {
	ctx := context.Background()
	start := time.Now()
	c := newTestAliasClaims(start, "ws-b")

	if _, err := c.claim(ctx, testOrg, "nats", "ghost", "ghost"); err != nil {
		t.Fatalf("claim: %v", err)
	}

	if _, err := c.claim(ctx, testOrg, "nats", "ws-b", "ws-b"); !errors.Is(err, ErrWebProxyAliasHeld) {
		t.Fatalf("claim within grace = %v, want ErrWebProxyAliasHeld", err)
	}

	c.now = func() time.Time { return start.Add(staleAliasClaimGrace + time.Second) }
	if _, err := c.claim(ctx, testOrg, "nats", "ws-b", "ws-b"); err != nil {
		t.Fatalf("claim after grace: %v", err)
	}
	if h := holderOf(t, c, "nats"); h != "ws-b" {
		t.Fatalf("holder = %q, want ws-b", h)
	}
}

func TestClaimLiveHolderNotTakenOver(t *testing.T) {
	ctx := context.Background()
	start := time.Now()
	c := newTestAliasClaims(start, "ws-a", "ws-b")
	if _, err := c.claim(ctx, testOrg, "nats", "ws-a", "ws-a"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	c.now = func() time.Time { return start.Add(24 * time.Hour) }
	if _, err := c.claim(ctx, testOrg, "nats", "ws-b", "ws-b"); !errors.Is(err, ErrWebProxyAliasHeld) {
		t.Fatalf("claim = %v, want ErrWebProxyAliasHeld", err)
	}
}

func TestReleaseOnlyTouchesOwnClaims(t *testing.T) {
	ctx := context.Background()
	c := newTestAliasClaims(time.Now())
	for _, alias := range []string{"one", "two"} {
		if _, err := c.claim(ctx, testOrg, alias, "ws-a", "ws-a"); err != nil {
			t.Fatalf("claim %s: %v", alias, err)
		}
	}
	if _, err := c.claim(ctx, testOrg, "three", "ws-b", "ws-b"); err != nil {
		t.Fatalf("claim three: %v", err)
	}

	if err := c.release(ctx, "ws-a", aliasClaimName(testOrg, "two")); err != nil {
		t.Fatalf("release: %v", err)
	}
	if holderOf(t, c, "one") != "" || holderOf(t, c, "two") != "ws-a" || holderOf(t, c, "three") != "ws-b" {
		t.Fatal("release removed the wrong claims")
	}
}

// Deleting a workspace sweeps namespaced objects by canonical-id label (see
// Workspace.Uninstall), which must include its alias claim.
func TestAliasClaimCarriesCanonicalIdForUninstallSweep(t *testing.T) {
	c := newTestAliasClaims(time.Now())
	name, err := c.claim(context.Background(), testOrg, "nats", "ws-a", "canon-a")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	cm, err := c.kube.CoreV1().ConfigMaps("test").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get claim: %v", err)
	}
	if cm.Labels[helm.LabelCanonicalId] != "canon-a" {
		t.Fatalf("claim labels = %v, want canonical-id canon-a", cm.Labels)
	}
}

func aliasTestPod(name, alias string) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   "test",
			Labels:      map[string]string{helm.LabelOrganization: testOrg},
			Annotations: map[string]string{helm.AnnotationWebProxyPort: "8080"},
		},
	}
	if alias != "" {
		pod.Labels[helm.LabelWebProxyAlias] = alias
	}
	return pod
}

func TestClaimForUpdate(t *testing.T) {
	ctx := context.Background()

	t.Run("collision is ErrWebProxyAliasHeld", func(t *testing.T) {
		c := newTestAliasClaims(time.Now(), "ws-a", "ws-b")
		if _, err := c.claim(ctx, testOrg, "nats", "ws-a", "ws-a"); err != nil {
			t.Fatalf("claim: %v", err)
		}
		_, err := c.claimForUpdate(ctx, aliasTestPod("ws-b", ""),
			UpdateOptions{ChangeWebProxyAlias: true, WebProxyAlias: "nats"}, "ws-b")
		if !errors.Is(err, ErrWebProxyAliasHeld) {
			t.Fatalf("err = %v, want ErrWebProxyAliasHeld", err)
		}
	})

	t.Run("no route is ErrNoWebProxyRoute", func(t *testing.T) {
		c := newTestAliasClaims(time.Now())
		pod := aliasTestPod("ws-a", "")
		delete(pod.Annotations, helm.AnnotationWebProxyPort)
		_, err := c.claimForUpdate(ctx, pod, UpdateOptions{ChangeWebProxyAlias: true, WebProxyAlias: "nats"}, "ws-a")
		if !errors.Is(err, ErrNoWebProxyRoute) {
			t.Fatalf("err = %v, want ErrNoWebProxyRoute", err)
		}
	})

	t.Run("route published by the same update", func(t *testing.T) {
		c := newTestAliasClaims(time.Now())
		pod := aliasTestPod("ws-a", "")
		delete(pod.Annotations, helm.AnnotationWebProxyPort)
		_, err := c.claimForUpdate(ctx, pod, UpdateOptions{
			ReplaceWebProxy: true, WebProxyPort: 3000,
			ChangeWebProxyAlias: true, WebProxyAlias: "nats",
		}, "ws-a")
		if err != nil {
			t.Fatalf("claimForUpdate: %v", err)
		}
	})

	t.Run("invalid alias", func(t *testing.T) {
		c := newTestAliasClaims(time.Now())
		_, err := c.claimForUpdate(ctx, aliasTestPod("ws-a", ""),
			UpdateOptions{ChangeWebProxyAlias: true, WebProxyAlias: "a--b"}, "ws-a")
		if !errors.Is(err, models.ErrInvalidParameters) {
			t.Fatalf("err = %v, want ErrInvalidParameters", err)
		}
	})

	t.Run("clearing claims nothing", func(t *testing.T) {
		c := newTestAliasClaims(time.Now())
		keep, err := c.claimForUpdate(ctx, aliasTestPod("ws-a", "nats"),
			UpdateOptions{ChangeWebProxyAlias: true, WebProxyAlias: ""}, "ws-a")
		if err != nil || keep != "" {
			t.Fatalf("claimForUpdate = (%q, %v), want nothing claimed", keep, err)
		}
	})
}

func TestApplyWebProxyAliasToPod(t *testing.T) {
	pod := aliasTestPod("ws-a", "nats")
	pod.Annotations[helm.AnnotationWebProxyAliasMessage] = "old"

	applyWebProxyAliasToPod(pod, "", `alias "nats" is held by workspace ws-b; not applied`)
	if _, ok := pod.Labels[helm.LabelWebProxyAlias]; ok {
		t.Fatal("lost alias still stamped on the pod")
	}
	if pod.Annotations[helm.AnnotationWebProxyAliasMessage] == "old" {
		t.Fatal("message not replaced")
	}

	applyWebProxyAliasToPod(pod, "nats", "")
	if pod.Labels[helm.LabelWebProxyAlias] != "nats" {
		t.Fatal("alias not stamped")
	}
}
