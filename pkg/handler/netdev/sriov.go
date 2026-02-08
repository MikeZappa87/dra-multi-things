package netdev

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/vishvananda/netlink"
	"k8s.io/klog/v2"

	"github.com/example/dra-poc/pkg/handler"
	cdispec "tags.cncf.io/container-device-interface/specs-go"
)

// SriovVfHandler manages SR-IOV Virtual Function devices
type SriovVfHandler struct{}

func (h *SriovVfHandler) Type() handler.DeviceType { return handler.DeviceTypeNetdev }
func (h *SriovVfHandler) Kinds() []string          { return []string{"sriov-vf"} }

func (h *SriovVfHandler) Validate(_ context.Context, cfg *handler.DeviceConfig) error {
	if cfg.Netdev == nil {
		return fmt.Errorf("netdev config is required for sriov-vf")
	}
	return nil
}

func (h *SriovVfHandler) Prepare(_ context.Context, req *handler.PrepareRequest) (*handler.PrepareResult, error) {
	cfg := req.Config.Netdev
	if cfg == nil {
		return nil, fmt.Errorf("netdev config is required for sriov-vf")
	}

	containerName := cfg.InterfaceName
	if containerName == "" {
		containerName = "eth1"
	}

	// For SR-IOV, the scheduler should have assigned us a specific VF via AllocatedDevice
	vfName := req.AllocatedDevice
	if vfName == "" {
		// Try to find a VF from the parent PF
		if cfg.Parent == "" {
			return nil, fmt.Errorf("either allocated device or parent PF must be specified for sriov-vf")
		}
		var err error
		vfName, err = findAvailableVF(cfg.Parent, cfg.VFIndex)
		if err != nil {
			return nil, fmt.Errorf("failed to find VF on parent %s: %w", cfg.Parent, err)
		}
	}

	// Verify the VF interface exists
	link, err := netlink.LinkByName(vfName)
	if err != nil {
		return nil, fmt.Errorf("VF interface %s not found: %w", vfName, err)
	}

	// Set MTU if specified
	if cfg.MTU > 0 {
		if err := netlink.LinkSetMTU(link, cfg.MTU); err != nil {
			return nil, fmt.Errorf("failed to set MTU on VF %s: %w", vfName, err)
		}
	}

	// Bring up the VF
	if err := netlink.LinkSetUp(link); err != nil {
		return nil, fmt.Errorf("failed to bring up VF %s: %w", vfName, err)
	}

	klog.Infof("Prepared SR-IOV VF %s for claim %s", vfName, req.ClaimUID)

	return &handler.PrepareResult{
		PoolName:   "default",
		DeviceName: vfName,
		CDIEdits: &cdispec.ContainerEdits{
			NetDevices: []*cdispec.LinuxNetDevice{
				{
					HostInterfaceName: vfName,
					Name:              containerName,
				},
			},
		},
		Allocation: &handler.AllocationInfo{
			Type:       handler.DeviceTypeNetdev,
			Kind:       "sriov-vf",
			ClaimUID:   req.ClaimUID,
			DeviceName: vfName,
			Metadata: map[string]string{
				"vfInterface":   vfName,
				"containerName": containerName,
			},
		},
	}, nil
}

func (h *SriovVfHandler) Unprepare(_ context.Context, req *handler.UnprepareRequest) error {
	vfName := req.Allocation.Metadata["vfInterface"]
	if vfName == "" {
		return nil
	}

	// For SR-IOV VFs, we don't delete the interface - just bring it down
	link, err := netlink.LinkByName(vfName)
	if err != nil {
		klog.V(2).Infof("SR-IOV VF %s not found during unprepare: %v", vfName, err)
		return nil
	}

	if err := netlink.LinkSetDown(link); err != nil {
		klog.Warningf("Failed to bring down VF %s: %v", vfName, err)
	}

	klog.Infof("Unprepared SR-IOV VF %s", vfName)
	return nil
}

// findAvailableVF finds an available VF interface on the given PF
func findAvailableVF(pfName string, vfIndex int) (string, error) {
	// If a specific VF index is requested, look for it directly
	if vfIndex >= 0 {
		vfDir := fmt.Sprintf("/sys/class/net/%s/device/virtfn%d/net", pfName, vfIndex)
		entries, err := os.ReadDir(vfDir)
		if err != nil {
			return "", fmt.Errorf("VF index %d not found on PF %s: %w", vfIndex, pfName, err)
		}
		if len(entries) > 0 {
			return entries[0].Name(), nil
		}
		return "", fmt.Errorf("no net device for VF index %d on PF %s", vfIndex, pfName)
	}

	// Scan for available VFs
	pfDeviceDir := filepath.Join("/sys/class/net", pfName, "device")
	entries, err := os.ReadDir(pfDeviceDir)
	if err != nil {
		return "", fmt.Errorf("failed to read PF device dir: %w", err)
	}

	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "virtfn") {
			continue
		}
		vfNetDir := filepath.Join(pfDeviceDir, entry.Name(), "net")
		netEntries, err := os.ReadDir(vfNetDir)
		if err != nil {
			continue
		}
		if len(netEntries) > 0 {
			return netEntries[0].Name(), nil
		}
	}

	return "", fmt.Errorf("no available VFs found on PF %s", pfName)
}
