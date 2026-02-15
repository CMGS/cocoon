# CNI-Based Networking

**Version**: 1.0
**Status**: Planned
**Phase**: Phase 2
**Last Updated**: 2026-02-15

## Executive Summary

This document specifies the design for CNI (Container Network Interface) based networking in Cocoon. VMs currently boot without network interfaces. This design introduces a CNI plugin integration layer that creates TAP devices, configures IP addressing via IPAM plugins, and passes the TAP device to Cloud Hypervisor as a virtio-net backend. The approach reuses the existing CNI plugin ecosystem (bridge, macvlan, host-local, dhcp, portmap) rather than building custom networking, providing compatibility with Kubernetes, Podman, and other CNI consumers. The default behavior remains `--network none` for backward compatibility with the AI Agent sandbox use case.

## Table of Contents

1. [Overview](#1-overview)
2. [Design](#2-design)
3. [Configuration](#3-configuration)
4. [Implementation](#4-implementation)
5. [CLI](#5-cli)
6. [IP Address Management](#6-ip-address-management)
7. [Port Forwarding](#7-port-forwarding)
8. [DNS](#8-dns)
9. [Error Handling](#9-error-handling)
10. [Security](#10-security)
11. [Testing](#11-testing)
12. [Cross-References](#12-cross-references)

---

## 1. Overview

### 1.1 Problem Statement

Cocoon Phase 1 VMs boot with no network interfaces. This is intentional for AI Agent sandboxes where network isolation is a security requirement. However, several use cases require network connectivity:

1. **Package installation**: VMs that need to install software from upstream repositories require outbound internet access. Without networking, all packages must be baked into the base image at build time.
2. **Service hosting**: Running a web server, database, or API inside a VM requires inbound connectivity from the host or other VMs.
3. **Multi-VM communication**: Workloads spanning multiple VMs (e.g., a frontend VM and a backend VM) need a private network segment for inter-VM traffic.
4. **Development workflows**: Developers iterating on VM-based applications need SSH or HTTP access into the guest without rebuilding the image.

### 1.2 Approach: CNI Plugin Integration

Rather than building a bespoke networking layer, Cocoon delegates all network plumbing to CNI plugins -- the same plugin interface used by Kubernetes (kubelet), Podman, CRI-O, and other container runtimes.

**Why CNI?**

- **Ecosystem reuse**: Hundreds of CNI plugins exist for bridge, macvlan, ipvlan, VXLAN, Calico, Cilium, and more. Cocoon does not need to re-implement any of them.
- **Separation of concerns**: CNI plugins handle the control plane (TAP creation, IP allocation, routing, iptables rules). Cocoon handles the data plane integration (passing TAP to Cloud Hypervisor).
- **Configuration portability**: CNI network configurations are JSON files stored in `/etc/cni/net.d/`. Administrators can reuse configurations across Cocoon, Podman, and Kubernetes.
- **IPAM abstraction**: IP address management is pluggable (host-local, dhcp, static). No IP allocation logic in Cocoon.

### 1.3 High-Level Flow

```
cocoon create --network bridge myimage
    |
    v
[1] Create persistent network namespace for VM
    |
    v
[2] CNI ADD (bridge plugin, inside netns)
    |-- Creates vethN pair
    |-- Moves one end into network namespace (eth0)
    |-- Attaches other end to host bridge (cni0)
    |-- Calls IPAM plugin (host-local) -> 10.88.0.5/16
    |
    v
[3] Cocoon network shim (inside netns)
    |-- Creates TAP device (tap0)
    |-- Sets up tc mirred: eth0 <-> tap0 bidirectional redirect
    |
    v
[4] Cloud Hypervisor launch (via nsenter --net=<netns>)
    |-- CH process enters VM's network namespace
    |-- --net tap=tap0,mac=AA:BB:CC:DD:EE:01
    |-- CH sees tap0 directly (same namespace)
    |
    v
[5] Guest sees eth0 (virtio-net)
    |-- IP configured via DHCP from dnsmasq (serving CNI IPAM result)
    |-- Gateway and DNS set from CNI result
```

**Key design decisions**:

- **nsenter for CH**: Cloud Hypervisor enters the VM's network namespace via `nsenter --net=<path>`. This is the only change to the CH launch path. CH's API socket is a Unix domain socket on the filesystem, which is NOT affected by network namespaces, so the host can still communicate with CH normally.
- **tc mirred for L2 bridging**: Traffic Control `mirred` rules redirect all packets between the CNI interface (`eth0`) and the TAP device (`tap0`) at layer 2. This avoids creating an additional Linux bridge inside the namespace, and preserves all CNI-installed IP addresses, routes, and iptables rules on `eth0` without modification.
- **IP ownership model**: The CNI IPAM plugin assigns an IP address to `eth0` (the veth inside the netns). The `tc mirred redirect` intercepts all ingress packets on `eth0` at the Traffic Control layer *before* the namespace's IP stack processes them, forwarding them directly to `tap0` and hence to the guest. The guest obtains the same IP via DHCP from a per-bridge dnsmasq instance that serves static leases matching the CNI IPAM allocation. There is no IP conflict because the namespace never processes packets for that IP on the data path — `tc mirred` diverts them first. The IP on the namespace `eth0` exists solely for CNI/IPAM bookkeeping (address pool tracking, route installation, iptables rules). This is the same model used by [Kata Containers' `tc-redirect-tap`](https://github.com/kata-containers/kata-containers/tree/main/tools/networking/cmd/tc-redirect-tap).
- **Broad CNI compatibility**: Any CNI plugin that produces a standard network interface inside a netns works with this model, with caveats for eBPF-based plugins. See the compatibility matrix below.

**CNI Plugin Compatibility Matrix**:

Tested and Supported:

| Plugin | Status | Notes |
|--------|--------|-------|
| bridge | Supported | Default CNI plugin, fully tested |
| ptp | Supported | Point-to-point, works with TC redirect |
| macvlan | Supported | Direct L2 attachment |
| host-local IPAM | Supported | Default IPAM backend |
| DHCP IPAM | Supported | Requires dhcp daemon |
| portmap | Supported | Host port forwarding |

Known Limitations:

| Plugin | Status | Notes |
|--------|--------|-------|
| Cilium | Experimental | TC mirred redirect may bypass Cilium's eBPF datapath; requires version-specific testing |
| Calico (eBPF mode) | Untested | Similar eBPF bypass concerns as Cilium |

**Why some plugins are incompatible**: TC mirred redirect operates at Layer 2, forwarding packets directly between the CNI veth and the TAP device. This bypasses any eBPF programs or iptables rules attached to the netns-side interface. Plugins that rely on netns-internal packet processing (e.g., Cilium's eBPF programs attached at the `tc ingress` hook, or Calico's eBPF datapath) may not work correctly because the mirred redirect replaces the ingress qdisc. Plugins that operate on the host side of the veth pair (e.g., Calico in iptables mode) are unaffected.

### 1.4 Backward Compatibility

The default network mode is `none`, matching Phase 1 behavior:

```bash
# Phase 1 behavior (unchanged)
cocoon create myimage              # No network (default)
cocoon create --network none myimage  # Explicit no network

# Phase 2 additions
cocoon create --network bridge myimage  # Bridge network
cocoon create --network bridge --network macvlan0 myimage  # Multiple networks
```

VMs created without `--network` (or with `--network none`) have no TAP device, no `--net` argument to Cloud Hypervisor, and no CNI invocations. This is a zero-cost path with no behavioral change.

---

## 2. Design

### 2.0 Design Constraints and Prerequisites

**Cloud Hypervisor Version Requirement**: Phase 2 networking requires Cloud Hypervisor v38.0 or later (the same minimum version as Phase 1; see [08-dependencies.md](./08-dependencies.md)). Older CH versions auto-assigned a default `192.168.249.1/24` address to virtio-net devices, which conflicts with CNI-managed IP assignment. CH v38.0 no longer applies this default when a TAP device is provided via `--net tap=...`, so no version bump is required beyond the existing Phase 1 minimum. `cocoon doctor` already enforces `>= v38.0`; the Phase 2 networking check reuses this existing validation.

### 2.1 Architecture Overview

```
+---------------------+     +-------------------+     +-------------------+
|  cocoon create      |     |  network.Manager  |     |  CNI Plugins      |
|  --network bridge   |---->|  SetupNetwork()   |---->|  bridge + host-   |
|                     |     |  TeardownNetwork()|     |  local + portmap  |
+---------------------+     +---+---------------+     +-------------------+
                                 |                            |
                                 |  1. CNI ADD in netns       |
                                 |  2. Create TAP in netns    |
                                 |  3. tc mirred eth0<->tap0  |
                                 v                            |
                          +--------------------+              |
                          |  VM Engine         |              |
                          |  nsenter --net=... |              |
                          |  CH --net tap=tap0 |<-------------+
                          +--------------------+
                                 |
                                 v
                          +--------------------+
                          | Cloud Hypervisor   |  (runs inside VM netns)
                          | virtio-net <-> TAP |
                          +--------------------+    API socket: host filesystem
                                 |                  (not affected by netns)
                                 v
                          +--------------------+
                          |   Guest VM         |
                          |   eth0: 10.88.0.5  |
                          +--------------------+
```

### 2.2 Network Namespace Strategy

Each VM gets its own network namespace. CNI plugins operate inside this namespace, and Cloud Hypervisor enters it via `nsenter` at launch time.

```
Host default namespace:
  cni0 (bridge) ---- vethXXXX_host

VM network namespace (/run/cocoon/vms/{vm-id}/netns):
  eth0 (veth, CNI-created, holds IP)
      |
      +-- tc mirred redirect -->  tap0 (TAP, Cocoon-created)
      |                             |
      +-- <-- tc mirred redirect --+
                                    |
                                    +--- Cloud Hypervisor (nsenter --net=..., --net tap=tap0)
                                           |
                                           +--- Guest VM (virtio-net -> eth0)
```

**Why per-VM namespaces?**

- **CNI contract**: CNI plugins require a namespace path (`CNI_NETNS`). This is non-negotiable for CNI compatibility.
- **Isolation**: Each VM's network stack is isolated from the host and other VMs at the kernel level.
- **Cleanup**: Deleting the namespace automatically destroys all devices inside it.
- **Multiple networks**: A single namespace can hold multiple interfaces from different CNI networks.

**Why nsenter for Cloud Hypervisor?**

- Network namespaces only affect the network view. File system, PID namespace, and mount namespace are shared. CH's API socket (`/run/cocoon/vms/{vmID}/api.sock`) is a Unix domain socket on the host filesystem and remains accessible from the host regardless of CH's network namespace.
- `nsenter` is part of `util-linux`, present on all Linux distributions.
- This is a single-line change to the CH launch path: prefix the command with `nsenter --net=<path> --`.

**Why tc mirred instead of an internal bridge?**

- **Preserves CNI state**: The IP address, routes, and iptables rules installed by CNI remain on `eth0` unchanged. No need to flush and re-assign IPs.
- **No extra devices**: No Linux bridge inside the namespace. The `tc` redirect operates at the kernel traffic control layer with zero additional network devices.
- **Proven model**: This is the same approach used by Kata Containers (`tc-redirect-tap` plugin) for connecting CNI interfaces to VM TAP devices.

### 2.3 TAP Device and TC Redirect Lifecycle

**Creation** (during `cocoon create --network`):

1. Cocoon creates a network namespace for the VM.
2. CNI ADD is invoked, creating a veth pair and bridge attachment. The namespace-side interface (`eth0`) receives an IP from the IPAM plugin.
3. Inside the namespace, Cocoon creates a persistent TAP device (`tap0`).
4. TC mirred rules are installed to redirect all traffic bidirectionally between `eth0` and `tap0`.
5. The TAP device name and namespace path are recorded in `config.json`.

**Runtime** (during `cocoon start`):

1. Cloud Hypervisor is launched via `nsenter --net=<netns_path>` with `--net tap=tap0`.
2. CH opens the TAP device (visible because CH is in the same namespace) and attaches it as a virtio-net backend.
3. Guest sees the device as `eth0` (or `eth1`, `eth2` for additional networks).
4. Packets from the guest flow: guest eth0 -> virtio-net -> tap0 -> tc mirred -> eth0 (veth) -> host bridge -> internet.

**Destruction** (during `cocoon delete`):

1. CNI DEL is invoked to clean up routes, iptables rules, and IPAM allocations.
2. The network namespace is deleted, which automatically destroys all devices (veth, TAP) and TC rules inside it.
3. If CNI DEL fails, Cocoon still deletes the namespace (force cleanup).

**Network Persistence Across Stop/Start (Design Decision)**:

Stopping a VM (via `cocoon stop`) does NOT tear down its network namespace, TAP device, or CNI allocation. This is intentional:

- **IP address stability**: The VM retains its IPAM allocation across stop/start cycles, avoiding IP churn and DHCP lease renegotiation.
- **Faster restarts**: No need to re-run CNI ADD, recreate TAP, or reinstall TC rules on `cocoon start` -- the network stack is already in place.
- **Trade-off**: Port mappings (iptables DNAT rules) remain active on stopped VMs, and the IPAM slot stays allocated even while the VM is not running. This is acceptable for the expected usage patterns (VMs are either running or deleted, not parked in STOPPED state for extended periods).

IPAM allocation happens only during `cocoon create` (via CNI ADD). On subsequent `cocoon start` calls, the existing network namespace and IP assignments are reused without re-running IPAM. IP addresses persist across stop/start cycles as long as the IPAM allocation files (e.g., `/var/lib/cni/networks/<network>/<ip>`) remain on disk.

Network teardown occurs only during `cocoon delete` (see Destruction above) or when the host reboots (see Section 2.4). A future `--network-cleanup-on-stop` option may be added if users require network teardown on stop.

### 2.4 Network State After Host Reboot

`/run/cocoon/` is a tmpfs mount, so all network namespace files, veth/TAP devices, bind mounts, and TC redirect rules are lost on host reboot. However, persistent state may partially survive:

- **Lost**: Network namespace bind mounts (`/run/cocoon/vms/{vm-id}/netns`), all veth pairs, TAP devices, TC rules, and iptables entries installed by CNI plugins.
- **Persists**: IPAM state on disk (if using `host-local` IPAM, allocations in `/var/lib/cni/networks/` survive). VM metadata on disk (`metadata.json`) still records `network_state.netns_path`, but the path no longer exists.

**Start() Idempotency (Network Recreation)**:

When `Start()` is called on a STOPPED VM whose network namespace no longer exists (detected by stat-ing the `netns_path` from metadata), the network layer must transparently recreate the full network stack. The recreation sequence:

1. **Detect missing netns**: Stat `metadata.NetworkState.NetNSPath`. If it returns `ENOENT`, proceed with recreation.
2. **Recreate network namespace**: Call `CreateNetNS(vmID)` to create a new namespace at the same path as the original.
3. **Re-run CNI ADD**: Invoke `AddNetwork()` for each attachment in `config.Network.Networks`. The IPAM plugin may return the same IP (if `host-local` allocation files survived) or a new IP (if they were cleaned up or if using `dhcp` IPAM).
4. **Recreate TAP device and TC redirect rules**: Call `SetupTAPAndRedirect()` for each attachment, restoring the bidirectional mirred rules.
5. **Update metadata**: Write the new `NetworkState` (which may have new IPs) to `metadata.json`.
6. **Proceed with CH launch**: Launch Cloud Hypervisor via `nsenter --net=<netns_path>` as normal.

This must be **idempotent**: calling `Start()` when the network is already fully set up (namespace exists, TAP present, TC rules active) must be a no-op for the network layer. The implementation checks for the existence of each resource before creating it.

**Note**: If the IPAM plugin assigns a different IP after reboot, Cocoon updates the dnsmasq static lease for the VM's MAC address to reflect the new IP. The guest's DHCP client obtains the updated IP on boot, so there is no stale-IP mismatch. This is an advantage of DHCP-based configuration over static injection approaches.

### 2.5 CNI Plugin Chain

Cocoon supports chained CNI plugins, where multiple plugins execute in sequence. A typical chain:

```
bridge (creates veth + bridge attachment)
  -> host-local (allocates IP from a local pool)
    -> portmap (installs DNAT rules for port forwarding)
```

The chain is defined in the CNI network configuration file, not in Cocoon's code. Cocoon invokes the chain as an opaque unit via `libcni`.

### 2.6 Multiple Networks Per VM

A VM can be attached to multiple CNI networks via repeated `--network` flags:

```bash
cocoon create --network bridge --network isolated myimage
```

Each `--network` invocation:
1. Calls CNI ADD for that network.
2. Creates a separate TAP device (tap0, tap1, ...).
3. Passes a separate `--net tap=tapN` argument to Cloud Hypervisor.

Inside the guest, each network appears as a separate `ethN` interface:
- `eth0` from `bridge` network
- `eth1` from `isolated` network

---

## 3. Configuration

### 3.1 NetworkConfig (Per-VM)

The `NetworkConfig` struct is stored in `config.json` as part of the VM configuration. It is immutable after creation.

```go
// NetworkConfig describes the network configuration for a VM.
// Stored in config.json, immutable after creation.
type NetworkConfig struct {
    // Networks is the list of CNI networks to attach.
    // Empty slice means no networking (backward compatible with Phase 1).
    Networks []NetworkAttachment `json:"networks,omitempty"`
}

// NetworkAttachment represents a single CNI network attachment.
type NetworkAttachment struct {
    // Name is the CNI network name (matches the "name" field in the CNI config file).
    Name string `json:"name"`

    // InterfaceName is the guest-visible interface name (eth0, eth1, ...).
    // Assigned sequentially during creation.
    InterfaceName string `json:"interface_name"`

    // PortMappings defines host-to-guest port forwarding rules.
    // Passed to the CNI portmap plugin via runtime config.
    PortMappings []PortMapping `json:"port_mappings,omitempty"`
}

// PortMapping defines a single host:guest port forwarding rule.
type PortMapping struct {
    HostPort      int    `json:"host_port"`
    ContainerPort int    `json:"container_port"` // CNI uses "container" terminology
    Protocol      string `json:"protocol"`       // "tcp" or "udp"
    HostIP        string `json:"host_ip,omitempty"` // "" means 0.0.0.0
}
```

### 3.2 NetworkState (Per-VM, Mutable)

Network runtime state is stored in `metadata.json` and updated during CNI ADD/DEL.

```go
// NetworkState tracks runtime network state for a VM.
// Stored in metadata.json, updated on CNI ADD/DEL.
type NetworkState struct {
    // Attachments maps interface name to the runtime state for that attachment.
    Attachments []NetworkAttachmentState `json:"network_attachments,omitempty"`

    // NetNSPath is the path to the VM's network namespace.
    // Empty if no networking is configured.
    NetNSPath string `json:"netns_path,omitempty"`
}

// NetworkAttachmentState is the runtime state for a single network attachment.
type NetworkAttachmentState struct {
    // NetworkName is the CNI network name (matches config).
    NetworkName string `json:"network_name"`

    // InterfaceName is the interface name inside the namespace (eth0, eth1, ...).
    InterfaceName string `json:"interface_name"`

    // TAPDevice is the TAP device name inside the namespace.
    TAPDevice string `json:"tap_device"`

    // MACAddress is the MAC assigned to the TAP device.
    MACAddress string `json:"mac_address"`

    // IPs is the list of IP addresses assigned by the IPAM plugin.
    IPs []string `json:"ips,omitempty"` // CIDR notation: "10.88.0.5/16"

    // Gateway is the default gateway assigned by the IPAM plugin.
    Gateway string `json:"gateway,omitempty"`

    // DNS servers returned by the CNI plugin.
    DNS []string `json:"dns,omitempty"`
}
```

### 3.3 VMConfig Extension

The existing `VMConfig` struct (`types/config.go`) gains a `Network` field:

```go
type VMConfig struct {
    // ... existing fields ...

    // Network configuration. Empty/nil means no networking (Phase 1 default).
    Network NetworkConfig `json:"network,omitempty"`
}
```

### 3.4 VMMetadataFile Extension

The existing `VMMetadataFile` struct gains a `NetworkState` field:

```go
type VMMetadataFile struct {
    // ... existing fields ...

    // NetworkState tracks runtime network state.
    // Populated during CNI ADD, cleared during CNI DEL.
    NetworkState NetworkState `json:"network_state,omitempty"`
}
```

### 3.5 CHVMConfig Extension

The `CHVMConfig` struct (`hypervisor/types.go`) gains a `Net` field for the `--net` argument:

```go
type CHVMConfig struct {
    CPUs    CHCPUConfig     `json:"cpus"`
    Memory  CHMemoryConfig  `json:"memory"`
    Disks   []CHDiskConfig  `json:"disks,omitempty"`
    Net     []CHNetConfig   `json:"net,omitempty"`    // NEW: network devices
    Serial  CHSerialConfig  `json:"serial"`
    Console CHConsoleConfig `json:"console"`
}

// CHNetConfig describes a single virtio-net device.
type CHNetConfig struct {
    // Tap is the TAP device name or file descriptor.
    Tap string `json:"tap,omitempty"`

    // MAC is the MAC address for the virtio-net device.
    // Format: "AA:BB:CC:DD:EE:FF".
    MAC string `json:"mac,omitempty"`

    // NumQueues is the number of virtio queues (default: 2, one TX + one RX).
    NumQueues int `json:"num_queues,omitempty"`

    // QueueSize is the depth of each virtio queue (default: 256).
    QueueSize int `json:"queue_size,omitempty"`
}
```

### 3.6 CNI Configuration Files

CNI network configurations live in `/etc/cni/net.d/` (the standard location). Cocoon reads these files using `libcni` — it does not maintain its own network configuration format.

**Example: Bridge network** (`/etc/cni/net.d/10-bridge.conflist`):

```json
{
  "cniVersion": "1.0.0",
  "name": "bridge",
  "plugins": [
    {
      "type": "bridge",
      "bridge": "cni0",
      "isGateway": true,
      "ipMasq": true,
      "ipam": {
        "type": "host-local",
        "ranges": [
          [{"subnet": "10.88.0.0/16"}]
        ],
        "routes": [
          {"dst": "0.0.0.0/0"}
        ]
      }
    },
    {
      "type": "portmap",
      "capabilities": {"portMappings": true},
      "snat": true
    }
  ]
}
```

**Example: Macvlan network** (`/etc/cni/net.d/20-macvlan.conflist`):

```json
{
  "cniVersion": "1.0.0",
  "name": "macvlan0",
  "plugins": [
    {
      "type": "macvlan",
      "master": "eth0",
      "mode": "bridge",
      "ipam": {
        "type": "dhcp"
      }
    }
  ]
}
```

### 3.7 CNI Plugin Binary Location

CNI plugins are searched in the following directories, in order:

1. `/opt/cni/bin` (standard CNI plugin directory)
2. `/usr/lib/cni` (distribution packages, e.g., Fedora)
3. `/usr/libexec/cni` (alternative distribution location)

The search path is configurable via the `COCOON_CNI_PATH` environment variable or the `cni_plugin_path` field in Cocoon's global configuration (`/etc/cocoon/config.toml`).

---

## 4. Implementation

### 4.1 Network Manager Interface

```go
package network

import (
    "context"

    "github.com/containernetworking/cni/libcni"
    cnitypes "github.com/containernetworking/cni/pkg/types"
)

// Manager handles CNI network operations for VMs.
type Manager interface {
    // AddNetwork invokes CNI ADD for a single network attachment.
    // Creates the TAP device inside the VM's network namespace.
    // Returns the attachment state (IP, gateway, DNS, TAP device name).
    AddNetwork(ctx context.Context, vmID string, netNSPath string, attachment NetworkAttachment) (*NetworkAttachmentState, error)

    // DeleteNetwork invokes CNI DEL for a single network attachment.
    // Cleans up IPAM allocations, iptables rules, and veth pairs.
    // The TAP device is destroyed when the namespace is deleted.
    DeleteNetwork(ctx context.Context, vmID string, netNSPath string, attachment NetworkAttachment) error

    // DeleteAllNetworks invokes CNI DEL for all network attachments of a VM.
    // Called during VM deletion. Errors are logged but do not block deletion.
    DeleteAllNetworks(ctx context.Context, vmID string, netNSPath string, attachments []NetworkAttachment) error

    // ListNetworks returns all available CNI network configurations
    // from the CNI config directory.
    ListNetworks() ([]*libcni.NetworkConfigList, error)

    // GetNetworkConfig loads a specific CNI network configuration by name.
    GetNetworkConfig(name string) (*libcni.NetworkConfigList, error)
}
```

### 4.2 Network Manager Implementation

```go
package network

import (
    "context"
    "crypto/rand"
    "fmt"
    "net"
    "os"
    "path/filepath"

    "github.com/containernetworking/cni/libcni"
    cnitypes "github.com/containernetworking/cni/pkg/types"
    current "github.com/containernetworking/cni/pkg/types/100"
)

// cniManager implements Manager using libcni.
type cniManager struct {
    cniConfig *libcni.CNIConfig
    confDir   string // e.g., /etc/cni/net.d
}

// NewManager creates a new CNI network manager.
func NewManager(confDir string, pluginDirs []string) Manager {
    return &cniManager{
        cniConfig: libcni.NewCNIConfig(pluginDirs, nil),
        confDir:   confDir,
    }
}

func (m *cniManager) AddNetwork(ctx context.Context, vmID string, netNSPath string, attachment NetworkAttachment) (*NetworkAttachmentState, error) {
    // 1. Load CNI network config.
    netConf, err := m.GetNetworkConfig(attachment.Name)
    if err != nil {
        return nil, fmt.Errorf("load CNI config %q: %w", attachment.Name, err)
    }

    // 2. Generate a deterministic MAC address for the TAP device.
    mac, err := generateMAC(vmID, attachment.InterfaceName)
    if err != nil {
        return nil, fmt.Errorf("generate MAC: %w", err)
    }

    // 3. Build the CNI runtime config.
    rt := &libcni.RuntimeConf{
        ContainerID: vmID,
        NetNS:       netNSPath,
        IfName:      attachment.InterfaceName,
        CapabilityArgs: map[string]interface{}{},
    }

    // 4. Add port mappings to capability args if present.
    if len(attachment.PortMappings) > 0 {
        rt.CapabilityArgs["portMappings"] = attachment.PortMappings
    }

    // 5. Invoke CNI ADD.
    result, err := m.cniConfig.AddNetworkList(ctx, netConf, rt)
    if err != nil {
        return nil, fmt.Errorf("CNI ADD for network %q: %w", attachment.Name, err)
    }

    // 6. Parse the CNI result.
    newResult, err := current.GetResult(result)
    if err != nil {
        return nil, fmt.Errorf("parse CNI result: %w", err)
    }

    // 7. Build and return the attachment state.
    // Note: TAP creation and tc mirred setup are handled separately by
    // SetupTAPAndRedirect() after CNI ADD completes. This keeps the CNI
    // manager decoupled from the TAP/redirect plumbing.
    state := &NetworkAttachmentState{
        NetworkName:   attachment.Name,
        InterfaceName: attachment.InterfaceName,
        TAPDevice:     "", // Set by caller after SetupTAPAndRedirect()
        MACAddress:    mac,
    }

    for _, ipConfig := range newResult.IPs {
        state.IPs = append(state.IPs, ipConfig.Address.String())
        if ipConfig.Gateway != nil {
            state.Gateway = ipConfig.Gateway.String()
        }
    }

    if newResult.DNS.Nameservers != nil {
        state.DNS = newResult.DNS.Nameservers
    }

    return state, nil
}

func (m *cniManager) DeleteNetwork(ctx context.Context, vmID string, netNSPath string, attachment NetworkAttachment) error {
    netConf, err := m.GetNetworkConfig(attachment.Name)
    if err != nil {
        return fmt.Errorf("load CNI config %q: %w", attachment.Name, err)
    }

    rt := &libcni.RuntimeConf{
        ContainerID: vmID,
        NetNS:       netNSPath,
        IfName:      attachment.InterfaceName,
    }

    if err := m.cniConfig.DelNetworkList(ctx, netConf, rt); err != nil {
        return fmt.Errorf("CNI DEL for network %q: %w", attachment.Name, err)
    }

    return nil
}

func (m *cniManager) DeleteAllNetworks(ctx context.Context, vmID string, netNSPath string, attachments []NetworkAttachment) error {
    var firstErr error
    // Delete in reverse order (last added, first removed).
    for i := len(attachments) - 1; i >= 0; i-- {
        if err := m.DeleteNetwork(ctx, vmID, netNSPath, attachments[i]); err != nil {
            if firstErr == nil {
                firstErr = err
            }
            // Log but continue — best-effort cleanup.
            fmt.Fprintf(os.Stderr, "warning: CNI DEL for %q failed: %v\n", attachments[i].Name, err)
        }
    }
    return firstErr
}

func (m *cniManager) ListNetworks() ([]*libcni.NetworkConfigList, error) {
    files, err := libcni.ConfFiles(m.confDir, []string{".conflist", ".conf"})
    if err != nil {
        return nil, fmt.Errorf("list CNI configs in %s: %w", m.confDir, err)
    }

    var configs []*libcni.NetworkConfigList
    for _, f := range files {
        if filepath.Ext(f) == ".conflist" {
            conf, err := libcni.ConfListFromFile(f)
            if err != nil {
                continue // Skip invalid configs.
            }
            configs = append(configs, conf)
        } else {
            conf, err := libcni.ConfFromFile(f)
            if err != nil {
                continue
            }
            confList, err := libcni.ConfListFromConf(conf)
            if err != nil {
                continue
            }
            configs = append(configs, confList)
        }
    }

    return configs, nil
}

func (m *cniManager) GetNetworkConfig(name string) (*libcni.NetworkConfigList, error) {
    configs, err := m.ListNetworks()
    if err != nil {
        return nil, err
    }
    for _, conf := range configs {
        if conf.Name == name {
            return conf, nil
        }
    }
    return nil, fmt.Errorf("CNI network %q not found in %s", name, m.confDir)
}
```

### 4.3 Network Namespace Management

```go
package network

import (
    "fmt"
    "os"
    "path/filepath"
    "runtime"
    "syscall"

    "golang.org/x/sys/unix"
)

// CreateNetNS creates a persistent network namespace for a VM.
// The namespace is bind-mounted to a file so it persists beyond the
// lifetime of any single process (required for CNI).
//
// Returns the path to the namespace file.
func CreateNetNS(vmID string) (string, error) {
    nsDir := "/run/cocoon/vms/" + vmID
    nsPath := filepath.Join(nsDir, "netns")

    // Ensure the directory exists.
    if err := os.MkdirAll(nsDir, 0755); err != nil {
        return "", fmt.Errorf("create netns dir: %w", err)
    }

    // Create the mount target file.
    f, err := os.Create(nsPath)
    if err != nil {
        return "", fmt.Errorf("create netns file: %w", err)
    }
    f.Close()

    // Create a new network namespace in a dedicated OS thread.
    // The goroutine is locked to the thread to prevent the Go runtime
    // from scheduling other goroutines on this thread (which would
    // see the wrong namespace).
    errCh := make(chan error, 1)
    go func() {
        runtime.LockOSThread()
        // Do NOT unlock — the thread has a modified namespace and must be
        // destroyed when the goroutine exits.

        // Save the current namespace.
        origNS, err := unix.Open("/proc/self/ns/net", unix.O_RDONLY|unix.O_CLOEXEC, 0)
        if err != nil {
            errCh <- fmt.Errorf("open current netns: %w", err)
            return
        }
        defer unix.Close(origNS)

        // Create new namespace.
        if err := unix.Unshare(unix.CLONE_NEWNET); err != nil {
            errCh <- fmt.Errorf("unshare CLONE_NEWNET: %w", err)
            return
        }

        // Bind-mount the new namespace to the target file.
        if err := unix.Mount("/proc/self/ns/net", nsPath, "", unix.MS_BIND, ""); err != nil {
            // Restore original namespace before returning error.
            _ = unix.Setns(origNS, unix.CLONE_NEWNET)
            errCh <- fmt.Errorf("bind mount netns: %w", err)
            return
        }

        // Restore the original namespace on this thread.
        if err := unix.Setns(origNS, unix.CLONE_NEWNET); err != nil {
            errCh <- fmt.Errorf("restore original netns: %w", err)
            return
        }

        errCh <- nil
    }()

    if err := <-errCh; err != nil {
        os.Remove(nsPath)
        return "", err
    }

    return nsPath, nil
}

// DeleteNetNS removes the persistent network namespace.
// Unmounts the bind mount and removes the file.
func DeleteNetNS(nsPath string) error {
    // Unmount. EINVAL means it was already unmounted.
    if err := unix.Unmount(nsPath, unix.MNT_DETACH); err != nil {
        if err != syscall.EINVAL {
            return fmt.Errorf("unmount netns %s: %w", nsPath, err)
        }
    }
    return os.Remove(nsPath)
}
```

### 4.4 TAP Device and TC Redirect Setup

```go
package network

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	tunDevice = "/dev/net/tun"

	// ioctl constants for TUN/TAP.
	ioctlTUNSETIFF     = 0x400454ca
	ioctlTUNSETPERSIST = 0x400454cb

	iffTAP  = 0x0002
	iffNOPI = 0x1000
)

// ifReq is the ifreq structure for TUN/TAP ioctl calls.
type ifReq struct {
	Name  [unix.IFNAMSIZ]byte
	Flags uint16
	_     [22]byte // padding
}

// SetupTAPAndRedirect creates a persistent TAP device inside the given
// network namespace and installs tc mirred rules to redirect all traffic
// bidirectionally between the CNI interface and the TAP device.
//
// This is the "glue" between CNI (which creates eth0 with an IP) and
// Cloud Hypervisor (which attaches to tap0 via --net tap=tap0).
func SetupTAPAndRedirect(nsPath string, cniIfName string, tapName string) error {
	// All operations must happen inside the target namespace.
	errCh := make(chan error, 1)
	go func() {
		runtime.LockOSThread()

		// Open target namespace.
		nsfd, err := unix.Open(nsPath, unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			errCh <- fmt.Errorf("open netns %s: %w", nsPath, err)
			return
		}
		defer unix.Close(nsfd)

		// Save current namespace.
		origNS, err := unix.Open("/proc/self/ns/net", unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			errCh <- fmt.Errorf("open current netns: %w", err)
			return
		}
		defer unix.Close(origNS)

		// Enter target namespace.
		if err := unix.Setns(nsfd, unix.CLONE_NEWNET); err != nil {
			errCh <- fmt.Errorf("setns to %s: %w", nsPath, err)
			return
		}

		// Create TAP device + setup tc redirect.
		err = setupTAPAndTC(cniIfName, tapName)

		// Restore original namespace.
		if restoreErr := unix.Setns(origNS, unix.CLONE_NEWNET); restoreErr != nil {
			if err == nil {
				err = fmt.Errorf("restore netns: %w", restoreErr)
			}
		}

		errCh <- err
	}()

	return <-errCh
}

// setupTAPAndTC runs inside the target namespace.
func setupTAPAndTC(cniIfName, tapName string) error {
	// 0. Validate CNI interface exists before proceeding.
	if _, err := net.InterfaceByName(cniIfName); err != nil {
		return fmt.Errorf("CNI interface %s not found in namespace: %w (ensure CNI ADD completed before calling SetupTAPAndRedirect)", cniIfName, err)
	}

	// 1. Create persistent TAP device.
	if err := createTAP(tapName); err != nil {
		return fmt.Errorf("create TAP %s: %w", tapName, err)
	}

	// 2. Install tc mirred redirect rules: cniIf <-> tap bidirectional.
	if err := setupTCRedirect(cniIfName, tapName); err != nil {
		return fmt.Errorf("setup tc redirect %s <-> %s: %w", cniIfName, tapName, err)
	}

	return nil
}

// createTAP creates a persistent TAP device in the current network namespace.
func createTAP(name string) error {
	if len(name) >= unix.IFNAMSIZ {
		return fmt.Errorf("TAP name %q exceeds IFNAMSIZ (%d)", name, unix.IFNAMSIZ)
	}

	fd, err := unix.Open(tunDevice, unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", tunDevice, err)
	}
	defer unix.Close(fd)

	var req ifReq
	copy(req.Name[:], name)
	req.Flags = iffTAP | iffNOPI

	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), ioctlTUNSETIFF, uintptr(unsafe.Pointer(&req)))
	if errno != 0 {
		return fmt.Errorf("ioctl TUNSETIFF: %w", errno)
	}

	// Make persistent (survives fd close).
	_, _, errno = unix.Syscall(unix.SYS_IOCTL, uintptr(fd), ioctlTUNSETPERSIST, 1)
	if errno != 0 {
		return fmt.Errorf("ioctl TUNSETPERSIST: %w", errno)
	}

	return setInterfaceUp(name)
}

// setupTCRedirect installs bidirectional tc mirred redirect rules between
// two interfaces. All packets arriving on ifA are redirected to ifB and
// vice versa. This operates at layer 2.
//
// Equivalent to:
//   tc qdisc add dev eth0 ingress
//   tc filter add dev eth0 parent ffff: protocol all u32 match u32 0 0 action mirred egress redirect dev tap0
//   tc qdisc add dev tap0 ingress
//   tc filter add dev tap0 parent ffff: protocol all u32 match u32 0 0 action mirred egress redirect dev eth0
func setupTCRedirect(ifA, ifB string) error {
	commands := [][]string{
		{"tc", "qdisc", "add", "dev", ifA, "ingress"},
		{"tc", "filter", "add", "dev", ifA, "parent", "ffff:", "protocol", "all",
			"u32", "match", "u32", "0", "0", "action", "mirred", "egress", "redirect", "dev", ifB},
		{"tc", "qdisc", "add", "dev", ifB, "ingress"},
		{"tc", "filter", "add", "dev", ifB, "parent", "ffff:", "protocol", "all",
			"u32", "match", "u32", "0", "0", "action", "mirred", "egress", "redirect", "dev", ifA},
	}

	for _, args := range commands {
		cmd := exec.Command(args[0], args[1:]...) //nolint:gosec // args are fixed literals
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%s: %s: %w", args[0], string(out), err)
		}
	}

	return nil
}

// setInterfaceUp sets the IFF_UP flag on a network interface.
func setInterfaceUp(name string) error {
	sock, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		return err
	}
	defer unix.Close(sock)

	var req ifReq
	copy(req.Name[:], name)

	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(sock), unix.SIOCGIFFLAGS, uintptr(unsafe.Pointer(&req)))
	if errno != 0 {
		return errno
	}

	req.Flags |= unix.IFF_UP

	_, _, errno = unix.Syscall(unix.SYS_IOCTL, uintptr(sock), unix.SIOCSIFFLAGS, uintptr(unsafe.Pointer(&req)))
	if errno != 0 {
		return errno
	}

	return nil
}
```

**Required Installation Order and Validation**:

The TC redirect setup has strict ordering requirements relative to the CNI and CH launch steps. Violating this order can cause IP conflicts (both the netns interface and the guest respond to ARP for the same IP) or packet loss.

```
Step 1:   CNI ADD (creates veth pair, assigns IP to netns-side interface)
Step 2:   Create TAP device inside the network namespace
Step 3:   Install TC mirred redirect rules (cni-veth <-> TAP, bidirectional)
Step 4:   Validate TC redirect rules are active:
            tc filter show dev <cni_if> ingress   (must show mirred redirect)
            tc filter show dev <tap> ingress       (must show mirred redirect)
          If either returns no rules, abort with error and roll back.
          This prevents launching CH with broken packet redirect.
Step 5:   Launch Cloud Hypervisor via nsenter
```

**IP Conflict Prevention**: TC redirect MUST be active before Cloud Hypervisor starts. Without the redirect, both the netns-side interface (which holds the IP for CNI bookkeeping) and the guest (which obtains the same IP via DHCP from dnsmasq) would respond to ARP requests for that IP, causing a Layer 2 conflict. The `tc mirred redirect` diverts all ingress packets on the netns interface to the TAP before the namespace IP stack sees them, preventing the conflict.

**Rollback on Failure**:

If any step fails, all previously completed steps are reversed in order:

```
If step 3 fails -> delete TAP (step 2 rollback) -> CNI DEL (step 1 rollback) -> ERROR
If step 4 fails -> remove TC rules -> delete TAP -> CNI DEL -> ERROR
If step 5 fails -> remove TC rules -> delete TAP -> CNI DEL -> ERROR
```

On failure, the VM transitions to the ERROR state. The network namespace is preserved (for debugging) but all devices and rules inside it are cleaned up. `cocoon delete` performs the final namespace cleanup.

CNI DEL automatically frees IPAM allocations (e.g., removes the IP allocation file for host-local IPAM). If CNI DEL fails during rollback, IPAM allocations may leak. The `cocoon doctor --fix` command detects and cleans stale IPAM allocations.

### 4.5 MAC Address Generation

```go
// generateMAC generates a locally-administered, unicast MAC address.
// The address is deterministic for a given (vmID, interfaceName) pair
// so that restarting a VM preserves the same MAC (and thus the same
// DHCP lease, if applicable).
func generateMAC(vmID string, ifName string) (string, error) {
    h := sha256.New()
    h.Write([]byte(vmID))
    h.Write([]byte(ifName))
    sum := h.Sum(nil)

    mac := net.HardwareAddr{
        (sum[0] & 0xfe) | 0x02, // Locally administered, unicast
        sum[1],
        sum[2],
        sum[3],
        sum[4],
        sum[5],
    }

    return mac.String(), nil
}
```

### 4.6 Cloud Hypervisor Integration

The `buildCHVMConfig` function in `vm/engine/manager.go` is extended to include `--net` arguments:

```go
func buildCHVMConfig(cfg *types.VMConfig, meta *types.VMMetadataFile) hypervisor.CHVMConfig {
	vmConfig := hypervisor.CHVMConfig{
		CPUs: hypervisor.CHCPUConfig{
			BootVCPUs: cfg.CPUs,
		},
		Memory: hypervisor.CHMemoryConfig{
			Size: cfg.MemoryMB * 1024 * 1024,
		},
		Disks: []hypervisor.CHDiskConfig{
			{Path: cfg.OverlayPath},
		},
		Serial: hypervisor.CHSerialConfig{
			Mode: "File",
			File: cfg.SerialLog,
		},
		Console: hypervisor.CHConsoleConfig{
			Mode: "Pty",
		},
	}

	// Add network devices from network state.
	for _, att := range meta.NetworkState.Attachments {
		vmConfig.Net = append(vmConfig.Net, hypervisor.CHNetConfig{
			Tap: att.TAPDevice,
			MAC: att.MACAddress,
		})
	}

	return vmConfig
}
```

The CH launch function is extended to enter the VM's network namespace when networking is configured:

```go
// LaunchVM starts a Cloud Hypervisor process for the given VM.
// If netnsPath is non-empty, CH enters the network namespace via nsenter.
func (c *client) LaunchVM(ctx context.Context, vmID string, vmConfig CHVMConfig, netnsPath string) (pid int, err error) {
	args := buildCHArgs(vmConfig)

	var cmd *exec.Cmd
	if netnsPath != "" {
		// Launch CH inside the VM's network namespace.
		// nsenter --net=<path> only changes the network namespace.
		// File system, PID, and mount namespaces are inherited from the host.
		// CH's API socket (Unix domain socket on host filesystem) remains
		// accessible from the host because UDS is filesystem-scoped, not
		// network-namespace-scoped.
		nsenterArgs := append([]string{"--net=" + netnsPath, "--", c.cfg.CHBinary}, args...)
		cmd = exec.CommandContext(ctx, "nsenter", nsenterArgs...) //nolint:gosec // args are from internal config
	} else {
		// No networking: launch CH directly in host namespace.
		cmd = exec.CommandContext(ctx, c.cfg.CHBinary, args...) //nolint:gosec // args are from internal config
	}

	configureCHProcess(cmd)

	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start cloud-hypervisor: %w", err)
	}

	// ... existing PID file, socket wait, process release logic ...
}
```

### 4.7 VM Create Flow (Updated)

```go
func (e *engine) Create(ctx context.Context, opts CreateOptions) (*types.VMConfig, error) {
    // ... existing create logic (image conversion, overlay, config.json) ...

    // Network setup (new in Phase 2).
    if len(opts.Networks) > 0 {
        // 1. Create network namespace.
        nsPath, err := network.CreateNetNS(vmID)
        if err != nil {
            return nil, fmt.Errorf("create network namespace: %w", err)
        }

        // 2. Build network attachments from CLI flags.
        var attachments []types.NetworkAttachment
        for i, netName := range opts.Networks {
            attachments = append(attachments, types.NetworkAttachment{
                Name:          netName,
                InterfaceName: fmt.Sprintf("eth%d", i),
                PortMappings:  opts.PortMappings[netName], // from --publish flags
            })
        }
        cfg.Network = types.NetworkConfig{Networks: attachments}

        // 3. Invoke CNI ADD for each network.
        var states []types.NetworkAttachmentState
        for i, att := range attachments {
            state, err := e.netMgr.AddNetwork(ctx, vmID, nsPath, att)
            if err != nil {
                // Rollback: delete all networks added so far.
                for j := len(states) - 1; j >= 0; j-- {
                    _ = e.netMgr.DeleteNetwork(ctx, vmID, nsPath, attachments[j])
                }
                _ = network.DeleteNetNS(nsPath)
                return nil, fmt.Errorf("add network %q: %w", att.Name, err)
            }

            // 4. Create TAP and install tc mirred redirect in the namespace.
            tapName := fmt.Sprintf("tap%d", i)
            if err := network.SetupTAPAndRedirect(nsPath, att.InterfaceName, tapName); err != nil {
                // Rollback: CNI DEL + previous networks.
                _ = e.netMgr.DeleteNetwork(ctx, vmID, nsPath, att)
                for j := len(states) - 1; j >= 0; j-- {
                    _ = e.netMgr.DeleteNetwork(ctx, vmID, nsPath, attachments[j])
                }
                _ = network.DeleteNetNS(nsPath)
                return nil, fmt.Errorf("setup TAP redirect for %q: %w", att.Name, err)
            }

            state.TAPDevice = tapName
            states = append(states, *state)
        }

        // 5. Register DHCP leases with dnsmasq:
        //    a. Ensure per-bridge dnsmasq is running (startDnsmasq if needed)
        //    b. For each attachment, add a static DHCP lease:
        //       addDHCPLease(bridge, mac, ip, hostname)
        //    c. dnsmasq serves the lease to the guest on boot via standard DHCP
        for _, s := range states {
            bridge := s.BridgeName
            if err := network.StartDnsmasq(bridge); err != nil {
                // Rollback: CNI DEL all networks + delete namespace.
                for j := len(states) - 1; j >= 0; j-- {
                    _ = e.netMgr.DeleteNetwork(ctx, vmID, nsPath, attachments[j])
                }
                _ = network.DeleteNetNS(nsPath)
                return nil, fmt.Errorf("start dnsmasq on bridge %s: %w", bridge, err)
            }
            if err := network.AddDHCPLease(bridge, s.MACAddress, s.IPs[0], vmID); err != nil {
                // Rollback: stop dnsmasq if we just started it, CNI DEL, delete namespace.
                for j := len(states) - 1; j >= 0; j-- {
                    _ = network.RemoveDHCPLease(states[j].BridgeName, states[j].MACAddress)
                    _ = e.netMgr.DeleteNetwork(ctx, vmID, nsPath, attachments[j])
                }
                _ = network.DeleteNetNS(nsPath)
                return nil, fmt.Errorf("add DHCP lease for %s on bridge %s: %w", s.MACAddress, bridge, err)
            }
        }

        // 6. Store network state in metadata.
        meta.NetworkState = types.NetworkState{
            Attachments: states,
            NetNSPath:   nsPath,
        }
    }

    // ... save config.json and metadata.json ...
}
```

### 4.8 VM Delete Flow (Updated)

```go
func (e *engine) Delete(ctx context.Context, vmID string, force bool) error {
    // ... existing stop/kill logic ...

    // Network cleanup (new in Phase 2).
    cfg, _ := e.LoadConfig(vmID)
    meta, _ := e.LoadMetadata(vmID)

    if meta.NetworkState.NetNSPath != "" {
        // 1. CNI DEL for all networks (best-effort, log errors).
        _ = e.netMgr.DeleteAllNetworks(ctx, vmID, meta.NetworkState.NetNSPath, cfg.Network.Networks)

        // 2. Delete the network namespace.
        if err := network.DeleteNetNS(meta.NetworkState.NetNSPath); err != nil {
            fmt.Fprintf(os.Stderr, "warning: failed to delete netns: %v\n", err)
        }
    }

    // ... existing file cleanup, name index removal ...
}
```

### 4.9 Project Structure Additions

```
cocoon/
├── network/
│   ├── manager.go              # Manager interface and cniManager implementation
│   ├── manager_test.go         # Unit tests with mock CNI
│   ├── netns.go                # CreateNetNS, DeleteNetNS
│   ├── netns_linux.go          # Linux-specific namespace operations
│   ├── netns_darwin.go         # Stub: returns "networking requires Linux"
│   ├── tap.go                  # TAP device creation
│   ├── tap_linux.go            # Linux-specific TAP ioctl
│   ├── tap_darwin.go           # Stub
│   ├── mac.go                  # MAC address generation
│   └── types.go                # NetworkConfig, NetworkState, etc.
├── cmd/cocoon/
│   ├── create.go               # Updated: --network, --publish flags
│   ├── run.go                  # Updated: same flags as create
│   ├── network.go              # cocoon network list|inspect
│   └── network_linux.go        # Linux-specific network commands
```

---

## 5. CLI

### 5.1 Create/Run Flags

The `--network` and `--publish` flags are added to both `cocoon create` and `cocoon run`:

```go
func vmCreateFlags() []cli.Flag {
    return []cli.Flag{
        // ... existing flags (--name, --cpus, --memory, --disk-size, --rm) ...

        &cli.StringSliceFlag{
            Name:    "network",
            Aliases: []string{"net"},
            Usage:   "attach to a CNI network (can be specified multiple times, 'none' for no network)",
            Value:   cli.NewStringSlice(), // default: empty (no network)
        },
        &cli.StringSliceFlag{
            Name:    "publish",
            Aliases: []string{"p"},
            Usage:   "publish a port (hostPort:guestPort[/protocol])",
        },
    }
}
```

### 5.2 Network Subcommand

```go
func networkCommand() *cli.Command {
    return &cli.Command{
        Name:  "network",
        Usage: "Manage CNI networks",
        Subcommands: []*cli.Command{
            {
                Name:   "list",
                Usage:  "List available CNI networks",
                Aliases: []string{"ls"},
                Action: networkListAction,
            },
            {
                Name:      "inspect",
                Usage:     "Show detailed CNI network configuration",
                ArgsUsage: "NETWORK_NAME",
                Action:    networkInspectAction,
            },
        },
    }
}
```

Registration in `cmd/cocoon/main.go`:

```go
app.Commands = []*cli.Command{
    // ... existing commands ...
    networkCommand(),
}
```

### 5.3 Usage Examples

```bash
# Create a VM with bridge networking
$ cocoon create --network bridge myorg/ubuntu-bootable:22.04 --name webserver
Created VM webserver (vm-01HXY...)
  Network: bridge
  IP: 10.88.0.5/16
  Gateway: 10.88.0.1

# Create a VM with port forwarding
$ cocoon create --network bridge --publish 8080:80/tcp --name nginx myorg/nginx-vm:latest
Created VM nginx (vm-01HXZ...)
  Network: bridge
  IP: 10.88.0.6/16
  Port: 0.0.0.0:8080 -> 80/tcp

# Create a VM with multiple networks
$ cocoon create --network bridge --network isolated myorg/ubuntu-bootable:22.04 --name multi
Created VM multi (vm-01HXW...)
  Network: bridge  -> eth0: 10.88.0.7/16
  Network: isolated -> eth1: 172.20.0.2/24

# Create a VM with no networking (default, backward compatible)
$ cocoon create myorg/ubuntu-bootable:22.04 --name sandbox
Created VM sandbox (vm-01HXV...)
  Network: none

# Run (create + start) with networking
$ cocoon run --network bridge --publish 2222:22/tcp myorg/ubuntu-bootable:22.04 --name dev
# VM starts with bridge network, SSH accessible on host port 2222

# List available CNI networks
$ cocoon network list
NAME        TYPE      SUBNET          GATEWAY       IPAM
bridge      bridge    10.88.0.0/16    10.88.0.1     host-local
macvlan0    macvlan   (dhcp)          (dhcp)        dhcp
isolated    bridge    172.20.0.0/24   172.20.0.1    host-local

# Inspect a CNI network
$ cocoon network inspect bridge
Name:    bridge
Type:    bridge
Bridge:  cni0
IPAM:    host-local
Subnet:  10.88.0.0/16
Gateway: 10.88.0.1 (auto)
Masquerade: yes
Plugins: bridge -> host-local -> portmap

# Inspect a VM with network details
$ cocoon inspect webserver
{
  "vm_id": "vm-01HXY...",
  "name": "webserver",
  "state": "RUNNING",
  ...
  "network": {
    "netns_path": "/run/cocoon/vms/vm-01HXY.../netns",
    "attachments": [
      {
        "network_name": "bridge",
        "interface_name": "eth0",
        "tap_device": "tap0",
        "mac_address": "02:a3:b5:c7:d9:e1",
        "ips": ["10.88.0.5/16"],
        "gateway": "10.88.0.1",
        "dns": ["10.88.0.1"]
      }
    ]
  }
}
```

### 5.4 Port Mapping Flag Parsing

```go
// parsePublishFlag parses a --publish flag value into a PortMapping.
// Format: [hostIP:]hostPort:containerPort[/protocol]
// Examples: "8080:80", "8080:80/tcp", "127.0.0.1:8080:80/udp"
func parsePublishFlag(value string) (PortMapping, error) {
    pm := PortMapping{Protocol: "tcp"} // Default protocol.

    // Split off protocol suffix.
    if idx := strings.LastIndex(value, "/"); idx != -1 {
        pm.Protocol = strings.ToLower(value[idx+1:])
        value = value[:idx]
        if pm.Protocol != "tcp" && pm.Protocol != "udp" {
            return pm, fmt.Errorf("unsupported protocol %q (must be tcp or udp)", pm.Protocol)
        }
    }

    // Split host and container parts.
    parts := strings.Split(value, ":")
    switch len(parts) {
    case 2:
        // hostPort:containerPort
        hostPort, err := strconv.Atoi(parts[0])
        if err != nil {
            return pm, fmt.Errorf("invalid host port %q: %w", parts[0], err)
        }
        containerPort, err := strconv.Atoi(parts[1])
        if err != nil {
            return pm, fmt.Errorf("invalid container port %q: %w", parts[1], err)
        }
        pm.HostPort = hostPort
        pm.ContainerPort = containerPort

    case 3:
        // hostIP:hostPort:containerPort
        pm.HostIP = parts[0]
        hostPort, err := strconv.Atoi(parts[1])
        if err != nil {
            return pm, fmt.Errorf("invalid host port %q: %w", parts[1], err)
        }
        containerPort, err := strconv.Atoi(parts[2])
        if err != nil {
            return pm, fmt.Errorf("invalid container port %q: %w", parts[2], err)
        }
        pm.HostPort = hostPort
        pm.ContainerPort = containerPort

    default:
        return pm, fmt.Errorf("invalid publish format %q (expected hostPort:containerPort)", value)
    }

    return pm, nil
}
```

---

## 6. IP Address Management

### 6.1 IPAM Plugin Integration

Cocoon does not implement any IP address management. IPAM is handled entirely by CNI IPAM plugins configured in the network's CNI configuration file.

Supported IPAM plugins:

| Plugin | Description | Use Case |
|--------|-------------|----------|
| `host-local` | Allocates IPs from a local subnet pool stored on disk | Default for bridge networks. Simple, no daemon. |
| `dhcp` | Obtains IPs via DHCP from an external server | Macvlan/ipvlan networks on a LAN with existing DHCP. |
| `static` | Uses a fixed IP specified in the config | Testing, single-VM scenarios. |

### 6.2 host-local IPAM

The `host-local` plugin allocates IP addresses from a configured subnet and stores allocations in `/var/lib/cni/networks/{network-name}/`. Each allocation is a file named with the IP address, containing the container (VM) ID.

```
/var/lib/cni/networks/bridge/
├── 10.88.0.2        # Contains: vm-01HXY...
├── 10.88.0.3        # Contains: vm-01HXZ...
├── 10.88.0.4        # Contains: vm-01HXW...
├── last_reserved_ip.0  # Tracks last allocated IP for round-robin
└── lock             # File lock for concurrent allocation
```

On CNI DEL, the IP file is removed, returning the address to the pool. No Cocoon-side logic is needed.

### 6.3 dhcp IPAM

The `dhcp` plugin runs a small daemon (`/opt/cni/bin/dhcp daemon`) that manages DHCP leases on behalf of VMs. When CNI ADD is called, the dhcp plugin sends a DHCP request from the VM's network namespace.

**Setup requirement**: The dhcp daemon must be running before VMs can use dhcp IPAM:

```bash
# Start the CNI DHCP daemon (typically via systemd)
/opt/cni/bin/dhcp daemon &
```

This is an external dependency, not managed by Cocoon.

### 6.4 IP Address Visibility

IP addresses allocated by IPAM plugins are:

1. **Returned in the CNI result** (captured by Cocoon during `AddNetwork`).
2. **Stored in metadata.json** (in `NetworkAttachmentState.IPs`).
3. **Visible in `cocoon inspect`** output.
4. **Served to the guest** via DHCP from a per-bridge dnsmasq instance (see Section 8).

```bash
$ cocoon inspect myvm --format json | jq '.network.attachments[0].ips'
["10.88.0.5/16"]
```

---

## 7. Port Forwarding

### 7.1 CNI Portmap Plugin

Port forwarding is implemented by the CNI `portmap` plugin, which is part of the standard CNI plugins distribution. The plugin installs iptables DNAT rules that forward traffic from a host port to the VM's IP and port.

The portmap plugin must be included in the CNI plugin chain for port forwarding to work. This is configured in the CNI network configuration file:

```json
{
  "cniVersion": "1.0.0",
  "name": "bridge",
  "plugins": [
    {
      "type": "bridge",
      "bridge": "cni0",
      "isGateway": true,
      "ipMasq": true,
      "ipam": {
        "type": "host-local",
        "ranges": [[{"subnet": "10.88.0.0/16"}]],
        "routes": [{"dst": "0.0.0.0/0"}]
      }
    },
    {
      "type": "portmap",
      "capabilities": {"portMappings": true},
      "snat": true
    }
  ]
}
```

### 7.2 Port Mapping Lifecycle

**On CNI ADD** (via `--publish` flag):

```
iptables -t nat -A COCOON-HOSTPORTS -p tcp --dport 8080 \
    -j DNAT --to-destination 10.88.0.5:80
```

**On CNI DEL** (during VM delete):

```
# iptables rules are automatically removed by the portmap plugin.
```

### 7.3 Port Mapping Storage

Port mappings are part of the immutable VM config (specified at create time) and passed to the portmap plugin via CNI runtime capabilities:

```go
rt := &libcni.RuntimeConf{
    ContainerID: vmID,
    NetNS:       netNSPath,
    IfName:      "eth0",
    CapabilityArgs: map[string]interface{}{
        "portMappings": []map[string]interface{}{
            {
                "hostPort":      8080,
                "containerPort": 80,
                "protocol":      "tcp",
            },
        },
    },
}
```

### 7.4 Port Conflict Detection

Cocoon does not implement port conflict detection. The portmap plugin will fail if the host port is already in use, and the error is propagated to the user:

```
$ cocoon create --network bridge --publish 8080:80 myimage --name vm2
Error: add network "bridge": CNI portmap: failed to add iptables rule: port 8080 already in use
```

---

## 8. DNS

### 8.1 DNS Configuration Sources

DNS server addresses come from two sources:

1. **CNI result**: The CNI plugin chain may return DNS server addresses in its result. This is common with the `dhcp` IPAM plugin.
2. **CNI network config**: The network configuration file can include a `dns` block with nameservers.

### 8.2 Guest Network Configuration via DHCP

Cocoon configures guest networking via standard DHCP, using a per-bridge **dnsmasq** instance that serves static leases matching the CNI IPAM allocation. This eliminates the need for cloud-init, seed disks, or any guest-side agent.

#### Architecture

```
                                     CNI bridge (e.g., cni0)
                                            |
                 +----------+----------+----+----+----------+
                 |          |          |         |          |
               veth1      veth2      veth3    dnsmasq    (host)
              (VM-A)     (VM-B)     (VM-C)   listening
                                              on bridge
```

Cocoon runs one dnsmasq process per CNI bridge interface. dnsmasq is configured to:

1. **Listen only on the bridge interface** (e.g., `--interface=cni0 --bind-interfaces`).
2. **Serve static DHCP leases** derived from the CNI IPAM result. Each VM's MAC address is mapped to the IP allocated by the IPAM plugin.
3. **Provide DNS forwarding** using the host's `/etc/resolv.conf` or CNI-specified nameservers.
4. **Set gateway and search domain** via DHCP options matching the CNI result.

#### DHCP Lease Example

When CNI allocates IP `10.88.0.5/16` with gateway `10.88.0.1` for a VM with MAC `02:a3:b5:c7:d9:e1`, Cocoon writes the following static lease entry to the dnsmasq hosts file:

```
# /var/lib/cocoon/dnsmasq/cni0/hosts
02:a3:b5:c7:d9:e1,10.88.0.5,vm-abc123
```

dnsmasq serves this as a DHCP response to the matching MAC address, including:
- IP address: `10.88.0.5`
- Subnet mask: `255.255.0.0` (derived from `/16`)
- Gateway: `10.88.0.1`
- DNS servers: `10.88.0.1`, `8.8.8.8`
- Search domain: `cocoon.local`
- Lease time: infinite (static lease)

#### Key Advantages

- **Works with ANY Linux guest**: No cloud-init, no guest agent, no special image requirements. Any distro with a DHCP client (dhclient, systemd-networkd, NetworkManager) works out of the box.
- **No seed disk tooling**: Eliminates mkdosfs, mcopy, dosfstools, and mtools dependencies.
- **No secondary disk device**: The VM needs only its root disk. No virtio-blk device for seed data.
- **Debuggable with standard tools**: `dhclient -v`, `tcpdump -i cni0 port 67`, `journalctl -u dnsmasq` all work as expected.
- **Handles IP changes on reboot**: If the IPAM plugin assigns a new IP after reboot, Cocoon updates the dnsmasq lease file. The guest's DHCP client picks up the new IP automatically.
- **Same approach as libvirt/QEMU**: This is the standard mechanism used by libvirt's default network, making it familiar to most virtualization engineers.

#### dnsmasq Configuration Template

```ini
# /var/lib/cocoon/dnsmasq/cni0/dnsmasq.conf
interface=cni0
bind-interfaces
except-interface=lo
dhcp-range=10.88.0.2,static,infinite
dhcp-hostsfile=/var/lib/cocoon/dnsmasq/cni0/hosts
dhcp-option=option:router,10.88.0.1
dhcp-option=option:dns-server,10.88.0.1,8.8.8.8
dhcp-option=option:domain-search,cocoon.local
dhcp-authoritative
no-resolv
server=8.8.8.8
server=8.8.4.4
pid-file=/var/lib/cocoon/dnsmasq/cni0/dnsmasq.pid
log-dhcp
```

The `dhcp-range=<start>,static,infinite` directive tells dnsmasq to only serve leases for MAC addresses listed in the hosts file. Unknown MACs are ignored, preventing rogue devices from obtaining addresses.

### 8.3 dnsmasq Lifecycle

Cocoon manages one dnsmasq process per CNI bridge interface. The lifecycle is tied to VMs using that bridge, not to individual VM lifecycles.

**State directory**: `/var/lib/cocoon/dnsmasq/{bridge}/` contains:
- `dnsmasq.conf` — Generated configuration file.
- `dnsmasq.pid` — PID file for the running dnsmasq process.
- `hosts` — Static DHCP lease entries (one per line: `<mac>,<ip>,<hostname>`).

**Start** (when first VM on a bridge needs networking):

1. Create the state directory `/var/lib/cocoon/dnsmasq/{bridge}/`.
2. Generate `dnsmasq.conf` from the bridge's subnet configuration (gateway, DNS, search domain).
3. Create an empty `hosts` file.
4. Start dnsmasq: `dnsmasq --conf-file=/var/lib/cocoon/dnsmasq/{bridge}/dnsmasq.conf`
5. Verify dnsmasq started successfully by checking the PID file.

If dnsmasq is already running for this bridge (PID file exists and process is alive), this step is a no-op.

**Update** (when a VM is created or deleted on the bridge):

1. Add or remove the static lease line in the `hosts` file.
2. Send `SIGHUP` to the running dnsmasq process (read from PID file). dnsmasq re-reads the hosts file without restarting, so existing leases for other VMs are unaffected.

```bash
# Example: add a lease
echo "02:a3:b5:c7:d9:e1,10.88.0.5,vm-abc123" >> /var/lib/cocoon/dnsmasq/cni0/hosts
kill -HUP $(cat /var/lib/cocoon/dnsmasq/cni0/dnsmasq.pid)
```

**Stop** (when last VM on a bridge is removed):

1. Send `SIGTERM` to the dnsmasq process (from PID file).
2. Wait for the process to exit (with a short timeout).
3. Remove the state directory `/var/lib/cocoon/dnsmasq/{bridge}/`.

If other VMs still reference the bridge, the dnsmasq process is kept running. The stop logic checks the number of remaining lease entries in the hosts file.

**Cleanup** (during `cocoon delete`):

1. Call `removeDHCPLease(bridge, mac)` to remove the VM's lease entry from the hosts file.
2. Send `SIGHUP` to dnsmasq so it drops the lease.
3. If the hosts file is now empty (no more VMs on this bridge), stop and clean up dnsmasq for this bridge.

No per-VM disk artifacts are created. The only state is the centralized hosts file per bridge.

### 8.4 dnsmasq Management in Code

```go
const dnsmasqStateDir = "/var/lib/cocoon/dnsmasq"

// startDnsmasq ensures a dnsmasq instance is running for the given bridge.
// If dnsmasq is already running (PID file exists, process alive), this is a no-op.
// The dnsmasq instance listens only on the bridge interface and serves static
// DHCP leases from the hosts file.
func startDnsmasq(bridge string) error {
    dir := filepath.Join(dnsmasqStateDir, bridge)
    pidFile := filepath.Join(dir, "dnsmasq.pid")

    // Check if already running.
    if pid, err := readPIDFile(pidFile); err == nil {
        if processAlive(pid) {
            return nil // Already running.
        }
        // Stale PID file; clean up and restart.
    }

    if err := os.MkdirAll(dir, 0755); err != nil {
        return fmt.Errorf("create dnsmasq state dir: %w", err)
    }

    confPath := filepath.Join(dir, "dnsmasq.conf")
    hostsPath := filepath.Join(dir, "hosts")

    // Create empty hosts file if it does not exist.
    if _, err := os.Stat(hostsPath); os.IsNotExist(err) {
        if err := os.WriteFile(hostsPath, nil, 0644); err != nil {
            return fmt.Errorf("create hosts file: %w", err)
        }
    }

    // Generate dnsmasq config from bridge network parameters.
    conf := generateDnsmasqConf(bridge, hostsPath, pidFile)
    if err := os.WriteFile(confPath, []byte(conf), 0644); err != nil {
        return fmt.Errorf("write dnsmasq config: %w", err)
    }

    cmd := exec.Command("dnsmasq", "--conf-file="+confPath)
    if err := cmd.Run(); err != nil {
        return fmt.Errorf("start dnsmasq on bridge %s: %w", bridge, err)
    }

    return nil
}

// stopDnsmasq stops the dnsmasq instance for the given bridge and cleans up
// its state directory. Called when the last VM on the bridge is removed.
func stopDnsmasq(bridge string) error {
    dir := filepath.Join(dnsmasqStateDir, bridge)
    pidFile := filepath.Join(dir, "dnsmasq.pid")

    if pid, err := readPIDFile(pidFile); err == nil {
        if processAlive(pid) {
            syscall.Kill(pid, syscall.SIGTERM)
            // Wait briefly for clean shutdown.
            waitForExit(pid, 5*time.Second)
        }
    }

    return os.RemoveAll(dir)
}

// addDHCPLease adds a static DHCP lease entry to the dnsmasq hosts file for
// the given bridge and sends SIGHUP to dnsmasq so it picks up the change.
// The lease maps the VM's MAC address to the CNI-allocated IP and hostname.
func addDHCPLease(bridge, mac, ip, hostname string) error {
    dir := filepath.Join(dnsmasqStateDir, bridge)
    hostsPath := filepath.Join(dir, "hosts")

    // Format: <mac>,<ip>,<hostname>
    entry := fmt.Sprintf("%s,%s,%s\n", mac, ip, hostname)

    f, err := os.OpenFile(hostsPath, os.O_APPEND|os.O_WRONLY, 0644)
    if err != nil {
        return fmt.Errorf("open hosts file: %w", err)
    }
    defer f.Close()

    if _, err := f.WriteString(entry); err != nil {
        return fmt.Errorf("write lease entry: %w", err)
    }

    // SIGHUP causes dnsmasq to re-read the hosts file.
    return signalDnsmasq(bridge, syscall.SIGHUP)
}

// removeDHCPLease removes the static DHCP lease for the given MAC address
// from the dnsmasq hosts file and sends SIGHUP. If the hosts file is now
// empty, the dnsmasq instance is stopped.
func removeDHCPLease(bridge, mac string) error {
    dir := filepath.Join(dnsmasqStateDir, bridge)
    hostsPath := filepath.Join(dir, "hosts")

    // Read, filter out the line matching the MAC, write back.
    data, err := os.ReadFile(hostsPath)
    if err != nil {
        return fmt.Errorf("read hosts file: %w", err)
    }

    var remaining []string
    for _, line := range strings.Split(string(data), "\n") {
        line = strings.TrimSpace(line)
        if line == "" || strings.HasPrefix(line, mac+",") {
            continue
        }
        remaining = append(remaining, line)
    }

    newData := strings.Join(remaining, "\n")
    if len(remaining) > 0 {
        newData += "\n"
    }
    if err := os.WriteFile(hostsPath, []byte(newData), 0644); err != nil {
        return fmt.Errorf("write hosts file: %w", err)
    }

    if len(remaining) == 0 {
        // No more VMs on this bridge; stop dnsmasq entirely.
        return stopDnsmasq(bridge)
    }

    // SIGHUP causes dnsmasq to re-read the hosts file.
    return signalDnsmasq(bridge, syscall.SIGHUP)
}
```

---

## 9. Error Handling

### 9.1 Error Cases

| Condition | Error Message | Recovery |
|-----------|---------------|----------|
| CNI network not found | `CNI network "foo" not found in /etc/cni/net.d` | Install the CNI config file. |
| CNI plugin binary missing | `CNI plugin "bridge" not found in [/opt/cni/bin ...]` | Install CNI plugins (`cocoon doctor` checks this). |
| CNI ADD failure | `CNI ADD for network "bridge": <plugin error>` | Fix the CNI config or plugin. |
| TAP creation failure | `create TAP device "tap0" in netns: operation not permitted` | Ensure `CAP_NET_ADMIN` capability or root. |
| Namespace creation failure | `unshare CLONE_NEWNET: operation not permitted` | Requires root or `CAP_SYS_ADMIN`. |
| Port conflict | `CNI portmap: port 8080 already in use` | Choose a different host port. |
| IPAM exhaustion | `CNI host-local: no IP addresses available in range set` | Expand the subnet or free unused allocations. |
| CNI DEL failure (on delete) | Warning logged, deletion continues | Manual cleanup via `ip netns` if needed. |

### 9.2 Rollback on Create Failure

If any step of the network setup fails during `cocoon create`, all previously completed steps are rolled back:

```
Step 1: Create namespace  -> Success
Step 2: CNI ADD (net1)    -> Success
Step 3: CNI ADD (net2)    -> FAILURE
  Rollback:
    - CNI DEL (net1)
    - Delete namespace
    - Return error
```

The VM is not created if network setup fails. This maintains the invariant that a CREATED VM has all its networks fully configured.

### 9.3 Cleanup on Delete Failure

Network cleanup during `cocoon delete` is best-effort. If CNI DEL fails, the namespace is still deleted (which destroys all devices inside it), and the VM deletion proceeds. Stale IPAM allocations may remain in `/var/lib/cni/networks/` and can be cleaned up via `cocoon doctor --fix`.

### 9.4 Dependencies and Prerequisites

Phase 2 networking relies on several external tools beyond the Phase 1 dependency set (see [08-dependencies.md](./08-dependencies.md) for Phase 1 dependencies). The table below consolidates all networking-specific dependencies.

| Dependency | Package | Required | Purpose | Install (Debian/Ubuntu) | Install (Fedora/RHEL) |
|------------|---------|----------|---------|------------------------|----------------------|
| `ip` | iproute2 | Yes | TAP device management, bridge inspection, interface configuration | `apt install iproute2` | `dnf install iproute` |
| `nsenter` | util-linux | Yes | Network namespace entry for Cloud Hypervisor launch (`nsenter --net=<path>`) | `apt install util-linux` | `dnf install util-linux` |
| CNI plugins (`bridge`, `host-local`, `portmap`, etc.) | containernetworking-plugins | Yes | Network plumbing: bridge creation, IP allocation, port forwarding | See [containernetworking/plugins releases](https://github.com/containernetworking/plugins/releases) | See [containernetworking/plugins releases](https://github.com/containernetworking/plugins/releases) |
| `dnsmasq` | dnsmasq / dnsmasq-base | Yes | Per-bridge DHCP server for guest IP assignment (static leases from CNI IPAM result) | `apt install dnsmasq-base` | `dnf install dnsmasq` |
| `iptables` or `nftables` | iptables / nftables | Yes | NAT/masquerade for outbound traffic, port forwarding via CNI portmap plugin | `apt install iptables` | `dnf install iptables-nft` |
| `tc` | iproute2 | Yes | TC mirred redirect rules between CNI veth and TAP device (core to the networking model) | (included with iproute2) | (included with iproute) |
| `/dev/net/tun` | (kernel) | Yes | TUN/TAP device creation for virtio-net backend | Built-in (kernel module `tun`) | Built-in (kernel module `tun`) |
| CNI config directory | (user-provided) | Yes | CNI network configuration files (`/etc/cni/net.d/*.conflist`) | Create manually or install from CNI quickstart | Create manually or install from CNI quickstart |

**Notes**:

- `ip`, `tc`, and `nsenter` are typically pre-installed on all modern Linux distributions. `iproute2` and `util-linux` are part of the minimal install on Ubuntu, Debian, and Fedora.
- CNI plugins must be installed separately. The standard location is `/opt/cni/bin/`. Cocoon also searches `/usr/lib/cni` and `/usr/libexec/cni`. See Section 3.7 for the plugin search path.
- `dnsmasq-base` (Debian/Ubuntu) is preferred over `dnsmasq` because it does not install a system-wide dnsmasq service. Cocoon manages its own per-bridge dnsmasq instances.
- Cloud Hypervisor (v38.0+) is a Phase 1 dependency and is not repeated here. See [08-dependencies.md](./08-dependencies.md).

**CNI Plugin Installation**:

```bash
# Download and install standard CNI plugins
CNI_VERSION="v1.4.0"
curl -LO https://github.com/containernetworking/plugins/releases/download/${CNI_VERSION}/cni-plugins-linux-amd64-${CNI_VERSION}.tgz
sudo mkdir -p /opt/cni/bin
sudo tar -xzf cni-plugins-linux-amd64-${CNI_VERSION}.tgz -C /opt/cni/bin
```

**Corresponding `cocoon doctor` Checks**:

Each dependency above has a corresponding `cocoon doctor` check (see Section 9.5 below). The checks are severity `warning` rather than `error` because networking is optional -- VMs without `--network` do not require any networking dependencies.

| Doctor Check | What It Verifies | Severity |
|-------------|------------------|----------|
| `ip-command` | `ip` binary exists in PATH | warning |
| `nsenter-command` | `nsenter` binary exists in PATH; process has root or `CAP_SYS_ADMIN` | warning |
| `cni-plugins` | At least one CNI plugin directory contains binaries | warning |
| `cni-config-dir` | `/etc/cni/net.d/` directory exists | warning |
| `dnsmasq-command` | `dnsmasq` binary exists in PATH | warning |
| `tc-command` | `tc` binary exists in PATH | warning |
| `tun-device` | `/dev/net/tun` device exists | warning |
| `ch-cap-net-admin` | CH binary has `CAP_NET_ADMIN` (informational; root bypasses) | info |
| `stale-ipam-allocations` | No IPAM allocation files referencing deleted VMs | info |

### 9.5 Doctor Integration

`cocoon doctor` is extended with network health checks:

```go
func networkDoctorChecks() []DoctorCheck {
    return []DoctorCheck{
        {
            Name: "cni-plugins",
            Check: func() error {
                // Verify at least one CNI plugin directory exists with binaries.
                for _, dir := range cniPluginDirs {
                    entries, err := os.ReadDir(dir)
                    if err == nil && len(entries) > 0 {
                        return nil
                    }
                }
                return fmt.Errorf("no CNI plugins found in %v", cniPluginDirs)
            },
            Severity: "warning", // Not critical if user doesn't use networking.
        },
        {
            Name: "cni-config-dir",
            Check: func() error {
                if _, err := os.Stat(cniConfDir); err != nil {
                    return fmt.Errorf("CNI config directory %s not found", cniConfDir)
                }
                return nil
            },
            Severity: "warning",
        },
        {
            Name: "tc-command",
            Check: func() error {
                // tc is required for traffic control rules (tc mirred redirect).
                if _, err := exec.LookPath("tc"); err != nil {
                    return fmt.Errorf("'tc' command not found (needed for traffic control rules): %w", err)
                }
                return nil
            },
            Severity: "warning",
        },
        {
            Name: "nsenter-command",
            Check: func() error {
                // nsenter is required for entering the VM network namespace.
                if _, err := exec.LookPath("nsenter"); err != nil {
                    return fmt.Errorf("'nsenter' command not found (needed for network namespace entry): %w", err)
                }
                // nsenter --net requires CAP_SYS_ADMIN (not CAP_NET_ADMIN) to
                // enter network namespaces. Check that the cocoon process has
                // sufficient privileges (root or CAP_SYS_ADMIN).
                if os.Getuid() != 0 {
                    return fmt.Errorf("nsenter --net requires root or CAP_SYS_ADMIN to enter network namespaces")
                }
                return nil
            },
            Severity: "warning",
        },
        {
            Name: "ip-command",
            Check: func() error {
                // ip is required for network namespace management.
                if _, err := exec.LookPath("ip"); err != nil {
                    return fmt.Errorf("'ip' command not found (needed for netns management): %w", err)
                }
                return nil
            },
            Severity: "warning",
        },
        {
            Name: "dnsmasq-command",
            Check: func() error {
                // dnsmasq is required to serve DHCP leases to guest VMs.
                // Cocoon runs a per-bridge dnsmasq instance that provides
                // static DHCP leases matching CNI IPAM allocations (see Section 8.2).
                // Install: Ubuntu/Debian: apt install dnsmasq-base
                //          Fedora/RHEL:   dnf install dnsmasq
                //          macOS:         brew install dnsmasq
                if _, err := exec.LookPath("dnsmasq"); err != nil {
                    return fmt.Errorf("'dnsmasq' command not found (needed to serve DHCP leases to guest VMs): %w\n  Install: apt install dnsmasq-base (Debian/Ubuntu) or dnf install dnsmasq (Fedora/RHEL)", err)
                }
                return nil
            },
            Severity: "warning",
        },
        {
            Name: "tun-device",
            Check: func() error {
                // /dev/net/tun is required for creating TAP devices.
                if _, err := os.Stat("/dev/net/tun"); err != nil {
                    return fmt.Errorf("/dev/net/tun not available (needed for TAP devices): %w", err)
                }
                return nil
            },
            Severity: "warning",
        },
        {
            Name: "ch-cap-net-admin",
            Check: func() error {
                // CAP_NET_ADMIN on the cloud-hypervisor binary is required
                // for TAP device operations when not running as root.
                // This check is informational; root bypasses capability checks.
                if os.Getuid() == 0 {
                    return nil // Root has all capabilities.
                }
                // Check file capabilities on the CH binary.
                out, err := exec.Command("getcap", chBinaryPath).CombinedOutput()
                if err != nil {
                    return fmt.Errorf("cannot check capabilities on %s: %w", chBinaryPath, err)
                }
                if !strings.Contains(string(out), "cap_net_admin") {
                    return fmt.Errorf("cloud-hypervisor binary %s lacks CAP_NET_ADMIN (needed for TAP devices in non-root mode)", chBinaryPath)
                }
                return nil
            },
            Severity: "info",
        },
        {
            Name: "stale-ipam-allocations",
            Check: func() error {
                // Check for IPAM allocations referencing deleted VMs.
                // ... implementation ...
                return nil
            },
            Severity: "info",
            FixFunc: func() error {
                // Remove stale allocation files.
                // ... implementation ...
                return nil
            },
        },
    }
}
```

---

## 10. Security

### 10.1 Network Isolation Model

By default, VMs have no network interface (Phase 1 behavior). When networking is enabled, isolation depends on the CNI plugin configuration:

| Configuration | Isolation Level | Use Case |
|---------------|----------------|----------|
| `--network none` (default) | Complete isolation | AI Agent sandbox |
| `--network bridge` with `ipMasq: true` | VM can reach internet, host can reach VM | Development, package installation |
| `--network bridge` with `ipMasq: false` | VM-to-VM only within bridge | Multi-VM private network |
| `--network macvlan` | VM appears as physical device on LAN | Service hosting |

### 10.2 Privilege Requirements

Network namespace creation and TAP device creation require elevated privileges:

- `CLONE_NEWNET` (for creating namespaces): Requires `CAP_SYS_ADMIN` or root.
- `/dev/net/tun` (for creating TAP devices): Requires `CAP_NET_ADMIN` or root.
- iptables modification (for portmap): Requires `CAP_NET_ADMIN` or root.

Since Cocoon requires root, all these privileges are available automatically.

### 10.3 Namespace Boundaries

Each VM's network namespace provides kernel-level isolation:

- The VM cannot see host network interfaces.
- The VM cannot see other VMs' network interfaces (unless explicitly bridged).
- The VM cannot manipulate host iptables rules.
- The network namespace is destroyed on VM deletion, preventing resource leaks.

### 10.4 MAC Address Security

MAC addresses are deterministically generated from the VM ID and interface name (see Section 4.5). This means:

- MACs are locally-administered (bit 1 of octet 0 is set), avoiding conflict with OUI-assigned hardware MACs.
- MACs are unicast (bit 0 of octet 0 is cleared).
- Restarting a VM preserves the same MAC, maintaining DHCP lease stability.
- Different VMs always get different MACs (different VM IDs produce different hashes).

### 10.5 CNI Plugin Trust

CNI plugins execute as root on the host. Cocoon trusts plugins found in the configured plugin directories (`/opt/cni/bin`, etc.). Administrators must ensure that:

- Plugin directories are owned by root and not world-writable.
- Only trusted plugins are installed.
- Plugin binaries are not symlinks to untrusted locations.

`cocoon doctor` verifies plugin directory permissions as a security check.

### 10.6 CNI Plugin Compatibility with TC Redirect

The tc mirred redirect model works with any CNI plugin that produces a standard network interface inside the namespace, with caveats for eBPF-based plugins. See also the compatibility matrix in Section 1.3.

| CNI Plugin | Compatible | Notes |
|------------|-----------|-------|
| bridge | Yes | Primary plugin. Produces veth pair. Fully tested. |
| ptp | Yes | Point-to-point veth pair. TC redirect works identically. |
| macvlan | Yes | Produces macvlan interface. TC redirect works identically. |
| ipvlan | Yes | Produces ipvlan interface. TC redirect works identically. |
| host-local (IPAM) | Yes | Pure IPAM plugin, no interaction with TC redirect. |
| dhcp (IPAM) | Yes | DHCP lease acquired on the CNI interface. Guest receives IP via dnsmasq DHCP on the bridge. |
| portmap | Yes | Installs iptables DNAT rules keyed on IP address. TC redirect does not move the IP, so portmap rules remain valid. |
| static (IPAM) | Yes | Fixed IP assignment. No interaction with TC redirect. |
| Calico (iptables mode) | Yes | Creates veth pair with BGP routing on host side. TC redirect operates inside netns only. |
| Calico (eBPF mode) | Untested | eBPF datapath may be bypassed by TC mirred redirect. Same concerns as Cilium. |
| Cilium | Experimental | Cilium attaches eBPF programs to the veth interface. TC mirred redirect may bypass Cilium's eBPF datapath depending on hook points. Requires testing with specific Cilium version. |
| SR-IOV | N/A | SR-IOV VFs should use PCI passthrough (see [14-device-passthrough.md](./14-device-passthrough.md)), not TAP/TC redirect. |

**eBPF bypass note**: TC mirred redirect operates at Layer 2, forwarding packets directly between the CNI veth and the TAP device. This bypasses any eBPF programs or iptables rules attached to the netns-side interface. For Cilium and Calico eBPF mode, the mirred redirect replaces the ingress qdisc and may conflict with eBPF hook points. An alternative integration path (e.g., Cilium's native VM support or a dedicated tap plugin) may be required for these environments. This is deferred beyond Phase 2.

---

## 11. Testing

### 11.1 Unit Tests

#### MAC Address Generation

```go
func TestGenerateMAC(t *testing.T) {
    tests := []struct {
        name   string
        vmID   string
        ifName string
    }{
        {"basic", "vm-01HXY", "eth0"},
        {"different vm", "vm-01HXZ", "eth0"},
        {"different interface", "vm-01HXY", "eth1"},
    }

    seen := make(map[string]bool)
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            mac, err := generateMAC(tt.vmID, tt.ifName)
            if err != nil {
                t.Fatalf("generateMAC() error: %v", err)
            }

            // Verify locally administered bit.
            hw, _ := net.ParseMAC(mac)
            if hw[0]&0x02 == 0 {
                t.Error("MAC should be locally administered (bit 1 of octet 0)")
            }

            // Verify unicast bit.
            if hw[0]&0x01 != 0 {
                t.Error("MAC should be unicast (bit 0 of octet 0)")
            }

            // Verify deterministic.
            mac2, _ := generateMAC(tt.vmID, tt.ifName)
            if mac != mac2 {
                t.Errorf("MAC not deterministic: %s != %s", mac, mac2)
            }

            // Verify uniqueness across different inputs.
            if seen[mac] {
                t.Errorf("MAC collision: %s", mac)
            }
            seen[mac] = true
        })
    }
}
```

#### Port Mapping Parser

```go
func TestParsePublishFlag(t *testing.T) {
    tests := []struct {
        input    string
        want     PortMapping
        wantErr  bool
    }{
        {"8080:80", PortMapping{HostPort: 8080, ContainerPort: 80, Protocol: "tcp"}, false},
        {"8080:80/tcp", PortMapping{HostPort: 8080, ContainerPort: 80, Protocol: "tcp"}, false},
        {"8080:80/udp", PortMapping{HostPort: 8080, ContainerPort: 80, Protocol: "udp"}, false},
        {"127.0.0.1:8080:80", PortMapping{HostIP: "127.0.0.1", HostPort: 8080, ContainerPort: 80, Protocol: "tcp"}, false},
        {"127.0.0.1:8080:80/udp", PortMapping{HostIP: "127.0.0.1", HostPort: 8080, ContainerPort: 80, Protocol: "udp"}, false},
        {"invalid", PortMapping{}, true},
        {"8080:80/sctp", PortMapping{}, true},
    }

    for _, tt := range tests {
        t.Run(tt.input, func(t *testing.T) {
            got, err := parsePublishFlag(tt.input)
            if (err != nil) != tt.wantErr {
                t.Errorf("parsePublishFlag(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
            }
            if !tt.wantErr && got != tt.want {
                t.Errorf("parsePublishFlag(%q) = %+v, want %+v", tt.input, got, tt.want)
            }
        })
    }
}
```

#### CNI Config Loading

```go
func TestGetNetworkConfig(t *testing.T) {
    // Create a temp directory with test CNI configs.
    confDir := t.TempDir()
    writeTestConfig(t, confDir, "10-bridge.conflist", `{
        "cniVersion": "1.0.0",
        "name": "bridge",
        "plugins": [{"type": "bridge"}]
    }`)

    mgr := NewManager(confDir, []string{"/opt/cni/bin"})

    // Found.
    conf, err := mgr.GetNetworkConfig("bridge")
    if err != nil {
        t.Fatalf("GetNetworkConfig(bridge) error: %v", err)
    }
    if conf.Name != "bridge" {
        t.Errorf("name = %q, want %q", conf.Name, "bridge")
    }

    // Not found.
    _, err = mgr.GetNetworkConfig("nonexistent")
    if err == nil {
        t.Error("GetNetworkConfig(nonexistent) should error")
    }
}
```

### 11.2 Integration Tests

```go
func TestNetworkCreateDelete(t *testing.T) {
    if testing.Short() {
        t.Skip("skipping integration test")
    }
    if os.Getuid() != 0 {
        t.Skip("network integration tests require root")
    }

    // 1. Create a VM with bridge networking.
    // 2. Verify network namespace exists.
    // 3. Verify TAP device exists in namespace.
    // 4. Verify IP address allocated in IPAM store.
    // 5. Start VM, verify guest can ping gateway.
    // 6. Delete VM.
    // 7. Verify namespace removed.
    // 8. Verify IPAM allocation freed.
}

func TestMultipleNetworks(t *testing.T) {
    if testing.Short() {
        t.Skip("skipping integration test")
    }

    // 1. Create a VM with two networks.
    // 2. Verify two TAP devices in namespace.
    // 3. Verify two --net arguments to Cloud Hypervisor.
    // 4. Start VM, verify both interfaces visible in guest.
    // 5. Delete VM, verify both networks cleaned up.
}

func TestPortForwarding(t *testing.T) {
    if testing.Short() {
        t.Skip("skipping integration test")
    }

    // 1. Create VM with --network bridge --publish 18080:80.
    // 2. Start VM.
    // 3. Start a listener on port 80 inside guest.
    // 4. Connect to host:18080, verify forwarding works.
    // 5. Delete VM, verify iptables rules removed.
}

func TestNetworkNone(t *testing.T) {
    // 1. Create VM without --network.
    // 2. Verify no namespace created.
    // 3. Verify no --net argument to Cloud Hypervisor.
    // 4. Verify VM boots without network interface.
    // Ensures backward compatibility with Phase 1.
}

func TestNetworkCreateRollback(t *testing.T) {
    if testing.Short() {
        t.Skip("skipping integration test")
    }

    // 1. Create a VM with --network valid --network invalid.
    // 2. First network should be rolled back.
    // 3. Namespace should be cleaned up.
    // 4. No VM directory should exist (create failed).
}
```

### 11.3 CNI Plugin Compatibility Matrix

| Plugin | Version | Tested | Notes |
|--------|---------|--------|-------|
| bridge | v1.0.0+ | Yes | Primary plugin for Cocoon |
| host-local | v1.0.0+ | Yes | Default IPAM |
| portmap | v1.0.0+ | Yes | Port forwarding |
| macvlan | v1.0.0+ | Yes | Direct LAN access |
| dhcp | v1.0.0+ | Yes | External DHCP server required |
| static | v1.0.0+ | Yes | Fixed IP assignment |

### 11.4 Manual Verification Checklist

- [ ] `cocoon create --network bridge` creates namespace and TAP
- [ ] `cocoon inspect` shows IP, gateway, DNS
- [ ] Guest `eth0` has correct IP from CNI result
- [ ] Guest can ping gateway
- [ ] Guest can reach internet (if `ipMasq: true`)
- [ ] Port forwarding from host to guest works
- [ ] `cocoon delete` cleans up namespace, IPAM, iptables
- [ ] `cocoon create` without `--network` has no network (backward compatible)
- [ ] `cocoon network list` shows available networks
- [ ] Multiple `--network` flags produce multiple guest interfaces
- [ ] VM restart preserves MAC address and DHCP lease

---

## 12. Cross-References

### 12.1 Related Cocoon Documents

- [03-hypervisor-integration.md](./03-hypervisor-integration.md): Cloud Hypervisor process model, REST API mapping. The `CHVMConfig` struct is extended with `Net` field for `--net` arguments.
- [06-concurrency.md](./06-concurrency.md): Lock hierarchy. Network namespace creation does not require Cocoon-level locking (kernel namespaces are inherently isolated). However, the metadata lock (Level 4) is held during `NetworkState` updates.
- [07-vm-lifecycle.md](./07-vm-lifecycle.md): VM state machine. Network setup occurs during CREATING -> CREATED transition. Network cleanup occurs during -> DELETED transition. CNI ADD is called before `config.json` is finalized. CNI DEL is called before files are removed.
- [09-cli-design.md](./09-cli-design.md): CLI command structure. The `--network` and `--publish` flags are added to `vmCreateFlags()`, shared by `create` and `run`. The `network` subcommand is registered alongside existing commands.
- [12-console.md](./12-console.md): Console access. Console and networking are independent features that can coexist. A networked VM with console provides both SSH and serial access paths.

### 12.2 Interaction with Other Phase 2 Features

- **Pause/Resume** ([13-pause-resume.md](./13-pause-resume.md)): Pausing a VM does not affect network configuration. The TAP device and namespace persist. Traffic arriving during pause is buffered (or dropped, depending on queue depth). On resume, networking resumes immediately.
- **Checkpoint/Restore** ([15-warm-start.md](./15-warm-start.md)): On restore, the network namespace and TAP device must be recreated. The restore flow calls `AddNetwork` for each attachment before launching Cloud Hypervisor. Each restored VM gets a unique VM ID and therefore a unique MAC address, different from the source VM (because MAC = hash(vmID, ifName)). However, the deterministic generation from (vmID, ifName) ensures the MAC is stable across restarts of the same restored VM, preserving its DHCP lease.
- **Device Passthrough** ([14-device-passthrough.md](./14-device-passthrough.md)): A physical NIC can be passed through via VFIO as an alternative to virtio-net. This is orthogonal to CNI networking. If both are used, the VM sees both a virtio-net device (from CNI/TAP) and a passthrough NIC.
- **Volume Passthrough** ([17-volume-passthrough.md](./17-volume-passthrough.md)): Independent of networking. Both can be configured on the same VM.

### 12.3 External References

- CNI specification: https://www.cni.dev/docs/spec/
- CNI plugins reference: https://www.cni.dev/plugins/current/
- `github.com/containernetworking/cni` Go library: https://pkg.go.dev/github.com/containernetworking/cni
- Cloud Hypervisor `--net` documentation: https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/networking.md
- Cloud Hypervisor API schema (net section): https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/vmm/src/api/openapi/cloud-hypervisor.yaml
- Linux TAP/TUN documentation: `man 4 tun`
- Linux network namespaces: `man 7 network_namespaces`
- dnsmasq documentation: https://thekelleys.org.uk/dnsmasq/doc.html
- dnsmasq man page: https://thekelleys.org.uk/dnsmasq/docs/dnsmasq-man.html

---

**End of CNI-Based Networking Design Document v1.0**
