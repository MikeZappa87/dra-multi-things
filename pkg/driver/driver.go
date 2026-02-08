package driver

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/klog/v2"
	cdispec "tags.cncf.io/container-device-interface/specs-go"

	"github.com/example/dra-poc/pkg/handler"
)

const (
	cdiDir     = "/etc/cdi" // Standard CDI spec directory
	cdiVersion = "1.1.0"    // CDI version with NetDevices support
)

// Driver implements kubeletplugin.DRAPlugin.
type Driver struct {
	driverName string
	registry   *handler.HandlerRegistry

	// Track allocated devices: claimUID -> AllocationInfo
	allocations map[string]*handler.AllocationInfo
}

// New creates a new DRA driver instance.
func New(driverName string, registry *handler.HandlerRegistry) *Driver {
	return &Driver{
		driverName:  driverName,
		registry:    registry,
		allocations: make(map[string]*handler.AllocationInfo),
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// kubeletplugin.DRAPlugin implementation
// ──────────────────────────────────────────────────────────────────────────────

// PrepareResourceClaims prepares all devices for the given claims.
// The helper has already fetched the full ResourceClaim objects and serialises
// calls, so we don't need to handle locking or API fetches.
func (d *Driver) PrepareResourceClaims(ctx context.Context, claims []*resourceapi.ResourceClaim) (map[types.UID]kubeletplugin.PrepareResult, error) {
	klog.Infof("PrepareResourceClaims called with %d claims", len(claims))

	// Reload persisted state so we can be idempotent across restarts and
	// rolling updates (the other pod may have prepared some claims).
	d.restoreAllocations()

	results := make(map[types.UID]kubeletplugin.PrepareResult, len(claims))

	for _, rc := range claims {
		uid := string(rc.UID)
		klog.Infof("Preparing claim: uid=%s namespace=%s name=%s", uid, rc.Namespace, rc.Name)

		// Idempotent: if we already have state for this claim, return it.
		if existing, ok := d.allocations[uid]; ok {
			cdiDeviceID := fmt.Sprintf("%s/%s=%s", d.driverName, existing.Type, existing.DeviceName)
			klog.Infof("Claim %s already prepared (restored state), returning cdi=%s", uid, cdiDeviceID)
			results[rc.UID] = d.prepareResultFromAlloc(existing, cdiDeviceID)
			continue
		}

		result, err := d.prepareClaim(ctx, rc)
		if err != nil {
			klog.Errorf("Failed to prepare claim %s: %v", uid, err)
			results[rc.UID] = kubeletplugin.PrepareResult{Err: err}
			continue
		}

		// Create CDI spec from handler's edits
		cdiDeviceID, err := d.createCDISpec(uid, result)
		if err != nil {
			d.unprepareAllocation(ctx, result.Allocation)
			results[rc.UID] = kubeletplugin.PrepareResult{Err: err}
			continue
		}

		d.allocations[uid] = result.Allocation

		klog.Infof("Successfully prepared claim %s: pool=%s device=%s cdi=%s",
			uid, result.PoolName, result.DeviceName, cdiDeviceID)

		results[rc.UID] = kubeletplugin.PrepareResult{
			Devices: []kubeletplugin.Device{{
				PoolName:     result.PoolName,
				DeviceName:   result.DeviceName,
				CDIDeviceIDs: []string{cdiDeviceID},
			}},
		}
	}

	return results, nil
}

// UnprepareResourceClaims undoes whatever PrepareResourceClaims did.
// The original ResourceClaim objects may already be deleted — the helper only
// passes UID, namespace, and name.
func (d *Driver) UnprepareResourceClaims(ctx context.Context, claims []kubeletplugin.NamespacedObject) (map[types.UID]error, error) {
	klog.Infof("UnprepareResourceClaims called with %d claims", len(claims))

	// Reload persisted state — the matching Prepare may have been in another pod.
	d.restoreAllocations()

	results := make(map[types.UID]error, len(claims))

	for _, claim := range claims {
		uid := string(claim.UID)
		klog.Infof("Unpreparing claim: %s", uid)

		alloc, ok := d.allocations[uid]
		if !ok {
			klog.Warningf("No tracked allocation for claim %s (already cleaned up?)", uid)
			results[claim.UID] = nil
			continue
		}

		if err := d.unprepareAllocation(ctx, alloc); err != nil {
			klog.Errorf("Failed to unprepare claim %s: %v", uid, err)
			results[claim.UID] = err
			continue
		}

		d.deleteCDISpec(uid)
		delete(d.allocations, uid)

		results[claim.UID] = nil
		klog.Infof("Successfully unprepared claim %s", uid)
	}

	return results, nil
}

// HandleError is called for background errors (e.g. ResourceSlice publishing).
func (d *Driver) HandleError(ctx context.Context, err error, msg string) {
	klog.ErrorS(err, msg)
}

// ──────────────────────────────────────────────────────────────────────────────
// Internal helpers
// ──────────────────────────────────────────────────────────────────────────────

// prepareClaim dispatches to the appropriate handler based on config.
func (d *Driver) prepareClaim(ctx context.Context, rc *resourceapi.ResourceClaim) (*handler.PrepareResult, error) {
	config := d.parseConfig(rc)
	allocatedDevice := d.getAllocatedDevice(rc)

	kind := config.GetKind()
	h, err := d.registry.MustGet(config.Type, kind)
	if err != nil {
		return nil, err
	}

	if err := h.Validate(ctx, config); err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}

	return h.Prepare(ctx, &handler.PrepareRequest{
		ClaimUID:        string(rc.UID),
		Namespace:       rc.Namespace,
		ClaimName:       rc.Name,
		AllocatedDevice: allocatedDevice,
		Config:          config,
	})
}

// unprepareAllocation delegates to the appropriate handler for cleanup.
func (d *Driver) unprepareAllocation(ctx context.Context, alloc *handler.AllocationInfo) error {
	h := d.registry.Get(alloc.Type, alloc.Kind)
	if h == nil {
		return fmt.Errorf("no handler for type=%s kind=%s during unprepare", alloc.Type, alloc.Kind)
	}

	return h.Unprepare(ctx, &handler.UnprepareRequest{
		ClaimUID:   alloc.ClaimUID,
		Allocation: alloc,
	})
}

// parseConfig extracts the DeviceConfig from a ResourceClaim.
// If rc is nil, a sensible default is returned.
func (d *Driver) parseConfig(rc *resourceapi.ResourceClaim) *handler.DeviceConfig {
	config := &handler.DeviceConfig{
		Type: handler.DeviceTypeNetdev,
		Netdev: &handler.NetdevConfig{
			Kind:          "dummy",
			InterfaceName: "eth1",
		},
	}

	if rc == nil {
		klog.V(2).Info("No ResourceClaim available, using default config")
		return config
	}

	for _, cfg := range rc.Spec.Devices.Config {
		if cfg.Opaque == nil || cfg.Opaque.Driver != d.driverName {
			continue
		}

		var parsed handler.DeviceConfig
		if err := json.Unmarshal(cfg.Opaque.Parameters.Raw, &parsed); err != nil {
			klog.V(2).Infof("Could not parse opaque config: %v", err)
			continue
		}

		klog.Infof("Parsed device config from ResourceClaim: type=%s kind=%s", parsed.Type, parsed.GetKind())
		return &parsed
	}

	return config
}

// getAllocatedDevice extracts the scheduler-assigned device name from the
// claim's allocation results.
func (d *Driver) getAllocatedDevice(rc *resourceapi.ResourceClaim) string {
	if rc == nil || rc.Status.Allocation == nil {
		return ""
	}

	for _, result := range rc.Status.Allocation.Devices.Results {
		if result.Driver != d.driverName {
			continue
		}
		klog.Infof("Scheduler allocated device: pool=%s device=%s (request=%s)",
			result.Pool, result.Device, result.Request)
		return result.Device
	}

	return ""
}

// prepareResultFromAlloc builds a kubeletplugin.PrepareResult from cached state.
func (d *Driver) prepareResultFromAlloc(alloc *handler.AllocationInfo, cdiDeviceID string) kubeletplugin.PrepareResult {
	poolName := "default"
	if p, ok := alloc.Metadata["poolName"]; ok {
		poolName = p
	}
	return kubeletplugin.PrepareResult{
		Devices: []kubeletplugin.Device{{
			PoolName:     poolName,
			DeviceName:   alloc.DeviceName,
			CDIDeviceIDs: []string{cdiDeviceID},
		}},
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// CDI spec management
// ──────────────────────────────────────────────────────────────────────────────

// cdiFilePrefix returns the common prefix for CDI and allocation state files.
func (d *Driver) cdiFilePrefix(claimUID string) string {
	return fmt.Sprintf("%s-%s", strings.ReplaceAll(d.driverName, "/", "-"), claimUID[:8])
}

// createCDISpec creates a CDI spec file from the handler's edits.
func (d *Driver) createCDISpec(claimUID string, result *handler.PrepareResult) (string, error) {
	if err := os.MkdirAll(cdiDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create CDI directory: %w", err)
	}

	spec := cdispec.Spec{
		Version: cdiVersion,
		Kind:    fmt.Sprintf("%s/%s", d.driverName, result.Allocation.Type),
		Devices: []cdispec.Device{{
			Name:           result.DeviceName,
			ContainerEdits: *result.CDIEdits,
		}},
	}

	data, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to marshal CDI spec: %w", err)
	}

	prefix := d.cdiFilePrefix(claimUID)
	cdiFilePath := filepath.Join(cdiDir, prefix+".json")

	if err := os.WriteFile(cdiFilePath, data, 0644); err != nil {
		return "", fmt.Errorf("failed to write CDI spec: %w", err)
	}

	if err := d.saveAllocation(claimUID, result.Allocation); err != nil {
		klog.Warningf("Failed to save allocation state for claim %s: %v", claimUID, err)
	}

	cdiDeviceID := fmt.Sprintf("%s/%s=%s", d.driverName, result.Allocation.Type, result.DeviceName)
	klog.Infof("Created CDI spec at %s (id: %s)", cdiFilePath, cdiDeviceID)
	return cdiDeviceID, nil
}

// deleteCDISpec removes the CDI spec and allocation state for a claim.
func (d *Driver) deleteCDISpec(claimUID string) {
	prefix := d.cdiFilePrefix(claimUID)

	cdiFilePath := filepath.Join(cdiDir, prefix+".json")
	if err := os.Remove(cdiFilePath); err != nil && !os.IsNotExist(err) {
		klog.Warningf("Failed to delete CDI spec %s: %v", cdiFilePath, err)
	} else {
		klog.Infof("Deleted CDI spec at %s", cdiFilePath)
	}

	d.removeAllocationState(claimUID)
}

// ──────────────────────────────────────────────────────────────────────────────
// Allocation state persistence
// ──────────────────────────────────────────────────────────────────────────────

// saveAllocation persists AllocationInfo to a sidecar file alongside the CDI spec.
func (d *Driver) saveAllocation(claimUID string, alloc *handler.AllocationInfo) error {
	data, err := json.MarshalIndent(alloc, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal allocation: %w", err)
	}
	path := filepath.Join(cdiDir, d.cdiFilePrefix(claimUID)+".alloc.json")
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("write allocation state: %w", err)
	}
	klog.V(2).Infof("Saved allocation state to %s", path)
	return nil
}

// removeAllocationState deletes the sidecar allocation state file.
func (d *Driver) removeAllocationState(claimUID string) {
	path := filepath.Join(cdiDir, d.cdiFilePrefix(claimUID)+".alloc.json")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		klog.Warningf("Failed to delete allocation state %s: %v", path, err)
	}
}

// restoreAllocations rebuilds the in-memory allocations map from persisted state files.
func (d *Driver) restoreAllocations() {
	pattern := filepath.Join(cdiDir, d.cdiFilePrefix("00000000")[:len(strings.ReplaceAll(d.driverName, "/", "-"))+1]+"*.alloc.json")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		klog.Warningf("Failed to glob allocation state files: %v", err)
		return
	}

	restored := 0
	for _, path := range matches {
		data, err := os.ReadFile(path)
		if err != nil {
			klog.Warningf("Failed to read allocation state %s: %v", path, err)
			continue
		}

		var alloc handler.AllocationInfo
		if err := json.Unmarshal(data, &alloc); err != nil {
			klog.Warningf("Failed to parse allocation state %s: %v", path, err)
			continue
		}

		if alloc.ClaimUID == "" {
			klog.Warningf("Skipping allocation state with empty claimUID: %s", path)
			continue
		}

		d.allocations[alloc.ClaimUID] = &alloc
		restored++
		klog.V(2).Infof("Restored allocation: claim=%s type=%s kind=%s device=%s",
			alloc.ClaimUID, alloc.Type, alloc.Kind, alloc.DeviceName)
	}

	if restored > 0 {
		klog.Infof("Restored %d allocations from disk", restored)
	}
}
