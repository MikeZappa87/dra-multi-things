package netdev

import (
	"context"
	"fmt"

	"github.com/vishvananda/netlink"
	"k8s.io/klog/v2"

	"github.com/example/dra-poc/pkg/handler"
	cdispec "tags.cncf.io/container-device-interface/specs-go"
)

// IpvlanHandler creates ipvlan interfaces off a parent
type IpvlanHandler struct{}

func (h *IpvlanHandler) Type() handler.DeviceType { return handler.DeviceTypeNetdev }
func (h *IpvlanHandler) Kinds() []string          { return []string{"ipvlan"} }

func (h *IpvlanHandler) Validate(_ context.Context, cfg *handler.DeviceConfig) error {
	if cfg.Netdev == nil {
		return fmt.Errorf("netdev config is required for ipvlan")
	}
	if cfg.Netdev.Parent == "" {
		return fmt.Errorf("parent interface is required for ipvlan")
	}
	return nil
}

func (h *IpvlanHandler) Prepare(_ context.Context, req *handler.PrepareRequest) (*handler.PrepareResult, error) {
	cfg := req.Config.Netdev
	if cfg == nil {
		return nil, fmt.Errorf("netdev config is required for ipvlan")
	}

	parent := cfg.Parent
	if parent == "" {
		return nil, fmt.Errorf("parent interface is required for ipvlan")
	}

	// Resolve ipvlan mode
	mode := netlink.IPVLAN_MODE_L2 // default
	switch cfg.Mode {
	case "l3":
		mode = netlink.IPVLAN_MODE_L3
	case "l2", "":
		mode = netlink.IPVLAN_MODE_L2
	default:
		return nil, fmt.Errorf("unsupported ipvlan mode: %s", cfg.Mode)
	}

	// Generate a unique interface name
	ifName := fmt.Sprintf("iv%s", req.ClaimUID[:8])

	containerName := cfg.InterfaceName
	if containerName == "" {
		containerName = "eth1"
	}

	// Get parent link
	parentLink, err := netlink.LinkByName(parent)
	if err != nil {
		return nil, fmt.Errorf("parent interface %s not found: %w", parent, err)
	}

	// Create ipvlan link
	iv := &netlink.IPVlan{
		LinkAttrs: netlink.LinkAttrs{
			Name:        ifName,
			ParentIndex: parentLink.Attrs().Index,
		},
		Mode: netlink.IPVlanMode(mode),
	}

	if cfg.MTU > 0 {
		iv.LinkAttrs.MTU = cfg.MTU
	}

	if err := netlink.LinkAdd(iv); err != nil {
		return nil, fmt.Errorf("failed to create ipvlan interface %s: %w", ifName, err)
	}

	if err := netlink.LinkSetUp(iv); err != nil {
		netlink.LinkDel(iv)
		return nil, fmt.Errorf("failed to bring up ipvlan interface %s: %w", ifName, err)
	}

	klog.Infof("Created ipvlan interface %s (parent=%s, mode=%s)", ifName, parent, cfg.Mode)

	return &handler.PrepareResult{
		PoolName:   "default",
		DeviceName: ifName,
		CDIEdits: &cdispec.ContainerEdits{
			NetDevices: []*cdispec.LinuxNetDevice{
				{
					HostInterfaceName: ifName,
					Name:              containerName,
				},
			},
		},
		Allocation: &handler.AllocationInfo{
			Type:       handler.DeviceTypeNetdev,
			Kind:       "ipvlan",
			ClaimUID:   req.ClaimUID,
			DeviceName: ifName,
			Metadata: map[string]string{
				"createdInterface": ifName,
				"parent":           parent,
				"containerName":    containerName,
			},
		},
	}, nil
}

func (h *IpvlanHandler) Unprepare(_ context.Context, req *handler.UnprepareRequest) error {
	ifName := req.Allocation.Metadata["createdInterface"]
	if ifName == "" {
		return nil
	}

	link, err := netlink.LinkByName(ifName)
	if err != nil {
		klog.V(2).Infof("ipvlan interface %s already removed: %v", ifName, err)
		return nil
	}

	if err := netlink.LinkDel(link); err != nil {
		return fmt.Errorf("failed to delete ipvlan interface %s: %w", ifName, err)
	}

	klog.Infof("Deleted ipvlan interface %s", ifName)
	return nil
}
