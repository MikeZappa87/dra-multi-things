package handler

import (
	"context"
	"testing"

	cdispec "tags.cncf.io/container-device-interface/specs-go"
)

func TestDeviceConfig_GetKind(t *testing.T) {
	tests := []struct {
		name string
		cfg  DeviceConfig
		want string
	}{
		{
			name: "netdev with kind",
			cfg:  DeviceConfig{Type: DeviceTypeNetdev, Netdev: &NetdevConfig{Kind: "macvlan"}},
			want: "macvlan",
		},
		{
			name: "netdev nil config",
			cfg:  DeviceConfig{Type: DeviceTypeNetdev},
			want: "",
		},
		{
			name: "rdma always uverbs",
			cfg:  DeviceConfig{Type: DeviceTypeRDMA},
			want: "uverbs",
		},
		{
			name: "combo always roce",
			cfg:  DeviceConfig{Type: DeviceTypeCombo},
			want: "roce",
		},
		{
			name: "unknown type",
			cfg:  DeviceConfig{Type: DeviceType("alien")},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cfg.GetKind()
			if got != tt.want {
				t.Errorf("GetKind() = %q, want %q", got, tt.want)
			}
		})
	}
}

// fakeHandler is a minimal DeviceHandler for registry tests.
type fakeHandler struct {
	typ   DeviceType
	kinds []string
}

func (f *fakeHandler) Type() DeviceType { return f.typ }
func (f *fakeHandler) Kinds() []string  { return f.kinds }
func (f *fakeHandler) Prepare(_ context.Context, _ *PrepareRequest) (*PrepareResult, error) {
	return &PrepareResult{CDIEdits: &cdispec.ContainerEdits{}}, nil
}
func (f *fakeHandler) Unprepare(_ context.Context, _ *UnprepareRequest) error { return nil }
func (f *fakeHandler) Validate(_ context.Context, _ *DeviceConfig) error      { return nil }

func TestHandlerRegistry_RegisterAndGet(t *testing.T) {
	reg := NewHandlerRegistry()

	h := &fakeHandler{typ: DeviceTypeNetdev, kinds: []string{"macvlan", "ipvlan"}}
	reg.Register(h)

	// Should find registered kinds
	if got := reg.Get(DeviceTypeNetdev, "macvlan"); got != h {
		t.Error("expected to find macvlan handler")
	}
	if got := reg.Get(DeviceTypeNetdev, "ipvlan"); got != h {
		t.Error("expected to find ipvlan handler")
	}

	// Should return nil for unregistered
	if got := reg.Get(DeviceTypeNetdev, "veth"); got != nil {
		t.Errorf("expected nil for unregistered kind, got %v", got)
	}
	if got := reg.Get(DeviceTypeRDMA, "uverbs"); got != nil {
		t.Errorf("expected nil for unregistered type, got %v", got)
	}
}

func TestHandlerRegistry_MustGet(t *testing.T) {
	reg := NewHandlerRegistry()

	h := &fakeHandler{typ: DeviceTypeRDMA, kinds: []string{"uverbs"}}
	reg.Register(h)

	got, err := reg.MustGet(DeviceTypeRDMA, "uverbs")
	if err != nil {
		t.Fatalf("MustGet returned unexpected error: %v", err)
	}
	if got != h {
		t.Error("MustGet returned wrong handler")
	}

	_, err = reg.MustGet(DeviceTypeNetdev, "dummy")
	if err == nil {
		t.Error("MustGet should return error for unregistered handler")
	}
}

func TestHandlerRegistry_ListRegistered(t *testing.T) {
	reg := NewHandlerRegistry()

	reg.Register(&fakeHandler{typ: DeviceTypeNetdev, kinds: []string{"macvlan", "veth"}})
	reg.Register(&fakeHandler{typ: DeviceTypeRDMA, kinds: []string{"uverbs"}})

	list := reg.ListRegistered()

	if len(list[DeviceTypeNetdev]) != 2 {
		t.Errorf("expected 2 netdev kinds, got %d", len(list[DeviceTypeNetdev]))
	}
	if len(list[DeviceTypeRDMA]) != 1 {
		t.Errorf("expected 1 rdma kind, got %d", len(list[DeviceTypeRDMA]))
	}
}

func TestHandlerRegistry_OverwriteKind(t *testing.T) {
	reg := NewHandlerRegistry()

	h1 := &fakeHandler{typ: DeviceTypeNetdev, kinds: []string{"dummy"}}
	h2 := &fakeHandler{typ: DeviceTypeNetdev, kinds: []string{"dummy"}}

	reg.Register(h1)
	reg.Register(h2) // overwrites

	got := reg.Get(DeviceTypeNetdev, "dummy")
	if got != h2 {
		t.Error("expected second handler to overwrite first")
	}
}
