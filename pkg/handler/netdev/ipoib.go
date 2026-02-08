package netdev

import (
	"context"
	"fmt"

	"github.com/vishvananda/netlink"
	"k8s.io/klog/v2"

	"github.com/example/dra-poc/pkg/handler"
	cdispec "tags.cncf.io/container-device-interface/specs-go"
)

// IpoibHandler creates IPoIB (IP over InfiniBand) child interfaces.
//
// An IPoIB child interface is derived from a parent IB interface (e.g. ib0)
// using a partition key (pkey). The kernel creates a child whose name is
// typically "<parent>.<pkey>" (e.g. "ib0.8001").  This handler creates the
// child, brings it up, and hands it to the container via CDI netDevices.
//
// Required config fields:
//   - parent: name of the parent IB interface (e.g. "ib0")
//   - pkey:   16-bit partition key (e.g. 0x8001). The high bit (full membership)
//     is set automatically if not already present.
//
// Optional:
//   - interfaceName: name the interface should have inside the container
//   - mtu:           override the default MTU
//   - mode:          "datagram" (default) or "connected"
type IpoibHandler struct{}

func (h *IpoibHandler) Type() handler.DeviceType { return handler.DeviceTypeNetdev }
func (h *IpoibHandler) Kinds() []string          { return []string{"ipoib"} }

func (h *IpoibHandler) Validate(_ context.Context, cfg *handler.DeviceConfig) error {
	if cfg.Netdev == nil {
		return fmt.Errorf("netdev config is required for ipoib")
	}
	if cfg.Netdev.Parent == "" {
		return fmt.Errorf("parent interface is required for ipoib")
	}
	if cfg.Netdev.Pkey == 0 {
		return fmt.Errorf("pkey is required for ipoib")
	}
	return nil
}

func (h *IpoibHandler) Prepare(_ context.Context, req *handler.PrepareRequest) (*handler.PrepareResult, error) {
	cfg := req.Config.Netdev
	if cfg == nil {
		return nil, fmt.Errorf("netdev config is required for ipoib")
	}

	parent := cfg.Parent
	if parent == "" {
		return nil, fmt.Errorf("parent interface is required for ipoib")
	}

	pkey := cfg.Pkey
	if pkey == 0 {
		return nil, fmt.Errorf("pkey is required for ipoib")
	}

	// Ensure the full-membership bit (0x8000) is set.
	pkey |= 0x8000

	// Resolve IPoIB mode (datagram is the kernel default).
	mode := netlink.IPOIB_MODE_DATAGRAM
	switch cfg.Mode {
	case "connected":
		mode = netlink.IPOIB_MODE_CONNECTED
	case "datagram", "":
		// already set
	default:
		return nil, fmt.Errorf("unsupported ipoib mode: %s (want datagram or connected)", cfg.Mode)
	}

	// Interface name on the host: <parent>.<pkey hex> truncated via claim UID
	// to avoid collisions when the same pkey is used across multiple claims.
	ifName := fmt.Sprintf("ib%s", req.ClaimUID[:8])

	containerName := cfg.InterfaceName
	if containerName == "" {
		containerName = "ib1"
	}

	parentLink, err := netlink.LinkByName(parent)
	if err != nil {
		return nil, fmt.Errorf("parent interface %s not found: %w", parent, err)
	}

	ipoib := &netlink.IPoIB{
		LinkAttrs: netlink.LinkAttrs{
			Name:        ifName,
			ParentIndex: parentLink.Attrs().Index,
		},
		Pkey: uint16(pkey),
		Mode: netlink.IPoIBMode(mode),
	}

	if cfg.MTU > 0 {
		ipoib.LinkAttrs.MTU = cfg.MTU
	}

	if err := netlink.LinkAdd(ipoib); err != nil {
		return nil, fmt.Errorf("failed to create ipoib interface %s (parent=%s, pkey=0x%04x): %w",
			ifName, parent, pkey, err)
	}

	if err := netlink.LinkSetUp(ipoib); err != nil {
		netlink.LinkDel(ipoib)
		return nil, fmt.Errorf("failed to bring up ipoib interface %s: %w", ifName, err)
	}

	klog.Infof("Created ipoib interface %s (parent=%s, pkey=0x%04x, mode=%s)",
		ifName, parent, pkey, cfg.Mode)

	return &handler.PrepareResult{
		PoolName:   "default",
		DeviceName: ifName,
		CDIEdits: &cdispec.ContainerEdits{
			NetDevices: []*cdispec.LinuxNetDevice{
				{HostInterfaceName: ifName, Name: containerName},
			},
		},
		Allocation: &handler.AllocationInfo{
			Type:       handler.DeviceTypeNetdev,
			Kind:       "ipoib",
			ClaimUID:   req.ClaimUID,
			DeviceName: ifName,
			Metadata: map[string]string{
				"createdInterface": ifName,
				"parent":           parent,
				"pkey":             fmt.Sprintf("0x%04x", pkey),
				"containerName":    containerName,
			},
		},
	}, nil
}

func (h *IpoibHandler) Unprepare(_ context.Context, req *handler.UnprepareRequest) error {
	ifName := req.Allocation.Metadata["createdInterface"]
	if ifName == "" {
		return nil
	}
	link, err := netlink.LinkByName(ifName)
	if err != nil {
		klog.V(2).Infof("ipoib interface %s already removed: %v", ifName, err)
		return nil
	}
	if err := netlink.LinkDel(link); err != nil {
		return fmt.Errorf("failed to delete ipoib interface %s: %w", ifName, err)
	}
	klog.Infof("Deleted ipoib interface %s", ifName)
	return nil
}
