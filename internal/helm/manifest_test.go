// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package helm

import "testing"

func TestManifestDocuments(t *testing.T) {
	manifest := `---
# Source: k8shell-workspace/templates/pat-secret.yaml
apiVersion: v1
kind: Secret
metadata:
  name: ws-1-pat
stringData:
  token: secret
---
# Source: k8shell-workspace/templates/config.yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: ws-1-config
data:
  config.yaml: "x: 1"
binaryData:
  blob: AAAA
---
# Source: k8shell-workspace/templates/workspace.yaml
apiVersion: v1
kind: Pod
metadata:
  name: ws-1
spec:
  containers:
    - name: shell
      image: alpine
`
	docs, err := ManifestDocuments(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 2 {
		t.Fatalf("expected 2 documents, got %v", docs)
	}
	if _, ok := docs["Secret/ws-1-pat"]; ok {
		t.Fatal("secret must be left out")
	}
	cm := docs["ConfigMap/ws-1-config"]
	if cm == nil || cm["data"] != nil || cm["binaryData"] != nil {
		t.Fatalf("configmap data must be stripped: %v", cm)
	}
	pod := docs["Pod/ws-1"]
	containers, _ := pod["spec"].(map[string]any)["containers"].([]any)
	if len(containers) != 1 || containers[0].(map[string]any)["image"] != "alpine" {
		t.Fatalf("unexpected pod: %v", pod)
	}

	if _, err := ManifestDocuments("kind: [unterminated"); err == nil {
		t.Fatal("expected decode error")
	}
}
