package nri

import (
	"context"
	"fmt"
	"runtime"
	"strings"

	"github.com/containerd/nri/pkg/api"
	"github.com/containerd/nri/pkg/stub"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"k8s.io/klog/v2"
)

const (
	pluginName = "dra-rdma"
	pluginIdx  = "90" // Run late — after most other NRI plugins.
)

// Plugin is an NRI plugin that moves RDMA devices into pod network namespaces
// in exclusive RDMA netns mode.
//
// It implements:
//   - RunPodInterface  — move RDMA devices into the new sandbox netns
//   - StopPodInterface — move RDMA devices back to the host (init) netns
type Plugin struct {
	stub    stub.Stub
	tracker *RDMANetnsTracker
}

// NewPlugin creates a new NRI plugin wired to the given tracker.
func NewPlugin(tracker *RDMANetnsTracker) (*Plugin, error) {
	p := &Plugin{tracker: tracker}

	opts := []stub.Option{
		stub.WithPluginName(pluginName),
		stub.WithPluginIdx(pluginIdx),
	}

	s, err := stub.New(p, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create NRI stub: %w", err)
	}
	p.stub = s
	return p, nil
}

// Run starts the NRI plugin and blocks until the context is cancelled.
func (p *Plugin) Run(ctx context.Context) error {
	klog.Info("Starting NRI plugin for RDMA netns management")
	return p.stub.Run(ctx)
}

// Stop cleanly shuts down the NRI plugin.
func (p *Plugin) Stop() {
	p.stub.Stop()
}

// ──────────────────────────────────────────────────────────────────────────────
// NRI handler methods
// ──────────────────────────────────────────────────────────────────────────────

// RunPodSandbox is called after the pod sandbox (and its netns) is created.
// We look for pending RDMA moves that belong to this pod's claims and execute
// them now via netlink.
func (p *Plugin) RunPodSandbox(_ context.Context, pod *api.PodSandbox) error {
	podUID := pod.GetUid()
	podName := fmt.Sprintf("%s/%s", pod.GetNamespace(), pod.GetName())

	// Extract claim UIDs from pod annotations.
	// The kubelet sets annotations of the form:
	//   resource.kubernetes.io/<container>: <claim-uid>[,<claim-uid>...]
	claimUIDs := extractClaimUIDs(pod.GetAnnotations())
	if len(claimUIDs) == 0 {
		return nil // No DRA claims — nothing to do.
	}

	// Check for pending moves matching these claims.
	moves := p.tracker.ConsumePendingForClaims(claimUIDs)
	if len(moves) == 0 {
		return nil // No RDMA devices need moving.
	}

	// Get a handle to the pod's network namespace.
	var (
		podNS netns.NsHandle
		err   error
	)
	netnsPath := getNetNSPath(pod)
	if netnsPath != "" {
		podNS, err = netns.GetFromPath(netnsPath)
	} else {
		klog.Warningf("Pod %s: no netns path available, cannot move RDMA devices", podName)
		// Re-register the moves so they aren't lost.
		for _, m := range moves {
			p.tracker.AddPending(m.ClaimUID, m.IBDev)
		}
		return nil
	}
	if err != nil {
		klog.Errorf("Pod %s: failed to get netns (path=%q pid=%d): %v", podName, netnsPath, pod.GetPid(), err)
		for _, m := range moves {
			p.tracker.AddPending(m.ClaimUID, m.IBDev)
		}
		return fmt.Errorf("get pod netns: %w", err)
	}
	defer podNS.Close()

	// Move each RDMA device into the pod's netns.
	for _, m := range moves {
		rdmaLink, err := netlink.RdmaLinkByName(m.IBDev)
		if err != nil {
			klog.Errorf("Pod %s: RDMA link %s not found: %v", podName, m.IBDev, err)
			continue
		}

		if err := netlink.RdmaLinkSetNsFd(rdmaLink, uint32(podNS)); err != nil {
			klog.Errorf("Pod %s: failed to move RDMA device %s to netns: %v", podName, m.IBDev, err)
			continue
		}

		p.tracker.MarkActive(m.ClaimUID, podUID, m.IBDev, netnsPath)
		klog.Infof("Moved RDMA device %s into pod %s netns (claim=%s)", m.IBDev, podName, m.ClaimUID)
	}

	return nil
}

// StopPodSandbox is called when a pod is stopping.  This is a backup path for
// returning RDMA devices to the host netns.  The primary return happens in
// Unprepare (called by the kubelet before StopPodSandbox), which removes the
// tracker entry — so in the normal flow RemoveActiveForPod returns nothing.
//
// This handler catches edge cases: kubelet crash between container stop and
// Unprepare, Unprepare failure with a later retry, or rolling driver updates
// where in-memory state was lost.
func (p *Plugin) StopPodSandbox(_ context.Context, pod *api.PodSandbox) error {
	podUID := pod.GetUid()
	podName := fmt.Sprintf("%s/%s", pod.GetNamespace(), pod.GetName())

	moves := p.tracker.RemoveActiveForPod(podUID)
	if len(moves) == 0 {
		return nil
	}

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// Always use /proc/1/ns/net for the init netns — netns.Get() returns the
	// calling thread's netns which may have been switched by another goroutine.
	hostNS, err := netns.GetFromPath("/proc/1/ns/net")
	if err != nil {
		klog.Errorf("Failed to open init netns (/proc/1/ns/net) for RDMA device return: %v", err)
		return nil // Don't fail the pod stop — the kernel may auto-return them.
	}
	defer hostNS.Close()

	// Enter the pod's netns so that RdmaLinkByName can see the devices that
	// were moved there.  In exclusive mode, RDMA devices are invisible from
	// any other netns.
	enteredPodNS := false
	netnsPath := getNetNSPath(pod)
	if netnsPath != "" {
		podNS, err := netns.GetFromPath(netnsPath)
		if err != nil {
			klog.V(2).Infof("Pod %s: could not open netns %s (may already be destroyed): %v", podName, netnsPath, err)
		} else {
			if err := netns.Set(podNS); err != nil {
				klog.Warningf("Pod %s: failed to enter pod netns: %v", podName, err)
			} else {
				enteredPodNS = true
				defer netns.Set(hostNS) // restore to host netns before unlocking the OS thread
			}
			podNS.Close()
		}
	}

	for _, m := range moves {
		rdmaLink, err := netlink.RdmaLinkByName(m.IBDev)
		if err != nil {
			if enteredPodNS {
				klog.V(2).Infof("Pod %s: RDMA link %s not found in pod netns (auto-returned when netns was destroyed?): %v", podName, m.IBDev, err)
			} else {
				klog.V(2).Infof("Pod %s: RDMA link %s not found (could not enter pod netns; kernel will auto-return): %v", podName, m.IBDev, err)
			}
			continue
		}

		if err := netlink.RdmaLinkSetNsFd(rdmaLink, uint32(hostNS)); err != nil {
			klog.V(2).Infof("Pod %s: RdmaLinkSetNsFd for %s: %v", podName, m.IBDev, err)
		} else {
			klog.Infof("Returned RDMA device %s to host netns (pod %s stopped)", m.IBDev, podName)
		}
	}

	return nil
}

// ──────────────────────────────────────────────────────────────────────────────
// Helpers
// ──────────────────────────────────────────────────────────────────────────────

// getNetNSPath extracts the network namespace path from a PodSandbox's Linux
// namespace list.
func getNetNSPath(pod *api.PodSandbox) string {
	linux := pod.GetLinux()
	if linux == nil {
		return ""
	}
	for _, ns := range linux.GetNamespaces() {
		if ns.GetType() == "network" {
			return ns.GetPath()
		}
	}
	return ""
}

// extractClaimUIDs extracts DRA claim UIDs from pod annotations.
//
// The kubelet annotates pods with their resource claim info.  The annotation
// format used by DRA is:
//
//	resource.kubernetes.io/<container-name>: <json-with-claim-uids>
//
// However, the exact annotation key/format can vary.  A simpler and more
// robust approach: scan all annotation values for anything that looks like
// a UUID (claim UIDs are Kubernetes UIDs = UUIDv4).
//
// For now we use the standard DRA annotation prefix.
func extractClaimUIDs(annotations map[string]string) []string {
	var uids []string
	seen := make(map[string]bool)

	for key, value := range annotations {
		// DRA claim annotations use this prefix
		if !strings.HasPrefix(key, "resource.kubernetes.io/") {
			continue
		}
		// The value contains claim UIDs — parse them out.
		// Format varies; common patterns include comma-separated UIDs
		// or JSON structures.  We look for UUID-shaped strings.
		for _, candidate := range extractUUIDs(value) {
			if !seen[candidate] {
				seen[candidate] = true
				uids = append(uids, candidate)
			}
		}
	}

	return uids
}

// extractUUIDs finds UUID-shaped strings (8-4-4-4-12 hex) in a string.
func extractUUIDs(s string) []string {
	var uuids []string
	// Simple scan: look for 36-character UUID patterns
	for i := 0; i <= len(s)-36; i++ {
		candidate := s[i : i+36]
		if isUUID(candidate) {
			uuids = append(uuids, candidate)
			i += 35 // Skip past this UUID
		}
	}
	return uuids
}

// isUUID checks if a string matches the UUID format (8-4-4-4-12 hex digits).
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !isHexDigit(byte(c)) {
				return false
			}
		}
	}
	return true
}

func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}
