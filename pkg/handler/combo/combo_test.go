package combo

import (
	"context"
	"fmt"
	"testing"

	"github.com/example/dra-poc/pkg/handler"
	cdispec "tags.cncf.io/container-device-interface/specs-go"
)

// fakeHandler is a mock DeviceHandler for testing the RoCE combo handler.
type fakeHandler struct {
	deviceType handler.DeviceType
	kinds      []string

	prepareResult *handler.PrepareResult
	prepareErr    error
	unprepareErr  error

	prepareCalled   bool
	unprepareCalled bool
}

func (f *fakeHandler) Type() handler.DeviceType { return f.deviceType }
func (f *fakeHandler) Kinds() []string          { return f.kinds }

func (f *fakeHandler) Validate(_ context.Context, _ *handler.DeviceConfig) error { return nil }

func (f *fakeHandler) Prepare(_ context.Context, _ *handler.PrepareRequest) (*handler.PrepareResult, error) {
	f.prepareCalled = true
	if f.prepareErr != nil {
		return nil, f.prepareErr
	}
	return f.prepareResult, nil
}

func (f *fakeHandler) Unprepare(_ context.Context, _ *handler.UnprepareRequest) error {
	f.unprepareCalled = true
	return f.unprepareErr
}

func TestRoCEHandler_Metadata(t *testing.T) {
	h := NewRoCEHandler(&fakeHandler{}, &fakeHandler{})
	if h.Type() != handler.DeviceTypeCombo {
		t.Errorf("Type() = %s, want combo", h.Type())
	}
	kinds := h.Kinds()
	if len(kinds) != 1 || kinds[0] != "roce" {
		t.Errorf("Kinds() = %v, want [roce]", kinds)
	}
}

func TestRoCEHandler_Validate(t *testing.T) {
	h := NewRoCEHandler(&fakeHandler{}, &fakeHandler{})

	if err := h.Validate(context.Background(), &handler.DeviceConfig{}); err == nil {
		t.Error("expected error when Combo is nil")
	}
	if err := h.Validate(context.Background(), &handler.DeviceConfig{
		Combo: &handler.ComboConfig{},
	}); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRoCEHandler_PrepareNilCombo(t *testing.T) {
	h := NewRoCEHandler(&fakeHandler{}, &fakeHandler{})
	_, err := h.Prepare(context.Background(), &handler.PrepareRequest{
		ClaimUID: "12345678-0000-0000-0000-000000000000",
		Config:   &handler.DeviceConfig{Type: handler.DeviceTypeCombo},
	})
	if err == nil {
		t.Error("expected error when combo config is nil")
	}
}

func TestRoCEHandler_PrepareSuccess(t *testing.T) {
	rdma := &fakeHandler{
		deviceType: handler.DeviceTypeRDMA,
		kinds:      []string{"uverbs"},
		prepareResult: &handler.PrepareResult{
			PoolName:   "rdma-pool",
			DeviceName: "uverbs0",
			CDIEdits: &cdispec.ContainerEdits{
				DeviceNodes: []*cdispec.DeviceNode{{
					Path:     "/dev/infiniband/uverbs0",
					HostPath: "/dev/infiniband/uverbs0",
				}},
			},
			Allocation: &handler.AllocationInfo{
				Type:     handler.DeviceTypeRDMA,
				Kind:     "uverbs",
				ClaimUID: "aabbccdd-0000-0000-0000-000000000000",
				Metadata: map[string]string{
					"uverbsDevice": "uverbs0",
					"ibdev":        "mlx5_0",
				},
			},
		},
	}

	netdev := &fakeHandler{
		deviceType: handler.DeviceTypeNetdev,
		kinds:      []string{"dummy"},
		prepareResult: &handler.PrepareResult{
			PoolName:   "net-pool",
			DeviceName: "dmaabbccdd",
			CDIEdits: &cdispec.ContainerEdits{
				NetDevices: []*cdispec.LinuxNetDevice{{
					Name:              "eth1",
					HostInterfaceName: "dmaabbccdd",
				}},
			},
			Allocation: &handler.AllocationInfo{
				Type:     handler.DeviceTypeNetdev,
				Kind:     "dummy",
				ClaimUID: "aabbccdd-0000-0000-0000-000000000000",
				Metadata: map[string]string{
					"createdInterface": "dmaabbccdd",
				},
			},
		},
	}

	h := NewRoCEHandler(rdma, netdev)

	req := &handler.PrepareRequest{
		ClaimUID: "aabbccdd-0000-0000-0000-000000000000",
		Config: &handler.DeviceConfig{
			Type: handler.DeviceTypeCombo,
			Combo: &handler.ComboConfig{
				RDMA:   handler.RDMAConfig{PreferDevice: "uverbs0"},
				Netdev: handler.NetdevConfig{Kind: "dummy", InterfaceName: "eth1"},
			},
		},
	}

	result, err := h.Prepare(context.Background(), req)
	if err != nil {
		t.Fatalf("Prepare failed: %v", err)
	}

	if !rdma.prepareCalled || !netdev.prepareCalled {
		t.Error("both sub-handlers should have been called")
	}

	// Pool should come from RDMA handler
	if result.PoolName != "rdma-pool" {
		t.Errorf("PoolName = %s, want rdma-pool", result.PoolName)
	}
	if result.DeviceName != "uverbs0" {
		t.Errorf("DeviceName = %s, want uverbs0", result.DeviceName)
	}

	// CDI edits should be merged
	if len(result.CDIEdits.DeviceNodes) != 1 {
		t.Errorf("expected 1 DeviceNode, got %d", len(result.CDIEdits.DeviceNodes))
	}
	if len(result.CDIEdits.NetDevices) != 1 {
		t.Errorf("expected 1 NetDevice, got %d", len(result.CDIEdits.NetDevices))
	}

	// Allocation metadata should have both
	if result.Allocation.Metadata["rdma_device"] != "uverbs0" {
		t.Errorf("rdma_device = %s, want uverbs0", result.Allocation.Metadata["rdma_device"])
	}
	if result.Allocation.Metadata["net_interface"] != "dmaabbccdd" {
		t.Errorf("net_interface = %s, want dmaabbccdd", result.Allocation.Metadata["net_interface"])
	}
	if result.Allocation.Kind != "roce" {
		t.Errorf("Allocation.Kind = %s, want roce", result.Allocation.Kind)
	}
}

func TestRoCEHandler_PrepareRdmaFails(t *testing.T) {
	rdma := &fakeHandler{
		deviceType: handler.DeviceTypeRDMA,
		kinds:      []string{"uverbs"},
		prepareErr: fmt.Errorf("rdma device not found"),
	}
	netdev := &fakeHandler{
		deviceType: handler.DeviceTypeNetdev,
		kinds:      []string{"dummy"},
	}

	h := NewRoCEHandler(rdma, netdev)
	_, err := h.Prepare(context.Background(), &handler.PrepareRequest{
		ClaimUID: "fail0000-0000-0000-0000-000000000000",
		Config: &handler.DeviceConfig{
			Type: handler.DeviceTypeCombo,
			Combo: &handler.ComboConfig{
				RDMA:   handler.RDMAConfig{},
				Netdev: handler.NetdevConfig{Kind: "dummy"},
			},
		},
	})
	if err == nil {
		t.Error("expected error when RDMA prepare fails")
	}
	if !rdma.prepareCalled {
		t.Error("RDMA handler should have been called")
	}
	if netdev.prepareCalled {
		t.Error("netdev handler should NOT have been called after RDMA failure")
	}
}

func TestRoCEHandler_PrepareNetdevFailsRollsBackRdma(t *testing.T) {
	rdma := &fakeHandler{
		deviceType: handler.DeviceTypeRDMA,
		kinds:      []string{"uverbs"},
		prepareResult: &handler.PrepareResult{
			DeviceName: "uverbs0",
			CDIEdits:   &cdispec.ContainerEdits{},
			Allocation: &handler.AllocationInfo{
				Type:     handler.DeviceTypeRDMA,
				Kind:     "uverbs",
				ClaimUID: "rollback0-0000-0000-0000-000000000000",
				Metadata: map[string]string{},
			},
		},
	}

	netdev := &fakeHandler{
		deviceType: handler.DeviceTypeNetdev,
		kinds:      []string{"dummy"},
		prepareErr: fmt.Errorf("netdev creation failed"),
	}

	h := NewRoCEHandler(rdma, netdev)
	_, err := h.Prepare(context.Background(), &handler.PrepareRequest{
		ClaimUID: "rollback0-0000-0000-0000-000000000000",
		Config: &handler.DeviceConfig{
			Type: handler.DeviceTypeCombo,
			Combo: &handler.ComboConfig{
				RDMA:   handler.RDMAConfig{},
				Netdev: handler.NetdevConfig{Kind: "dummy"},
			},
		},
	})

	if err == nil {
		t.Fatal("expected error when netdev prepare fails")
	}
	if !rdma.prepareCalled {
		t.Error("RDMA handler should have been called")
	}
	if !netdev.prepareCalled {
		t.Error("netdev handler should have been called")
	}
	if !rdma.unprepareCalled {
		t.Error("RDMA handler should have been rolled back via Unprepare")
	}
}

func TestRoCEHandler_UnprepareSuccess(t *testing.T) {
	rdma := &fakeHandler{deviceType: handler.DeviceTypeRDMA, kinds: []string{"uverbs"}}
	netdev := &fakeHandler{deviceType: handler.DeviceTypeNetdev, kinds: []string{"dummy"}}

	h := NewRoCEHandler(rdma, netdev)

	err := h.Unprepare(context.Background(), &handler.UnprepareRequest{
		ClaimUID: "cleanup0-0000-0000-0000-000000000000",
		Allocation: &handler.AllocationInfo{
			Type: handler.DeviceTypeCombo,
			Kind: "roce",
			Metadata: map[string]string{
				"rdma_uverbs_device": "uverbs0",
				"rdma_ibdev":         "mlx5_0",
				"net_created":        "dmaabbccdd",
				"net_host_end":       "",
			},
		},
	})
	if err != nil {
		t.Fatalf("Unprepare failed: %v", err)
	}
	if !rdma.unprepareCalled || !netdev.unprepareCalled {
		t.Error("both sub-handlers should have been called for unprepare")
	}
}

func TestRoCEHandler_UnpreparePartialFailure(t *testing.T) {
	rdma := &fakeHandler{
		deviceType:   handler.DeviceTypeRDMA,
		kinds:        []string{"uverbs"},
		unprepareErr: fmt.Errorf("rdma cleanup failed"),
	}
	netdev := &fakeHandler{deviceType: handler.DeviceTypeNetdev, kinds: []string{"dummy"}}

	h := NewRoCEHandler(rdma, netdev)

	err := h.Unprepare(context.Background(), &handler.UnprepareRequest{
		ClaimUID: "partfail-0000-0000-0000-000000000000",
		Allocation: &handler.AllocationInfo{
			Type:     handler.DeviceTypeCombo,
			Kind:     "roce",
			Metadata: map[string]string{},
		},
	})
	if err == nil {
		t.Error("expected error when RDMA unprepare fails")
	}
	// Both should still be called even if one fails
	if !netdev.unprepareCalled {
		t.Error("netdev unprepare should still be called")
	}
	if !rdma.unprepareCalled {
		t.Error("rdma unprepare should be called")
	}
}

func TestMergeCDIEdits(t *testing.T) {
	a := &cdispec.ContainerEdits{
		DeviceNodes: []*cdispec.DeviceNode{{Path: "/dev/a"}},
		Env:         []string{"A=1"},
	}
	b := &cdispec.ContainerEdits{
		NetDevices: []*cdispec.LinuxNetDevice{{Name: "eth0"}},
		Env:        []string{"B=2"},
	}
	merged := mergeCDIEdits(a, b)

	if len(merged.DeviceNodes) != 1 {
		t.Errorf("DeviceNodes: got %d, want 1", len(merged.DeviceNodes))
	}
	if len(merged.NetDevices) != 1 {
		t.Errorf("NetDevices: got %d, want 1", len(merged.NetDevices))
	}
	if len(merged.Env) != 2 {
		t.Errorf("Env: got %d, want 2", len(merged.Env))
	}
}
