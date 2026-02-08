// Package nri provides an NRI (Node Resource Interface) plugin that handles
// RDMA device namespace moves in exclusive netns mode.
//
// When the RDMA subsystem is in exclusive mode, each RDMA device belongs to
// exactly one network namespace.  The DRA driver's Prepare() runs before the
// pod sandbox (and its netns) exists, so it cannot move the device at that
// point.  Instead it registers a "pending move" in the RDMANetnsTracker.
//
// The NRI plugin receives RunPodSandbox events — fired after the sandbox netns
// is created — and executes the actual netlink move.  StopPodSandbox returns
// the device to the host netns.
package nri

import (
	"fmt"
	"sync"

	"k8s.io/klog/v2"
)

// PendingMove describes an RDMA device that should be moved into a pod's netns
// when the pod sandbox is created.
type PendingMove struct {
	// IBDev is the kernel IB device name (e.g. "mlx5_0").
	IBDev string
	// ClaimUID is the DRA ResourceClaim UID that owns this allocation.
	ClaimUID string
}

// ActiveMove records a device that was successfully moved into a pod.
type ActiveMove struct {
	IBDev     string
	PodUID    string
	NetnsPath string // Pod netns path — needed to re-enter and retrieve the device.
}

// RDMANetnsTracker coordinates between the DRA driver (which knows what RDMA
// devices need moving) and the NRI plugin (which knows when pod sandboxes are
// created and has access to the netns).
//
// Thread-safe — called from both the DRA gRPC goroutines and NRI ttrpc goroutines.
type RDMANetnsTracker struct {
	mu sync.Mutex

	// pending maps claimUID → PendingMove.
	// Populated by Prepare(), consumed by RunPodSandbox.
	pending map[string]*PendingMove

	// active maps claimUID → ActiveMove.
	// Populated by RunPodSandbox, consumed by StopPodSandbox/Unprepare.
	active map[string]*ActiveMove
}

// NewRDMANetnsTracker creates a new tracker.
func NewRDMANetnsTracker() *RDMANetnsTracker {
	return &RDMANetnsTracker{
		pending: make(map[string]*PendingMove),
		active:  make(map[string]*ActiveMove),
	}
}

// AddPending registers a device that should be moved into the pod's netns
// once the sandbox is created.  Called from UverbsHandler.Prepare().
func (t *RDMANetnsTracker) AddPending(claimUID, ibDev string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pending[claimUID] = &PendingMove{
		IBDev:    ibDev,
		ClaimUID: claimUID,
	}
	klog.Infof("Registered pending RDMA netns move: claim=%s ibdev=%s", claimUID, ibDev)
}

// RemovePending removes a pending (not yet executed) move.  Called if Prepare
// fails after registration, or on Unprepare when no pod was created.
func (t *RDMANetnsTracker) RemovePending(claimUID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.pending, claimUID)
}

// ConsumePendingForClaims returns and removes all pending moves whose claim UID
// is in the provided set.  Called by NRI RunPodSandbox.
func (t *RDMANetnsTracker) ConsumePendingForClaims(claimUIDs []string) []*PendingMove {
	t.mu.Lock()
	defer t.mu.Unlock()

	var moves []*PendingMove
	for _, uid := range claimUIDs {
		if m, ok := t.pending[uid]; ok {
			moves = append(moves, m)
			delete(t.pending, uid)
		}
	}
	return moves
}

// MarkActive records that a device was successfully moved into a pod's netns.
func (t *RDMANetnsTracker) MarkActive(claimUID, podUID, ibDev, netnsPath string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.active[claimUID] = &ActiveMove{
		IBDev:     ibDev,
		PodUID:    podUID,
		NetnsPath: netnsPath,
	}
}

// GetActiveForPod returns all active moves for a given pod UID.
func (t *RDMANetnsTracker) GetActiveForPod(podUID string) []*ActiveMove {
	t.mu.Lock()
	defer t.mu.Unlock()

	var moves []*ActiveMove
	for _, m := range t.active {
		if m.PodUID == podUID {
			moves = append(moves, m)
		}
	}
	return moves
}

// RemoveActive removes and returns an active move for a claim.
// Called from Unprepare or StopPodSandbox.
func (t *RDMANetnsTracker) RemoveActive(claimUID string) (*ActiveMove, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	m, ok := t.active[claimUID]
	if ok {
		delete(t.active, claimUID)
	}
	return m, ok
}

// RemoveActiveForPod removes and returns all active moves for a pod.
// Called from StopPodSandbox.
func (t *RDMANetnsTracker) RemoveActiveForPod(podUID string) []*ActiveMove {
	t.mu.Lock()
	defer t.mu.Unlock()

	var moves []*ActiveMove
	for uid, m := range t.active {
		if m.PodUID == podUID {
			moves = append(moves, m)
			delete(t.active, uid)
		}
	}
	return moves
}

// String returns a summary for debugging.
func (t *RDMANetnsTracker) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return fmt.Sprintf("RDMANetnsTracker{pending=%d, active=%d}", len(t.pending), len(t.active))
}
