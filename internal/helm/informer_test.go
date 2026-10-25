// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package helm

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"helm.sh/helm/v3/pkg/release"
	helmtime "helm.sh/helm/v3/pkg/time"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8slabels "k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes/fake"
)

const testTargetNamespace = "workspaces"

func testReleaseSecret(t *testing.T, name, manifest string, deployed time.Time) *corev1.Secret {
	t.Helper()
	rel := &release.Release{
		Name:     name,
		Manifest: manifest,
		Info:     &release.Info{LastDeployed: helmtime.Time{Time: deployed}},
	}
	raw, err := json.Marshal(rel)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sh.helm.release.v1." + name + ".v1",
			Namespace: testTargetNamespace,
			Labels: map[string]string{
				"owner": "helm", "status": "deployed", "name": name,
				"app.kubernetes.io/name": WORKSPACE_CHART_NAME,
			},
		},
		Data: map[string][]byte{"release": []byte(base64.StdEncoding.EncodeToString(buf.Bytes()))},
	}
}

func testPod(name, namespace string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels}}
}

func startTestInformers(t *testing.T, injectNamespaces []string, objs ...interface{}) (*Client, *fake.Clientset) {
	t.Helper()
	kube := fake.NewClientset()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	for _, o := range objs {
		var err error
		switch v := o.(type) {
		case *corev1.Pod:
			_, err = kube.CoreV1().Pods(v.Namespace).Create(ctx, v, metav1.CreateOptions{})
		case *corev1.Secret:
			_, err = kube.CoreV1().Secrets(v.Namespace).Create(ctx, v, metav1.CreateOptions{})
		}
		if err != nil {
			t.Fatal(err)
		}
	}

	l := zerolog.Nop()
	c := &Client{
		log:             &l,
		kubeClient:      kube,
		targetNamespace: testTargetNamespace,
		releaseCache:    newTTLCache[[]*ReleasePod](),
		podCache:        newTTLCache[[]corev1.Pod](),
	}
	c.StartWorkspaceInformers(ctx, injectNamespaces)
	waitFor(t, func() bool {
		inf := c.informers.Load()
		if !inf.targetPods.HasSynced() || !inf.releases.HasSynced() {
			return false
		}
		for _, i := range inf.injectedPods {
			if !i.HasSynced() {
				return false
			}
		}
		return true
	})
	return c, kube
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met before deadline")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

const testPodManifest = `---
apiVersion: v1
kind: ConfigMap
metadata:
  name: ws-1-config
---
apiVersion: v1
kind: Pod
metadata:
  name: ws-1
  labels:
    k8shell.io/username: alice
`

func TestReleasePodInformerDecodesOnce(t *testing.T) {
	deployed := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	c, kube := startTestInformers(t, nil,
		testReleaseSecret(t, "ws-1", testPodManifest, deployed),
		testReleaseSecret(t, "other", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n", deployed),
	)

	rps, err := c.ListDeployedReleasePodsCached(context.Background(), testTargetNamespace)
	if err != nil {
		t.Fatal(err)
	}
	if len(rps) != 2 {
		t.Fatalf("expected 2 release pods, got %d", len(rps))
	}
	byName := map[string]*ReleasePod{}
	for _, rp := range rps {
		byName[rp.Labels["name"]] = rp
	}
	ws := byName["ws-1"]
	if ws == nil || ws.Pod == nil || ws.Pod.Name != "ws-1" || ws.Pod.Labels[LabelUsername] != "alice" {
		t.Fatalf("workspace release pod not decoded: %+v", ws)
	}
	if !ws.LastDeployed.Equal(deployed) {
		t.Errorf("LastDeployed = %v, want %v", ws.LastDeployed, deployed)
	}
	if other := byName["other"]; other == nil || other.Pod != nil {
		t.Errorf("non-workspace release should be kept with a nil Pod: %+v", other)
	}

	// An uninstalled release drops out without waiting out a TTL.
	if err := kube.CoreV1().Secrets(testTargetNamespace).Delete(context.Background(),
		"sh.helm.release.v1.ws-1.v1", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		rps, _ := c.ListDeployedReleasePodsCached(context.Background(), testTargetNamespace)
		return len(rps) == 1
	})
}

func TestListPodsCachedFromInformers(t *testing.T) {
	injected := map[string]string{LabelInjected: "true", LabelUsername: "bob"}
	c, kube := startTestInformers(t, []string{"apps"},
		testPod("ws-1", testTargetNamespace, map[string]string{LabelUsername: "alice"}),
		testPod("ws-2", testTargetNamespace, map[string]string{LabelUsername: "bob"}),
		testPod("app-1", "apps", injected),
		testPod("app-2", "apps", map[string]string{LabelUsername: "bob"}),
	)
	ctx := context.Background()

	pods, err := c.ListPodsCached(ctx, testTargetNamespace, LabelUsername+"=alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(pods) != 1 || pods[0].Name != "ws-1" {
		t.Fatalf("expected [ws-1], got %v", podNames(pods))
	}

	pods, err = c.ListPodsCached(ctx, "apps", LabelInjected+"=true,"+LabelUsername+"=bob")
	if err != nil {
		t.Fatal(err)
	}
	if len(pods) != 1 || pods[0].Name != "app-1" {
		t.Fatalf("expected [app-1], got %v", podNames(pods))
	}

	// The informers pick up changes without waiting out a TTL.
	if _, err := kube.CoreV1().Pods(testTargetNamespace).Create(ctx,
		testPod("ws-3", testTargetNamespace, map[string]string{LabelUsername: "alice"}), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		pods, _ := c.ListPodsCached(ctx, testTargetNamespace, LabelUsername+"=alice")
		return len(pods) == 2
	})
}

func TestListPodsCachedFallsBackWhenNotCovered(t *testing.T) {
	c, _ := startTestInformers(t, []string{"apps"},
		testPod("app-2", "apps", map[string]string{LabelUsername: "bob"}),
		testPod("x", "elsewhere", map[string]string{LabelInjected: "true"}),
	)
	inf := c.informers.Load()

	// The injected-pods informer only holds injected pods, so it must not
	// answer a selector that doesn't require them.
	if _, ok, _ := inf.listPods("apps", LabelUsername+"=bob"); ok {
		t.Error("injected informer served a selector not requiring injected pods")
	}
	// Namespaces outside injectNamespaces aren't watched.
	if _, ok, _ := inf.listPods("elsewhere", LabelInjected+"=true"); ok {
		t.Error("served a namespace no informer watches")
	}

	pods, err := c.ListPodsCached(context.Background(), "apps", LabelUsername+"=bob")
	if err != nil {
		t.Fatal(err)
	}
	if len(pods) != 1 || pods[0].Name != "app-2" {
		t.Fatalf("fallback listing: expected [app-2], got %v", podNames(pods))
	}
}

func TestListPodsCachedClusterWideInjected(t *testing.T) {
	c, _ := startTestInformers(t, []string{"*"},
		testPod("a", "ns-a", map[string]string{LabelInjected: "true"}),
		testPod("b", "ns-b", map[string]string{LabelInjected: "true"}),
	)
	inf := c.informers.Load()

	pods, ok, err := inf.listPods("", LabelInjected+"=true")
	if err != nil || !ok || len(pods) != 2 {
		t.Fatalf("cluster-wide: ok=%v err=%v pods=%v", ok, err, podNames(pods))
	}
	pods, ok, err = inf.listPods("ns-b", LabelInjected+" in (true)")
	if err != nil || !ok || len(pods) != 1 || pods[0].Name != "b" {
		t.Fatalf("single namespace: ok=%v err=%v pods=%v", ok, err, podNames(pods))
	}
}

func TestRequiresInjected(t *testing.T) {
	cases := map[string]bool{
		LabelInjected + "=true":                 true,
		LabelInjected + "==true":                true,
		LabelInjected + " in (true)":            true,
		LabelInjected + " in (true,false)":      false,
		LabelInjected + "!=true":                false,
		LabelUsername + "=bob":                  false,
		LabelUsername + "=bob," + LabelInjected: false,
		"":                                      false,
	}
	for s, want := range cases {
		sel, err := k8slabels.Parse(s)
		if err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		if got := requiresInjected(sel); got != want {
			t.Errorf("requiresInjected(%q) = %v, want %v", s, got, want)
		}
	}
}

func podNames(pods []corev1.Pod) []string {
	names := make([]string, 0, len(pods))
	for _, p := range pods {
		names = append(names, p.Name)
	}
	return names
}
