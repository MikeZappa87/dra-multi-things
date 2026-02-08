package netdev

import (
	"context"
	"os"
	"testing"

	"github.com/vishvananda/netlink"

	"github.com/example/dra-poc/pkg/handler"
)

// skipUnlessRoot skips a test if not running as root (netlink requires CAP_NET_ADMIN).
func skipUnlessRoot(t *testing.T) {
	t.Helper()
	if os.Getuid() != 0 {
		t.Skip("skipping: requires root / CAP_NET_ADMIN")
	}
}

// cleanupLink deletes a link by name, ignoring errors.
func cleanupLink(name string) {
	if link, err := netlink.LinkByName(name); err == nil {
		netlink.LinkDel(link)
	}
}

// ─── Validate tests (no root needed) ─────────────────────────────────────────

func TestDummyHandler_Validate(t *testing.T) {
	h := &DummyHandler{}
	if err := h.Validate(context.Background(), &handler.DeviceConfig{}); err == nil {
		t.Error("expected error when Netdev is nil")
	}
	if err := h.Validate(context.Background(), &handler.DeviceConfig{
		Netdev: &handler.NetdevConfig{Kind: "dummy"},
	}); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestMacvlanHandler_Validate(t *testing.T) {
	h := &MacvlanHandler{}
	if err := h.Validate(context.Background(), &handler.DeviceConfig{}); err == nil {
		t.Error("expected error when Netdev is nil")
	}
	if err := h.Validate(context.Background(), &handler.DeviceConfig{
		Netdev: &handler.NetdevConfig{Kind: "macvlan"},
	}); err == nil {
		t.Error("expected error when parent is empty")
	}
	if err := h.Validate(context.Background(), &handler.DeviceConfig{
		Netdev: &handler.NetdevConfig{Kind: "macvlan", Parent: "eth0"},
	}); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestIpvlanHandler_Validate(t *testing.T) {
	h := &IpvlanHandler{}
	if err := h.Validate(context.Background(), &handler.DeviceConfig{}); err == nil {
		t.Error("expected error when Netdev is nil")
	}
	if err := h.Validate(context.Background(), &handler.DeviceConfig{
		Netdev: &handler.NetdevConfig{Kind: "ipvlan"},
	}); err == nil {
		t.Error("expected error when parent is empty")
	}
	if err := h.Validate(context.Background(), &handler.DeviceConfig{
		Netdev: &handler.NetdevConfig{Kind: "ipvlan", Parent: "eth0"},
	}); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestVethHandler_Validate(t *testing.T) {
	h := &VethHandler{}
	if err := h.Validate(context.Background(), &handler.DeviceConfig{}); err == nil {
		t.Error("expected error when Netdev is nil")
	}
	if err := h.Validate(context.Background(), &handler.DeviceConfig{
		Netdev: &handler.NetdevConfig{Kind: "veth"},
	}); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestSriovVfHandler_Validate(t *testing.T) {
	h := &SriovVfHandler{}
	if err := h.Validate(context.Background(), &handler.DeviceConfig{}); err == nil {
		t.Error("expected error when Netdev is nil")
	}
	if err := h.Validate(context.Background(), &handler.DeviceConfig{
		Netdev: &handler.NetdevConfig{Kind: "sriov-vf"},
	}); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestHostDeviceHandler_Validate(t *testing.T) {
	h := &HostDeviceHandler{}

	if err := h.Validate(context.Background(), &handler.DeviceConfig{}); err == nil {
		t.Error("expected error when Netdev is nil")
	}
	if err := h.Validate(context.Background(), &handler.DeviceConfig{
		Netdev: &handler.NetdevConfig{Kind: "host-device"},
	}); err == nil {
		t.Error("expected error when HostDevice is empty")
	}
	if err := h.Validate(context.Background(), &handler.DeviceConfig{
		Netdev: &handler.NetdevConfig{Kind: "host-device", HostDevice: "mynet0"},
	}); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

// ─── Type / Kind metadata ────────────────────────────────────────────────────

func TestHandlerMetadata(t *testing.T) {
	tests := []struct {
		name      string
		handler   handler.DeviceHandler
		wantType  handler.DeviceType
		wantKinds []string
	}{
		{"dummy", &DummyHandler{}, handler.DeviceTypeNetdev, []string{"dummy"}},
		{"macvlan", &MacvlanHandler{}, handler.DeviceTypeNetdev, []string{"macvlan"}},
		{"ipvlan", &IpvlanHandler{}, handler.DeviceTypeNetdev, []string{"ipvlan"}},
		{"veth", &VethHandler{}, handler.DeviceTypeNetdev, []string{"veth"}},
		{"sriov-vf", &SriovVfHandler{}, handler.DeviceTypeNetdev, []string{"sriov-vf"}},
		{"host-device", &HostDeviceHandler{}, handler.DeviceTypeNetdev, []string{"host-device"}},
		{"ipoib", &IpoibHandler{}, handler.DeviceTypeNetdev, []string{"ipoib"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.handler.Type(); got != tt.wantType {
				t.Errorf("Type() = %s, want %s", got, tt.wantType)
			}
			kinds := tt.handler.Kinds()
			if len(kinds) != len(tt.wantKinds) {
				t.Fatalf("Kinds() len = %d, want %d", len(kinds), len(tt.wantKinds))
			}
			for i, k := range kinds {
				if k != tt.wantKinds[i] {
					t.Errorf("Kinds()[%d] = %s, want %s", i, k, tt.wantKinds[i])
				}
			}
		})
	}
}

// ─── Prepare / Unprepare tests (require root) ────────────────────────────────

func TestDummyHandler_PrepareAndUnprepare(t *testing.T) {
	skipUnlessRoot(t)
	h := &DummyHandler{}
	ctx := context.Background()

	req := &handler.PrepareRequest{
		ClaimUID: "aabbccdd-1111-2222-3333-444444444444",
		Config: &handler.DeviceConfig{
			Type:   handler.DeviceTypeNetdev,
			Netdev: &handler.NetdevConfig{Kind: "dummy", InterfaceName: "test0"},
		},
	}

	result, err := h.Prepare(ctx, req)
	if err != nil {
		t.Fatalf("Prepare failed: %v", err)
	}
	defer cleanupLink(result.DeviceName)

	if result.DeviceName != "dmaabbccdd" {
		t.Errorf("DeviceName = %s, want dmaabbccdd", result.DeviceName)
	}
	if result.Allocation.Kind != "dummy" {
		t.Errorf("Allocation.Kind = %s, want dummy", result.Allocation.Kind)
	}
	if len(result.CDIEdits.NetDevices) != 1 {
		t.Fatal("expected 1 NetDevice in CDI edits")
	}
	if result.CDIEdits.NetDevices[0].Name != "test0" {
		t.Errorf("container name = %s, want test0", result.CDIEdits.NetDevices[0].Name)
	}

	// Verify interface exists on host
	if _, err := netlink.LinkByName(result.DeviceName); err != nil {
		t.Fatalf("dummy interface not found: %v", err)
	}

	// Unprepare
	err = h.Unprepare(ctx, &handler.UnprepareRequest{
		ClaimUID:   req.ClaimUID,
		Allocation: result.Allocation,
	})
	if err != nil {
		t.Fatalf("Unprepare failed: %v", err)
	}

	// Verify interface is gone
	if _, err := netlink.LinkByName(result.DeviceName); err == nil {
		t.Error("dummy interface should have been deleted")
	}
}

func TestDummyHandler_DefaultContainerName(t *testing.T) {
	skipUnlessRoot(t)
	h := &DummyHandler{}
	ctx := context.Background()

	req := &handler.PrepareRequest{
		ClaimUID: "bbccddee-0000-0000-0000-000000000000",
		Config: &handler.DeviceConfig{
			Type:   handler.DeviceTypeNetdev,
			Netdev: &handler.NetdevConfig{Kind: "dummy"}, // no InterfaceName
		},
	}

	result, err := h.Prepare(ctx, req)
	if err != nil {
		t.Fatalf("Prepare failed: %v", err)
	}
	defer cleanupLink(result.DeviceName)

	if result.CDIEdits.NetDevices[0].Name != "eth1" {
		t.Errorf("default container name = %s, want eth1", result.CDIEdits.NetDevices[0].Name)
	}

	_ = h.Unprepare(ctx, &handler.UnprepareRequest{ClaimUID: req.ClaimUID, Allocation: result.Allocation})
}

func TestDummyHandler_PrepareNilConfig(t *testing.T) {
	h := &DummyHandler{}
	_, err := h.Prepare(context.Background(), &handler.PrepareRequest{
		ClaimUID: "12345678-0000-0000-0000-000000000000",
		Config:   &handler.DeviceConfig{Type: handler.DeviceTypeNetdev},
	})
	if err == nil {
		t.Error("Prepare should fail with nil netdev config")
	}
}

func TestVethHandler_PrepareAndUnprepare(t *testing.T) {
	skipUnlessRoot(t)
	h := &VethHandler{}
	ctx := context.Background()

	req := &handler.PrepareRequest{
		ClaimUID: "vethtest-1111-2222-3333-444444444444",
		Config: &handler.DeviceConfig{
			Type:   handler.DeviceTypeNetdev,
			Netdev: &handler.NetdevConfig{Kind: "veth", InterfaceName: "data0"},
		},
	}

	result, err := h.Prepare(ctx, req)
	if err != nil {
		t.Fatalf("Prepare failed: %v", err)
	}
	hostEnd := result.Allocation.Metadata["hostEnd"]
	containerEnd := result.Allocation.Metadata["containerEnd"]
	defer cleanupLink(hostEnd)

	// Both ends should exist
	if _, err := netlink.LinkByName(hostEnd); err != nil {
		t.Errorf("host veth end not found: %v", err)
	}
	if _, err := netlink.LinkByName(containerEnd); err != nil {
		t.Errorf("container veth end not found: %v", err)
	}
	if result.CDIEdits.NetDevices[0].Name != "data0" {
		t.Errorf("container name = %s, want data0", result.CDIEdits.NetDevices[0].Name)
	}

	// Unprepare - deleting one end removes both
	err = h.Unprepare(ctx, &handler.UnprepareRequest{
		ClaimUID:   req.ClaimUID,
		Allocation: result.Allocation,
	})
	if err != nil {
		t.Fatalf("Unprepare failed: %v", err)
	}
	if _, err := netlink.LinkByName(hostEnd); err == nil {
		t.Error("host veth end should be gone")
	}
	if _, err := netlink.LinkByName(containerEnd); err == nil {
		t.Error("container veth end should be gone")
	}
}

func TestMacvlanHandler_PrepareAndUnprepare(t *testing.T) {
	skipUnlessRoot(t)

	// Create a parent dummy for the macvlan
	parent := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "testparent0"}}
	if err := netlink.LinkAdd(parent); err != nil {
		t.Fatalf("failed to create parent: %v", err)
	}
	if err := netlink.LinkSetUp(parent); err != nil {
		netlink.LinkDel(parent)
		t.Fatalf("failed to up parent: %v", err)
	}
	defer cleanupLink("testparent0")

	h := &MacvlanHandler{}
	ctx := context.Background()

	req := &handler.PrepareRequest{
		ClaimUID: "mvtest00-1111-2222-3333-444444444444",
		Config: &handler.DeviceConfig{
			Type: handler.DeviceTypeNetdev,
			Netdev: &handler.NetdevConfig{
				Kind:          "macvlan",
				Parent:        "testparent0",
				Mode:          "bridge",
				InterfaceName: "mvtest",
			},
		},
	}

	result, err := h.Prepare(ctx, req)
	if err != nil {
		t.Fatalf("Prepare failed: %v", err)
	}
	defer cleanupLink(result.DeviceName)

	if result.Allocation.Kind != "macvlan" {
		t.Errorf("Kind = %s, want macvlan", result.Allocation.Kind)
	}
	if _, err := netlink.LinkByName(result.DeviceName); err != nil {
		t.Errorf("macvlan interface not found: %v", err)
	}
	if result.CDIEdits.NetDevices[0].Name != "mvtest" {
		t.Errorf("container name = %s, want mvtest", result.CDIEdits.NetDevices[0].Name)
	}

	// Unprepare
	err = h.Unprepare(ctx, &handler.UnprepareRequest{
		ClaimUID:   req.ClaimUID,
		Allocation: result.Allocation,
	})
	if err != nil {
		t.Fatalf("Unprepare failed: %v", err)
	}
	if _, err := netlink.LinkByName(result.DeviceName); err == nil {
		t.Error("macvlan interface should be gone")
	}
}

func TestMacvlanHandler_UnsupportedMode(t *testing.T) {
	skipUnlessRoot(t)

	parent := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "testparent1"}}
	if err := netlink.LinkAdd(parent); err != nil {
		t.Fatalf("failed to create parent: %v", err)
	}
	if err := netlink.LinkSetUp(parent); err != nil {
		netlink.LinkDel(parent)
		t.Fatalf("failed to up parent: %v", err)
	}
	defer cleanupLink("testparent1")

	h := &MacvlanHandler{}
	_, err := h.Prepare(context.Background(), &handler.PrepareRequest{
		ClaimUID: "mvbad000-0000-0000-0000-000000000000",
		Config: &handler.DeviceConfig{
			Type:   handler.DeviceTypeNetdev,
			Netdev: &handler.NetdevConfig{Kind: "macvlan", Parent: "testparent1", Mode: "badmode"},
		},
	})
	if err == nil {
		t.Error("should fail with unsupported mode")
	}
}

func TestMacvlanHandler_MissingParent(t *testing.T) {
	h := &MacvlanHandler{}
	_, err := h.Prepare(context.Background(), &handler.PrepareRequest{
		ClaimUID: "mvnop000-0000-0000-0000-000000000000",
		Config: &handler.DeviceConfig{
			Type:   handler.DeviceTypeNetdev,
			Netdev: &handler.NetdevConfig{Kind: "macvlan", Parent: "does-not-exist-xyz"},
		},
	})
	if err == nil {
		t.Error("should fail with missing parent")
	}
}

func TestIpvlanHandler_PrepareAndUnprepare(t *testing.T) {
	skipUnlessRoot(t)

	parent := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "testparent2"}}
	if err := netlink.LinkAdd(parent); err != nil {
		t.Fatalf("failed to create parent: %v", err)
	}
	if err := netlink.LinkSetUp(parent); err != nil {
		netlink.LinkDel(parent)
		t.Fatalf("failed to up parent: %v", err)
	}
	defer cleanupLink("testparent2")

	h := &IpvlanHandler{}
	ctx := context.Background()

	req := &handler.PrepareRequest{
		ClaimUID: "ivtest00-1111-2222-3333-444444444444",
		Config: &handler.DeviceConfig{
			Type: handler.DeviceTypeNetdev,
			Netdev: &handler.NetdevConfig{
				Kind:          "ipvlan",
				Parent:        "testparent2",
				Mode:          "l2",
				InterfaceName: "ipv0",
			},
		},
	}

	result, err := h.Prepare(ctx, req)
	if err != nil {
		t.Fatalf("Prepare failed: %v", err)
	}
	defer cleanupLink(result.DeviceName)

	if result.Allocation.Kind != "ipvlan" {
		t.Errorf("Kind = %s, want ipvlan", result.Allocation.Kind)
	}
	if _, err := netlink.LinkByName(result.DeviceName); err != nil {
		t.Errorf("ipvlan interface not found: %v", err)
	}

	err = h.Unprepare(ctx, &handler.UnprepareRequest{
		ClaimUID:   req.ClaimUID,
		Allocation: result.Allocation,
	})
	if err != nil {
		t.Fatalf("Unprepare failed: %v", err)
	}
	if _, err := netlink.LinkByName(result.DeviceName); err == nil {
		t.Error("ipvlan interface should be gone")
	}
}

func TestIpvlanHandler_UnsupportedMode(t *testing.T) {
	skipUnlessRoot(t)

	parent := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "testparent3"}}
	if err := netlink.LinkAdd(parent); err != nil {
		t.Fatalf("failed to create parent: %v", err)
	}
	if err := netlink.LinkSetUp(parent); err != nil {
		netlink.LinkDel(parent)
		t.Fatalf("failed to up parent: %v", err)
	}
	defer cleanupLink("testparent3")

	h := &IpvlanHandler{}
	_, err := h.Prepare(context.Background(), &handler.PrepareRequest{
		ClaimUID: "ivbad000-0000-0000-0000-000000000000",
		Config: &handler.DeviceConfig{
			Type:   handler.DeviceTypeNetdev,
			Netdev: &handler.NetdevConfig{Kind: "ipvlan", Parent: "testparent3", Mode: "l99"},
		},
	})
	if err == nil {
		t.Error("should fail with unsupported mode")
	}
}

func TestHostDeviceHandler_PrepareAndUnprepare(t *testing.T) {
	skipUnlessRoot(t)

	// Create a dummy interface to simulate an externally-created device
	ext := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "extnet0"}}
	if err := netlink.LinkAdd(ext); err != nil {
		t.Fatalf("failed to create external interface: %v", err)
	}
	if err := netlink.LinkSetUp(ext); err != nil {
		netlink.LinkDel(ext)
		t.Fatalf("failed to up external interface: %v", err)
	}
	defer cleanupLink("extnet0")

	h := &HostDeviceHandler{}
	ctx := context.Background()

	req := &handler.PrepareRequest{
		ClaimUID: "hostdev0-1111-2222-3333-444444444444",
		Config: &handler.DeviceConfig{
			Type: handler.DeviceTypeNetdev,
			Netdev: &handler.NetdevConfig{
				Kind:          "host-device",
				HostDevice:    "extnet0",
				InterfaceName: "inside0",
			},
		},
	}

	result, err := h.Prepare(ctx, req)
	if err != nil {
		t.Fatalf("Prepare failed: %v", err)
	}

	// Verify result
	if result.DeviceName != "extnet0" {
		t.Errorf("DeviceName = %s, want extnet0", result.DeviceName)
	}
	if result.CDIEdits.NetDevices[0].HostInterfaceName != "extnet0" {
		t.Errorf("HostInterfaceName = %s, want extnet0", result.CDIEdits.NetDevices[0].HostInterfaceName)
	}
	if result.CDIEdits.NetDevices[0].Name != "inside0" {
		t.Errorf("container name = %s, want inside0", result.CDIEdits.NetDevices[0].Name)
	}
	if result.Allocation.Kind != "host-device" {
		t.Errorf("Allocation.Kind = %s, want host-device", result.Allocation.Kind)
	}

	// Unprepare should NOT delete the interface (externally owned)
	err = h.Unprepare(ctx, &handler.UnprepareRequest{
		ClaimUID:   req.ClaimUID,
		Allocation: result.Allocation,
	})
	if err != nil {
		t.Fatalf("Unprepare failed: %v", err)
	}

	// Interface should STILL exist after unprepare
	if _, err := netlink.LinkByName("extnet0"); err != nil {
		t.Error("host-device interface should still exist after unprepare")
	}
}

func TestHostDeviceHandler_DefaultContainerName(t *testing.T) {
	skipUnlessRoot(t)

	ext := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "extnet1"}}
	if err := netlink.LinkAdd(ext); err != nil {
		t.Fatalf("failed to create external interface: %v", err)
	}
	defer cleanupLink("extnet1")
	netlink.LinkSetUp(ext)

	h := &HostDeviceHandler{}
	result, err := h.Prepare(context.Background(), &handler.PrepareRequest{
		ClaimUID: "hostdev1-0000-0000-0000-000000000000",
		Config: &handler.DeviceConfig{
			Type: handler.DeviceTypeNetdev,
			Netdev: &handler.NetdevConfig{
				Kind:       "host-device",
				HostDevice: "extnet1",
				// no InterfaceName set
			},
		},
	})
	if err != nil {
		t.Fatalf("Prepare failed: %v", err)
	}

	// Default: container name should be same as host name
	if result.CDIEdits.NetDevices[0].Name != "extnet1" {
		t.Errorf("default container name = %s, want extnet1", result.CDIEdits.NetDevices[0].Name)
	}
}

func TestHostDeviceHandler_MissingInterface(t *testing.T) {
	h := &HostDeviceHandler{}
	_, err := h.Prepare(context.Background(), &handler.PrepareRequest{
		ClaimUID: "hostdevX-0000-0000-0000-000000000000",
		Config: &handler.DeviceConfig{
			Type: handler.DeviceTypeNetdev,
			Netdev: &handler.NetdevConfig{
				Kind:       "host-device",
				HostDevice: "totally-does-not-exist",
			},
		},
	})
	if err == nil {
		t.Error("Prepare should fail when host interface doesn't exist")
	}
}

// ─── IPoIB tests ─────────────────────────────────────────────────────────────

func TestIpoibHandler_Validate(t *testing.T) {
	h := &IpoibHandler{}
	if err := h.Validate(context.Background(), &handler.DeviceConfig{}); err == nil {
		t.Error("expected error when Netdev is nil")
	}
	if err := h.Validate(context.Background(), &handler.DeviceConfig{
		Netdev: &handler.NetdevConfig{Kind: "ipoib"},
	}); err == nil {
		t.Error("expected error when parent is empty")
	}
	if err := h.Validate(context.Background(), &handler.DeviceConfig{
		Netdev: &handler.NetdevConfig{Kind: "ipoib", Parent: "ib0"},
	}); err == nil {
		t.Error("expected error when pkey is zero")
	}
	if err := h.Validate(context.Background(), &handler.DeviceConfig{
		Netdev: &handler.NetdevConfig{Kind: "ipoib", Parent: "ib0", Pkey: 0x8001},
	}); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestIpoibHandler_PrepareNilConfig(t *testing.T) {
	h := &IpoibHandler{}
	_, err := h.Prepare(context.Background(), &handler.PrepareRequest{
		ClaimUID: "ibnilcfg-0000-0000-0000-000000000000",
		Config:   &handler.DeviceConfig{Type: handler.DeviceTypeNetdev},
	})
	if err == nil {
		t.Error("Prepare should fail with nil netdev config")
	}
}

func TestIpoibHandler_MissingParent(t *testing.T) {
	h := &IpoibHandler{}
	_, err := h.Prepare(context.Background(), &handler.PrepareRequest{
		ClaimUID: "ibnop000-0000-0000-0000-000000000000",
		Config: &handler.DeviceConfig{
			Type: handler.DeviceTypeNetdev,
			Netdev: &handler.NetdevConfig{
				Kind:   "ipoib",
				Parent: "does-not-exist-ib",
				Pkey:   0x8001,
			},
		},
	})
	if err == nil {
		t.Error("should fail with missing parent")
	}
}

func TestIpoibHandler_UnsupportedMode(t *testing.T) {
	h := &IpoibHandler{}
	_, err := h.Prepare(context.Background(), &handler.PrepareRequest{
		ClaimUID: "ibbad000-0000-0000-0000-000000000000",
		Config: &handler.DeviceConfig{
			Type: handler.DeviceTypeNetdev,
			Netdev: &handler.NetdevConfig{
				Kind:   "ipoib",
				Parent: "ib0",
				Pkey:   0x8001,
				Mode:   "badmode",
			},
		},
	})
	if err == nil {
		t.Error("should fail with unsupported mode")
	}
}

func TestIpoibHandler_UnprepareAlreadyGone(t *testing.T) {
	h := &IpoibHandler{}
	err := h.Unprepare(context.Background(), &handler.UnprepareRequest{
		ClaimUID: "gone0000-0000-0000-0000-000000000000",
		Allocation: &handler.AllocationInfo{
			Metadata: map[string]string{
				"createdInterface": "does-not-exist-xyz",
			},
		},
	})
	if err != nil {
		t.Errorf("Unprepare should be idempotent, got error: %v", err)
	}
}

// Test Unprepare is a no-op when the interface is already gone (idempotent)
func TestDummyHandler_UnprepareAlreadyGone(t *testing.T) {
	h := &DummyHandler{}
	err := h.Unprepare(context.Background(), &handler.UnprepareRequest{
		ClaimUID: "gone0000-0000-0000-0000-000000000000",
		Allocation: &handler.AllocationInfo{
			Metadata: map[string]string{
				"createdInterface": "does-not-exist-xyz",
			},
		},
	})
	if err != nil {
		t.Errorf("Unprepare should be idempotent, got error: %v", err)
	}
}

func TestVethHandler_UnprepareAlreadyGone(t *testing.T) {
	h := &VethHandler{}
	err := h.Unprepare(context.Background(), &handler.UnprepareRequest{
		ClaimUID: "gone0000-0000-0000-0000-000000000000",
		Allocation: &handler.AllocationInfo{
			Metadata: map[string]string{
				"hostEnd": "does-not-exist-xyz",
			},
		},
	})
	if err != nil {
		t.Errorf("Unprepare should be idempotent, got error: %v", err)
	}
}

func TestMacvlanHandler_UnprepareAlreadyGone(t *testing.T) {
	h := &MacvlanHandler{}
	err := h.Unprepare(context.Background(), &handler.UnprepareRequest{
		ClaimUID: "gone0000-0000-0000-0000-000000000000",
		Allocation: &handler.AllocationInfo{
			Metadata: map[string]string{
				"createdInterface": "does-not-exist-xyz",
			},
		},
	})
	if err != nil {
		t.Errorf("Unprepare should be idempotent, got error: %v", err)
	}
}

func TestIpvlanHandler_UnprepareAlreadyGone(t *testing.T) {
	h := &IpvlanHandler{}
	err := h.Unprepare(context.Background(), &handler.UnprepareRequest{
		ClaimUID: "gone0000-0000-0000-0000-000000000000",
		Allocation: &handler.AllocationInfo{
			Metadata: map[string]string{
				"createdInterface": "does-not-exist-xyz",
			},
		},
	})
	if err != nil {
		t.Errorf("Unprepare should be idempotent, got error: %v", err)
	}
}

func TestUnprepareEmptyMetadata(t *testing.T) {
	handlers := []handler.DeviceHandler{
		&DummyHandler{},
		&MacvlanHandler{},
		&IpvlanHandler{},
		&VethHandler{},
		&HostDeviceHandler{},
		&IpoibHandler{},
	}
	for _, h := range handlers {
		kind := h.Kinds()[0]
		t.Run(kind, func(t *testing.T) {
			err := h.Unprepare(context.Background(), &handler.UnprepareRequest{
				ClaimUID: "empty000-0000-0000-0000-000000000000",
				Allocation: &handler.AllocationInfo{
					Metadata: map[string]string{},
				},
			})
			if err != nil {
				t.Errorf("Unprepare with empty metadata should not fail, got: %v", err)
			}
		})
	}
}
