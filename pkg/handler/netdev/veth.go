package netdev

import (
	"context"
	"fmt"

	"github.com/vishvananda/netlink"
	"k8s.io/klog/v2"

	"github.com/example/dra-poc/pkg/handler"
	cdispec "tags.cncf.io/container-device-interface/specs-go"
)

// VethHandler creates veth pairs (one end for the container)
type VethHandler struct{}

func (h *VethHandler) Type() handler.DeviceType { return handler.DeviceTypeNetdev }
func (h *VethHandler) Kinds() []string          { return []string{"veth"} }

func (h *VethHandler) Validate(_ context.Context, cfg *handler.DeviceConfig) error {
	if cfg.Netdev == nil {
		return fmt.Errorf("netdev config is required for veth")
	}
	return nil
}

func (h *VethHandler) Prepare(_ context.Context, req *handler.PrepareRequest) (*handler.PrepareResult, error) {
	cfg := req.Config.Netdev
	if cfg == nil {
		return nil, fmt.Errorf("netdev config is required for veth")
	}

	// Generate unique veth pair names
	hostEnd := fmt.Sprintf("vh%s", req.ClaimUID[:8])
	containerEnd := fmt.Sprintf("vc%s", req.ClaimUID[:8])

	containerName := cfg.InterfaceName
	if containerName == "" {
		containerName = "eth1"
	}

	// Create veth pair
	veth := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{
			Name: hostEnd,
		},
		PeerName: containerEnd,
	}

	if cfg.MTU > 0 {
		veth.LinkAttrs.MTU = cfg.MTU
	}

	if err := netlink.LinkAdd(veth); err != nil {
		return nil, fmt.Errorf("failed to create veth pair %s/%s: %w", hostEnd, containerEnd, err)
	}

	// Bring up host end
	hostLink, err := netlink.LinkByName(hostEnd)
	if err != nil {
		netlink.LinkDel(veth)
		return nil, fmt.Errorf("failed to find host veth %s: %w", hostEnd, err)
	}
	if err := netlink.LinkSetUp(hostLink); err != nil {
		netlink.LinkDel(veth)
		return nil, fmt.Errorf("failed to bring up host veth %s: %w", hostEnd, err)
	}

	// Bring up container end (CDI will move it)
	containerLink, err := netlink.LinkByName(containerEnd)
	if err != nil {
		netlink.LinkDel(veth)
		return nil, fmt.Errorf("failed to find container veth %s: %w", containerEnd, err)
	}
	if err := netlink.LinkSetUp(containerLink); err != nil {
		netlink.LinkDel(veth)
		return nil, fmt.Errorf("failed to bring up container veth %s: %w", containerEnd, err)
	}

	klog.Infof("Created veth pair %s/%s", hostEnd, containerEnd)

	// The container end gets moved into the container netns via CDI
	return &handler.PrepareResult{
		PoolName:   "default",
		DeviceName: containerEnd,
		CDIEdits: &cdispec.ContainerEdits{
			NetDevices: []*cdispec.LinuxNetDevice{
				{
					HostInterfaceName: containerEnd,
					Name:              containerName,
				},
			},
		},
		Allocation: &handler.AllocationInfo{
			Type:       handler.DeviceTypeNetdev,
			Kind:       "veth",
			ClaimUID:   req.ClaimUID,
			DeviceName: containerEnd,
			Metadata: map[string]string{
				"hostEnd":       hostEnd,
				"containerEnd":  containerEnd,
				"containerName": containerName,
			},
		},
	}, nil
}

func (h *VethHandler) Unprepare(_ context.Context, req *handler.UnprepareRequest) error {
	// Deleting one end of the veth pair deletes both
	hostEnd := req.Allocation.Metadata["hostEnd"]
	if hostEnd == "" {
		return nil
	}

	link, err := netlink.LinkByName(hostEnd)
	if err != nil {
		klog.V(2).Infof("veth host end %s already removed: %v", hostEnd, err)
		return nil
	}

	if err := netlink.LinkDel(link); err != nil {
		return fmt.Errorf("failed to delete veth pair (host=%s): %w", hostEnd, err)
	}

	klog.Infof("Deleted veth pair (host=%s)", hostEnd)
	return nil
}
