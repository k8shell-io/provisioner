// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package nodepool

import (
	"context"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func node(name string, labels map[string]string, ready, cordoned bool) *corev1.Node {
	st := corev1.ConditionFalse
	if ready {
		st = corev1.ConditionTrue
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Spec:       corev1.NodeSpec{Unschedulable: cordoned},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
			{Type: corev1.NodeReady, Status: st},
		}},
	}
}

func TestSummarize(t *testing.T) {
	nodes := []*corev1.Node{
		node("a", map[string]string{"pool": "x", "zone": "z1"}, true, false),
		node("b", map[string]string{"pool": "x"}, true, true),   // cordoned
		node("c", map[string]string{"pool": "x"}, false, false), // not ready
		node("d", map[string]string{"pool": "w"}, true, false),
		node("e", map[string]string{"other": "q"}, true, false), // no keys
	}
	got := summarize([]string{"pool", "zone", "missing"}, nodes)
	want := []Key{
		{Key: "pool", Values: []Value{
			{Value: "w", NodeCount: 1, ReadyCount: 1, SchedulableCount: 1},
			{Value: "x", NodeCount: 3, ReadyCount: 2, SchedulableCount: 1},
		}},
		{Key: "zone", Values: []Value{{Value: "z1", NodeCount: 1, ReadyCount: 1, SchedulableCount: 1}}},
		{Key: "missing", Values: []Value{}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestDiscoveryStartAndOptions(t *testing.T) {
	client := fake.NewSimpleClientset(
		node("a", map[string]string{"pool": "x"}, true, false),
		node("b", map[string]string{"pool": "x"}, true, true),
	)
	d := New(client, []string{"pool"})
	if d.Options() != nil {
		t.Fatal("Options before Start must be nil")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatal(err)
	}
	got := d.Options()
	want := []Key{{Key: "pool", Values: []Value{{Value: "x", NodeCount: 2, ReadyCount: 2, SchedulableCount: 1}}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestStartNoKeys(t *testing.T) {
	d := New(fake.NewSimpleClientset(), nil)
	if err := d.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d.Options() != nil {
		t.Fatal("expected nil options")
	}
}
