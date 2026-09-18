package ipam

import (
	"fmt"
	"net"
)

// Allocator allocates addresses from a pool and records them in the store.
type Allocator struct {
	store *Store
}

// NewAllocator constructs a new allocator backed by the given store.
func NewAllocator(store *Store) (*Allocator, error) {
	if store == nil {
		return nil, fmt.Errorf("ipam store is required")
	}
	return &Allocator{store: store}, nil
}

// CreatePool registers a CIDR pool for allocations.
func (a *Allocator) CreatePool(pool Pool) error {
	if pool.Name == "" {
		return fmt.Errorf("pool name is required")
	}
	return a.store.CreatePool(pool)
}

// Allocate reserves the next available IP in the pool for the claim.
func (a *Allocator) Allocate(poolName, claimUID, podUID, iface string) (*Lease, error) {
	pool, err := a.store.GetPool(poolName)
	if err != nil {
		return nil, err
	}
	_, ipnet, err := net.ParseCIDR(pool.CIDR)
	if err != nil {
		return nil, fmt.Errorf("invalid pool CIDR %q: %w", pool.CIDR, err)
	}

	used, err := a.store.ListUsedIPs(poolName)
	if err != nil {
		return nil, err
	}
	usedSet := make(map[string]bool, len(used))
	for _, ip := range used {
		usedSet[ip] = true
	}
	if pool.Gateway != "" {
		gateway := net.ParseIP(pool.Gateway).To4()
		if gateway == nil || !ipnet.Contains(gateway) {
			return nil, fmt.Errorf("invalid gateway %q for pool %s", pool.Gateway, poolName)
		}
		usedSet[gateway.String()] = true
	}

	candidate := nextAvailableIP(ipnet, usedSet)
	if candidate == "" {
		return nil, fmt.Errorf("pool %s has no free addresses left", poolName)
	}

	lease := &Lease{
		ClaimUID:  claimUID,
		PodUID:    podUID,
		Interface: iface,
		PoolName:  poolName,
		IP:        candidate,
		CIDR:      pool.CIDR,
		Gateway:   pool.Gateway,
		Routes:    pool.Routes,
	}
	if err := a.store.SaveLease(*lease); err != nil {
		return nil, err
	}
	return lease, nil
}

// Release removes a claim's lease and frees the IP again.
func (a *Allocator) Release(claimUID string) error {
	_, err := a.store.GetLeaseByClaim(claimUID)
	if err != nil {
		return nil
	}
	return a.store.DeleteLease(claimUID)
}

// LeaseForClaim returns the current lease for a claim, if any.
func (a *Allocator) LeaseForClaim(claimUID string) (*Lease, error) {
	return a.store.GetLeaseByClaim(claimUID)
}

func nextAvailableIP(ipnet *net.IPNet, used map[string]bool) string {
	if ipnet == nil || ipnet.IP == nil {
		return ""
	}
	base := ipnet.IP.Mask(ipnet.Mask).To4()
	if base == nil {
		return ""
	}

	ones, bits := ipnet.Mask.Size()
	if bits != 32 {
		return ""
	}
	addrCount := 1 << (bits - ones)
	for i := 1; i < addrCount-1; i++ {
		candidate := incrementIP(base, i)
		if candidate == nil {
			continue
		}
		if !used[candidate.String()] {
			return candidate.String()
		}
	}
	return ""
}

func incrementIP(ip net.IP, n int) net.IP {
	ip4 := ip.To4()
	if ip4 == nil {
		return nil
	}
	v := uint32(ip4[0])<<24 | uint32(ip4[1])<<16 | uint32(ip4[2])<<8 | uint32(ip4[3])
	v += uint32(n)
	return net.IPv4(byte(v>>24), byte(v>>16), byte(v>>8), byte(v)).To4()
}
