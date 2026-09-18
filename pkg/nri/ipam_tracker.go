package nri

import (
	"fmt"
	"sync"

	"github.com/example/dra-poc/pkg/ipam"
	"k8s.io/klog/v2"
)

// IPAMLeaseTracker coordinates between the DRA driver (which allocates IPAM leases)
// and the NRI plugin (which applies them to pod namespaces).
//
// Thread-safe — called from both the DRA gRPC goroutines and NRI ttrpc goroutines.
type IPAMLeaseTracker struct {
	mu sync.Mutex

	// Allocator is the backing IPAM allocator.
	Allocator *ipam.Allocator

	// pending maps claimUID → Lease.
	// Populated by Prepare(), consumed by RunPodSandbox.
	pending map[string]*ipam.Lease

	// active maps claimUID → Lease.
	// Populated by RunPodSandbox, consumed by StopPodSandbox/Unprepare.
	active map[string]*ipam.Lease
}

// NewIPAMLeaseTracker creates a new tracker backed by the given allocator.
func NewIPAMLeaseTracker(allocator *ipam.Allocator) *IPAMLeaseTracker {
	return &IPAMLeaseTracker{
		Allocator: allocator,
		pending:   make(map[string]*ipam.Lease),
		active:    make(map[string]*ipam.Lease),
	}
}

// AddPending registers a lease that should be applied to the pod's netns once
// the sandbox is created. Called from bridge-veth handler Prepare().
func (t *IPAMLeaseTracker) AddPending(claimUID string, lease *ipam.Lease) error {
	if lease == nil {
		return fmt.Errorf("lease is required")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pending[claimUID] = lease
	klog.Infof("Registered pending IPAM lease: claim=%s pool=%s ip=%s", claimUID, lease.PoolName, lease.IP)
	return nil
}

// RemovePending removes a pending (not yet applied) lease. Called if Prepare fails
// after allocation, or on Unprepare when no pod was created. The IP is released
// back to the pool.
func (t *IPAMLeaseTracker) RemovePending(claimUID string) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	lease, ok := t.pending[claimUID]
	if !ok {
		return nil
	}
	delete(t.pending, claimUID)

	if t.Allocator != nil {
		if err := t.Allocator.Release(claimUID); err != nil {
			klog.Errorf("Failed to release pending lease for claim %s: %v", claimUID, err)
			return err
		}
	}
	klog.Infof("Removed pending IPAM lease: claim=%s pool=%s ip=%s", claimUID, lease.PoolName, lease.IP)
	return nil
}

// ConsumePendingForClaims returns and removes all pending leases whose claim UID
// is in the provided set. Called by NRI RunPodSandbox.
func (t *IPAMLeaseTracker) ConsumePendingForClaims(claimUIDs []string) map[string]*ipam.Lease {
	t.mu.Lock()
	defer t.mu.Unlock()

	leases := make(map[string]*ipam.Lease)
	for _, uid := range claimUIDs {
		if lease, ok := t.pending[uid]; ok {
			leases[uid] = lease
			delete(t.pending, uid)
		}
	}
	return leases
	// MarkActive records that a lease was successfully applied to a pod's netns.
}

// GetAllPending returns all pending leases without removing them.
// Used as a fallback when claim UIDs can't be extracted from annotations.
func (t *IPAMLeaseTracker) GetAllPending() map[string]*ipam.Lease {
	t.mu.Lock()
	defer t.mu.Unlock()

	leases := make(map[string]*ipam.Lease, len(t.pending))
	for uid, lease := range t.pending {
		leases[uid] = lease
	}
	return leases
}

// ConsumeAllPending returns and removes all pending leases.
func (t *IPAMLeaseTracker) ConsumeAllPending() map[string]*ipam.Lease {
	t.mu.Lock()
	defer t.mu.Unlock()

	leases := t.pending
	t.pending = make(map[string]*ipam.Lease)
	return leases
}

// MarkActive records that a lease was successfully applied to a pod's netns.
func (t *IPAMLeaseTracker) MarkActive(claimUID string, lease *ipam.Lease) {
	if lease == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.active[claimUID] = lease
	klog.Infof("Marked IPAM lease as active: claim=%s pool=%s ip=%s", claimUID, lease.PoolName, lease.IP)
}

// RemoveActiveForPod returns all active leases for a given pod and removes them.
// Called by NRI StopPodSandbox. The IPs are released back to the pool.
func (t *IPAMLeaseTracker) RemoveActiveForPod(podUID string) (map[string]*ipam.Lease, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	removed := make(map[string]*ipam.Lease)
	for claimUID, lease := range t.active {
		if lease.PodUID == podUID {
			removed[claimUID] = lease
			delete(t.active, claimUID)

			if t.Allocator != nil {
				if err := t.Allocator.Release(claimUID); err != nil {
					klog.Errorf("Failed to release active lease for claim %s: %v", claimUID, err)
					return nil, err
				}
			}
		}
	}
	if len(removed) > 0 {
		klog.Infof("Removed %d IPAM leases for pod %s", len(removed), podUID)
	}
	return removed, nil
}

// GetActiveLease returns the active lease for a claim, if any.
func (t *IPAMLeaseTracker) GetActiveLease(claimUID string) *ipam.Lease {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.active[claimUID]
}
