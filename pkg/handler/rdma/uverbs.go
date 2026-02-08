package rdma

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"k8s.io/klog/v2"

	"github.com/example/dra-poc/pkg/handler"
	"github.com/example/dra-poc/pkg/nri"
	cdispec "tags.cncf.io/container-device-interface/specs-go"
)

const ibDevDir = "/dev/infiniband"

// UverbsHandler manages RDMA uverbs devices (/dev/infiniband/uverbs*).
//
// For a fully functional RDMA userspace path the handler exposes:
//   - /dev/infiniband/uverbsN       — main verbs data-path
//   - /dev/infiniband/rdma_cm        — RDMA-CM connection manager (shared)
//   - /dev/infiniband/umadN          — management datagrams (if present, index-matched)
//   - /sys/class/infiniband/<ibdev>  — sysfs for ibv_get_device_list
//
// In exclusive RDMA netns mode, the handler registers pending netns moves in
// the RDMANetnsTracker.  The NRI plugin performs the actual move when the pod
// sandbox is created (RunPodSandbox), because the pod's netns does not exist
// at DRA Prepare time.
type UverbsHandler struct {
	// Tracker coordinates RDMA netns moves with the NRI plugin.
	// Nil when running in shared mode (no moves needed).
	Tracker *nri.RDMANetnsTracker
}

func (h *UverbsHandler) Type() handler.DeviceType { return handler.DeviceTypeRDMA }
func (h *UverbsHandler) Kinds() []string          { return []string{"uverbs"} }

func (h *UverbsHandler) Validate(_ context.Context, _ *handler.DeviceConfig) error {
	return nil
}

func (h *UverbsHandler) Prepare(_ context.Context, req *handler.PrepareRequest) (*handler.PrepareResult, error) {
	deviceName := req.AllocatedDevice
	if deviceName == "" {
		var err error
		deviceName, err = findAvailableUverbs()
		if err != nil {
			return nil, fmt.Errorf("no uverbs device allocated or available: %w", err)
		}
	}

	devPath := filepath.Join(ibDevDir, deviceName)
	if _, err := os.Stat(devPath); err != nil {
		return nil, fmt.Errorf("uverbs device %s not found: %w", devPath, err)
	}

	ibDev := resolveIBDevice(deviceName)
	klog.Infof("Prepared RDMA uverbs device %s (ibdev=%s)", deviceName, ibDev)

	// Build the CDI device-node list.
	edits := &cdispec.ContainerEdits{
		DeviceNodes: []*cdispec.DeviceNode{
			{
				Path:        devPath,
				HostPath:    devPath,
				Permissions: "rw",
			},
		},
	}

	// rdma_cm is shared across all RDMA devices and needed by rdma_resolve_addr /
	// rdma_create_id.  Always include it when present.
	rdmaCMPath := filepath.Join(ibDevDir, "rdma_cm")
	if _, err := os.Stat(rdmaCMPath); err == nil {
		edits.DeviceNodes = append(edits.DeviceNodes, &cdispec.DeviceNode{
			Path:        rdmaCMPath,
			HostPath:    rdmaCMPath,
			Permissions: "rw",
		})
	}

	// umadN is the management datagram device for the same HCA.  The index
	// usually matches the uverbs index (uverbs0 ↔ umad0).
	umadName := strings.Replace(deviceName, "uverbs", "umad", 1)
	umadPath := filepath.Join(ibDevDir, umadName)
	if _, err := os.Stat(umadPath); err == nil {
		edits.DeviceNodes = append(edits.DeviceNodes, &cdispec.DeviceNode{
			Path:        umadPath,
			HostPath:    umadPath,
			Permissions: "rw",
		})
	}

	// Sysfs bind-mount so userspace can enumerate IB devices.
	if ibDev != "" {
		sysPath := filepath.Join("/sys/class/infiniband", ibDev)
		if _, err := os.Stat(sysPath); err == nil {
			edits.Mounts = []*cdispec.Mount{
				{
					HostPath:      sysPath,
					ContainerPath: sysPath,
					Options:       []string{"ro", "bind"},
				},
			}
		}
	}

	// In exclusive RDMA netns mode, register a pending move in the tracker.
	// The NRI plugin will perform the actual RdmaLinkSetNsFd when the pod
	// sandbox is created (RunPodSandbox), because the pod's netns does not
	// exist yet at DRA Prepare time.
	if DetectNetnsMode() == NetnsExclusive && ibDev != "" && h.Tracker != nil {
		h.Tracker.AddPending(req.ClaimUID, ibDev)
		klog.Infof("Registered pending RDMA netns move for %s (claim=%s, exclusive mode)", ibDev, req.ClaimUID)
	}

	return &handler.PrepareResult{
		PoolName:   "default",
		DeviceName: deviceName,
		CDIEdits:   edits,
		Allocation: &handler.AllocationInfo{
			Type:       handler.DeviceTypeRDMA,
			Kind:       "uverbs",
			ClaimUID:   req.ClaimUID,
			DeviceName: deviceName,
			Metadata: map[string]string{
				"uverbsDevice": deviceName,
				"ibdev":        ibDev,
				"devPath":      devPath,
			},
		},
	}, nil
}

func (h *UverbsHandler) Unprepare(_ context.Context, req *handler.UnprepareRequest) error {
	deviceName := req.Allocation.Metadata["uverbsDevice"]
	ibDev := req.Allocation.Metadata["ibdev"]

	if DetectNetnsMode() == NetnsExclusive && ibDev != "" && h.Tracker != nil {
		// Remove any pending move that was never consumed (pod never started).
		h.Tracker.RemovePending(req.Allocation.ClaimUID)

		// Remove active tracking and retrieve the pod netns path so we
		// can enter it to find the RDMA device (invisible from host in
		// exclusive mode).  This is the primary return path — the kubelet
		// calls Unprepare before StopPodSandbox, so the pod netns still
		// exists at this point.
		var podNetnsPath string
		if active, ok := h.Tracker.RemoveActive(req.Allocation.ClaimUID); ok {
			podNetnsPath = active.NetnsPath
		}

		returnRDMAToHost(ibDev, podNetnsPath)
	}

	klog.Infof("Released RDMA uverbs device %s for claim %s", deviceName, req.ClaimUID)
	return nil
}

// returnRDMAToHost is a best-effort attempt to move an RDMA device back to the
// host (init) network namespace using netlink.
//
// In exclusive mode, RDMA devices are only visible from the netns they belong
// to.  We must enter the pod's netns to find the device before moving it back
// to the host.  If podNetnsPath is empty or the netns is already destroyed,
// we fall back to looking in the host netns (the device may have been
// auto-returned when the pod netns was destroyed).
func returnRDMAToHost(ibDev, podNetnsPath string) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// Always use /proc/1/ns/net for the init netns — netns.Get() returns the
	// calling thread's netns which may have been switched by another goroutine.
	hostNS, err := netns.GetFromPath("/proc/1/ns/net")
	if err != nil {
		klog.Warningf("Could not open init netns (/proc/1/ns/net) for RDMA device return: %v", err)
		return
	}
	defer hostNS.Close()

	// Enter the pod netns so we can see the RDMA device.  If the pod netns
	// is already gone the device was auto-returned by the kernel.
	enteredPodNS := false
	if podNetnsPath != "" {
		podNS, err := netns.GetFromPath(podNetnsPath)
		if err != nil {
			klog.V(2).Infof("Could not open pod netns %s for RDMA return (may be destroyed, device auto-returned): %v", podNetnsPath, err)
		} else {
			if err := netns.Set(podNS); err != nil {
				klog.Warningf("Failed to enter pod netns %s: %v", podNetnsPath, err)
			} else {
				enteredPodNS = true
				defer netns.Set(hostNS) // restore to host netns before unlocking OS thread
			}
			podNS.Close()
		}
	}

	link, err := netlink.RdmaLinkByName(ibDev)
	if err != nil {
		if enteredPodNS {
			klog.V(2).Infof("RDMA link %s not found in pod netns (auto-returned when netns was destroyed?): %v", ibDev, err)
		} else {
			klog.V(2).Infof("RDMA link %s not found in host netns (still in pod netns or auto-returned): %v", ibDev, err)
		}
		return
	}

	if err := netlink.RdmaLinkSetNsFd(link, uint32(hostNS)); err != nil {
		klog.V(2).Infof("RdmaLinkSetNsFd %s to init netns: %v (may already be there)", ibDev, err)
	} else {
		klog.Infof("Moved RDMA device %s back to host netns via netlink (unprepare safety-net)", ibDev)
	}
}

func findAvailableUverbs() (string, error) {
	entries, err := os.ReadDir(ibDevDir)
	if err != nil {
		return "", fmt.Errorf("failed to read %s: %w", ibDevDir, err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "uverbs") {
			return entry.Name(), nil
		}
	}
	return "", fmt.Errorf("no uverbs devices found in %s", ibDevDir)
}

func resolveIBDevice(uverbsName string) string {
	sysPath := filepath.Join("/sys/class/infiniband_verbs", uverbsName, "ibdev")
	data, err := os.ReadFile(sysPath)
	if err != nil {
		klog.V(2).Infof("Could not resolve IB device for %s: %v", uverbsName, err)
		return ""
	}
	return strings.TrimSpace(string(data))
}
