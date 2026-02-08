# DRA Multi-Device Driver

A Kubernetes [Dynamic Resource Allocation](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/) (DRA) driver that manages multiple device types through a unified handler framework. Devices are injected into containers using **CDI v1.1.0 netDevices** — moving network interfaces directly into a container's network namespace at the OCI runtime level.

## Supported Device Types

| Type | Kinds | Description |
|------|-------|-------------|
| **netdev** | `macvlan`, `ipvlan`, `veth`, `sriov-vf`, `dummy`, `host-device`, `ipoib` | Network interfaces created on-demand or moved into the pod |
| **rdma** | `uverbs` | RDMA userspace verbs devices (`/dev/infiniband/uverbsN`) |
| **combo** | `roce` | Composes an RDMA device + a network interface in a single claim |

## Architecture

```
┌──────────────────────────────────────────────────────────────────┐
│                          DRA Driver                              │
│                                                                  │
│  NodePrepareResources() ──► HandlerRegistry                      │
│                              │                                   │
│                              ├─► NetdevHandler                   │
│                              │    ├─ MacvlanHandler              │
│                              │    ├─ IpvlanHandler               │
│                              │    ├─ VethHandler                 │
│                              │    ├─ SriovVfHandler              │
│                              │    ├─ DummyHandler                │
│                              │    └─ HostDeviceHandler           │
│                              │                                   │
│                              ├─► RDMAHandler                     │
│                              │    └─ UverbsHandler               │
│                              │                                   │
│                              └─► ComboHandler (composes others)  │
│                                   └─ RoCEHandler                 │
└──────────────────────────────────────────────────────────────────┘
```

### Resource Capacity Model

Physical devices (SR-IOV VFs, RDMA HCAs) are enumerated 1:1 in the ResourceSlice. Virtual devices that are created on-demand use the **DRAConsumableCapacity** feature gate — a single `netdev-virtual` device is published with `allowMultipleAllocations: true` and a consumable `slots` capacity. Each allocation consumes one slot, letting the scheduler track how many virtual devices a node can support without needing to pre-create fake device entries.

```yaml
# What the driver publishes for virtual devices
devices:
  - name: netdev-virtual
    allowMultipleAllocations: true
    attributes:
      dra.example.com/type:  { string: "netdev" }
      dra.example.com/kind:  { string: "virtual" }
    capacity:
      dra.example.com/slots:
        value: "128"
        requestPolicy:
          default: "1"
```

### State Persistence

Allocations are persisted to disk (`.alloc.json` sidecar files alongside CDI specs in `/etc/cdi/`). On driver restart, allocations are restored from disk and `NodePrepareResources` is idempotent — it returns cached results for already-prepared claims instead of re-creating devices.

## Prerequisites

- Docker
- [kind](https://kind.sigs.k8s.io/) (Kubernetes in Docker)
- kubectl
- Go 1.25+
- **containerd ≥ v2.2.1** — required for CDI `netDevices` injection support
- **runc ≥ v1.4.0** — required for CDI spec 1.1.0 (`netDevices` field)

> The included custom kind node image (`kind-node/Dockerfile`) bundles these versions automatically.

## Quick Start

```bash
# Full pipeline: cluster → build → deploy → test
make all

# Or step by step:
make cluster       # Create kind cluster (builds custom node image if needed)
make build load    # Build driver image and load into kind
make deploy        # Deploy driver DaemonSet + DeviceClasses

# Run the example workloads
make network-test  # Deploy a pod with a dummy network interface
make network-check # Verify the interface was injected
```

## Examples

### Single Network Interface

Deploy a pod with a dummy network device (`net1`):

```bash
kubectl apply -f deploy/deployment.yaml
```

The dummy example from `deployment.yaml` creates a `ResourceClaimTemplate` that requests a netdev and configures it as a dummy interface named `net1`:

```yaml
config:
- requests: ["nic-request"]
  opaque:
    driver: dra.example.com
    parameters:
      type: netdev
      netdev:
        kind: dummy
        interfaceName: net1
```

### Multiple Network Interfaces (Multi-NIC)

Each claim maps to exactly one device. For multiple interfaces, use multiple claims:

```bash
kubectl apply -f deploy/multi-nic-deployment.yaml
```

This creates two `ResourceClaimTemplate`s — one for a macvlan (`data0`) and one for a dummy (`ctrl0`):

```yaml
# Pod references both claims
resourceClaims:
  - name: data-net
    resourceClaimTemplateName: data-net-claim
  - name: control-net
    resourceClaimTemplateName: control-net-claim
```

Verify both interfaces inside the pod:

```bash
kubectl exec deployment/multi-nic-test -- ip -brief link show
# eth0@if14  UP  ...          ← CNI (standard pod networking)
# data0@if11 UP  ...          ← macvlan via DRA
# ctrl0      UNKNOWN  ...     ← dummy via DRA
```

### Other Device Types

`deployment.yaml` also includes claim templates for:

- **Macvlan** — `macvlan-claim-template` (bridge mode off `eth0`, interface `data0`)
- **RDMA** — `rdma-claim-template` (uverbs device)
- **RoCE** — `roce-claim-template` (combo: RDMA + dummy interface `rdma0`)

## Project Structure

```
.
├── cmd/dra-driver/
│   └── main.go                  # Entrypoint: gRPC server + publisher + plugin registration
├── pkg/
│   ├── driver/
│   │   ├── driver.go            # DRA gRPC server (Prepare/Unprepare + state persistence)
│   │   └── publisher.go         # ResourceSlice publisher (device discovery)
│   ├── handler/
│   │   ├── types.go             # DeviceHandler interface, registry, config types
│   │   ├── registry.go          # HandlerRegistry (type → kind → handler dispatch)
│   │   ├── netdev/              # macvlan, ipvlan, veth, sriov, dummy, host-device handlers
│   │   ├── rdma/                # uverbs handler
│   │   └── combo/               # roce handler (composes netdev + rdma)
│   └── plugin/
│       └── registration.go      # Kubelet plugin registration
├── deploy/
│   ├── namespace.yaml           # dra-system namespace
│   ├── driver.yaml              # DaemonSet + RBAC
│   ├── resourceclass.yaml       # DeviceClasses (network-devices, rdma-devices, roce-devices)
│   ├── deployment.yaml          # Example workloads + ResourceClaimTemplates
│   └── multi-nic-deployment.yaml # Multi-NIC example (2 claims per pod)
├── kind-node/
│   └── Dockerfile               # Custom kind node image (runc 1.4.0 + containerd CDI 1.1.0)
├── kind-config.yaml             # Kind cluster config (DRA + DRAConsumableCapacity gates)
├── design.md                    # Detailed design document
├── Dockerfile
├── Makefile
└── go.mod
```

## Feature Gates

The kind cluster is configured with:

| Feature Gate | Purpose |
|---|---|
| `DynamicResourceAllocation` | Core DRA support |
| `DRAConsumableCapacity` | Allows a single device to be shared across multiple allocations with tracked capacity |

## Makefile Targets

| Target | Description |
|--------|-------------|
| `make all` | Full pipeline: cluster → build → load → deploy → test |
| `make cluster` | Create kind cluster (auto-builds node image if needed) |
| `make build` | Build the driver container image |
| `make load` | Load the image into the kind cluster |
| `make deploy` | Apply all deployment manifests |
| `make undeploy` | Remove all deployed resources |
| `make network-test` | Deploy the dummy netdev test workload |
| `make network-check` | Verify injected interfaces in test pods |
| `make restart` | Rolling restart of the driver DaemonSet |
| `make logs` | Tail driver logs |
| `make debug` | Print cluster state (nodes, pods, slices, claims) |
| `make clean` | Delete the kind cluster |

## Manual Debugging

```bash
# Driver logs
kubectl logs -n dra-system -l app=dra-driver -f

# Published devices
kubectl get resourceslices -o json | jq '.items[].spec.devices[].name'

# Claim allocation status
kubectl get resourceclaims

# CDI specs on a node
docker exec dra-demo-worker ls -la /etc/cdi/

# Cluster overview
make debug
```

