package handler

import (
	"context"

	cdispec "tags.cncf.io/container-device-interface/specs-go"
)

// DeviceType is the broad category of device.
type DeviceType string

const (
	DeviceTypeNetdev DeviceType = "netdev"
	DeviceTypeRDMA   DeviceType = "rdma"
	DeviceTypeCombo  DeviceType = "combo"
)

// DeviceHandler manages a specific device type/kind.
type DeviceHandler interface {
	Type() DeviceType
	Kinds() []string
	Prepare(ctx context.Context, req *PrepareRequest) (*PrepareResult, error)
	Unprepare(ctx context.Context, req *UnprepareRequest) error
	Validate(ctx context.Context, cfg *DeviceConfig) error
}

// PrepareRequest contains information needed to prepare a device.
type PrepareRequest struct {
	ClaimUID        string
	Namespace       string
	ClaimName       string
	AllocatedDevice string
	Config          *DeviceConfig
}

// PrepareResult contains the result of preparing a device.
type PrepareResult struct {
	PoolName   string
	DeviceName string
	CDIEdits   *cdispec.ContainerEdits
	Allocation *AllocationInfo
}

// UnprepareRequest contains information needed to unprepare a device.
type UnprepareRequest struct {
	ClaimUID   string
	Allocation *AllocationInfo
}

// AllocationInfo tracks information about an allocated device for cleanup.
type AllocationInfo struct {
	Type       DeviceType        `json:"type"`
	Kind       string            `json:"kind"`
	CDIEnabled bool              `json:"cdiEnabled"`
	ClaimUID   string            `json:"claimUID"`
	DeviceName string            `json:"deviceName"`
	Metadata   map[string]string `json:"metadata"`
}

// DeviceConfig holds the parsed configuration from ResourceClaim opaque parameters.
type DeviceConfig struct {
	Type   DeviceType    `json:"type"`
	Netdev *NetdevConfig `json:"netdev,omitempty"`
	RDMA   *RDMAConfig   `json:"rdma,omitempty"`
	Combo  *ComboConfig  `json:"combo,omitempty"`
}

// NetdevConfig holds network device specific configuration.
type NetdevConfig struct {
	Kind          string      `json:"kind"`
	InterfaceName string      `json:"interfaceName,omitempty"`
	MTU           int         `json:"mtu,omitempty"`
	Parent        string      `json:"parent,omitempty"`
	Mode          string      `json:"mode,omitempty"`
	VFIndex       int         `json:"vfIndex,omitempty"`
	HostDevice    string      `json:"hostDevice,omitempty"` // host-device: name of a pre-existing interface to move into the pod
	BridgeName    string      `json:"bridgeName,omitempty"` // bridge-veth: host bridge to attach the veth to
	VLANID        int         `json:"vlanId,omitempty"`     // bridge-veth: VLAN ID to assign to the bridge port
	PoolName      string      `json:"poolName,omitempty"`   // ipam: name of the IPAM pool to allocate from
	CIDR          string      `json:"cidr,omitempty"`       // ipam: CIDR for a pod-side IP address, e.g. 10.240.0.0/24
	Gateway       string      `json:"gateway,omitempty"`    // ipam: gateway for the interface
	Routes        []RouteSpec `json:"routes,omitempty"`     // ipam: explicit routes to add inside the pod netns
	Pkey          int         `json:"pkey,omitempty"`       // ipoib: partition key (e.g. 0x8001)
}

// RouteSpec encodes a static route intended for a pod-side interface.
type RouteSpec struct {
	Dst string `json:"dst"`
	Via string `json:"via,omitempty"`
}

// RDMAConfig holds RDMA device specific configuration.
type RDMAConfig struct {
	PreferDevice string `json:"preferDevice,omitempty"`
}

// ComboConfig holds combo device configuration (e.g., RoCE = RDMA + netdev).
type ComboConfig struct {
	RDMA   RDMAConfig   `json:"rdma"`
	Netdev NetdevConfig `json:"netdev"`
}

// GetKind returns the specific kind from the config based on the device type.
func (c *DeviceConfig) GetKind() string {
	switch c.Type {
	case DeviceTypeNetdev:
		if c.Netdev != nil {
			return c.Netdev.Kind
		}
	case DeviceTypeRDMA:
		return "uverbs"
	case DeviceTypeCombo:
		return "roce"
	}
	return ""
}
