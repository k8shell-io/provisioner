// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package workspace

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestParseNodeSelectors(t *testing.T) {
	got, err := ParseNodeSelectors(map[string]string{
		"k8shell.io/pool": "gpu, cpu,gpu",
		"zone":            "a",
	})
	if err != nil {
		t.Fatalf("valid selectors rejected: %v", err)
	}
	want := map[string][]string{"k8shell.io/pool": {"gpu", "cpu"}, "zone": {"a"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}

	for _, bad := range []map[string]string{
		{"bad key!": "x"},
		{"pool": "has space"},
		{"pool": "a,,b"},
		{"pool": ""},
		{"": "x"},
	} {
		if _, err := ParseNodeSelectors(bad); err == nil {
			t.Errorf("expected error for %v", bad)
		}
	}
}

func TestSetNodeSelectors(t *testing.T) {
	w := &Workspace{}
	if err := w.SetNodeSelectors(nil); err != nil {
		t.Fatalf("empty selectors rejected: %v", err)
	}
	if err := w.SetNodeSelectors(map[string]string{"pool": "a b"}); err == nil {
		t.Error("expected error")
	}
	if len(w.nodeSelectors) != 0 {
		t.Errorf("invalid selectors stored: %v", w.nodeSelectors)
	}
}

func TestPodNodeSelectors(t *testing.T) {
	in := func(key string, vals ...string) corev1.NodeSelectorRequirement {
		return corev1.NodeSelectorRequirement{Key: key, Operator: corev1.NodeSelectorOpIn, Values: vals}
	}
	pod := &corev1.Pod{Spec: corev1.PodSpec{
		Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{
					in(corev1.LabelHostname, "node-1"),
					in("k8shell.io/pool", "gpu", "cpu"),
					{Key: "zone", Operator: corev1.NodeSelectorOpNotIn, Values: []string{"b"}},
				}}},
			},
		}},
	}}
	got := podNodeSelectors(pod)
	want := map[string]string{"k8shell.io/pool": "gpu,cpu"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if got := podNodeSelectors(&corev1.Pod{}); got != nil {
		t.Errorf("unconstrained pod: got %v, want nil", got)
	}
}
