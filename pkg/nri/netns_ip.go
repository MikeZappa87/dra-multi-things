package nri

import (
	"fmt"
	"net"
	"runtime"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"

	"github.com/example/dra-poc/pkg/ipam"
)

// ApplyLeaseToPodNetns applies the allocated IP, gateway, and static routes to a
// pod network namespace identified by the provided netns path.
func ApplyLeaseToPodNetns(netnsPath, iface string, lease *ipam.Lease) error {
	if netnsPath == "" {
		return fmt.Errorf("pod netns path is required")
	}
	if lease == nil {
		return fmt.Errorf("lease is required")
	}
	if iface == "" {
		iface = "eth1"
	}

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	hostNS, err := netns.Get()
	if err != nil {
		return fmt.Errorf("open current netns: %w", err)
	}
	defer hostNS.Close()

	podNS, err := netns.GetFromPath(netnsPath)
	if err != nil {
		return fmt.Errorf("open pod netns %q: %w", netnsPath, err)
	}
	defer podNS.Close()

	if err := netns.Set(podNS); err != nil {
		return fmt.Errorf("enter pod netns %q: %w", netnsPath, err)
	}
	defer func() {
		_ = netns.Set(hostNS)
	}()

	link, err := netlink.LinkByName(iface)
	if err != nil {
		return fmt.Errorf("find pod interface %s: %w", iface, err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("bring up pod interface %s: %w", iface, err)
	}

	addr, routes, err := buildNetlinkConfig(iface, lease)
	if err != nil {
		return err
	}
	if addr != nil {
		if err := netlink.AddrReplace(link, addr); err != nil {
			return fmt.Errorf("configure address %s on %s: %w", addr.IPNet.String(), iface, err)
		}
	}
	for _, route := range routes {
		if err := netlink.RouteReplace(route); err != nil {
			return fmt.Errorf("configure route %v on %s: %w", route, iface, err)
		}
	}
	return nil
}

// MoveInterfaceToPodNetns moves a host-side interface into a pod netns and
// gives it the name expected by the pod workload.
func MoveInterfaceToPodNetns(netnsPath, hostIface, podIface string) error {
	if netnsPath == "" || hostIface == "" || podIface == "" {
		return fmt.Errorf("netns path, host interface, and pod interface are required")
	}

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	hostNS, err := netns.Get()
	if err != nil {
		return fmt.Errorf("open current netns: %w", err)
	}
	defer hostNS.Close()

	link, err := netlink.LinkByName(hostIface)
	if err != nil {
		return fmt.Errorf("find host interface %s: %w", hostIface, err)
	}
	podNS, err := netns.GetFromPath(netnsPath)
	if err != nil {
		return fmt.Errorf("open pod netns %q: %w", netnsPath, err)
	}
	defer podNS.Close()

	if err := netlink.LinkSetNsFd(link, int(podNS)); err != nil {
		return fmt.Errorf("move interface %s to pod netns: %w", hostIface, err)
	}
	if err := netns.Set(podNS); err != nil {
		return fmt.Errorf("enter pod netns %q: %w", netnsPath, err)
	}
	defer netns.Set(hostNS)

	link, err = netlink.LinkByName(hostIface)
	if err != nil {
		return fmt.Errorf("find moved interface %s: %w", hostIface, err)
	}
	if err := netlink.LinkSetName(link, podIface); err != nil {
		return fmt.Errorf("rename interface %s to %s: %w", hostIface, podIface, err)
	}
	return nil
}

func buildNetlinkConfig(iface string, lease *ipam.Lease) (*netlink.Addr, []*netlink.Route, error) {
	if lease == nil {
		return nil, nil, fmt.Errorf("lease is required")
	}
	if iface == "" {
		iface = "eth1"
	}

	var addr *netlink.Addr
	var routes []*netlink.Route
	if lease.CIDR != "" {
		_, ipnet, err := net.ParseCIDR(lease.CIDR)
		if err != nil {
			return nil, nil, fmt.Errorf("parse CIDR %q: %w", lease.CIDR, err)
		}
		ip := net.ParseIP(lease.IP)
		if ip == nil {
			return nil, nil, fmt.Errorf("parse lease IP %q", lease.IP)
		}
		addr = &netlink.Addr{
			IPNet: &net.IPNet{IP: ip, Mask: ipnet.Mask},
		}
	}

	if lease.Gateway != "" {
		routes = append(routes, &netlink.Route{
			Gw:    net.ParseIP(lease.Gateway),
			Scope: netlink.SCOPE_UNIVERSE,
		})
	}
	for _, routeSpec := range lease.Routes {
		if routeSpec.Dst == "" {
			continue
		}
		_, dst, err := net.ParseCIDR(routeSpec.Dst)
		if err != nil {
			return nil, nil, fmt.Errorf("parse route dst %q: %w", routeSpec.Dst, err)
		}
		r := &netlink.Route{
			Dst:   dst,
			Scope: netlink.SCOPE_UNIVERSE,
		}
		if routeSpec.Via != "" {
			r.Gw = net.ParseIP(routeSpec.Via)
		}
		routes = append(routes, r)
	}
	return addr, routes, nil
}
