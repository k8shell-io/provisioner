package workspace

import (
	"context"
	"fmt"
	"os"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

type WorkspaceLock struct {
	client    kubernetes.Interface
	namespace string
	lockName  string
	leaseName string
	timeout   time.Duration
	expire    time.Duration
}

func NewWorkspaceLock(client kubernetes.Interface, workspaceName, namespace string) *WorkspaceLock {
	return &WorkspaceLock{
		client:    client,
		namespace: namespace,
		lockName:  fmt.Sprintf("workspace-lock-%s", workspaceName),
		leaseName: fmt.Sprintf("workspace-lease-%s", workspaceName),
		timeout:   5 * time.Minute,
		expire:    10 * time.Minute,
	}
}

func (l *WorkspaceLock) Acquire(ctx context.Context) (bool, error) {
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      l.leaseName,
			Namespace: l.namespace,
		},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       stringPtr(l.getInstanceID()),
			LeaseDurationSeconds: int32Ptr(int32(l.expire.Seconds())),
			AcquireTime:          &metav1.MicroTime{Time: time.Now()},
			RenewTime:            &metav1.MicroTime{Time: time.Now()},
		},
	}

	_, err := l.client.CoordinationV1().Leases(l.namespace).Create(ctx, lease, metav1.CreateOptions{})
	if err == nil {
		return true, nil
	}

	existingLease, err := l.client.CoordinationV1().Leases(l.namespace).Get(ctx, l.leaseName, metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("failed to get existing lease: %w", err)
	}

	if l.isLeaseExpired(existingLease) {
		// Try to take over the expired lease
		lease.ResourceVersion = existingLease.ResourceVersion
		_, err := l.client.CoordinationV1().Leases(l.namespace).Update(ctx, lease, metav1.UpdateOptions{})
		return err == nil, err
	}

	return false, nil
}

func (l *WorkspaceLock) Release(ctx context.Context) error {
	return l.client.CoordinationV1().Leases(l.namespace).Delete(ctx, l.leaseName, metav1.DeleteOptions{})
}

func (l *WorkspaceLock) getInstanceID() string {
	if podName := os.Getenv("HOSTNAME"); podName != "" {
		return podName
	}
	return fmt.Sprintf("provisioner-%d", time.Now().UnixNano())
}

func (l *WorkspaceLock) isLeaseExpired(lease *coordinationv1.Lease) bool {
	if lease.Spec.RenewTime == nil {
		return true
	}
	expireTime := lease.Spec.RenewTime.Add(time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second)
	return time.Now().After(expireTime)
}

// Helper functions
func stringPtr(s string) *string { return &s }
func int32Ptr(i int32) *int32    { return &i }
