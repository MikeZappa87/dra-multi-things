package combo

import (
	"context"
	"fmt"

	"k8s.io/klog/v2"

	"github.com/example/dra-poc/pkg/handler"
	cdispec "tags.cncf.io/container-device-interface/specs-go"
)

// RoCEHandler composes RDMA + netdev handlers for RoCE devices
type RoCEHandler struct {
	rdmaHandler   handler.DeviceHandler
	netdevHandler handler.DeviceHandler
}

// NewRoCEHandler creates a new RoCE combo handler
func NewRoCEHandler(rdmaHandler, netdevHandler handler.DeviceHandler) *RoCEHandler {
	return &RoCEHandler{
		rdmaHandler:   rdmaHandler,
		netdevHandler: netdevHandler,
	}
}

func (h *RoCEHandler) Type() handler.DeviceType { return handler.DeviceTypeCombo }
func (h *RoCEHandler) Kinds() []string          { return []string{"roce"} }

func (h *RoCEHandler) Validate(ctx context.Context, cfg *handler.DeviceConfig) error {
	if cfg.Combo == nil {
		return fmt.Errorf("combo config is required for roce")
	}
	return nil
}

func (h *RoCEHandler) Prepare(ctx context.Context, req *handler.PrepareRequest) (*handler.PrepareResult, error) {
	cfg := req.Config.Combo
	if cfg == nil {
		return nil, fmt.Errorf("combo config is required for roce")
	}

	// Prepare RDMA first (finite resource)
	rdmaReq := &handler.PrepareRequest{
		ClaimUID:        req.ClaimUID,
		Namespace:       req.Namespace,
		ClaimName:       req.ClaimName,
		AllocatedDevice: req.AllocatedDevice, // scheduler picked this
		Config:          &handler.DeviceConfig{Type: handler.DeviceTypeRDMA, RDMA: &cfg.RDMA},
	}
	rdmaResult, err := h.rdmaHandler.Prepare(ctx, rdmaReq)
	if err != nil {
		return nil, fmt.Errorf("rdma prepare failed: %w", err)
	}

	// Prepare netdev (could be virtual or the associated RoCE interface)
	netReq := &handler.PrepareRequest{
		ClaimUID:  req.ClaimUID,
		Namespace: req.Namespace,
		ClaimName: req.ClaimName,
		Config:    &handler.DeviceConfig{Type: handler.DeviceTypeNetdev, Netdev: &cfg.Netdev},
	}
	netResult, err := h.netdevHandler.Prepare(ctx, netReq)
	if err != nil {
		// Rollback RDMA
		h.rdmaHandler.Unprepare(ctx, &handler.UnprepareRequest{
			ClaimUID:   req.ClaimUID,
			Allocation: rdmaResult.Allocation,
		})
		return nil, fmt.Errorf("netdev prepare failed: %w", err)
	}

	// Merge CDI edits from both handlers
	merged := mergeCDIEdits(rdmaResult.CDIEdits, netResult.CDIEdits)

	klog.Infof("Prepared RoCE device: rdma=%s, net=%s for claim %s",
		rdmaResult.DeviceName, netResult.DeviceName, req.ClaimUID)

	return &handler.PrepareResult{
		PoolName:   rdmaResult.PoolName,
		DeviceName: rdmaResult.DeviceName,
		CDIEdits:   merged,
		Allocation: &handler.AllocationInfo{
			Type:     handler.DeviceTypeCombo,
			Kind:     "roce",
			ClaimUID: req.ClaimUID,
			Metadata: map[string]string{
				"rdma_device":   rdmaResult.DeviceName,
				"net_interface": netResult.DeviceName,
				// Propagate sub-allocation metadata for cleanup
				"rdma_uverbs_device": rdmaResult.Allocation.Metadata["uverbsDevice"],
				"rdma_ibdev":         rdmaResult.Allocation.Metadata["ibdev"],
				"net_created":        netResult.Allocation.Metadata["createdInterface"],
				"net_host_end":       netResult.Allocation.Metadata["hostEnd"],
			},
		},
	}, nil
}

func (h *RoCEHandler) Unprepare(ctx context.Context, req *handler.UnprepareRequest) error {
	var errs []error

	// Unprepare netdev
	netAlloc := &handler.AllocationInfo{
		Type:     handler.DeviceTypeNetdev,
		Kind:     "dummy", // The actual kind doesn't matter for unprepare
		ClaimUID: req.ClaimUID,
		Metadata: map[string]string{
			"createdInterface": req.Allocation.Metadata["net_created"],
			"hostEnd":          req.Allocation.Metadata["net_host_end"],
		},
	}
	if err := h.netdevHandler.Unprepare(ctx, &handler.UnprepareRequest{
		ClaimUID:   req.ClaimUID,
		Allocation: netAlloc,
	}); err != nil {
		errs = append(errs, fmt.Errorf("netdev unprepare: %w", err))
	}

	// Unprepare RDMA
	rdmaAlloc := &handler.AllocationInfo{
		Type:     handler.DeviceTypeRDMA,
		Kind:     "uverbs",
		ClaimUID: req.ClaimUID,
		Metadata: map[string]string{
			"uverbsDevice": req.Allocation.Metadata["rdma_uverbs_device"],
			"ibdev":        req.Allocation.Metadata["rdma_ibdev"],
		},
	}
	if err := h.rdmaHandler.Unprepare(ctx, &handler.UnprepareRequest{
		ClaimUID:   req.ClaimUID,
		Allocation: rdmaAlloc,
	}); err != nil {
		errs = append(errs, fmt.Errorf("rdma unprepare: %w", err))
	}

	if len(errs) > 0 {
		return fmt.Errorf("roce unprepare errors: %v", errs)
	}

	klog.Infof("Unprepared RoCE device for claim %s", req.ClaimUID)
	return nil
}

// mergeCDIEdits merges two sets of CDI container edits
func mergeCDIEdits(a, b *cdispec.ContainerEdits) *cdispec.ContainerEdits {
	return &cdispec.ContainerEdits{
		DeviceNodes: append(a.DeviceNodes, b.DeviceNodes...),
		Mounts:      append(a.Mounts, b.Mounts...),
		NetDevices:  append(a.NetDevices, b.NetDevices...),
		Env:         append(a.Env, b.Env...),
	}
}
