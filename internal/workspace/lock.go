package workspace

import (
	"context"
	"fmt"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

var (
	ErrLockAlreadyHeld = fmt.Errorf("lock is already held by another process")
)

type WorkspaceLock struct {
	client    kubernetes.Interface
	namespace string
	leaseName string
	holderID  string
}

func NewWorkspaceLock(client kubernetes.Interface, namespace, lockName string) *WorkspaceLock {
	return &WorkspaceLock{
		client:    client,
		namespace: namespace,
		leaseName: fmt.Sprintf("workspace-lease-%s", lockName),
		holderID:  fmt.Sprintf("provisioner-%d", time.Now().UnixNano()), // Unique holder ID
	}
}

// Acquire attempts to acquire the lock with waiting behavior
func (l *WorkspaceLock) Acquire(ctx context.Context) (bool, error) {
	return l.AcquireWithOptions(ctx, true)
}

// TryAcquire attempts to acquire the lock without waiting
func (l *WorkspaceLock) TryAcquire(ctx context.Context) (bool, error) {
	return l.AcquireWithOptions(ctx, false)
}

// AcquireWithOptions attempts to acquire the lock with configurable waiting behavior
func (l *WorkspaceLock) AcquireWithOptions(ctx context.Context, waitForLock bool) (bool, error) {
	leaseClient := l.client.CoordinationV1().Leases(l.namespace)

	retryDelay := 1 * time.Second
	maxRetryDelay := 5 * time.Second

	for {
		select {
		case <-ctx.Done():
			return false, fmt.Errorf("context cancelled while waiting for lock: %w", ctx.Err())
		default:
		}

		existingLease, err := leaseClient.Get(ctx, l.leaseName, metav1.GetOptions{})
		if errors.IsNotFound(err) {
			created, err := l.createLease(ctx)
			if err != nil {
				return false, err
			}
			if created {
				return true, nil
			}
		} else if err != nil {
			return false, fmt.Errorf("failed to get existing lease: %w", err)
		} else {
			if l.isLeaseAvailable(existingLease) {
				acquired, err := l.acquireExistingLease(ctx, existingLease)
				if err != nil {
					if !waitForLock {
						return false, ErrLockAlreadyHeld
					}
					fmt.Printf("Failed to acquire existing lease, retrying: %v\n", err)
				} else if acquired {
					return true, nil
				}
			} else {
				if !waitForLock {
					return false, ErrLockAlreadyHeld
				}
			}
		}

		if !waitForLock {
			return false, ErrLockAlreadyHeld
		}

		select {
		case <-ctx.Done():
			return false, fmt.Errorf("context cancelled while waiting for lock: %w", ctx.Err())
		case <-time.After(retryDelay):
			retryDelay = retryDelay * 2
			if retryDelay > maxRetryDelay {
				retryDelay = maxRetryDelay
			}
		}
	}
}

func (l *WorkspaceLock) createLease(ctx context.Context) (bool, error) {
	leaseClient := l.client.CoordinationV1().Leases(l.namespace)

	now := metav1.NewMicroTime(time.Now())
	leaseDuration := int32(30) // 30 seconds lease duration

	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      l.leaseName,
			Namespace: l.namespace,
		},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       &l.holderID,
			LeaseDurationSeconds: &leaseDuration,
			AcquireTime:          &now,
			RenewTime:            &now,
		},
	}

	_, err := leaseClient.Create(ctx, lease, metav1.CreateOptions{})
	if err != nil {
		if errors.IsAlreadyExists(err) {
			// Someone else created it while we were trying, return false to continue loop
			return false, nil
		}
		return false, fmt.Errorf("failed to create lease: %w", err)
	}

	return true, nil
}

func (l *WorkspaceLock) acquireExistingLease(ctx context.Context, existingLease *coordinationv1.Lease) (bool, error) {
	leaseClient := l.client.CoordinationV1().Leases(l.namespace)

	now := metav1.NewMicroTime(time.Now())

	// Update lease to claim it
	existingLease.Spec.HolderIdentity = &l.holderID
	existingLease.Spec.AcquireTime = &now
	existingLease.Spec.RenewTime = &now

	_, err := leaseClient.Update(ctx, existingLease, metav1.UpdateOptions{})
	if err != nil {
		return false, fmt.Errorf("failed to update lease: %w", err)
	}

	return true, nil
}

func (l *WorkspaceLock) isLeaseAvailable(lease *coordinationv1.Lease) bool {
	if lease.Spec.HolderIdentity == nil {
		return true // No holder
	}

	if lease.Spec.RenewTime == nil || lease.Spec.LeaseDurationSeconds == nil {
		return true // Invalid lease
	}

	// Check if lease has expired
	renewTime := lease.Spec.RenewTime.Time
	leaseDuration := time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second
	expireTime := renewTime.Add(leaseDuration)

	return time.Now().After(expireTime)
}

func (l *WorkspaceLock) Release(ctx context.Context) error {
	leaseClient := l.client.CoordinationV1().Leases(l.namespace)

	lease, err := leaseClient.Get(ctx, l.leaseName, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return nil // Already released/deleted
	}

	if err != nil {
		return fmt.Errorf("failed to get lease for release: %w", err)
	}

	// Only release if we hold the lease
	if lease.Spec.HolderIdentity != nil && *lease.Spec.HolderIdentity == l.holderID {
		err = leaseClient.Delete(ctx, l.leaseName, metav1.DeleteOptions{})
		if err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("failed to delete lease: %w", err)
		}
	}

	return nil
}
