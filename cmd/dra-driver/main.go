package main

import (
	"context"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/klog/v2"

	"github.com/example/dra-poc/pkg/driver"
	"github.com/example/dra-poc/pkg/handler"
	"github.com/example/dra-poc/pkg/handler/combo"
	"github.com/example/dra-poc/pkg/handler/netdev"
	"github.com/example/dra-poc/pkg/handler/rdma"
	nriplugin "github.com/example/dra-poc/pkg/nri"
)

var (
	driverName string
	nodeName   string
	podUID     string
)

func main() {
	cmd := &cobra.Command{
		Use:   "dra-driver",
		Short: "Multi-device DRA driver supporting network, RDMA, and combo devices",
		Run:   run,
	}

	cmd.Flags().StringVar(&driverName, "driver-name", "dra.example.com", "Name of the DRA driver")
	cmd.Flags().StringVar(&nodeName, "node-name", "", "Name of the node (from downward API)")
	cmd.Flags().StringVar(&podUID, "pod-uid", "", "UID of this driver pod (from downward API, enables rolling updates)")

	if err := cmd.Execute(); err != nil {
		klog.Fatal(err)
	}
}

func run(cmd *cobra.Command, args []string) {
	if nodeName == "" {
		nodeName = os.Getenv("NODE_NAME")
		if nodeName == "" {
			klog.Fatal("node-name is required (use --node-name or NODE_NAME env var)")
		}
	}
	if podUID == "" {
		podUID = os.Getenv("POD_UID")
	}

	klog.Infof("Starting multi-device DRA driver: %s on node %s", driverName, nodeName)

	// Create RDMA netns tracker.  In exclusive mode this coordinates between
	// the DRA Prepare path (which registers pending moves) and the NRI plugin
	// (which performs the actual netlink move when the pod sandbox is created).
	rdmaTracker := nriplugin.NewRDMANetnsTracker()

	// Build the handler registry with all supported device handlers
	registry := buildHandlerRegistry(rdmaTracker)
	for typ, kinds := range registry.ListRegistered() {
		klog.Infof("Registered handlers for type=%s: %v", typ, kinds)
	}

	// Create the DRA plugin implementation
	plugin := driver.New(driverName, registry)

	// Build in-cluster Kubernetes client (required by kubeletplugin)
	config, err := rest.InClusterConfig()
	if err != nil {
		klog.Fatalf("Failed to get in-cluster config: %v", err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.Fatalf("Failed to create Kubernetes client: %v", err)
	}

	// Ensure the plugin directory exists so the kubelet plugin can create its
	// Unix domain socket.  The kubelet only provides the parent directory
	// (/var/lib/kubelet/plugins); the driver-specific subdirectory must be
	// created by the driver itself.
	pluginDir := filepath.Join("/var/lib/kubelet/plugins", driverName)
	if err := os.MkdirAll(pluginDir, 0750); err != nil {
		klog.Fatalf("Failed to create plugin directory %s: %v", pluginDir, err)
	}

	// Assemble kubeletplugin options
	opts := []kubeletplugin.Option{
		kubeletplugin.DriverName(driverName),
		kubeletplugin.NodeName(nodeName),
		kubeletplugin.KubeClient(clientset),
	}

	// Enable rolling updates when the pod UID is provided
	if podUID != "" {
		klog.Infof("Rolling update mode enabled (pod UID: %s)", podUID)
		opts = append(opts, kubeletplugin.RollingUpdate(types.UID(podUID)))
	}

	// Set up context with signal handling — clean shutdown is critical for
	// rolling updates so that stale sockets are removed.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		klog.Infof("Received signal %v, shutting down", sig)
		cancel()
	}()

	// Start the kubelet plugin helper — this handles:
	//   • gRPC server for DRA plugin API
	//   • kubelet plugin registration
	//   • serialization (in-memory mutex or file-lock for rolling updates)
	//   • ResourceClaim fetching before calling PrepareResourceClaims
	helper, err := kubeletplugin.Start(ctx, plugin, opts...)
	if err != nil {
		klog.Fatalf("Failed to start kubelet plugin: %v", err)
	}

	// Start the NRI plugin for RDMA netns management in exclusive mode.
	// The plugin receives RunPodSandbox/StopPodSandbox events and moves
	// RDMA devices into/out of pod network namespaces via netlink.
	if rdma.DetectNetnsMode() == rdma.NetnsExclusive {
		nriPlugin, err := nriplugin.NewPlugin(rdmaTracker)
		if err != nil {
			klog.Fatalf("Failed to create NRI plugin: %v", err)
		}
		go func() {
			if err := nriPlugin.Run(ctx); err != nil {
				klog.Errorf("NRI plugin exited: %v", err)
			}
		}()
		defer nriPlugin.Stop()
		klog.Info("NRI plugin started for exclusive RDMA netns mode")
	}

	// Publish ResourceSlices
	resources := driver.DiscoverResources(driverName, nodeName)
	if err := helper.PublishResources(ctx, resources); err != nil {
		klog.Errorf("Failed to publish resources: %v", err)
	}

	// Block until context is cancelled
	<-ctx.Done()
	klog.Info("Stopping helper")
	helper.Stop()
	klog.Info("Driver stopped")
}

// buildHandlerRegistry creates and populates the handler registry with all device handlers
func buildHandlerRegistry(rdmaTracker *nriplugin.RDMANetnsTracker) *handler.HandlerRegistry {
	registry := handler.NewHandlerRegistry()

	// Network device handlers
	registry.Register(&netdev.MacvlanHandler{})
	registry.Register(&netdev.IpvlanHandler{})
	registry.Register(&netdev.VethHandler{})
	registry.Register(&netdev.SriovVfHandler{})
	registry.Register(&netdev.DummyHandler{})
	registry.Register(&netdev.HostDeviceHandler{})
	registry.Register(&netdev.IpoibHandler{})

	// RDMA device handlers — pass the tracker for exclusive netns mode.
	uverbsHandler := &rdma.UverbsHandler{Tracker: rdmaTracker}
	registry.Register(uverbsHandler)

	// Combo device handlers (composed from others)
	// RoCE uses a dummy netdev handler by default for the network side;
	// in production, this could be a specific handler for the RoCE net interface
	dummyHandler := &netdev.DummyHandler{}
	roceHandler := combo.NewRoCEHandler(uverbsHandler, dummyHandler)
	registry.Register(roceHandler)

	return registry
}
