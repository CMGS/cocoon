# PCI Device Passthrough

**Version**: 1.0
**Status**: Planned
**Phase**: Phase 3
**Last Updated**: 2026-02-14

## Executive Summary

This document specifies the design for PCI device passthrough in Cocoon, enabling VMs to directly access host hardware (GPUs, NICs, NVMe drives) via VFIO. Device passthrough is essential for workloads that require near-native hardware performance: GPU-accelerated AI/ML training, high-performance networking with SR-IOV NICs, and low-latency NVMe storage. The design covers VFIO bind/unbind automation, IOMMU group validation, CLI integration, hotplug support, and safety mechanisms to prevent host instability.

## Table of Contents

1. [Overview](#1-overview)
2. [Prerequisites](#2-prerequisites)
3. [Design](#3-design)
4. [Configuration Changes](#4-configuration-changes)
5. [Implementation](#5-implementation)
6. [CLI](#6-cli)
7. [Safety](#7-safety)
8. [Error Handling](#8-error-handling)
9. [Security](#9-security)
10. [Testing](#10-testing)
11. [Implementation Plan](#11-implementation-plan)
12. [Unresolved Questions](#12-unresolved-questions)
13. [Cross-References](#13-cross-references)

---

## 1. Overview

### 1.1 Problem Statement

Cocoon Phase 1 VMs use paravirtualized devices for disk (virtio-blk) and serial I/O. This is sufficient for general-purpose workloads, but several critical use cases require direct hardware access:

1. **GPU for AI/ML**: CUDA/ROCm training and inference require direct GPU access. Paravirtualized or emulated GPUs are orders of magnitude slower. VFIO passthrough gives the guest full control of the GPU, enabling training performance within 1-3% of bare-metal.
2. **NIC for high-performance networking**: SR-IOV-capable NICs present Virtual Functions (VFs) that can be passed through individually, giving each VM near-line-rate networking without hypervisor overhead.
3. **NVMe for direct storage**: NVMe drives passed through directly provide consistent, low-latency storage I/O for databases and data-intensive workloads.
4. **SR-IOV for multi-tenant GPU sharing**: NVIDIA MIG (Multi-Instance GPU) partitions a single physical GPU into isolated virtual GPUs, each passable to a different VM.

### 1.2 Approach

Device passthrough uses the VFIO (Virtual Function I/O) framework, which provides safe, IOMMU-protected device assignment. A host PCI device is unbound from its host driver, bound to the `vfio-pci` kernel driver, and then assigned to a VM through Cloud Hypervisor's device configuration.

```
 Host                                 Guest VM
+----------------------------------+ +-----------------------------+
|                                  | |                             |
|  PCI Device (e.g., GPU)          | |  Guest driver (e.g., NVIDIA)|
|  0000:01:00.0                    | |                             |
|       |                          | |                             |
|       v                          | |                             |
|  vfio-pci driver                 | |                             |
|       |                          | |                             |
|       v                          | |                             |
|  /dev/vfio/14  (IOMMU group 14) | |                             |
|       |                          | |                             |
|       +------ IOMMU ------------>+ |  /dev/pci/0000:01:00.0     |
|              (DMA isolation)     | |  (direct hardware access)   |
|                                  | |                             |
+----------------------------------+ +-----------------------------+
|          Cloud Hypervisor (CH)                                    |
|  api.sock: PUT /api/v1/vm.create { "devices": [...] }           |
+------------------------------------------------------------------+
|  Cocoon CLI                                                      |
|  cocoon run --device 0000:01:00.0 ubuntu-gpu --cpus 8 --memory 16G|
+------------------------------------------------------------------+
```

### 1.3 Scope

**Phase 3 (this document)**:
- VFIO PCI device passthrough at VM creation time (`--device` flag)
- GPU convenience flag (`--gpu`)
- Automatic vfio-pci driver bind/unbind
- IOMMU group validation and safety checks
- Device reference tracking (prevent double-assignment)
- Cleanup on VM delete (restore host driver)
- Device hotplug via CH REST API (`cocoon device add/remove`)

**Phase 3 (future)**:
- Mediated devices (mdev) and vGPU (NVIDIA vGPU, Intel GVT-g)
- SR-IOV VF auto-creation and lifecycle management
- GPU resource pools and scheduling
- Live migration with device state
- Device health monitoring and automatic failover

---

## 2. Prerequisites

### 2.1 IOMMU Enabled

The host kernel must have IOMMU enabled via kernel boot parameters:

**Intel**:
```
intel_iommu=on iommu=pt
```

**AMD**:
```
amd_iommu=on iommu=pt
```

The `iommu=pt` (passthrough) flag avoids IOMMU translation overhead for host devices that do not need isolation.

Verification:
```bash
# Check IOMMU is enabled
dmesg | grep -i iommu
# Should show: "DMAR: IOMMU enabled" or "AMD-Vi: AMD IOMMUv2"

# Check IOMMU groups exist
ls /sys/kernel/iommu_groups/
```

### 2.2 vfio-pci Kernel Module

The `vfio-pci` kernel module must be loaded:

```bash
modprobe vfio-pci
```

For persistent loading, add to `/etc/modules-load.d/vfio.conf`:
```
vfio
vfio_iommu_type1
vfio_pci
```

### 2.3 IOMMU Group Isolation

All devices within an IOMMU group share the same isolation boundary. For safe passthrough, **every device in the group** must be either:

1. Passed through to the same VM, or
2. Bound to a safe stub driver (`vfio-pci` or `pci-stub`), or
3. A bridge/switch that does not require a driver

```
IOMMU Group 14:
  0000:01:00.0  NVIDIA GPU         -> must bind to vfio-pci
  0000:01:00.1  NVIDIA Audio       -> must also bind to vfio-pci (or pci-stub)
```

Cocoon validates this before permitting passthrough (see Section 7.1).

### 2.4 /dev/vfio Permissions

The Cloud Hypervisor process must have read/write access to the VFIO group device file:

```bash
ls -l /dev/vfio/14
# crw------- 1 root root 241, 0 Feb 14 10:00 /dev/vfio/14
```

Since Cocoon runs as root, this is satisfied automatically.

---

## 3. Design

### 3.1 VFIO Bind Flow

```
cocoon create --device 0000:01:00.0
        |
        v
+--- Validate PCI address format ---+
        |
        v
+--- Read IOMMU group from sysfs ---+
        |
        v
+--- Validate IOMMU group safety ---+  (Section 7.1)
        |
        v
+--- Check device not assigned ------+  (Section 7.3)
        |
        v
+--- Record original driver ---------+
        |  read /sys/bus/pci/devices/XXXX/driver -> e.g., "nvidia"
        v
+--- Unbind from host driver --------+
        |  echo XXXX > /sys/bus/pci/devices/XXXX/driver/unbind
        v
+--- Write vendor:device to vfio ----+
        |  echo "VVVV DDDD" > /sys/bus/pci/drivers/vfio-pci/new_id
        v
+--- Bind to vfio-pci ---------------+
        |  echo XXXX > /sys/bus/pci/drivers/vfio-pci/bind
        v
+--- Verify binding ------------------+
        |  readlink /sys/bus/pci/devices/XXXX/driver -> vfio-pci
        v
+--- Record in DeviceConfig ----------+
```

### 3.2 VFIO Unbind Flow (VM Delete)

```
cocoon delete myvm
        |
        v
+--- Load DeviceConfig from config.json ---+
        |  (includes original_driver)
        v
+--- Unbind from vfio-pci -----------------+
        |  echo XXXX > /sys/bus/pci/drivers/vfio-pci/unbind
        v
+--- Remove vendor:device from vfio -------+
        |  echo "VVVV DDDD" > /sys/bus/pci/drivers/vfio-pci/remove_id
        v
+--- Rebind to original driver -------------+
        |  echo XXXX > /sys/bus/pci/drivers/{original}/bind
        v
+--- Verify binding -------------------------+
        |  readlink /sys/bus/pci/devices/XXXX/driver -> original
        v
+--- Remove device from tracking ------------+
```

### 3.3 Hotplug Flow

Cloud Hypervisor supports runtime device hotplug via the REST API:

```
cocoon device add myvm 0000:03:00.0
    |
    v
[1] Resolve VM, verify RUNNING state
    |
    v
[2] Validate device, check IOMMU group, check not assigned
    |
    v
[3] Bind to vfio-pci (same as creation-time flow)
    |
    v
[4] PUT /api/v1/vm.add-device
    {
      "path": "/sys/bus/pci/devices/0000:03:00.0",
      "id": "hotplug-0000:03:00.0",
      "iommu": true
    }
    |
    v
[5] Track device, update metadata with hotplugged device
    |
    v
[6] Print success
```

Device removal:

```
cocoon device remove myvm 0000:03:00.0
    |
    v
[1] Resolve VM, verify RUNNING state
    |
    v
[2] PUT /api/v1/vm.remove-device { "id": "hotplug-0000:03:00.0" }
    |
    v
[3] Restore host driver
    |
    v
[4] Untrack device, update metadata
```

---

## 4. Configuration Changes

### 4.1 DeviceConfig Type

A new `DeviceConfig` type represents a single passthrough device:

```go
// In types/device.go

// DeviceConfig represents a PCI device passed through to a VM via VFIO.
type DeviceConfig struct {
    // PCIAddress is the BDF (Bus:Device.Function) address, e.g., "0000:01:00.0".
    PCIAddress string `json:"pci_address"`

    // SysfsPath is the full sysfs path, derived from PCIAddress.
    // e.g., "/sys/bus/pci/devices/0000:01:00.0"
    SysfsPath string `json:"sysfs_path"`

    // OriginalDriver is the host driver that was bound before passthrough.
    // Stored so that the device can be restored on VM delete.
    OriginalDriver string `json:"original_driver,omitempty"`

    // IOMMUGroup is the IOMMU group number for this device.
    IOMMUGroup int `json:"iommu_group"`

    // VendorID and DeviceID for device identification.
    VendorID string `json:"vendor_id"`
    DeviceID string `json:"device_id"`

    // DeviceClass is the PCI class code (e.g., "0300" for VGA, "0302" for 3D controller).
    DeviceClass string `json:"device_class"`

    // IsGPU indicates whether this device was specified via --gpu flag.
    IsGPU bool `json:"is_gpu,omitempty"`
}
```

### 4.2 VMConfig Changes

Add a `Devices` field to `types.VMConfig`:

```go
// In types/config.go (additions only)

type VMConfig struct {
    // ... existing fields ...

    // Devices lists PCI devices passed through via VFIO.
    // Empty in Phase 1; populated when --device or --gpu is used.
    Devices []DeviceConfig `json:"devices,omitempty"`
}
```

### 4.3 CHVMConfig Changes

Extend the Cloud Hypervisor API config to include the `devices` array:

```go
// In hypervisor/types.go (additions only)

type CHVMConfig struct {
    CPUs    CHCPUConfig     `json:"cpus"`
    Memory  CHMemoryConfig  `json:"memory"`
    Disks   []CHDiskConfig  `json:"disks,omitempty"`
    Serial  CHSerialConfig  `json:"serial"`
    Console CHConsoleConfig `json:"console"`
    // Phase 3: VFIO device passthrough
    Devices []CHDeviceConfig `json:"devices,omitempty"`
}

// CHDeviceConfig describes a VFIO device for the CH REST API.
// Corresponds to the "devices" array in PUT /api/v1/vm.create.
type CHDeviceConfig struct {
    // Path is the sysfs path to the PCI device.
    // e.g., "/sys/bus/pci/devices/0000:01:00.0"
    Path string `json:"path"`

    // ID is an optional identifier for the device within CH.
    // Used for hotplug remove operations.
    ID string `json:"id,omitempty"`

    // IOMMU indicates whether IOMMU translation is enabled for this device.
    // Should always be true for VFIO passthrough.
    IOMMU bool `json:"iommu"`
}
```

### 4.4 VMMetadataFile Changes

Track hotplugged devices in the mutable metadata:

```go
// In types/metadata.go (additions only)

type VMMetadataFile struct {
    // ... existing fields ...

    // HotpluggedDevices tracks devices added at runtime (not in config.json).
    // These are restored on VM delete but not on VM restart.
    HotpluggedDevices []DeviceConfig `json:"hotplugged_devices,omitempty"`
}
```

### 4.5 CocoonConfig Expansion

Add device-related global configuration:

```go
// In config/config.go (additions only)

type CocoonConfig struct {
    // ... existing fields ...

    // AutoBindVFIO controls whether Cocoon automatically binds devices
    // to vfio-pci. If false, the user must bind manually before passing
    // --device. Default: true.
    AutoBindVFIO bool `json:"auto_bind_vfio"`

    // AutoRestoreDriver controls whether Cocoon restores the original
    // host driver on VM delete. If false, devices remain bound to vfio-pci.
    // Default: true.
    AutoRestoreDriver bool `json:"auto_restore_driver"`
}
```

---

## 5. Implementation

### 5.1 DeviceManager Interface

```go
// In device/device.go

package device

import (
    "context"

    "github.com/CMGS/cocoon/types"
)

// Manager handles PCI device passthrough lifecycle.
type Manager interface {
    // Validate checks that a PCI address is valid and the device exists.
    Validate(ctx context.Context, pciAddr string) (*types.DeviceConfig, error)

    // ValidateIOMMUGroup checks that all devices in the IOMMU group
    // are safe for passthrough (bound to vfio-pci, pci-stub, or being
    // passed through together).
    ValidateIOMMUGroup(ctx context.Context, iommuGroup int, passingThrough []string) error

    // BindToVFIO unbinds the device from its host driver and binds it
    // to vfio-pci. Records the original driver in the returned DeviceConfig.
    BindToVFIO(ctx context.Context, dev *types.DeviceConfig) error

    // RestoreHostDriver unbinds the device from vfio-pci and rebinds it
    // to the original host driver recorded in DeviceConfig.
    RestoreHostDriver(ctx context.Context, dev *types.DeviceConfig) error

    // IsDeviceAssigned checks whether a PCI device is currently assigned
    // to any VM (prevents double-assignment).
    IsDeviceAssigned(pciAddr string) (vmID string, assigned bool)

    // TrackDevice records that a device is assigned to a VM.
    TrackDevice(pciAddr string, vmID string)

    // UntrackDevice removes the device-to-VM assignment record.
    UntrackDevice(pciAddr string)

    // UntrackAllForVM removes all device assignments for a given VM.
    UntrackAllForVM(vmID string)

    // ListHostDevices returns all PCI devices on the host with their
    // IOMMU group, current driver, and class information.
    ListHostDevices(ctx context.Context) ([]types.DeviceConfig, error)
}
```

### 5.2 IOMMU Group Validation

```go
// validateIOMMUGroup checks that passthrough of the requested devices
// does not leave unsafe devices under host control in the same IOMMU group.
func (m *deviceManager) ValidateIOMMUGroup(
    ctx context.Context,
    iommuGroup int,
    passingThrough []string,
) error {
    // Read all devices in the IOMMU group.
    groupPath := fmt.Sprintf("/sys/kernel/iommu_groups/%d/devices", iommuGroup)
    entries, err := os.ReadDir(groupPath)
    if err != nil {
        return fmt.Errorf("failed to read IOMMU group %d: %w", iommuGroup, err)
    }

    passThroughSet := make(map[string]bool)
    for _, addr := range passingThrough {
        passThroughSet[addr] = true
    }

    for _, entry := range entries {
        pciAddr := entry.Name()
        if passThroughSet[pciAddr] {
            continue // This device is being passed through.
        }

        // Check what driver is bound.
        driverPath := fmt.Sprintf("/sys/bus/pci/devices/%s/driver", pciAddr)
        driver, err := os.Readlink(driverPath)
        if err != nil {
            // No driver bound -- safe (no host driver to conflict).
            continue
        }
        driverName := filepath.Base(driver)

        // Safe stub drivers.
        if driverName == "vfio-pci" || driverName == "pci-stub" {
            continue
        }

        // Check if device is a PCI bridge/switch (safe to leave under host).
        classPath := fmt.Sprintf("/sys/bus/pci/devices/%s/class", pciAddr)
        class, _ := os.ReadFile(classPath)
        classStr := strings.TrimSpace(string(class))
        if strings.HasPrefix(classStr, "0x0604") || strings.HasPrefix(classStr, "0x0600") {
            continue
        }

        return fmt.Errorf(
            "IOMMU group %d: device %s is bound to host driver %q; "+
                "all devices in the group must be passed through or bound to "+
                "vfio-pci/pci-stub for safe isolation. Either pass through %s "+
                "as well (--device %s) or manually bind it to pci-stub",
            iommuGroup, pciAddr, driverName, pciAddr, pciAddr,
        )
    }

    return nil
}
```

### 5.3 GPU-Specific Handling

NVIDIA GPUs typically have a companion audio device (HDA controller) in the same IOMMU group. The `--gpu` flag automatically detects and includes companion functions:

```go
// resolveGPUDevices expands a --gpu PCI address to include all functions
// in the same slot (e.g., 0000:01:00.0 -> [0000:01:00.0, 0000:01:00.1]).
func resolveGPUDevices(gpuAddr string) ([]string, error) {
    // Parse the BDF address.
    slot := gpuAddr[:len(gpuAddr)-2] // "0000:01:00"

    // Find all functions in the same slot.
    pattern := fmt.Sprintf("/sys/bus/pci/devices/%s.*", slot)
    matches, err := filepath.Glob(pattern)
    if err != nil {
        return nil, err
    }

    var devices []string
    for _, m := range matches {
        devices = append(devices, filepath.Base(m))
    }

    return devices, nil
}
```

Active GPU process detection before unbinding the NVIDIA driver:

```go
func checkNVIDIAProcesses(pciAddr string) error {
    cmd := exec.Command("nvidia-smi",
        "--query-compute-apps=pid,name",
        "--format=csv,noheader",
        fmt.Sprintf("--id=%s", pciAddr),
    )
    out, err := cmd.Output()
    if err != nil {
        return nil // nvidia-smi not available or failed, skip check.
    }
    if len(strings.TrimSpace(string(out))) > 0 {
        return fmt.Errorf(
            "GPU %s has active processes; terminate them before passthrough:\n%s",
            pciAddr, string(out),
        )
    }
    return nil
}
```

### 5.4 Device Tracking

Device assignment tracking is stored in a global file to prevent double-assignment across VMs:

```
/var/lib/cocoon/db/device-assignments.json
```

```json
{
  "assignments": {
    "0000:01:00.0": {
      "vm_id": "vm-01HXYZ5A3B7C8D9E0F1G2H3J4K",
      "original_driver": "nvidia",
      "iommu_group": 14,
      "assigned_at": "2026-02-14T10:30:00Z"
    },
    "0000:01:00.1": {
      "vm_id": "vm-01HXYZ5A3B7C8D9E0F1G2H3J4K",
      "original_driver": "snd_hda_intel",
      "iommu_group": 14,
      "assigned_at": "2026-02-14T10:30:00Z"
    }
  },
  "schema_version": 1
}
```

This file is protected by a file lock (`/var/lib/cocoon/db/device-assignments.lock`) to prevent race conditions during concurrent VM creation.

```go
func (m *deviceManager) IsDeviceAssigned(pciAddr string) (string, bool) {
    m.mu.RLock()
    defer m.mu.RUnlock()

    if assignment, ok := m.assignments[pciAddr]; ok {
        return assignment.VMID, true
    }
    return "", false
}

func (m *deviceManager) UntrackAllForVM(vmID string) {
    m.mu.Lock()
    defer m.mu.Unlock()

    for addr, assignment := range m.assignments {
        if assignment.VMID == vmID {
            delete(m.assignments, addr)
        }
    }
    m.persist()
}
```

### 5.5 Hypervisor Client Additions

```go
// In hypervisor/hypervisor.go (additions to Client interface)

type Client interface {
    // ... existing methods ...

    // AddDevice hotplugs a VFIO device to a running VM.
    // Calls PUT /api/v1/vm.add-device.
    AddDevice(ctx context.Context, socketPath string, dev *CHDeviceConfig) error

    // RemoveDevice removes a previously hotplugged device from a running VM.
    // Calls PUT /api/v1/vm.remove-device with the device ID.
    RemoveDevice(ctx context.Context, socketPath string, deviceID string) error
}
```

### 5.6 VM Delete Extension

When a VM is deleted, Cocoon restores all passed-through devices to their original host drivers:

```go
func deleteVMWithDeviceCleanup(ctx context.Context, vmID string, force bool) error {
    // 1. Load config to get device list.
    cfg, err := LoadConfig(vmID)
    if err != nil {
        return err
    }

    // 2. Stop VM (existing flow).
    // ... existing stop/kill logic ...

    // 3. Restore all static devices (from config.json).
    for _, dev := range cfg.Devices {
        if err := devMgr.RestoreHostDriver(ctx, &dev); err != nil {
            log.Printf("WARNING: failed to restore driver for %s: %v", dev.PCIAddress, err)
            // Continue cleanup -- don't fail the entire delete.
        }
        devMgr.UntrackDevice(dev.PCIAddress)
    }

    // 4. Restore hotplugged devices (from metadata.json).
    meta, _ := LoadMetadata(vmID)
    if meta != nil {
        for _, dev := range meta.HotpluggedDevices {
            if err := devMgr.RestoreHostDriver(ctx, &dev); err != nil {
                log.Printf("WARNING: failed to restore driver for %s: %v", dev.PCIAddress, err)
            }
            devMgr.UntrackDevice(dev.PCIAddress)
        }
    }

    // 5. Continue with existing delete flow (remove overlay, config, metadata, refs).
    // ...
}
```

### 5.7 appContext Expansion

Add the device manager to the application context:

```go
// In cmd/cocoon/app.go

type appContext struct {
    cfg    *config.CocoonConfig
    vmMgr  vm.Manager
    imgMgr image.Manager
    hyper  hypervisor.Client
    refCtr storage.ReferenceCounter
    cowMgr storage.COWManager
    gc     storage.GarbageCollector
    devMgr device.Manager  // Phase 3: device passthrough
}
```

---

## 6. CLI

### 6.1 Static Device Assignment Flags

Devices are specified on `cocoon create` or `cocoon run` via the `--device` flag:

```go
func vmCreateFlags() []cli.Flag {
    return []cli.Flag{
        // ... existing flags (name, cpus, memory, disk, boot-strategy) ...
        &cli.StringSliceFlag{
            Name:  "device",
            Usage: "PCI device to pass through (e.g., 0000:01:00.0). Repeatable.",
        },
        &cli.StringSliceFlag{
            Name:  "gpu",
            Usage: "GPU PCI address to pass through (e.g., 0000:01:00.0). Repeatable.",
        },
    }
}
```

### 6.2 Device Subcommand

```go
func deviceCommand() *cli.Command {
    return &cli.Command{
        Name:  "device",
        Usage: "Manage PCI device passthrough for VMs",
        Subcommands: []*cli.Command{
            {
                Name:      "add",
                Usage:     "Attach a PCI device to a running VM",
                ArgsUsage: "VM_REF PCI_ADDR",
                Action:    deviceAddAction,
            },
            {
                Name:      "remove",
                Usage:     "Detach a PCI device from a running VM",
                ArgsUsage: "VM_REF PCI_ADDR",
                Action:    deviceRemoveAction,
            },
            {
                Name:      "list",
                Aliases:   []string{"ls"},
                Usage:     "List PCI devices attached to a VM",
                ArgsUsage: "VM_REF",
                Flags: []cli.Flag{
                    &cli.StringFlag{
                        Name:  "format",
                        Value: "table",
                        Usage: "output format (table, json)",
                    },
                },
                Action: deviceListAction,
            },
        },
    }
}
```

### 6.3 Usage Examples

```bash
# Pass a single device at creation time
cocoon run ubuntu-gpu --device 0000:01:00.0 --cpus 8 --memory 16G

# Pass multiple devices (GPU + audio function)
cocoon run ubuntu-gpu \
  --device 0000:01:00.0 \
  --device 0000:01:00.1 \
  --cpus 8 --memory 16G

# GPU convenience flag (auto-includes companion functions)
cocoon run ubuntu-gpu --gpu 0000:01:00.0 --cpus 8 --memory 16G

# Hotplug a device to a running VM
cocoon device add myvm 0000:03:00.0

# Remove a hotplugged device
cocoon device remove myvm 0000:03:00.0

# List devices attached to a VM
cocoon device list myvm
PCI ADDRESS      VENDOR   DEVICE   CLASS   IOMMU GROUP   TYPE       SOURCE
0000:01:00.0     10de     2204     0300    14             GPU        config
0000:01:00.1     10de     1aef     0403    14             Audio      config
0000:03:00.0     15b3     1017     0200    22             NIC        hotplug

# JSON output
cocoon device list myvm --format json
```

### 6.4 Inspect Output with Devices

```json
{
  "vm_id": "vm-01HXYZ5A3B7C8D9E0F1G2H3J4K",
  "name": "gpu-worker",
  "state": "RUNNING",
  "devices": [
    {
      "pci_address": "0000:01:00.0",
      "vendor_id": "10de",
      "device_id": "2204",
      "device_class": "0300",
      "iommu_group": 14,
      "original_driver": "nvidia",
      "is_gpu": true
    },
    {
      "pci_address": "0000:01:00.1",
      "vendor_id": "10de",
      "device_id": "1aef",
      "device_class": "0403",
      "iommu_group": 14,
      "original_driver": "snd_hda_intel"
    }
  ]
}
```

### 6.5 VMInspect Extension

```go
// In types/inspect.go (addition)

type VMInspect struct {
    // ... existing fields ...

    // Devices lists all PCI devices attached to the VM.
    Devices []InspectDeviceInfo `json:"devices,omitempty"`
}

type InspectDeviceInfo struct {
    PCIAddress     string `json:"pci_address"`
    VendorID       string `json:"vendor_id"`
    DeviceID       string `json:"device_id"`
    DeviceClass    string `json:"device_class"`
    IOMMUGroup     int    `json:"iommu_group"`
    OriginalDriver string `json:"original_driver"`
    IsGPU          bool   `json:"is_gpu,omitempty"`
    Hotplugged     bool   `json:"hotplugged,omitempty"`
}
```

---

## 7. Safety

### 7.1 IOMMU Group Validation

Before permitting passthrough, Cocoon validates the entire IOMMU group (see Section 5.2 for implementation). This prevents:

- DMA attacks from host-controlled devices in the same group
- Guest-to-host memory access via shared IOMMU contexts
- Data corruption from conflicting IOMMU mappings

The check runs at VM creation time (for `--device` / `--gpu` flags) and at hotplug time (for `cocoon device add`).

### 7.2 Blocked Device Classes

Certain PCI devices must never be passed through because they are required for host operation:

```go
// blockedDeviceClasses contains PCI class codes that must never be passed through.
var blockedDeviceClasses = map[string]string{
    "0x0600": "host bridge",
    "0x0601": "ISA bridge",
    "0x0602": "EISA bridge",
    "0x0603": "MCA bridge",
    "0x0604": "PCI bridge",
    "0x0605": "PCMCIA bridge",
    "0x0880": "system peripheral",
}

// blockedDeviceAddresses are specific PCI addresses that must never be passed.
var blockedDeviceAddresses = map[string]string{
    "0000:00:00.0": "root complex / host bridge",
}

func isBlockedDevice(pciAddr string, classCode string) (string, bool) {
    if reason, blocked := blockedDeviceAddresses[pciAddr]; blocked {
        return reason, true
    }
    prefix := classCode[:6] // "0x0600"
    if reason, blocked := blockedDeviceClasses[prefix]; blocked {
        return reason, true
    }
    return "", false
}
```

### 7.3 Double-Assignment Prevention

A PCI device can only be assigned to one VM at a time. The assignment protocol:

1. Acquire `device-assignments.lock`
2. Check if the device is already assigned
3. If assigned to a different VM, reject with an error
4. If unassigned, record the assignment
5. Release the lock

### 7.4 Cleanup on VM Delete

When a VM is deleted (or force-killed), Cocoon restores all passed-through devices to their original host drivers (see Section 5.6). Restoration failures are logged as warnings but do not block the delete operation.

### 7.5 Reconciliation

The `cocoon doctor` reconciliation flow is extended to detect stale device assignments:

- If a device is tracked as assigned to a VM that no longer exists, untrack and restore the host driver
- If a device is tracked as assigned but is not currently bound to `vfio-pci`, update tracking
- If a VM's config references devices that are not tracked, reconcile the tracking state

---

## 8. Error Handling

### 8.1 Error Cases

| Condition | Error Message | Exit Code |
|-----------|--------------|-----------|
| Invalid PCI address format | `invalid PCI address "foo": expected format 0000:00:00.0` | 1 |
| Device does not exist | `PCI device 0000:99:00.0 not found in sysfs` | 1 |
| IOMMU not enabled | `IOMMU not enabled: /sys/kernel/iommu_groups/ is empty` | 1 |
| vfio-pci module not loaded | `vfio-pci driver not available: /sys/bus/pci/drivers/vfio-pci not found` | 1 |
| IOMMU group unsafe | `IOMMU group 14: device 0000:01:00.1 is bound to host driver "snd_hda_intel"...` | 1 |
| Device already assigned | `device 0000:01:00.0 is already assigned to VM gpu-worker` | 1 |
| Blocked device | `cannot pass through 0000:00:00.0: root complex / host bridge` | 1 |
| GPU has active processes | `GPU 0000:01:00.0 has active processes; terminate them before passthrough` | 1 |
| Unbind failure | `failed to unbind 0000:01:00.0 from nvidia: permission denied` | 1 |
| Bind failure | `failed to bind 0000:01:00.0 to vfio-pci: device or resource busy` | 1 |
| Hotplug on non-running VM | `VM myvm is not running (state: STOPPED)` | 1 |
| CH hotplug API failure | `hotplug failed: CH returned 500: ...` | 1 |
| Restore driver failure (delete) | WARNING logged, delete continues | 0 |

### 8.2 Rollback on Bind Failure

If binding to vfio-pci fails after unbinding from the host driver, Cocoon attempts to rebind to the original driver:

```go
func (m *deviceManager) BindToVFIO(ctx context.Context, dev *DeviceConfig) error {
    // Record original driver.
    origDriver, err := readCurrentDriver(dev.PCIAddress)
    if err != nil {
        return err
    }
    dev.OriginalDriver = origDriver

    // Unbind from host driver.
    if err := unbindDriver(dev.PCIAddress); err != nil {
        return fmt.Errorf("unbind from %s: %w", origDriver, err)
    }

    // Bind to vfio-pci.
    if err := bindToVFIO(dev.PCIAddress, dev.VendorID, dev.DeviceID); err != nil {
        // Rollback: attempt to rebind original driver.
        _ = rebindDriver(dev.PCIAddress, origDriver)
        return fmt.Errorf("bind to vfio-pci: %w", err)
    }

    return nil
}
```

---

## 9. Security

### 9.1 VFIO and IOMMU Isolation

VFIO with IOMMU provides strong DMA isolation: the guest can only access memory regions explicitly mapped by the hypervisor through IOMMU page tables.

| Threat | Mitigation |
|--------|-----------|
| DMA to host memory | IOMMU isolates guest DMA; CH configures IOMMU mappings |
| Device reset attacks | CH performs function-level reset (FLR) on VM shutdown |
| IOMMU bypass | Firmware-level IOMMU (VT-d / AMD-Vi) required; no software-only fallback |
| Partial group passthrough | IOMMU group validation (Section 7.1) prevents this |
| P2P DMA between devices | Safe when all devices in group are assigned to the same VM |
| Guest exploiting device firmware | Out of scope; depends on device vendor security |

### 9.2 /dev/vfio Permissions

VFIO group device files (`/dev/vfio/N`) are owned by root with mode `0600` by default. Cocoon and CH run as root, so access is automatic.

### 9.3 Kernel Parameter Hardening

For production deployments with device passthrough:

```
iommu.strict=1              # Strict IOMMU mode
intremap=on                 # Enable interrupt remapping
pcie_acs_override=downstream,multifunction  # Better IOMMU grouping (non-upstream patch)
```

---

## 10. Testing

### 10.1 Unit Tests

```go
func TestPCIAddressValidation(t *testing.T) {
    tests := []struct {
        addr    string
        wantErr bool
    }{
        {"0000:01:00.0", false},
        {"0000:ff:1f.7", false},
        {"01:00.0", true},       // missing domain
        {"0000:01:00", true},    // missing function
        {"xxxx:01:00.0", true},  // invalid hex
        {"", true},
    }
    for _, tt := range tests {
        err := validatePCIAddress(tt.addr)
        if (err != nil) != tt.wantErr {
            t.Errorf("validatePCIAddress(%q) error = %v, wantErr %v", tt.addr, err, tt.wantErr)
        }
    }
}

func TestBlockedDeviceDetection(t *testing.T) {
    reason, blocked := isBlockedDevice("0000:00:00.0", "0x060000")
    if !blocked {
        t.Error("expected root complex to be blocked")
    }
    if reason == "" {
        t.Error("expected a reason for blocking")
    }

    _, blocked = isBlockedDevice("0000:01:00.0", "0x030000")
    if blocked {
        t.Error("VGA controller should not be blocked")
    }
}

func TestDeviceAssignmentTracking(t *testing.T) {
    mgr := newTestDeviceManager(t)

    // Initially unassigned.
    _, assigned := mgr.IsDeviceAssigned("0000:01:00.0")
    if assigned {
        t.Error("device should not be assigned initially")
    }

    // Assign.
    mgr.TrackDevice("0000:01:00.0", "vm-test-001")
    vmID, assigned := mgr.IsDeviceAssigned("0000:01:00.0")
    if !assigned || vmID != "vm-test-001" {
        t.Errorf("expected assigned to vm-test-001, got %s/%v", vmID, assigned)
    }

    // Untrack.
    mgr.UntrackDevice("0000:01:00.0")
    _, assigned = mgr.IsDeviceAssigned("0000:01:00.0")
    if assigned {
        t.Error("device should be unassigned after untrack")
    }
}

func TestUntrackAllForVM(t *testing.T) {
    mgr := newTestDeviceManager(t)

    mgr.TrackDevice("0000:01:00.0", "vm-test-001")
    mgr.TrackDevice("0000:01:00.1", "vm-test-001")
    mgr.TrackDevice("0000:02:00.0", "vm-test-002")

    mgr.UntrackAllForVM("vm-test-001")

    _, a1 := mgr.IsDeviceAssigned("0000:01:00.0")
    _, a2 := mgr.IsDeviceAssigned("0000:01:00.1")
    _, a3 := mgr.IsDeviceAssigned("0000:02:00.0")

    if a1 || a2 {
        t.Error("vm-test-001 devices should be untracked")
    }
    if !a3 {
        t.Error("vm-test-002 device should still be tracked")
    }
}
```

### 10.2 Integration Tests (Requires VFIO-Capable Host)

```go
func TestVFIOBindUnbind(t *testing.T) {
    if testing.Short() {
        t.Skip("skipping integration test requiring VFIO hardware")
    }

    pciAddr := os.Getenv("TEST_VFIO_PCI_ADDR")
    if pciAddr == "" {
        t.Skip("TEST_VFIO_PCI_ADDR not set")
    }

    mgr := newDeviceManager()

    dev, err := mgr.Validate(context.Background(), pciAddr)
    if err != nil {
        t.Fatalf("validate failed: %v", err)
    }

    err = mgr.BindToVFIO(context.Background(), dev)
    if err != nil {
        t.Fatalf("bind failed: %v", err)
    }

    driver := readCurrentDriver(pciAddr)
    if driver != "vfio-pci" {
        t.Errorf("expected vfio-pci driver, got %s", driver)
    }

    err = mgr.RestoreHostDriver(context.Background(), dev)
    if err != nil {
        t.Fatalf("restore failed: %v", err)
    }

    driver = readCurrentDriver(pciAddr)
    if driver != dev.OriginalDriver {
        t.Errorf("expected %s driver, got %s", dev.OriginalDriver, driver)
    }
}
```

### 10.3 End-to-End Test

```go
func TestVMWithGPUPassthrough(t *testing.T) {
    if testing.Short() {
        t.Skip("skipping E2E test")
    }

    pciAddr := os.Getenv("TEST_GPU_PCI_ADDR")
    if pciAddr == "" {
        t.Skip("TEST_GPU_PCI_ADDR not set")
    }

    vmID := createTestVM(t, "--device", pciAddr, "--cpus", "4", "--memory", "8G")
    defer deleteTestVM(t, vmID)

    inspect := inspectTestVM(t, vmID)
    if len(inspect.Devices) == 0 {
        t.Fatal("expected at least one device in inspect output")
    }
    if inspect.Devices[0].PCIAddress != pciAddr {
        t.Errorf("expected device %s, got %s", pciAddr, inspect.Devices[0].PCIAddress)
    }

    stopTestVM(t, vmID)

    driver := readCurrentDriver(pciAddr)
    if driver == "vfio-pci" {
        t.Error("device should be restored to original driver after VM delete")
    }
}
```

---

## 11. Implementation Plan

### Stage 1: VFIO Basic Passthrough (2-3 weeks)

1. Add `DeviceConfig` type to `types/device.go`
2. Add `Devices` field to `VMConfig` and `CHVMConfig`
3. Implement `device.Manager` with bind/unbind/validate/track
4. Add `--device` flag to `cocoon create` / `cocoon run`
5. Extend `CreateVM` REST call to include `devices` array
6. Implement `device-assignments.json` tracking
7. Extend VM delete flow with driver restoration
8. Extend `cocoon inspect` with device information
9. Extend `cocoon doctor` for device reconciliation
10. Unit and integration tests

### Stage 2: GPU Convenience (1 week)

1. Add `--gpu` flag with PCI class validation
2. Implement `resolveGPUDevices()` for multi-function expansion
3. Add NVIDIA-specific checks (active process detection)
4. Add `cocoon device list-host` for discovering available devices
5. Documentation and examples for common GPU models

### Stage 3: Hotplug Support (1-2 weeks)

1. Add `AddDevice` / `RemoveDevice` to `hypervisor.Client`
2. Implement `cocoon device add` / `cocoon device remove` commands
3. Track hotplugged devices in `metadata.json`
4. Handle cleanup of hotplugged devices on VM stop/delete
5. Integration tests with device hotplug

### Stage 4: SR-IOV Awareness (1 week, stretch)

1. Detect VF/PF relationships from sysfs
2. Warn when passing through a PF that has active VFs
3. Display VF/PF info in `cocoon device list-host`
4. Documentation for SR-IOV NIC passthrough

---

## 12. Unresolved Questions

1. **iommu=off mode**: Should Cocoon support passthrough without IOMMU (unsafe, but faster for development)? This would require a `--unsafe-no-iommu` flag. Decision: Defer until user demand.

2. **Multi-GPU assignment**: Should there be a `--gpu-count` option with automatic selection? Decision: Start with explicit PCI addresses; add `--gpu-count` if needed.

3. **Hotplug persistence across restart**: Hotplugged devices are tracked in `metadata.json` and restored on VM delete but NOT re-attached on VM restart. Only `config.json` devices are re-attached. This matches the "config is immutable" principle. Decision: Keep as-is.

4. **Device passthrough and live migration**: VFIO devices cannot be live-migrated in Cloud Hypervisor today. Decision: Defer to Phase 3; reject migration requests for VMs with passthrough devices.

5. **ACS override kernel patch**: Many consumer motherboards have poor IOMMU grouping. The `pcie_acs_override` patch fixes this but is not upstream. Decision: Document in troubleshooting but do not require.

6. **Device permissions**: VFIO group access requires root. Cocoon runs as root, so `/dev/vfio/N` access is automatic.

---

## 13. Cross-References

### 13.1 Related Cocoon Documents

- [03-hypervisor-integration.md](./03-hypervisor-integration.md): CH process model and REST API integration. Device passthrough adds the `devices` array to `vm.create` and uses `vm.add-device`/`vm.remove-device` for hotplug.
- [05-storage-management.md](./05-storage-management.md): Storage layout. Device assignments are tracked in `/var/lib/cocoon/db/device-assignments.json`.
- [06-concurrency.md](./06-concurrency.md): Lock hierarchy. The device assignment lock is at Level 2 (same as name-index.lock).
- [07-vm-lifecycle.md](./07-vm-lifecycle.md): VM state machine and delete flow. Device cleanup extends the delete sequence.
- [09-cli-design.md](./09-cli-design.md): CLI command structure. New `--device`/`--gpu` flags and `cocoon device` subcommand.

### 13.2 Interaction with Other Advanced Features

- **Console** ([12-console.md](./12-console.md)): Device passthrough is independent of console. Both can coexist on the same VM.
- **Pause/Resume** ([13-pause-resume.md](./13-pause-resume.md)): VMs with passthrough devices can be paused and resumed. VFIO binding is maintained across pause/resume.
- **Warm Start** ([15-warm-start.md](./15-warm-start.md)): VMs with passthrough devices cannot be checkpointed because CH does not support snapshotting VFIO device state. `cocoon checkpoint` returns a clear error when passthrough devices are detected.

### 13.3 Combined CHVMConfig Target

When all advanced features (Phase 2 + Phase 3) are implemented, the unified `CHVMConfig`:

```go
type CHVMConfig struct {
    Payload *CHPayloadConfig `json:"payload,omitempty"`   // Boot firmware or kernel
    CPUs    CHCPUConfig      `json:"cpus"`
    Memory  CHMemoryConfig   `json:"memory"`
    Disks   []CHDiskConfig   `json:"disks,omitempty"`
    Fs      []CHFsConfig     `json:"fs,omitempty"`       // Volume passthrough (virtio-fs)
    Serial  CHSerialConfig   `json:"serial"`
    Console CHConsoleConfig  `json:"console"`             // Console: mode "Pty"
    TPM     *CHTPMConfig     `json:"tpm,omitempty"`       // TPM 2.0 emulation (swtpm)
    Devices []CHDeviceConfig `json:"devices,omitempty"`   // Device passthrough (VFIO)
}
```

### 13.4 External References

- Cloud Hypervisor VFIO: https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/vfio.md
- Cloud Hypervisor API: https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/vmm/src/api/openapi/cloud-hypervisor.yaml
- VFIO documentation: https://docs.kernel.org/driver-api/vfio.html
- IOMMU grouping: https://vfio.blogspot.com/2014/08/iommu-groups-inside-and-out.html

---

**End of PCI Device Passthrough Design Document v1.0**
