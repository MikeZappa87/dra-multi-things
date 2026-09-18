package netdev

import (
	"context"
	"errors"
	"fmt"
	"syscall"

	"github.com/vishvananda/netlink"
	"k8s.io/klog/v2"

	"github.com/example/dra-poc/pkg/handler"
	"github.com/example/dra-poc/pkg/nri"
)

// BridgeVethHandler creates a Linux bridge in the host netns and attaches a veth
// pair where one end is moved into the pod network namespace through CDI.
// If an IPAMLeaseTracker is provided, it will allocate and manage IPs from configured pools.
type BridgeVethHandler struct {
	IPAMTracker *nri.IPAMLeaseTracker
}

func (h *BridgeVethHandler) Type() handler.DeviceType { return handler.DeviceTypeNetdev }
func (h *BridgeVethHandler) Kinds() []string          { return []string{"bridge-veth"} }

func (h *BridgeVethHandler) Validate(_ context.Context, cfg *handler.DeviceConfig) error {
	if cfg.Netdev == nil {
		return fmt.Errorf("netdev config is required for bridge-veth")
	}
	if cfg.Netdev.BridgeName == "" {
		return fmt.Errorf("bridge name is required for bridge-veth")
	}
	return nil
}

func (h *BridgeVethHandler) Prepare(_ context.Context, req *handler.PrepareRequest) (*handler.PrepareResult, error) {
	cfg := req.Config.Netdev
	if cfg == nil {
		return nil, fmt.Errorf("netdev config is required for bridge-veth")
	}
	bridgeName := cfg.BridgeName
	if bridgeName == "" {
		return nil, fmt.Errorf("bridge name is required for bridge-veth")
	}

	containerName := cfg.InterfaceName
	if containerName == "" {
		containerName = "eth1"
	}

	hostEnd := fmt.Sprintf("vh%s", req.ClaimUID[:8])
	containerEnd := fmt.Sprintf("vc%s", req.ClaimUID[:8])

	bridgeLink, bridgeCreated, err := ensureBridge(bridgeName)
	if err != nil {
		return nil, err
	}

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
		return nil, fmt.Errorf("failed to create bridge veth pair %s/%s: %w", hostEnd, containerEnd, err)
	}

	hostLink, err := netlink.LinkByName(hostEnd)
	if err != nil {
		netlink.LinkDel(veth)
		return nil, fmt.Errorf("failed to find host veth %s: %w", hostEnd, err)
	}
	if err := netlink.LinkSetMaster(hostLink, bridgeLink); err != nil {
		netlink.LinkDel(veth)
		return nil, fmt.Errorf("failed to attach veth %s to bridge %s: %w", hostEnd, bridgeName, err)
	}
	if err := netlink.LinkSetUp(hostLink); err != nil {
		netlink.LinkDel(veth)
		return nil, fmt.Errorf("failed to bring up host veth %s: %w", hostEnd, err)
	}

	if cfg.VLANID > 0 {
		if err := applyBridgeVLAN(bridgeLink, hostLink, bridgeName, hostEnd, cfg.VLANID); err != nil {
			netlink.LinkDel(veth)
			return nil, err
		}
	}

	containerLink, err := netlink.LinkByName(containerEnd)
	if err != nil {
		netlink.LinkDel(veth)
		return nil, fmt.Errorf("failed to find container veth %s: %w", containerEnd, err)
	}
	if err := netlink.LinkSetUp(containerLink); err != nil {
		netlink.LinkDel(veth)
		return nil, fmt.Errorf("failed to bring up container veth %s: %w", containerEnd, err)
	}

	klog.Infof("Created bridge veth pair %s/%s on bridge %s (vlan=%d)", hostEnd, containerEnd, bridgeName, cfg.VLANID)

	poolName := "default"
	var ipamErr error

	// If pool name is configured, allocate an IP and register as pending in the tracker
	if cfg.PoolName != "" && h.IPAMTracker != nil {
		poolName = cfg.PoolName
		lease, err := h.IPAMTracker.Allocator.Allocate(cfg.PoolName, req.ClaimUID, "", containerName)
		if err != nil {
			klog.Errorf("Failed to allocate IP from pool %s: %v", cfg.PoolName, err)
			ipamErr = err
		} else {
			lease.HostInterface = containerEnd
			if err := h.IPAMTracker.AddPending(req.ClaimUID, lease); err != nil {
				klog.Errorf("Failed to register pending lease: %v", err)
				ipamErr = err
			}
		}
	}

	// Clean up veth on IPAM error
	if ipamErr != nil {
		netlink.LinkDel(veth)
		return nil, fmt.Errorf("IPAM allocation failed: %w", ipamErr)
	}

	return &handler.PrepareResult{
		PoolName:   poolName,
		DeviceName: containerEnd,
		CDIEdits:   nil,
		Allocation: &handler.AllocationInfo{
			Type:       handler.DeviceTypeNetdev,
			Kind:       "bridge-veth",
			ClaimUID:   req.ClaimUID,
			DeviceName: containerEnd,
			Metadata: map[string]string{
				"bridgeName":    bridgeName,
				"bridgeCreated": fmt.Sprintf("%t", bridgeCreated),
				"hostEnd":       hostEnd,
				"containerEnd":  containerEnd,
				"containerName": containerName,
				"poolName":      poolName,
			},
		},
	}, nil
}

func (h *BridgeVethHandler) Unprepare(_ context.Context, req *handler.UnprepareRequest) error {
	hostEnd := req.Allocation.Metadata["hostEnd"]
	bridgeName := req.Allocation.Metadata["bridgeName"]
	bridgeCreated := req.Allocation.Metadata["bridgeCreated"] == "true"

	// Remove pending IPAM lease if it exists
	if h.IPAMTracker != nil {
		if err := h.IPAMTracker.RemovePending(req.ClaimUID); err != nil {
			klog.Warningf("Failed to remove pending lease for claim %s: %v", req.ClaimUID, err)
		}
	}

	if hostEnd != "" {
		link, err := netlink.LinkByName(hostEnd)
		if err != nil {
			klog.V(2).Infof("bridge-veth host end %s already removed: %v", hostEnd, err)
		} else if err := netlink.LinkDel(link); err != nil {
			return fmt.Errorf("failed to delete bridge-veth pair (host=%s): %w", hostEnd, err)
		}
	}

	if bridgeCreated && bridgeName != "" {
		bridgeLink, err := netlink.LinkByName(bridgeName)
		if err != nil {
			klog.V(2).Infof("bridge %s already removed: %v", bridgeName, err)
			return nil
		}
		if err := netlink.LinkDel(bridgeLink); err != nil {
			return fmt.Errorf("failed to delete bridge %s: %w", bridgeName, err)
		}
		klog.Infof("Deleted bridge %s", bridgeName)
	}

	return nil
}

func ensureBridge(name string) (netlink.Link, bool, error) {
	link, err := netlink.LinkByName(name)
	if err == nil {
		if _, ok := link.(*netlink.Bridge); !ok {
			return nil, false, fmt.Errorf("interface %s exists but is not a bridge", name)
		}
		if err := netlink.LinkSetUp(link); err != nil {
			return nil, false, fmt.Errorf("failed to bring up bridge %s: %w", name, err)
		}
		return link, false, nil
	}

	bridge := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: name}}
	if err := netlink.LinkAdd(bridge); err != nil {
		return nil, false, fmt.Errorf("failed to create bridge %s: %w", name, err)
	}
	if err := netlink.LinkSetUp(bridge); err != nil {
		netlink.LinkDel(bridge)
		return nil, false, fmt.Errorf("failed to bring up bridge %s: %w", name, err)
	}
	return bridge, true, nil
}

func applyBridgeVLAN(bridgeLink, portLink netlink.Link, bridgeName, portName string, vlanID int) error {
	if vlanID <= 0 {
		return nil
	}

	bridgeAttrs := bridgeLink.Attrs()
	bridgeUpdate := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{
		Index: bridgeAttrs.Index,
		Name:  bridgeAttrs.Name,
	}}
	if err := netlink.BridgeSetVlanFiltering(bridgeUpdate, true); err != nil {
		if errors.Is(err, syscall.EINVAL) {
			return fmt.Errorf("bridge VLAN filtering unsupported on %s for VLAN %d: %w", bridgeName, vlanID, err)
		}
		return fmt.Errorf("failed to enable vlan filtering on bridge %s: %w", bridgeName, err)
	}
	if err := netlink.BridgeVlanAdd(portLink, uint16(vlanID), true, true, false, false); err != nil {
		if errors.Is(err, syscall.EINVAL) {
			return fmt.Errorf("bridge VLAN tagging unsupported for %s on %s for VLAN %d: %w", portName, bridgeName, vlanID, err)
		}
		return fmt.Errorf("failed to add vlan %d to bridge port %s: %w", vlanID, portName, err)
	}
	if vlanID != 1 {
		if err := netlink.BridgeVlanDel(portLink, 1, false, false, false, false); err != nil {
			return fmt.Errorf("failed to remove default vlan 1 from bridge port %s: %w", portName, err)
		}
	}
	return nil
}
