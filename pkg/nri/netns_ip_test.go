package nri

import (
	"testing"

	"github.com/example/dra-poc/pkg/ipam"
)

func TestBuildNetlinkConfig(t *testing.T) {
	lease := &ipam.Lease{
		IP:      "10.240.0.10",
		CIDR:    "10.240.0.0/24",
		Gateway: "10.240.0.1",
		Routes: []ipam.Route{
			{Dst: "10.200.0.0/16", Via: "10.240.0.1"},
		},
	}

	addr, routes, err := buildNetlinkConfig("vlan100", lease)
	if err != nil {
		t.Fatalf("buildNetlinkConfig() error = %v", err)
	}
	if addr == nil {
		t.Fatal("expected address to be built")
	}
	if addr.IPNet == nil || addr.IPNet.IP.String() != "10.240.0.10" {
		t.Fatalf("addr IP = %v, want 10.240.0.10", addr.IPNet)
	}
	if len(routes) != 2 {
		t.Fatalf("len(routes) = %d, want 2", len(routes))
	}
	if routes[0].Gw == nil || routes[0].Gw.String() != "10.240.0.1" {
		t.Fatalf("default route gw = %v, want 10.240.0.1", routes[0].Gw)
	}
}

func TestApplyLeaseToPodNetnsRequiresNetns(t *testing.T) {
	if err := ApplyLeaseToPodNetns("", "vlan100", &ipam.Lease{IP: "10.240.0.10", CIDR: "10.240.0.0/24"}); err == nil {
		t.Fatal("ApplyLeaseToPodNetns should fail when netns path is empty")
	}
}
