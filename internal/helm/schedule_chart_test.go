// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package helm

import (
	"strings"
	"testing"

	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// renderWorkspacePod renders the workspace chart's pod with the given extra
// values and decodes it.
func renderWorkspacePod(t *testing.T, extra map[string]interface{}) *corev1.Pod {
	t.Helper()

	ch, err := LoadChartFromMemory(WORKSPACE_CHART_NAME)
	if err != nil {
		t.Fatalf("load chart: %v", err)
	}
	// Render only the pod and the helpers it includes; the chart's other
	// templates need values that have nothing to do with the schedule.
	kept := ch.Templates[:0]
	for _, tpl := range ch.Templates {
		if strings.HasSuffix(tpl.Name, "/workspace.yaml") || strings.HasSuffix(tpl.Name, "/_helpers.tpl") {
			kept = append(kept, tpl)
		}
	}
	ch.Templates = kept

	values := map[string]interface{}{
		"__registry__":     map[string]interface{}{},
		"__userstr__":      "dXNlcg==",
		"__username__":     "alice",
		"__organization__": "acme",
		"__blueprint__":    "bp",
		"__canonicalid__":  "alice-1",
		"__appversion__":   "1.0.0",
		"k8shelld": map[string]interface{}{
			"image":           "k8shelld:test",
			"imagePullPolicy": "IfNotPresent",
		},
		"__user__":        map[string]interface{}{"username": "alice", "organization": "acme"},
		"__certmanager__": map[string]interface{}{"enabled": false},
		"image":           "dev:1",
	}
	for k, v := range extra {
		values[k] = v
	}

	renderValues, err := chartutil.ToRenderValues(ch, values, chartutil.ReleaseOptions{
		Name: "ws-1", Namespace: "ns", IsInstall: true,
	}, chartutil.DefaultCapabilities)
	if err != nil {
		t.Fatalf("render values: %v", err)
	}
	out, err := engine.Render(ch, renderValues)
	if err != nil {
		t.Fatalf("render chart: %v", err)
	}

	var doc string
	for name, content := range out {
		if strings.HasSuffix(name, "/workspace.yaml") {
			doc = content
		}
	}
	if doc == "" {
		t.Fatal("workspace.yaml not rendered")
	}
	pod := &corev1.Pod{}
	if err := yaml.Unmarshal([]byte(doc), pod); err != nil {
		t.Fatalf("decode pod: %v\n%s", err, doc)
	}
	return pod
}

func TestChartStampsSchedule(t *testing.T) {
	pod := renderWorkspacePod(t, map[string]interface{}{
		"schedule": map[string]interface{}{
			"from":           "2026-11-10T08:00:00Z",
			"to":             "2026-11-10T18:00:00Z",
			"stopGrace":      "15m",
			"terminateGrace": "7d",
		},
	})
	want := map[string]string{
		AnnotationScheduleFrom:           "2026-11-10T08:00:00Z",
		AnnotationScheduleTo:             "2026-11-10T18:00:00Z",
		AnnotationScheduleStopGrace:      "15m",
		AnnotationScheduleTerminateGrace: "7d",
	}
	for k, v := range want {
		if pod.Annotations[k] != v {
			t.Errorf("annotation %s = %q, want %q", k, pod.Annotations[k], v)
		}
	}
}

func TestChartStampsOnlySetScheduleFields(t *testing.T) {
	pod := renderWorkspacePod(t, map[string]interface{}{
		"schedule": map[string]interface{}{"stopGrace": "15m"},
	})
	if pod.Annotations[AnnotationScheduleStopGrace] != "15m" {
		t.Errorf("stop grace not stamped: %v", pod.Annotations)
	}
	for _, k := range []string{AnnotationScheduleFrom, AnnotationScheduleTo, AnnotationScheduleTerminateGrace} {
		if _, ok := pod.Annotations[k]; ok {
			t.Errorf("annotation %s should not be stamped", k)
		}
	}
}

func TestChartWithoutScheduleStampsNothing(t *testing.T) {
	pod := renderWorkspacePod(t, nil)
	for _, k := range []string{AnnotationScheduleFrom, AnnotationScheduleTo, AnnotationScheduleStopGrace, AnnotationScheduleTerminateGrace} {
		if _, ok := pod.Annotations[k]; ok {
			t.Errorf("annotation %s should not be stamped", k)
		}
	}
}
