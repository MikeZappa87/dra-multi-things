package nri

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/containerd/nri/pkg/api"
	"github.com/containerd/nri/pkg/stub"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/example/dra-poc/pkg/ipam"
)

const (
	pluginName = "dra-rdma"
	pluginIdx  = "90" // Run late — after most other NRI plugins.
)

// Plugin is an NRI plugin that handles device namespace moves and IPAM lease application.
//
// It implements:
//   - RunPodInterface  — move RDMA devices into the new sandbox netns and apply IPAM leases
//   - StopPodInterface — move RDMA devices back to the host (init) netns and release IPAM leases
type Plugin struct {
	stub          stub.Stub
	rdmaTracker   *RDMANetnsTracker
	ipamTracker   *IPAMLeaseTracker
	kubeClient    kubernetes.Interface
	dynamicClient dynamic.Interface
}

// NewPlugin creates a new NRI plugin wired to the RDMA tracker only (backward compat).
func NewPlugin(tracker *RDMANetnsTracker) (*Plugin, error) {
	return NewPluginWithTrackers(tracker, nil, nil, nil)
}

// NewPluginWithTrackers creates a new NRI plugin wired to both RDMA and IPAM trackers.
func NewPluginWithTrackers(rdmaTracker *RDMANetnsTracker, ipamTracker *IPAMLeaseTracker, kubeClient kubernetes.Interface, dynamicClient dynamic.Interface) (*Plugin, error) {
	p := &Plugin{
		rdmaTracker:   rdmaTracker,
		ipamTracker:   ipamTracker,
		kubeClient:    kubeClient,
		dynamicClient: dynamicClient,
	}

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
// We look for pending RDMA moves and IPAM leases that belong to this pod's claims,
// and execute them now via netlink.
func (p *Plugin) RunPodSandbox(ctx context.Context, pod *api.PodSandbox) error {
	podUID := pod.GetUid()
	podName := fmt.Sprintf("%s/%s", pod.GetNamespace(), pod.GetName())

	// Extract claim UIDs from pod annotations.
	// The kubelet sets annotations of the form:
	//   resource.kubernetes.io/<container>: <claim-uid>[,<claim-uid>...]
	claimUIDs := extractClaimUIDs(pod.GetAnnotations())
	if len(claimUIDs) == 0 {
		claimUIDs = p.claimUIDsFromKubernetes(ctx, pod.GetNamespace(), pod.GetName())
	}
	klog.Infof("RunPodSandbox received: pod=%s uid=%s annotations=%d claimUIDs=%d netns=%s", podName, podUID, len(pod.GetAnnotations()), len(claimUIDs), getNetNSPath(pod))

	// Get the pod's network namespace path
	netnsPath := getNetNSPath(pod)

	// Handle RDMA device moves (if tracker is available)
	if p.rdmaTracker != nil {
		moves := p.rdmaTracker.ConsumePendingForClaims(claimUIDs)
		if len(moves) > 0 {
			if netnsPath == "" {
				klog.Warningf("Pod %s: no netns path available, cannot move RDMA devices", podName)
				for _, m := range moves {
					p.rdmaTracker.AddPending(m.ClaimUID, m.IBDev)

				}
			} else {
				p.applyRDMAMoves(pod, moves, podName, podUID)
			}
		}
	}

	// Handle IPAM lease application (if tracker is available and netns path is known)
	// If no claim UIDs were found in annotations, try consuming ALL pending leases
	// (this is a fallback for the case where annotations aren't set)
	if p.ipamTracker != nil && netnsPath != "" {
		leases := p.ipamTracker.ConsumePendingForClaims(claimUIDs)

		for claimUID, lease := range leases {
			lease.PodUID = podUID
			lease.NetnsPath = netnsPath

			if lease.HostInterface != "" {
				if err := MoveInterfaceToPodNetns(netnsPath, lease.HostInterface, lease.Interface); err != nil {
					klog.Errorf("Pod %s: failed to move interface %s: %v", podName, lease.HostInterface, err)
					if err := p.ipamTracker.AddPending(claimUID, lease); err != nil {
						klog.Errorf("Pod %s: failed to re-register pending lease: %v", podName, err)
					}
					continue
				}
			}

			klog.Infof("Applying IPAM lease to pod %s: claim=%s iface=%s ip=%s", podName, claimUID, lease.Interface, lease.IP)
			if err := applyLeaseWithRetry(netnsPath, lease.Interface, lease); err != nil {
				klog.Errorf("Pod %s: failed to apply IPAM lease for claim %s: %v", podName, claimUID, err)
				// Re-register as pending so the lease isn't lost
				if err := p.ipamTracker.AddPending(claimUID, lease); err != nil {
					klog.Errorf("Pod %s: failed to re-register pending lease: %v", podName, err)
				}
			} else {
				p.ipamTracker.MarkActive(claimUID, lease)
				klog.Infof("Applied IPAM lease to pod %s: claim=%s pool=%s ip=%s", podName, claimUID, lease.PoolName, lease.IP)
			}
		}
	}

	return nil
}

func applyLeaseWithRetry(netnsPath, iface string, lease *ipam.Lease) error {
	var err error
	for attempt := 0; attempt < 20; attempt++ {
		err = ApplyLeaseToPodNetns(netnsPath, iface, lease)
		if err == nil || !strings.Contains(err.Error(), "Link not found") {
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
	return err
}

// StopPodSandbox is called when a pod is stopping. This handles both RDMA device
// return to host netns and IPAM lease cleanup. The primary cleanup happens in
// Unprepare (called by the kubelet before StopPodSandbox), which removes the
// tracker entries — so in the normal flow, StopPodSandbox is a backup cleanup path.
func (p *Plugin) StopPodSandbox(_ context.Context, pod *api.PodSandbox) error {
	podUID := pod.GetUid()
	podName := fmt.Sprintf("%s/%s", pod.GetNamespace(), pod.GetName())

	// Handle RDMA device return (if tracker is available)
	if p.rdmaTracker != nil {
		rmoves := p.rdmaTracker.RemoveActiveForPod(podUID)
		if len(rmoves) > 0 {
			p.cleanupRDMADevices(pod, rmoves, podName)
		}
	}

	// Handle IPAM lease cleanup (if tracker is available)
	if p.ipamTracker != nil {
		leases, err := p.ipamTracker.RemoveActiveForPod(podUID)
		if err != nil {
			klog.Errorf("Pod %s: failed to remove active IPAM leases: %v", podName, err)
			return err
		}

		// Release the IPs back to the pool
		for claimUID, lease := range leases {
			if err := p.ipamTracker.Allocator.Release(claimUID); err != nil {
				klog.Errorf("Pod %s: failed to release IPAM lease for claim %s: %v", podName, claimUID, err)
			} else {
				klog.Infof("Released IPAM lease for claim %s: ip=%s", claimUID, lease.IP)
			}
		}
	}

	return nil
}

func (p *Plugin) claimUIDsFromKubernetes(ctx context.Context, namespace, podName string) []string {
	if p.kubeClient == nil || p.dynamicClient == nil {
		return nil
	}
	pod, err := p.kubeClient.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		klog.Warningf("Pod %s/%s: failed to look up pod claims: %v", namespace, podName, err)
		return nil
	}

	claimResource := schema.GroupVersionResource{
		Group: "resource.k8s.io", Version: resourceapi.SchemeGroupVersion.Version, Resource: "resourceclaims",
	}
	claimUIDs := make([]string, 0, len(pod.Status.ResourceClaimStatuses))
	for _, status := range pod.Status.ResourceClaimStatuses {
		if status.ResourceClaimName == nil || *status.ResourceClaimName == "" {
			continue
		}
		claim, err := p.dynamicClient.Resource(claimResource).Namespace(namespace).Get(ctx, *status.ResourceClaimName, metav1.GetOptions{})
		if err != nil {
			klog.Warningf("Pod %s/%s: failed to look up ResourceClaim %s: %v", namespace, podName, *status.ResourceClaimName, err)
			continue
		}
		claimUIDs = append(claimUIDs, string(claim.GetUID()))
	}
	return claimUIDs
}

// ──────────────────────────────────────────────────────────────────────────────
// RDMA device handling
// ──────────────────────────────────────────────────────────────────────────────

// applyRDMAMoves moves RDMA devices from pending list into a pod's netns.
func (p *Plugin) applyRDMAMoves(pod *api.PodSandbox, moves []*PendingMove, podName, podUID string) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	hostNS, err := netns.Get()
	if err != nil {
		klog.Errorf("Pod %s: could not get host netns: %v", podName, err)
		return
	}
	defer hostNS.Close()

	netnsPath := getNetNSPath(pod)
	if netnsPath == "" {
		klog.Errorf("Pod %s: no netns path available", podName)
		return
	}

	podNS, err := netns.GetFromPath(netnsPath)
	if err != nil {
		klog.Errorf("Pod %s: could not open netns %s: %v", podName, netnsPath, err)
		return
	}
	defer podNS.Close()

	if err := netns.Set(podNS); err != nil {
		klog.Errorf("Pod %s: failed to enter pod netns: %v", podName, err)
		return
	}
	defer netns.Set(hostNS) // restore to host netns before unlocking the OS thread

	for _, m := range moves {
		rdmaLink, err := netlink.RdmaLinkByName(m.IBDev)
		if err != nil {
			klog.Errorf("Pod %s: RDMA link %s not found in host: %v", podName, m.IBDev, err)
			continue
		}

		if err := netlink.RdmaLinkSetNsFd(rdmaLink, uint32(podNS)); err != nil {
			klog.Errorf("Pod %s: failed to move RDMA device %s to pod netns: %v", podName, m.IBDev, err)
			p.rdmaTracker.AddPending(m.ClaimUID, m.IBDev)
		} else {
			p.rdmaTracker.MarkActive(m.ClaimUID, podUID, m.IBDev, netnsPath)
			klog.Infof("Moved RDMA device %s to pod %s", m.IBDev, podName)
		}
	}
}

// cleanupRDMADevices returns RDMA devices from a pod's namespace back to the host.
func (p *Plugin) cleanupRDMADevices(pod *api.PodSandbox, moves []*ActiveMove, podName string) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// Always use /proc/1/ns/net for the init netns — netns.Get() returns the
	// current namespace when there are no changes pending.  We want to guarantee
	// the host (init) namespace, so we hard-code the path.
	hostNS, err := netns.GetFromPath("/proc/1/ns/net")
	if err != nil {
		klog.Errorf("Pod %s: could not get host netns: %v", podName, err)
		return
	}
	defer hostNS.Close()

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
}

// getNetNSPath extracts the network namespace path from a pod's Linux info.
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

// isUUID checks if a string looks like a UUID.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	parts := strings.Split(s, "-")
	if len(parts) != 5 {
		return false
	}
	if len(parts[0]) != 8 || len(parts[1]) != 4 || len(parts[2]) != 4 || len(parts[3]) != 4 || len(parts[4]) != 12 {
		return false
	}
	for _, part := range parts {
		for _, ch := range part {
			if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F')) {
				return false
			}
		}
	}
	return true
}
