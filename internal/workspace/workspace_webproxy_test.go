// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package workspace

import (
	"encoding/base64"
	"testing"

	"github.com/k8shell-io/common/pkg/models"
	"github.com/k8shell-io/common/pkg/userstr"
	"github.com/k8shell-io/provisioner/internal/helm"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// testUserStrB64 canonicalizes a raw userstr and encodes it the way the chart
// stamps k8shell.io/userstr on a workspace pod.
func testUserStrB64(t *testing.T, raw string) string {
	t.Helper()
	u, err := userstr.ParseUserStr(raw)
	if err != nil {
		t.Fatalf("ParseUserStr(%q): %v", raw, err)
	}
	c, err := u.Canonicalize()
	if err != nil {
		t.Fatalf("Canonicalize(%q): %v", raw, err)
	}
	return base64.RawURLEncoding.EncodeToString([]byte(c.CanonicalUserStr()))
}

func baseWebProxyPod(t *testing.T) *corev1.Pod {
	t.Helper()
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "workspace1",
			Namespace: "test",
			Labels: map[string]string{
				helm.LabelCanonicalId: "canon-1",
			},
			Annotations: map[string]string{
				helm.AnnotationUserStr: testUserStrB64(t, "alice~pod=workspace1+ns=test+user=root"),
			},
		},
	}
}

func TestWorkspaceDetailsCoreWebProxy(t *testing.T) {
	t.Run("route present", func(t *testing.T) {
		pod := baseWebProxyPod(t)
		pod.Annotations[helm.AnnotationWebProxyPort] = "8080"
		pod.Annotations[helm.AnnotationWebProxyRoles] = `["developer","qa"]`

		d := workspaceDetailsCore(pod)
		if d == nil {
			t.Fatal("workspaceDetailsCore returned nil")
		}
		if d.WebProxyPort != 8080 {
			t.Fatalf("WebProxyPort = %d, want 8080", d.WebProxyPort)
		}
		if len(d.WebProxyRoles) != 2 || d.WebProxyRoles[0] != models.Role("developer") || d.WebProxyRoles[1] != models.Role("qa") {
			t.Fatalf("WebProxyRoles = %v, want [developer qa]", d.WebProxyRoles)
		}
	})

	t.Run("no route", func(t *testing.T) {
		d := workspaceDetailsCore(baseWebProxyPod(t))
		if d == nil {
			t.Fatal("workspaceDetailsCore returned nil")
		}
		if d.WebProxyPort != 0 {
			t.Fatalf("WebProxyPort = %d, want 0", d.WebProxyPort)
		}
		if d.WebProxyRoles != nil {
			t.Fatalf("WebProxyRoles = %v, want nil", d.WebProxyRoles)
		}
	})

	t.Run("malformed annotations are ignored", func(t *testing.T) {
		pod := baseWebProxyPod(t)
		pod.Annotations[helm.AnnotationWebProxyPort] = "not-a-number"
		pod.Annotations[helm.AnnotationWebProxyRoles] = "{not json}"

		d := workspaceDetailsCore(pod)
		if d == nil {
			t.Fatal("workspaceDetailsCore returned nil")
		}
		if d.WebProxyPort != 0 {
			t.Fatalf("WebProxyPort = %d, want 0", d.WebProxyPort)
		}
		if d.WebProxyRoles != nil {
			t.Fatalf("WebProxyRoles = %v, want nil", d.WebProxyRoles)
		}
	})
}
