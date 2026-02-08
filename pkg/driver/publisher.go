package driver

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/klog/v2"

	"github.com/example/dra-poc/pkg/handler/rdma"
)

// DiscoverResources discovers all devices on this node and returns them as
// a DriverResources structure suitable for kubeletplugin.Helper.PublishResources.
// The helper takes care of creating/updating/deleting ResourceSlices.
func DiscoverResources(driverName, nodeName string) resourceslice.DriverResources {
	var allDevices []resourceapi.Device

	netDevices := discoverNetworkDevices()
	allDevices = append(allDevices, netDevices...)

	rdmaDevices := discoverRDMADevices()
	allDevices = append(allDevices, rdmaDevices...)

	virtualDevices := discoverVirtualPools()
	allDevices = append(allDevices, virtualDevices...)

	klog.Infof("Discovered %d devices (net=%d, rdma=%d, virtual=%d)",
		len(allDevices), len(netDevices), len(rdmaDevices), len(virtualDevices))

	// One pool per node. The pool name must be non-empty; we use the node
	// name so every node publishes into its own pool.
	return resourceslice.DriverResources{
		Pools: map[string]resourceslice.Pool{
			nodeName: {
				Slices: []resourceslice.Slice{{
					Devices: allDevices,
				}},
			},
		},
	}
}

// discoverNetworkDevices discovers physical/SR-IOV network interfaces
func discoverNetworkDevices() []resourceapi.Device {
	var devices []resourceapi.Device

	netDir := "/sys/class/net"
	entries, err := os.ReadDir(netDir)
	if err != nil {
		klog.Warningf("Failed to read %s: %v", netDir, err)
		return devices
	}

	for _, entry := range entries {
		name := entry.Name()
		// Skip loopback, common host interfaces, and container veth pairs
		if name == "lo" || name == "eth0" || name == "docker0" || name == "cni0" || strings.HasPrefix(name, "veth") {
			continue
		}

		// Check for SR-IOV VFs
		if isVF(name) {
			device := resourceapi.Device{
				Name: name,
				Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
					"dra.example.com/type": {
						StringValue: stringPtr("netdev"),
					},
					"dra.example.com/kind": {
						StringValue: stringPtr("sriov-vf"),
					},
					"dra.example.com/parent": {
						StringValue: stringPtr(getVFParent(name)),
					},
				},
			}
			// Try to read PCI address
			if pciAddr := getPCIAddress(name); pciAddr != "" {
				device.Attributes["dra.example.com/pci-address"] = resourceapi.DeviceAttribute{
					StringValue: stringPtr(pciAddr),
				}
			}
			// Try to read NUMA node
			if numaNode := getNUMANode(name); numaNode >= 0 {
				device.Attributes["dra.example.com/numa-node"] = resourceapi.DeviceAttribute{
					IntValue: int64Ptr(int64(numaNode)),
				}
			}
			devices = append(devices, device)
			klog.V(2).Infof("Discovered SR-IOV VF: %s", name)
			continue
		}

		// Check if this is an allocatable physical interface
		if isAllocatableInterface(name) {
			device := resourceapi.Device{
				Name: fmt.Sprintf("netdev-%s", name),
				Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
					"dra.example.com/type": {
						StringValue: stringPtr("netdev"),
					},
					"dra.example.com/kind": {
						StringValue: stringPtr("physical"),
					},
					"dra.example.com/interface": {
						StringValue: stringPtr(name),
					},
				},
			}
			devices = append(devices, device)
			klog.V(2).Infof("Discovered allocatable interface: %s", name)
		}
	}

	return devices
}

// discoverRDMADevices discovers RDMA uverbs devices
func discoverRDMADevices() []resourceapi.Device {
	var devices []resourceapi.Device

	mode := rdma.DetectNetnsMode()

	ibDir := "/dev/infiniband"
	entries, err := os.ReadDir(ibDir)
	if err != nil {
		klog.V(2).Infof("No RDMA devices found (%s not readable): %v", ibDir, err)
		return devices
	}

	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "uverbs") {
			continue
		}

		device := resourceapi.Device{
			Name: name,
			Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				"dra.example.com/type": {
					StringValue: stringPtr("rdma"),
				},
				"dra.example.com/device": {
					StringValue: stringPtr(filepath.Join("/dev/infiniband", name)),
				},
				"dra.example.com/rdma-netns-mode": {
					StringValue: stringPtr(string(mode)),
				},
			},
		}

		// In shared mode, multiple containers can open the same uverbs device
		// concurrently — each gets independent protection domains and QPs.
		// In exclusive mode, the device is bound to one netns at a time.
		if mode == rdma.NetnsShared {
			device.AllowMultipleAllocations = boolPtr(true)
		}

		// Try to resolve the IB device name
		ibDev := resolveIBDeviceName(name)
		if ibDev != "" {
			device.Attributes["dra.example.com/ibdev"] = resourceapi.DeviceAttribute{
				StringValue: stringPtr(ibDev),
			}
		}

		devices = append(devices, device)
		klog.V(2).Infof("Discovered RDMA device: %s (ibdev=%s, mode=%s)", name, ibDev, mode)
	}

	return devices
}

// maxVirtualSlots is the total number of virtual netdev allocations allowed per node.
// Virtual devices (dummy, veth, macvlan, ipvlan, host-device) are created on-demand,
// so there is no hard physical limit.  We use DRAConsumableCapacity to advertise a
// single device with a consumable "slots" capacity — each allocation consumes one slot.
const maxVirtualSlots = 128

// discoverVirtualPools discovers parent interfaces suitable for virtual device pools
func discoverVirtualPools() []resourceapi.Device {
	var devices []resourceapi.Device

	// Single virtual device with consumable capacity (DRAConsumableCapacity feature gate).
	// AllowMultipleAllocations lets the scheduler allocate this device to many claims;
	// each allocation consumes 1 slot out of maxVirtualSlots.
	defaultSlot := resource.MustParse("1")
	devices = append(devices, resourceapi.Device{
		Name:                     "netdev-virtual",
		AllowMultipleAllocations: boolPtr(true),
		Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			"dra.example.com/type": {
				StringValue: stringPtr("netdev"),
			},
			"dra.example.com/kind": {
				StringValue: stringPtr("virtual"),
			},
		},
		Capacity: map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
			"dra.example.com/slots": {
				Value: resource.MustParse(fmt.Sprintf("%d", maxVirtualSlots)),
				RequestPolicy: &resourceapi.CapacityRequestPolicy{
					Default: &defaultSlot,
				},
			},
		},
	})

	// Discover interfaces that can be parents for macvlan/ipvlan
	netDir := "/sys/class/net"
	entries, err := os.ReadDir(netDir)
	if err != nil {
		return devices
	}

	for _, entry := range entries {
		name := entry.Name()
		if name == "lo" {
			continue
		}

		// Physical interfaces can serve as macvlan/ipvlan parents
		if isPhysicalInterface(name) {
			// Macvlan pool template
			devices = append(devices, resourceapi.Device{
				Name: fmt.Sprintf("%s-macvlan-pool", name),
				Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
					"dra.example.com/type": {
						StringValue: stringPtr("netdev"),
					},
					"dra.example.com/kind": {
						StringValue: stringPtr("macvlan"),
					},
					"dra.example.com/parent": {
						StringValue: stringPtr(name),
					},
				},
			})

			// Ipvlan pool template
			devices = append(devices, resourceapi.Device{
				Name: fmt.Sprintf("%s-ipvlan-pool", name),
				Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
					"dra.example.com/type": {
						StringValue: stringPtr("netdev"),
					},
					"dra.example.com/kind": {
						StringValue: stringPtr("ipvlan"),
					},
					"dra.example.com/parent": {
						StringValue: stringPtr(name),
					},
				},
			})

			klog.V(2).Infof("Discovered virtual pool parent: %s", name)
		}
	}

	return devices
}

// --- Helper functions ---

func stringPtr(s string) *string {
	return &s
}

func int64Ptr(i int64) *int64 {
	return &i
}

func boolPtr(b bool) *bool {
	return &b
}

// isVF checks if a network interface is an SR-IOV Virtual Function
func isVF(name string) bool {
	// A VF has a symlink at /sys/class/net/<name>/device/physfn
	physfnPath := filepath.Join("/sys/class/net", name, "device", "physfn")
	_, err := os.Readlink(physfnPath)
	return err == nil
}

// getVFParent returns the PF name for a VF
func getVFParent(vfName string) string {
	physfnPath := filepath.Join("/sys/class/net", vfName, "device", "physfn", "net")
	entries, err := os.ReadDir(physfnPath)
	if err != nil {
		return ""
	}
	if len(entries) > 0 {
		return entries[0].Name()
	}
	return ""
}

// getPCIAddress returns the PCI address for a network device
func getPCIAddress(name string) string {
	devicePath := filepath.Join("/sys/class/net", name, "device")
	resolved, err := os.Readlink(devicePath)
	if err != nil {
		return ""
	}
	// The resolved path ends with the PCI address, e.g., ../../../0000:3b:00.2
	return filepath.Base(resolved)
}

// getNUMANode returns the NUMA node for a network device
func getNUMANode(name string) int {
	numaPath := filepath.Join("/sys/class/net", name, "device", "numa_node")
	data, err := os.ReadFile(numaPath)
	if err != nil {
		return -1
	}
	node, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return -1
	}
	return node
}

// isAllocatableInterface determines if a network interface can be allocated
func isAllocatableInterface(name string) bool {
	patterns := []string{
		"vf", "enp", "ens", "ib", "rdma", "veth", "dummy",
	}
	for _, pattern := range patterns {
		if strings.HasPrefix(name, pattern) || strings.Contains(name, pattern) {
			return true
		}
	}
	return false
}

// isPhysicalInterface checks if an interface is a physical NIC (has a device symlink)
func isPhysicalInterface(name string) bool {
	devicePath := filepath.Join("/sys/class/net", name, "device")
	_, err := os.Stat(devicePath)
	return err == nil
}

// resolveIBDeviceName maps a uverbs device to its IB device name
func resolveIBDeviceName(uverbsName string) string {
	sysPath := filepath.Join("/sys/class/infiniband_verbs", uverbsName, "ibdev")
	data, err := os.ReadFile(sysPath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
