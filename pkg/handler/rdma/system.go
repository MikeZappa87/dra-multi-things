package rdma

import (
	"sync"

	"github.com/vishvananda/netlink"
	"k8s.io/klog/v2"
)

// NetnsMode represents the RDMA network namespace mode configured on the system.
//
//   - shared (default):    all network namespaces see all RDMA devices.
//     Multiple containers can open the same uverbs device concurrently,
//     each getting independent protection domains and queue pairs.
//   - exclusive:           RDMA devices are bound to a specific netns.
//     The device must be moved with RdmaLinkSetNsFd before a container
//     can use it; only one netns owns it at a time.
type NetnsMode string

const (
	NetnsShared    NetnsMode = "shared"
	NetnsExclusive NetnsMode = "exclusive"
)

var (
	cachedMode NetnsMode
	detectOnce sync.Once
)

// DetectNetnsMode returns the system-wide RDMA network namespace mode.
// The result is cached for the lifetime of the process (the mode cannot
// change without a system reconfiguration and driver restart).
//
// Uses the netlink RDMA API (equivalent to `rdma system show netns`).
// Falls back to "shared" if detection fails (e.g., no RDMA kernel modules).
func DetectNetnsMode() NetnsMode {
	detectOnce.Do(func() {
		mode, err := netlink.RdmaSystemGetNetnsMode()
		if err != nil {
			klog.V(2).Infof("Could not detect RDMA netns mode (%v), defaulting to shared", err)
			cachedMode = NetnsShared
			return
		}

		switch mode {
		case "exclusive":
			klog.Infof("Detected RDMA netns mode: exclusive")
			cachedMode = NetnsExclusive
		default:
			klog.Infof("Detected RDMA netns mode: shared")
			cachedMode = NetnsShared
		}
	})
	return cachedMode
}

// ResetDetectedMode clears the cached mode so the next call to
// DetectNetnsMode re-runs detection.  Intended for tests only.
func ResetDetectedMode() {
	detectOnce = sync.Once{}
	cachedMode = ""
}
