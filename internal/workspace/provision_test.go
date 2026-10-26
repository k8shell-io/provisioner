// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package workspace

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/k8shell-io/common/pkg/models"
	"github.com/k8shell-io/provisioner/internal/helm"
	"github.com/rs/zerolog"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

const provisionTestNS = "workspaces"

func testLogger() *zerolog.Logger {
	l := zerolog.Nop()
	return &l
}

// provisionFixture is a workspace with an existing release, a PVC and a pod,
// backed by a fake clientset. Helm itself is not reachable: any call to the
// real Helm client (including Uninstall) fails, and doStart/doInstallation
// are recorded instead of run.
type provisionFixture struct {
	w         *Workspace
	kube      *fake.Clientset
	pvcUID    types.UID
	started   int
	installed int
}

func newProvisionFixture(t *testing.T, pod *corev1.Pod, releaseExists func() bool) *provisionFixture {
	t.Helper()
	ctx := context.Background()
	kube := fake.NewClientset()

	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: "home", Namespace: provisionTestNS, UID: "pvc-uid-1"}}
	if _, err := kube.CoreV1().PersistentVolumeClaims(provisionTestNS).Create(ctx, pvc, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if pod != nil {
		pod.Name, pod.Namespace = "ws", provisionTestNS
		if _, err := kube.CoreV1().Pods(provisionTestNS).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	f := &provisionFixture{kube: kube, pvcUID: pvc.UID}
	f.w = &Workspace{
		Name:   "ws",
		log:    testLogger(),
		client: helm.NewClientWithKube(kube, provisionTestNS),
		steps: provisionSteps{
			isInstalled: func(context.Context) (bool, error) { return releaseExists(), nil },
			doStart: func(ctx context.Context, _ *ProvisionOptions) (*models.WorkspaceStatus, error) {
				f.started++
				// doStart creates the pod; it must not collide with an old one
				_, err := kube.CoreV1().Pods(provisionTestNS).Create(ctx,
					&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "ws", Namespace: provisionTestNS}}, metav1.CreateOptions{})
				if err != nil {
					return nil, err
				}
				return &models.WorkspaceStatus{Status: models.WorkspaceStatusRunning}, nil
			},
			doInstallation: func(context.Context, *ProvisionOptions) (*models.WorkspaceStatus, error) {
				f.installed++
				return &models.WorkspaceStatus{Status: models.WorkspaceStatusRunning}, nil
			},
		},
	}
	return f
}

// removeOnDeletion mimics the kubelet finishing a graceful termination: the
// pod disappears shortly after it is first seen.
func (f *provisionFixture) removeLater(after time.Duration) {
	go func() {
		time.Sleep(after)
		_ = f.kube.CoreV1().Pods(provisionTestNS).Delete(context.Background(), "ws", metav1.DeleteOptions{})
	}()
}

func TestProvisionExistingKeepsReleaseAndPVC(t *testing.T) {
	deleting := metav1.NewTime(time.Now())
	tests := []struct {
		name       string
		pod        *corev1.Pod
		status     models.WorkspaceStatusMessage
		removeSelf bool // pod outlives the call until removed, as during graceful termination
	}{
		{"stopping", &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			DeletionTimestamp: &deleting, Finalizers: []string{"test/hold"},
			Labels: map[string]string{helm.LabelStopRequested: "true"}}}, models.WorkspaceStatusStopping, true},
		{"terminating", &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			DeletionTimestamp: &deleting, Finalizers: []string{"test/hold"}}}, models.WorkspaceStatusTerminating, true},
		{"failing", &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodFailed}}, models.WorkspaceStatusFailing, false},
		{"stopped", &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodSucceeded}}, models.WorkspaceStatusStopped, false},
		{"unknown", &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodUnknown}}, models.WorkspaceStatusUnknown, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newProvisionFixture(t, tt.pod, func() bool { return true })
			if tt.removeSelf {
				// the fake clientset ignores finalizers, so delete explicitly
				f.removeLater(100 * time.Millisecond)
			}

			st, err := f.w.provisionExisting(context.Background(), &ProvisionOptions{Timeout: 5},
				&models.WorkspaceStatus{Status: tt.status})
			if err != nil {
				t.Fatal(err)
			}
			if st.Status != models.WorkspaceStatusRunning {
				t.Errorf("status = %s, want Running", st.Status)
			}
			if f.started != 1 || f.installed != 0 {
				t.Errorf("doStart=%d doInstallation=%d, want 1 and 0", f.started, f.installed)
			}
			pvc, err := f.kube.CoreV1().PersistentVolumeClaims(provisionTestNS).Get(context.Background(), "home", metav1.GetOptions{})
			if err != nil || pvc.UID != f.pvcUID {
				t.Errorf("PVC changed: err=%v uid=%q want %q", err, pvc.UID, f.pvcUID)
			}
			if _, err := f.kube.CoreV1().Pods(provisionTestNS).Get(context.Background(), "ws", metav1.GetOptions{}); err != nil {
				t.Errorf("new pod missing: %v", err)
			}
		})
	}
}

func TestProvisionExistingReleaseRemovedDuringWait(t *testing.T) {
	deleting := metav1.NewTime(time.Now())
	var released atomic.Bool
	released.Store(true)
	f := newProvisionFixture(t, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &deleting}},
		func() bool { return released.Load() })
	go func() {
		time.Sleep(100 * time.Millisecond)
		released.Store(false) // a terminate finished
		_ = f.kube.CoreV1().Pods(provisionTestNS).Delete(context.Background(), "ws", metav1.DeleteOptions{})
	}()

	if _, err := f.w.provisionExisting(context.Background(), &ProvisionOptions{Timeout: 5},
		&models.WorkspaceStatus{Status: models.WorkspaceStatusTerminating}); err != nil {
		t.Fatal(err)
	}
	if f.installed != 1 || f.started != 0 {
		t.Errorf("doInstallation=%d doStart=%d, want 1 and 0", f.installed, f.started)
	}
}

func TestWaitForPodGoneTimesOut(t *testing.T) {
	f := newProvisionFixture(t, &corev1.Pod{}, func() bool { return true })
	err := f.w.WaitForPodGone(context.Background(), 50*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want deadline exceeded", err)
	}
}

// A pod another request is already starting must be watched, never deleted.
func TestProvisionExistingProvisioningPodIsKept(t *testing.T) {
	f := newProvisionFixture(t, &corev1.Pod{Spec: corev1.PodSpec{NodeName: "n1"}, Status: corev1.PodStatus{Phase: corev1.PodRunning,
		ContainerStatuses: []corev1.ContainerStatus{{Name: "c", Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}},
		func() bool { return true })
	if _, err := f.w.provisionExisting(context.Background(), &ProvisionOptions{Timeout: 5},
		&models.WorkspaceStatus{Status: models.WorkspaceStatusProvisioning}); err != nil {
		t.Fatal(err)
	}
	if f.started != 0 || f.installed != 0 {
		t.Errorf("doStart=%d doInstallation=%d, want 0 and 0", f.started, f.installed)
	}
	if _, err := f.kube.CoreV1().Pods(provisionTestNS).Get(context.Background(), "ws", metav1.GetOptions{}); err != nil {
		t.Errorf("pod was deleted: %v", err)
	}
}
