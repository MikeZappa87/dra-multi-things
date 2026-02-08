package netdev

import (
	"context"
	"fmt"

	"github.com/vishvananda/netlink"
	"k8s.io/klog/v2"

	"github.com/example/dra-poc/pkg/handler"
	cdispec "tags.cncf.io/container-device-interface/specs-go"
)

// HostDeviceHandler moves a pre-existing host network interface into a pod.
// The interface is expected to already exist on the host, fully configured
// (IP addresses, routes, etc.) by an external system. This handler simply
// tells the container runtime to move it into the container's network
// namespace via CDI netDevices.
type HostDeviceHandler struct{}

func (h *HostDeviceHandler) Type() handler.DeviceType { return handler.DeviceTypeNetdev }
func (h *HostDeviceHandler) Kinds() []string          { return []string{"host-device"} }

func (h *HostDeviceHandler) Validate(_ context.Context, cfg *handler.DeviceConfig) error {
	if cfg.Netdev == nil {
		return fmt.Errorf("netdev config is required for host-device")
	}
	if cfg.Netdev.HostDevice == "" {
		return fmt.Errorf("hostDevice (the name of the existing host interface) is required for host-device")
	}
	return nil
}

func (h *HostDeviceHandler) Prepare(_ context.Context, req *handler.PrepareRequest) (*handler.PrepareResult, error) {
	cfg := req.Config.Netdev
	if cfg == nil {
		return nil, fmt.Errorf("netdev config is required for host-device")
	}

	hostIF := cfg.HostDevice
	if hostIF == "" {
		return nil, fmt.Errorf("hostDevice is required for host-device")
	}

	containerName := cfg.InterfaceName
	if containerName == "" {
		containerName = hostIF // keep the same name inside the container by default
	}

	// Verify the interface actually exists on the host right now.
	link, err := netlink.LinkByName(hostIF)
	if err != nil {
		return nil, fmt.Errorf("host interface %q not found: %w", hostIF, err)
	}

	// Optionally set MTU before handing it off.
	if cfg.MTU > 0 {
		if err := netlink.LinkSetMTU(link, cfg.MTU); err != nil {
			return nil, fmt.Errorf("failed to set MTU on %s: %w", hostIF, err)
		}
	}

	klog.Infof("Prepared host-device %s for claim %s (will appear as %s in container)",
		hostIF, req.ClaimUID, containerName)

	return &handler.PrepareResult{
		PoolName:   "default",
		DeviceName: hostIF,
		CDIEdits: &cdispec.ContainerEdits{
			NetDevices: []*cdispec.LinuxNetDevice{
				{
					HostInterfaceName: hostIF,
					Name:              containerName,
				},
			},
		},
		Allocation: &handler.AllocationInfo{
			Type:       handler.DeviceTypeNetdev,
			Kind:       "host-device",
			ClaimUID:   req.ClaimUID,
			DeviceName: hostIF,
			Metadata: map[string]string{
				"hostDevice":    hostIF,
				"containerName": containerName,
			},
		},
	}, nil
}

func (h *HostDeviceHandler) Unprepare(_ context.Context, req *handler.UnprepareRequest) error {
	// The interface was created by an external system and will be returned to
	// the host network namespace automatically when the container exits.
	// We intentionally do NOT delete it.
	hostIF := req.Allocation.Metadata["hostDevice"]
	klog.Infof("Released host-device %s for claim %s (device owned externally, not deleted)",
		hostIF, req.ClaimUID)
	return nil
}
