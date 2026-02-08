package netdev

import (
	"context"
	"fmt"

	"github.com/vishvananda/netlink"
	"k8s.io/klog/v2"

	"github.com/example/dra-poc/pkg/handler"
	cdispec "tags.cncf.io/container-device-interface/specs-go"
)

// DummyHandler creates dummy network interfaces (useful for testing)
type DummyHandler struct{}

func (h *DummyHandler) Type() handler.DeviceType { return handler.DeviceTypeNetdev }
func (h *DummyHandler) Kinds() []string          { return []string{"dummy"} }

func (h *DummyHandler) Validate(_ context.Context, cfg *handler.DeviceConfig) error {
	if cfg.Netdev == nil {
		return fmt.Errorf("netdev config is required for dummy")
	}
	return nil
}

func (h *DummyHandler) Prepare(_ context.Context, req *handler.PrepareRequest) (*handler.PrepareResult, error) {
	cfg := req.Config.Netdev
	if cfg == nil {
		return nil, fmt.Errorf("netdev config is required for dummy")
	}

	// Generate a unique dummy interface name
	ifName := fmt.Sprintf("dm%s", req.ClaimUID[:8])

	containerName := cfg.InterfaceName
	if containerName == "" {
		containerName = "eth1"
	}

	// Create dummy interface
	dummy := &netlink.Dummy{
		LinkAttrs: netlink.LinkAttrs{
			Name: ifName,
		},
	}

	if cfg.MTU > 0 {
		dummy.LinkAttrs.MTU = cfg.MTU
	}

	if err := netlink.LinkAdd(dummy); err != nil {
		return nil, fmt.Errorf("failed to create dummy interface %s: %w", ifName, err)
	}

	link, err := netlink.LinkByName(ifName)
	if err != nil {
		netlink.LinkDel(dummy)
		return nil, fmt.Errorf("failed to find dummy interface %s after creation: %w", ifName, err)
	}

	if err := netlink.LinkSetUp(link); err != nil {
		netlink.LinkDel(link)
		return nil, fmt.Errorf("failed to bring up dummy interface %s: %w", ifName, err)
	}

	klog.Infof("Created dummy interface %s", ifName)

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
			Kind:       "dummy",
			ClaimUID:   req.ClaimUID,
			DeviceName: ifName,
			Metadata: map[string]string{
				"createdInterface": ifName,
				"containerName":    containerName,
			},
		},
	}, nil
}

func (h *DummyHandler) Unprepare(_ context.Context, req *handler.UnprepareRequest) error {
	ifName := req.Allocation.Metadata["createdInterface"]
	if ifName == "" {
		return nil
	}

	link, err := netlink.LinkByName(ifName)
	if err != nil {
		klog.V(2).Infof("dummy interface %s already removed: %v", ifName, err)
		return nil
	}

	if err := netlink.LinkDel(link); err != nil {
		return fmt.Errorf("failed to delete dummy interface %s: %w", ifName, err)
	}

	klog.Infof("Deleted dummy interface %s", ifName)
	return nil
}
