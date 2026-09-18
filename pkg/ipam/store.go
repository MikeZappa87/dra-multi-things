package ipam

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	bbolt "go.etcd.io/bbolt"
)

const (
	defaultDBDir = "/var/lib/dra-ipam"
)

// Pool describes a CIDR-based IP allocation pool.
type Pool struct {
	Name     string    `json:"name"`
	CIDR     string    `json:"cidr"`
	Gateway  string    `json:"gateway,omitempty"`
	Routes   []Route   `json:"routes,omitempty"`
	Created  time.Time `json:"created"`
	Modified time.Time `json:"modified"`
}

// Route is a static route to be applied inside a pod netns.
type Route struct {
	Dst string `json:"dst"`
	Via string `json:"via,omitempty"`
}

// Lease describes a single IP allocation for a claim.
type Lease struct {
	ClaimUID      string    `json:"claimUID"`
	PodUID        string    `json:"podUID"`
	HostInterface string    `json:"hostInterface,omitempty"`
	Interface     string    `json:"interface"`
	PoolName      string    `json:"poolName"`
	IP            string    `json:"ip"`
	CIDR          string    `json:"cidr"`
	Gateway       string    `json:"gateway,omitempty"`
	Routes        []Route   `json:"routes,omitempty"`
	Created       time.Time `json:"created"`
	Updated       time.Time `json:"updated"`
	NetnsPath     string    `json:"netnsPath,omitempty"`
}

// Store persists pools and leases in a bbolt DB.
type Store struct {
	db *bbolt.DB
}

// NewStore opens or creates the IPAM bbolt database.
func NewStore(path string) (*Store, error) {
	if path == "" {
		path = filepath.Join(defaultDBDir, "ipam.db")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, fmt.Errorf("create ipam dir: %w", err)
	}
	bd, err := bbolt.Open(path, 0600, nil)
	if err != nil {
		return nil, fmt.Errorf("open bbolt db: %w", err)
	}
	store := &Store{db: bd}
	if err := store.init(); err != nil {
		bd.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) init() error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		for _, bucket := range []string{"pools", "leases"} {
			if _, err := tx.CreateBucketIfNotExists([]byte(bucket)); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) CreatePool(pool Pool) error {
	if pool.Name == "" {
		return fmt.Errorf("pool name is required")
	}
	if _, _, err := net.ParseCIDR(pool.CIDR); err != nil {
		return fmt.Errorf("invalid CIDR %q: %w", pool.CIDR, err)
	}
	pool.Created = time.Now().UTC()
	pool.Modified = pool.Created
	data, err := json.Marshal(pool)
	if err != nil {
		return fmt.Errorf("marshal pool: %w", err)
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte("pools"))
		return b.Put([]byte(pool.Name), data)
	})
}

func (s *Store) GetPool(name string) (*Pool, error) {
	var out Pool
	if err := s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte("pools"))
		v := b.Get([]byte(name))
		if v == nil {
			return fmt.Errorf("pool %q not found", name)
		}
		return json.Unmarshal(v, &out)
	}); err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *Store) ListPools() ([]Pool, error) {
	var out []Pool
	if err := s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte("pools"))
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var p Pool
			if err := json.Unmarshal(v, &p); err != nil {
				return err
			}
			out = append(out, p)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *Store) SaveLease(lease Lease) error {
	lease.Updated = time.Now().UTC()
	if lease.Created.IsZero() {
		lease.Created = lease.Updated
	}
	data, err := json.Marshal(lease)
	if err != nil {
		return fmt.Errorf("marshal lease: %w", err)
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte("leases"))
		return b.Put([]byte(lease.ClaimUID), data)
	})
}

func (s *Store) GetLeaseByClaim(claimUID string) (*Lease, error) {
	var out Lease
	if err := s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte("leases"))
		v := b.Get([]byte(claimUID))
		if v == nil {
			return fmt.Errorf("lease for claim %q not found", claimUID)
		}
		return json.Unmarshal(v, &out)
	}); err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *Store) DeleteLease(claimUID string) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte("leases"))
		return b.Delete([]byte(claimUID))
	})
}

func (s *Store) ListLeases() ([]Lease, error) {
	var out []Lease
	if err := s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte("leases"))
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var l Lease
			if err := json.Unmarshal(v, &l); err != nil {
				return err
			}
			out = append(out, l)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ClaimUID < out[j].ClaimUID })
	return out, nil
}

func (s *Store) LeaseExistsForIP(poolName, ip string) (bool, error) {
	leases, err := s.ListLeases()
	if err != nil {
		return false, err
	}
	for _, lease := range leases {
		if lease.PoolName == poolName && lease.IP == ip {
			return true, nil
		}
	}
	return false, nil
}

func (s *Store) ListUsedIPs(poolName string) ([]string, error) {
	leases, err := s.ListLeases()
	if err != nil {
		return nil, err
	}
	used := make([]string, 0)
	for _, lease := range leases {
		if lease.PoolName == poolName {
			used = append(used, lease.IP)
		}
	}
	sort.Strings(used)
	return used, nil
}

func (s *Store) IsPoolConfigured(name string) bool {
	_, err := s.GetPool(name)
	return err == nil
}

func (s *Store) PoolKey(name string) string {
	return strings.TrimSpace(name)
}
