// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package workspace

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/k8shell-io/common/pkg/models"
	"github.com/k8shell-io/provisioner/internal/helm"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// lostPod is a pod whose deletion deadline passed `overdue` ago on node n1,
// with the node controller's not-Ready mark and stale Running containers.
func lostPod(overdue time.Duration) *corev1.Pod {
	ts := metav1.NewTime(time.Now().Add(-overdue))
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &ts, Finalizers: []string{"test/hold"}},
		Spec:       corev1.PodSpec{NodeName: "n1"},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "c", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
			},
		},
	}
}

func TestAnalyzePodNodeLost(t *testing.T) {
	notLost := func(mut func(*corev1.Pod)) *corev1.Pod { p := lostPod(time.Minute); mut(p); return p }
	tests := []struct {
		name string
		pod  *corev1.Pod
		want models.WorkspaceStatusMessage
	}{
		{"stuck past deadline", lostPod(time.Minute), models.WorkspaceStatusNodeLost},
		{"within grace slack", lostPod(5 * time.Second), models.WorkspaceStatusTerminating},
		{"deadline not reached", lostPod(-20 * time.Second), models.WorkspaceStatusTerminating},
		{"still ready", notLost(func(p *corev1.Pod) { p.Status.Conditions[0].Status = corev1.ConditionTrue }), models.WorkspaceStatusTerminating},
		{"containers terminated", notLost(func(p *corev1.Pod) {
			p.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}
		}), models.WorkspaceStatusTerminating},
		{"not scheduled", notLost(func(p *corev1.Pod) { p.Spec.NodeName = "" }), models.WorkspaceStatusTerminating},
		{"stop requested too", notLost(func(p *corev1.Pod) { p.Labels = map[string]string{helm.LabelStopRequested: "true"} }), models.WorkspaceStatusNodeLost},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snap := AnalyzePod(tt.pod, nil, defaultCrashLoopThreshold)
			if snap.Status != tt.want {
				t.Errorf("status = %s, want %s", snap.Status, tt.want)
			}
			if tt.want == models.WorkspaceStatusNodeLost &&
				snap.Message != "Node n1 is unreachable; workspace will stop when it returns" {
				t.Errorf("message = %q", snap.Message)
			}
		})
	}
}

func TestStopPodRefusedWhenNodeLost(t *testing.T) {
	f := newProvisionFixture(t, lostPod(time.Minute), func() bool { return true })
	err := f.w.StopPod(context.Background())
	if !errors.Is(err, ErrNodeUnreachable) {
		t.Fatalf("err = %v, want ErrNodeUnreachable", err)
	}
	if !strings.Contains(err.Error(), "terminated") {
		t.Errorf("error should point at terminate: %v", err)
	}
	pod, err := f.kube.CoreV1().Pods(provisionTestNS).Get(context.Background(), "ws", metav1.GetOptions{})
	if err != nil || pod.Labels[helm.LabelStopRequested] != "" {
		t.Errorf("pod must be left untouched: %v %v", err, pod.Labels)
	}
}

func TestProvisionExistingNodeLostFailsFast(t *testing.T) {
	f := newProvisionFixture(t, lostPod(time.Minute), func() bool { return true })
	start := time.Now()
	_, err := f.w.provisionExisting(context.Background(), &ProvisionOptions{Timeout: 5},
		&models.WorkspaceStatus{Status: models.WorkspaceStatusNodeLost, Message: "Node n1 is unreachable"})
	if !errors.Is(err, ErrNodeUnreachable) {
		t.Fatalf("err = %v, want ErrNodeUnreachable", err)
	}
	if time.Since(start) > time.Second || f.started != 0 || f.installed != 0 {
		t.Errorf("must fail fast without starting (took %s)", time.Since(start))
	}
}
