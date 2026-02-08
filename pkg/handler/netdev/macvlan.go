package netdev

import (
	"context"
	"fmt"

	"github.com/vishvananda/netlink"
	"k8s.io/klog/v2"

	"github.com/example/dra-poc/pkg/handler"
	cdispec "tags.cncf.io/container-device-interface/specs-go"
)

// MacvlanHandler creates macvlan interfaces off a parent.
type MacvlanHandler struct{}

func (h *MacvlanHandler) Type() handler.DeviceType { return handler.DeviceTypeNetdev }
func (h *MacvlanHandler) Kinds() []string          { return []string{"macvlan"} }

func (h *MacvlanHandler) Validate(_ context.Context, cfg *handler.DeviceConfig) error {
	if cfg.Netdev == nil {
		return fmt.Errorf("netdev config is required for macvlan")
	}
	if cfg.Netdev.Parent == "" {
		return fmt.Errorf("parent interface is required for macvlan")
	}
	return nil
}

func (h *MacvlanHandler) Prepare(_ context.Context, req *handler.PrepareRequest) (*handler.PrepareResult, error) {
	cfg := req.Config.Netdev
	if cfg == nil {
		return nil, fmt.Errorf("netdev config is required for macvlan")
	}
	parent := cfg.Parent
	if parent == "" {
		return nil, fmt.Errorf("parent interface is required for macvlan")
	}

	mode := netlink.MACVLAN_MODE_BRIDGE
	switch cfg.Mode {
	case "vepa":
		mode = netlink.MACVLAN_MODE_VEPA
	case "private":
		mode = netlink.MACVLAN_MODE_PRIVATE
	case "bridge", "":
		mode = netlink.MACVLAN_MODE_BRIDGE
	default:
		return nil, fmt.Errorf("unsupported macvlan mode: %s", cfg.Mode)
	}

	ifName := fmt.Sprintf("mv%s", req.ClaimUID[:8])
	containerName := cfg.InterfaceName
	if containerName == "" {
		containerName = "eth1"
	}

	parentLink, err := netlink.LinkByName(parent)
	if err != nil {
		return nil, fmt.Errorf("parent interface %s not found: %w", parent, err)
	}

	mv := &netlink.Macvlan{
		LinkAttrs: netlink.LinkAttrs{
			Name:        ifName,
			ParentIndex: parentLink.Attrs().Index,
		},
		Mode: netlink.MacvlanMode(mode),
	}
	if cfg.MTU > 0 {
		mv.LinkAttrs.MTU = cfg.MTU
	}

	if err := netlink.LinkAdd(mv); err != nil {
		return nil, fmt.Errorf("failed to create macvlan interface %s: %w", ifName, err)
	}
	if err := netlink.LinkSetUp(mv); err != nil {
		netlink.LinkDel(mv)
		return nil, fmt.Errorf("failed to bring up macvlan interface %s: %w", ifName, err)
	}

	klog.Infof("Created macvlan interface %s (parent=%s, mode=%s)", ifName, parent, cfg.Mode)

	return &handler.PrepareResult{
		PoolName:   "default",
		DeviceName: ifName,
		CDIEdits: &cdispec.ContainerEdits{
			NetDevices: []*cdispec.LinuxNetDevice{
				{HostInterfaceName: ifName, Name: containerName},
			},
		},
		Allocation: &handler.AllocationInfo{
			Type: handler.DeviceTypeNetdev, Kind: "macvlan",
			ClaimUID: req.ClaimUID, DeviceName: ifName,
			Metadata: map[string]string{
				"createdInterface": ifName,
				"parent":           parent,
				"containerName":    containerName,
			},
		},
	}, nil
}

func (h *MacvlanHandler) Unprepare(_ context.Context, req *handler.UnprepareRequest) error {
	ifName := req.Allocation.Metadata["createdInterface"]
	if ifName == "" {
		return nil
	}
	link, err := netlink.LinkByName(ifName)
	if err != nil {
		klog.V(2).Infof("macvlan interface %s already removed: %v", ifName, err)
		return nil
	}
	if err := netlink.LinkDel(link); err != nil {
		return fmt.Errorf("failed to delete macvlan interface %s: %w", ifName, err)
	}
	klog.Infof("Deleted macvlan interface %s", ifName)
	return nil
}
