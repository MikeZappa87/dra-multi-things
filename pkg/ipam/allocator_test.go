package ipam

import (
	"path/filepath"
	"testing"
)

func TestAllocatorAllocateAndRelease(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ipam.db")
	store, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	defer store.Close()

	alloc, err := NewAllocator(store)
	if err != nil {
		t.Fatalf("NewAllocator() error = %v", err)
	}

	pool := Pool{
		Name:    "vlan-100",
		CIDR:    "10.240.0.0/24",
		Gateway: "10.240.0.1",
	}
	if err := alloc.CreatePool(pool); err != nil {
		t.Fatalf("CreatePool() error = %v", err)
	}

	lease1, err := alloc.Allocate("vlan-100", "claim-1", "pod-1", "vlan100")
	if err != nil {
		t.Fatalf("first Allocate() error = %v", err)
	}
	lease2, err := alloc.Allocate("vlan-100", "claim-2", "pod-2", "vlan101")
	if err != nil {
		t.Fatalf("second Allocate() error = %v", err)
	}

	if lease1.IP == lease2.IP {
		t.Fatalf("allocated duplicate IPs: %s and %s", lease1.IP, lease2.IP)
	}
	if lease1.CIDR != "10.240.0.0/24" {
		t.Fatalf("lease CIDR = %s, want 10.240.0.0/24", lease1.CIDR)
	}
	if lease1.IP != "10.240.0.2" {
		t.Fatalf("first allocated IP = %s, want gateway-skipping 10.240.0.2", lease1.IP)
	}

	if err := alloc.Release("claim-1"); err != nil {
		t.Fatalf("Release() error = %v", err)
	}

	lease3, err := alloc.Allocate("vlan-100", "claim-3", "pod-3", "vlan102")
	if err != nil {
		t.Fatalf("Allocate() after release error = %v", err)
	}
	if lease3.IP != lease1.IP {
		t.Fatalf("expected released IP to be reused, got %s want %s", lease3.IP, lease1.IP)
	}
}

func TestAllocatorRejectsPoolExhaustion(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ipam.db")
	store, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	defer store.Close()

	alloc, err := NewAllocator(store)
	if err != nil {
		t.Fatalf("NewAllocator() error = %v", err)
	}

	if err := alloc.CreatePool(Pool{Name: "vlan-100", CIDR: "10.240.0.0/30"}); err != nil {
		t.Fatalf("CreatePool() error = %v", err)
	}

	if _, err := alloc.Allocate("vlan-100", "claim-1", "pod-1", "vlan100"); err != nil {
		t.Fatalf("first Allocate() error = %v", err)
	}
	if _, err := alloc.Allocate("vlan-100", "claim-2", "pod-2", "vlan101"); err != nil {
		t.Fatalf("second Allocate() error = %v", err)
	}
	if _, err := alloc.Allocate("vlan-100", "claim-3", "pod-3", "vlan102"); err == nil {
		t.Fatal("expected allocation failure when pool is exhausted")
	}
}
