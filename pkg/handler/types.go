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
	Kind          string `json:"kind"`
	InterfaceName string `json:"interfaceName,omitempty"`
	MTU           int    `json:"mtu,omitempty"`
	Parent        string `json:"parent,omitempty"`
	Mode          string `json:"mode,omitempty"`
	VFIndex       int    `json:"vfIndex,omitempty"`
	HostDevice    string `json:"hostDevice,omitempty"` // host-device: name of a pre-existing interface to move into the pod
	Pkey          int    `json:"pkey,omitempty"`       // ipoib: partition key (e.g. 0x8001)
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
