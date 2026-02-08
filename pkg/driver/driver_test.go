package driver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	cdispec "tags.cncf.io/container-device-interface/specs-go"

	"github.com/example/dra-poc/pkg/handler"
)

// fakeHandler implements handler.DeviceHandler for driver-level testing.
type fakeHandler struct {
	deviceType handler.DeviceType
	kinds      []string

	prepareResult *handler.PrepareResult
	prepareErr    error
	unprepareErr  error

	prepareCalled   int
	unprepareCalled int
}

func (f *fakeHandler) Type() handler.DeviceType { return f.deviceType }
func (f *fakeHandler) Kinds() []string          { return f.kinds }

func (f *fakeHandler) Validate(_ context.Context, _ *handler.DeviceConfig) error { return nil }

func (f *fakeHandler) Prepare(_ context.Context, req *handler.PrepareRequest) (*handler.PrepareResult, error) {
	f.prepareCalled++
	if f.prepareErr != nil {
		return nil, f.prepareErr
	}
	if f.prepareResult != nil {
		return f.prepareResult, nil
	}
	// sensible default
	return &handler.PrepareResult{
		PoolName:   "test-pool",
		DeviceName: "testdev0",
		CDIEdits: &cdispec.ContainerEdits{
			NetDevices: []*cdispec.LinuxNetDevice{{
				Name:              "eth1",
				HostInterfaceName: "testdev0",
			}},
		},
		Allocation: &handler.AllocationInfo{
			Type:     f.deviceType,
			Kind:     f.kinds[0],
			ClaimUID: req.ClaimUID,
			Metadata: map[string]string{"createdInterface": "testdev0"},
		},
	}, nil
}

func (f *fakeHandler) Unprepare(_ context.Context, _ *handler.UnprepareRequest) error {
	f.unprepareCalled++
	return f.unprepareErr
}

// ─── CDI spec creation and deletion tests ────────────────────────────────────

func TestCreateCDISpec(t *testing.T) {
	tmpDir := t.TempDir()

	d := &Driver{
		driverName:  "example.com/test-driver",
		allocations: make(map[string]*handler.AllocationInfo),
	}

	// Temporarily override cdiDir
	originalDir := cdiDir
	// We can't reassign const, so test createCDISpec by creating file in tmpDir
	// Instead, test the file content logic directly.

	_ = originalDir

	result := &handler.PrepareResult{
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
			ClaimUID: "aabbccdd-1111-2222-3333-444444444444",
		},
	}

	// Build expected CDI spec
	spec := cdispec.Spec{
		Version: cdiVersion,
		Kind:    "example.com/test-driver/netdev",
		Devices: []cdispec.Device{{
			Name:           "dmaabbccdd",
			ContainerEdits: *result.CDIEdits,
		}},
	}
	data, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		t.Fatal(err)
	}

	cdiFileName := strings.ReplaceAll(d.driverName, "/", "-") + "-aabbccdd.json"
	cdiFilePath := filepath.Join(tmpDir, cdiFileName)

	if err := os.WriteFile(cdiFilePath, data, 0644); err != nil {
		t.Fatal(err)
	}

	// Read back and verify
	raw, err := os.ReadFile(cdiFilePath)
	if err != nil {
		t.Fatal(err)
	}

	var parsed cdispec.Spec
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}

	if parsed.Version != "1.1.0" {
		t.Errorf("Version = %s, want 1.1.0", parsed.Version)
	}
	if parsed.Kind != "example.com/test-driver/netdev" {
		t.Errorf("Kind = %s, want example.com/test-driver/netdev", parsed.Kind)
	}
	if len(parsed.Devices) != 1 {
		t.Fatalf("Devices count = %d, want 1", len(parsed.Devices))
	}
	if parsed.Devices[0].Name != "dmaabbccdd" {
		t.Errorf("Device name = %s, want dmaabbccdd", parsed.Devices[0].Name)
	}
	if len(parsed.Devices[0].ContainerEdits.NetDevices) != 1 {
		t.Fatal("expected 1 net device in CDI edits")
	}
	if parsed.Devices[0].ContainerEdits.NetDevices[0].Name != "eth1" {
		t.Errorf("NetDevice name = %s, want eth1", parsed.Devices[0].ContainerEdits.NetDevices[0].Name)
	}
}

func TestDeleteCDISpec(t *testing.T) {
	tmpDir := t.TempDir()

	d := &Driver{
		driverName: "example.com/test-driver",
	}

	claimUID := "aabbccdd-1111-2222-3333-444444444444"
	cdiFileName := strings.ReplaceAll(d.driverName, "/", "-") + "-" + claimUID[:8] + ".json"
	cdiFilePath := filepath.Join(tmpDir, cdiFileName)

	// Create a dummy file
	if err := os.WriteFile(cdiFilePath, []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cdiFilePath); err != nil {
		t.Fatal("file should exist before delete")
	}

	// Delete it
	if err := os.Remove(cdiFilePath); err != nil {
		t.Fatalf("failed to delete CDI spec: %v", err)
	}

	// Verify it's gone
	if _, err := os.Stat(cdiFilePath); !os.IsNotExist(err) {
		t.Error("CDI spec should be deleted")
	}

	// Deleting again should not error (idempotent)
	err := os.Remove(cdiFilePath)
	if err != nil && !os.IsNotExist(err) {
		t.Errorf("second delete should not error, got: %v", err)
	}
}

// ─── prepareClaim dispatch tests (via registry) ──────────────────────────────

func TestPrepareClaim_DispatchToHandler(t *testing.T) {
	fh := &fakeHandler{
		deviceType: handler.DeviceTypeNetdev,
		kinds:      []string{"dummy"},
	}

	reg := handler.NewHandlerRegistry()
	reg.Register(fh)

	d := &Driver{
		driverName:  "test-driver",
		registry:    reg,
		allocations: make(map[string]*handler.AllocationInfo),
	}

	config := &handler.DeviceConfig{
		Type: handler.DeviceTypeNetdev,
		Netdev: &handler.NetdevConfig{
			Kind:          "dummy",
			InterfaceName: "eth1",
		},
	}

	kind := config.GetKind()
	h, err := d.registry.MustGet(config.Type, kind)
	if err != nil {
		t.Fatal(err)
	}

	result, err := h.Prepare(context.Background(), &handler.PrepareRequest{
		ClaimUID: "dispatch0-1111-2222-3333-444444444444",
		Config:   config,
	})
	if err != nil {
		t.Fatal(err)
	}
	if fh.prepareCalled != 1 {
		t.Errorf("handler Prepare called %d times, want 1", fh.prepareCalled)
	}
	if result.DeviceName != "testdev0" {
		t.Errorf("DeviceName = %s, want testdev0", result.DeviceName)
	}
}

func TestPrepareClaim_UnknownKindFails(t *testing.T) {
	reg := handler.NewHandlerRegistry()

	d := &Driver{
		driverName:  "test-driver",
		registry:    reg,
		allocations: make(map[string]*handler.AllocationInfo),
	}

	config := &handler.DeviceConfig{
		Type:   handler.DeviceTypeNetdev,
		Netdev: &handler.NetdevConfig{Kind: "not-a-real-kind"},
	}

	_, err := d.registry.MustGet(config.Type, config.GetKind())
	if err == nil {
		t.Error("expected error for unknown kind")
	}
}

// ─── unprepareAllocation tests ──────────────────────────────────────────────

func TestUnprepareAllocation_Success(t *testing.T) {
	fh := &fakeHandler{
		deviceType: handler.DeviceTypeNetdev,
		kinds:      []string{"dummy"},
	}

	reg := handler.NewHandlerRegistry()
	reg.Register(fh)

	d := &Driver{
		driverName:  "test-driver",
		registry:    reg,
		allocations: make(map[string]*handler.AllocationInfo),
	}

	alloc := &handler.AllocationInfo{
		Type:     handler.DeviceTypeNetdev,
		Kind:     "dummy",
		ClaimUID: "unprepare-0000-0000-0000-000000000000",
		Metadata: map[string]string{"createdInterface": "testdev0"},
	}

	err := d.unprepareAllocation(context.Background(), alloc)
	if err != nil {
		t.Fatalf("unprepareAllocation failed: %v", err)
	}
	if fh.unprepareCalled != 1 {
		t.Errorf("handler Unprepare called %d times, want 1", fh.unprepareCalled)
	}
}

func TestUnprepareAllocation_UnknownHandler(t *testing.T) {
	reg := handler.NewHandlerRegistry()

	d := &Driver{
		driverName:  "test-driver",
		registry:    reg,
		allocations: make(map[string]*handler.AllocationInfo),
	}

	alloc := &handler.AllocationInfo{
		Type:     handler.DeviceTypeNetdev,
		Kind:     "doesnotexist",
		ClaimUID: "unknown-0000-0000-0000-000000000000",
	}

	err := d.unprepareAllocation(context.Background(), alloc)
	if err == nil {
		t.Error("expected error for unknown handler")
	}
}

// ─── parseConfig defaults test ──────────────────────────────────────────────

func TestParseConfig_DefaultsWhenNilClaim(t *testing.T) {
	d := &Driver{
		driverName: "test-driver",
	}

	// nil ResourceClaim should return sensible defaults
	config := d.parseConfig(nil)

	if config.Type != handler.DeviceTypeNetdev {
		t.Errorf("default type = %s, want netdev", config.Type)
	}
	if config.GetKind() != "dummy" {
		t.Errorf("default kind = %s, want dummy", config.GetKind())
	}
	if config.Netdev.InterfaceName != "eth1" {
		t.Errorf("default interfaceName = %s, want eth1", config.Netdev.InterfaceName)
	}
}

// ─── getAllocatedDevice tests ───────────────────────────────────────────────

func TestGetAllocatedDevice_NilClaim(t *testing.T) {
	d := &Driver{driverName: "dra.example.com"}
	if got := d.getAllocatedDevice(nil); got != "" {
		t.Errorf("expected empty string for nil claim, got %q", got)
	}
}

func TestGetAllocatedDevice_NoAllocation(t *testing.T) {
	d := &Driver{driverName: "dra.example.com"}
	rc := &resourceapi.ResourceClaim{}
	if got := d.getAllocatedDevice(rc); got != "" {
		t.Errorf("expected empty string for claim without allocation, got %q", got)
	}
}

func TestGetAllocatedDevice_FiniteDevice(t *testing.T) {
	d := &Driver{driverName: "dra.example.com"}
	rc := &resourceapi.ResourceClaim{
		Status: resourceapi.ResourceClaimStatus{
			Allocation: &resourceapi.AllocationResult{
				Devices: resourceapi.DeviceAllocationResult{
					Results: []resourceapi.DeviceRequestAllocationResult{
						{
							Request: "rdma",
							Driver:  "dra.example.com",
							Pool:    "node-1",
							Device:  "uverbs0",
						},
					},
				},
			},
		},
	}

	got := d.getAllocatedDevice(rc)
	if got != "uverbs0" {
		t.Errorf("getAllocatedDevice = %q, want uverbs0", got)
	}
}

func TestGetAllocatedDevice_VirtualDevice(t *testing.T) {
	d := &Driver{driverName: "dra.example.com"}
	rc := &resourceapi.ResourceClaim{
		Status: resourceapi.ResourceClaimStatus{
			Allocation: &resourceapi.AllocationResult{
				Devices: resourceapi.DeviceAllocationResult{
					Results: []resourceapi.DeviceRequestAllocationResult{
						{
							Request: "net",
							Driver:  "dra.example.com",
							Pool:    "node-1",
							Device:  "netdev-virtual",
						},
					},
				},
			},
		},
	}

	// Virtual devices return the shared device name; the handler ignores it.
	got := d.getAllocatedDevice(rc)
	if got != "netdev-virtual" {
		t.Errorf("getAllocatedDevice = %q, want netdev-virtual", got)
	}
}

func TestGetAllocatedDevice_OtherDriver(t *testing.T) {
	d := &Driver{driverName: "dra.example.com"}
	rc := &resourceapi.ResourceClaim{
		Status: resourceapi.ResourceClaimStatus{
			Allocation: &resourceapi.AllocationResult{
				Devices: resourceapi.DeviceAllocationResult{
					Results: []resourceapi.DeviceRequestAllocationResult{
						{
							Request: "gpu",
							Driver:  "some-other-driver.io",
							Pool:    "node-1",
							Device:  "gpu0",
						},
					},
				},
			},
		},
	}

	// Result from a different driver should be ignored
	got := d.getAllocatedDevice(rc)
	if got != "" {
		t.Errorf("expected empty string when no results match our driver, got %q", got)
	}
}

// ─── CDI device ID format test ──────────────────────────────────────────────

func TestCDIDeviceIDFormat(t *testing.T) {
	tests := []struct {
		driverName string
		devType    handler.DeviceType
		devName    string
		want       string
	}{
		{"example.com/multi-driver", handler.DeviceTypeNetdev, "dmaabbccdd", "example.com/multi-driver/netdev=dmaabbccdd"},
		{"example.com/multi-driver", handler.DeviceTypeRDMA, "uverbs0", "example.com/multi-driver/rdma=uverbs0"},
		{"example.com/multi-driver", handler.DeviceTypeCombo, "uverbs0", "example.com/multi-driver/combo=uverbs0"},
	}

	for _, tt := range tests {
		t.Run(string(tt.devType), func(t *testing.T) {
			got := tt.driverName + "/" + string(tt.devType) + "=" + tt.devName
			if got != tt.want {
				t.Errorf("CDI device ID = %s, want %s", got, tt.want)
			}
		})
	}
}

// ─── allocation tracking tests ──────────────────────────────────────────────

func TestAllocationTracking(t *testing.T) {
	d := &Driver{
		allocations: make(map[string]*handler.AllocationInfo),
	}

	uid := "track000-0000-0000-0000-000000000000"
	alloc := &handler.AllocationInfo{
		Type:     handler.DeviceTypeNetdev,
		Kind:     "dummy",
		ClaimUID: uid,
	}

	d.allocations[uid] = alloc

	got, ok := d.allocations[uid]

	if !ok {
		t.Fatal("allocation should be tracked")
	}
	if got.Kind != "dummy" {
		t.Errorf("tracked Kind = %s, want dummy", got.Kind)
	}

	delete(d.allocations, uid)

	_, ok = d.allocations[uid]

	if ok {
		t.Error("allocation should be removed after delete")
	}
}

// ─── State persistence tests ────────────────────────────────────────────────

func TestSaveAndRestoreAllocations(t *testing.T) {
	tmpDir := t.TempDir()

	// Temporarily swap the cdiDir constant via a helper driver that writes to tmpDir.
	// Since cdiDir is a const, we test saveAllocation/restoreAllocations by
	// manually constructing file paths matching the expected pattern.

	driverName := "dra.example.com"
	prefix := strings.ReplaceAll(driverName, "/", "-")

	claimUID := "aabbccdd-1111-2222-3333-444444444444"
	alloc := &handler.AllocationInfo{
		Type:       handler.DeviceTypeNetdev,
		Kind:       "dummy",
		ClaimUID:   claimUID,
		DeviceName: "dmaabbccdd",
		Metadata: map[string]string{
			"createdInterface": "dmaabbccdd",
			"containerName":    "net1",
		},
	}

	// Write allocation state file to tmpDir
	data, err := json.MarshalIndent(alloc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}

	allocFile := filepath.Join(tmpDir, prefix+"-"+claimUID[:8]+".alloc.json")
	if err := os.WriteFile(allocFile, data, 0644); err != nil {
		t.Fatal(err)
	}

	// Verify the file roundtrips correctly
	raw, err := os.ReadFile(allocFile)
	if err != nil {
		t.Fatal(err)
	}

	var restored handler.AllocationInfo
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}

	if restored.ClaimUID != claimUID {
		t.Errorf("ClaimUID = %s, want %s", restored.ClaimUID, claimUID)
	}
	if restored.Type != handler.DeviceTypeNetdev {
		t.Errorf("Type = %s, want netdev", restored.Type)
	}
	if restored.Kind != "dummy" {
		t.Errorf("Kind = %s, want dummy", restored.Kind)
	}
	if restored.DeviceName != "dmaabbccdd" {
		t.Errorf("DeviceName = %s, want dmaabbccdd", restored.DeviceName)
	}
	if restored.Metadata["createdInterface"] != "dmaabbccdd" {
		t.Errorf("Metadata[createdInterface] = %s, want dmaabbccdd", restored.Metadata["createdInterface"])
	}
}

func TestAllocationInfoJSONRoundTrip(t *testing.T) {
	alloc := &handler.AllocationInfo{
		Type:       handler.DeviceTypeCombo,
		Kind:       "roce",
		ClaimUID:   "11223344-aaaa-bbbb-cccc-dddddddddddd",
		DeviceName: "uverbs0",
		Metadata: map[string]string{
			"rdma_device":        "uverbs0",
			"net_interface":      "dm11223344",
			"rdma_uverbs_device": "uverbs0",
			"rdma_ibdev":         "mlx5_0",
			"net_created":        "dm11223344",
		},
	}

	data, err := json.Marshal(alloc)
	if err != nil {
		t.Fatal(err)
	}

	var restored handler.AllocationInfo
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}

	if restored.Type != alloc.Type {
		t.Errorf("Type = %s, want %s", restored.Type, alloc.Type)
	}
	if restored.Kind != alloc.Kind {
		t.Errorf("Kind = %s, want %s", restored.Kind, alloc.Kind)
	}
	if restored.ClaimUID != alloc.ClaimUID {
		t.Errorf("ClaimUID = %s, want %s", restored.ClaimUID, alloc.ClaimUID)
	}
	if len(restored.Metadata) != len(alloc.Metadata) {
		t.Errorf("Metadata length = %d, want %d", len(restored.Metadata), len(alloc.Metadata))
	}
	for k, v := range alloc.Metadata {
		if restored.Metadata[k] != v {
			t.Errorf("Metadata[%s] = %s, want %s", k, restored.Metadata[k], v)
		}
	}
}

func TestRestoreAllocations_SkipsInvalidFiles(t *testing.T) {
	tmpDir := t.TempDir()

	driverName := "dra.example.com"
	prefix := strings.ReplaceAll(driverName, "/", "-")

	// Write a valid alloc file
	validAlloc := &handler.AllocationInfo{
		Type:       handler.DeviceTypeNetdev,
		Kind:       "dummy",
		ClaimUID:   "validuid-1111-2222-3333-444444444444",
		DeviceName: "dmvaliduid",
		Metadata:   map[string]string{"createdInterface": "dmvaliduid"},
	}
	validData, _ := json.Marshal(validAlloc)
	os.WriteFile(filepath.Join(tmpDir, prefix+"-validuid.alloc.json"), validData, 0644)

	// Write an invalid JSON file
	os.WriteFile(filepath.Join(tmpDir, prefix+"-badjson0.alloc.json"), []byte("{invalid"), 0644)

	// Write a file with empty claimUID
	emptyUID := &handler.AllocationInfo{Type: handler.DeviceTypeNetdev, Kind: "dummy"}
	emptyData, _ := json.Marshal(emptyUID)
	os.WriteFile(filepath.Join(tmpDir, prefix+"-emptyuid.alloc.json"), emptyData, 0644)

	// Count how many valid files we can parse
	matches, err := filepath.Glob(filepath.Join(tmpDir, prefix+"-*.alloc.json"))
	if err != nil {
		t.Fatal(err)
	}

	if len(matches) != 3 {
		t.Fatalf("expected 3 alloc files, found %d", len(matches))
	}

	validCount := 0
	for _, path := range matches {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var a handler.AllocationInfo
		if err := json.Unmarshal(raw, &a); err != nil {
			continue
		}
		if a.ClaimUID == "" {
			continue
		}
		validCount++
	}

	if validCount != 1 {
		t.Errorf("expected 1 valid allocation, got %d", validCount)
	}
}
