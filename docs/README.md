# Cocoon Design Documents

This directory contains design documents, specifications, and RFCs for Cocoon. These are living documents that evolve with the project.

## Design Specifications

### Phase 1: Core VM Management (docs/00-10)

| Doc | Title | Status | Description |
|-----|-------|--------|-------------|
| [00-overview.md](./00-overview.md) | Project Overview | Implemented | High-level architecture, supported image contract, and deployment strategy |
| [01-boot-contract.md](./01-boot-contract.md) | Boot Contract Specification | Implemented | Defines boot modes (UEFI/direct kernel boot), guest initialization, I/O mechanisms, and image requirements |
| [02-installation.md](./02-installation.md) | Installation | Implemented | Cloud Hypervisor installation, KVM setup, and host prerequisites |
| [03-hypervisor-integration.md](./03-hypervisor-integration.md) | Cloud Hypervisor Integration | Implemented | Process model, socket management, HTTP API integration, and crash recovery |
| [04-oci-conversion.md](./04-oci-conversion.md) | OCI to qcow2 Conversion | Implemented | Pipeline for converting OCI images into bootable qcow2 disk images |
| [05-storage-management.md](./05-storage-management.md) | Storage Management | Implemented | Directory layout, copy-on-write optimization, reference counting, and garbage collection |
| [06-concurrency.md](./06-concurrency.md) | Concurrency Design | Implemented | Lock hierarchy, atomic operations, crash consistency, and deadlock prevention |
| [07-vm-lifecycle.md](./07-vm-lifecycle.md) | VM Lifecycle Management | Implemented | State machine, identifier rules, metadata schema, idempotency, and reconciliation |
| [08-dependencies.md](./08-dependencies.md) | Dependencies and Requirements | Implemented | External tools, version requirements, installation instructions, and permissions |
| [09-cli-design.md](./09-cli-design.md) | CLI Design and Commands | Implemented | Command structure, flags, output formats, and supported image types |
| [10-implementation-roadmap.md](./10-implementation-roadmap.md) | Implementation Roadmap | Historical | Phase 1 development plan, critical path, testing strategy, and validation milestones |

### Phase 2: Advanced Features (docs/04.1, 11-13, 15-17)

| Doc | Title | Status | Description |
|-----|-------|--------|-------------|
| [04.1-oci-vm-images.md](./04.1-oci-vm-images.md) | OCI VM Image Format | Partial | Build/push/login/tag/inspect/verify/list implemented; direct kernel boot and virtiofs rootfs are Phase 2 planned |
| [11-bootable-oci-build.md](./11-bootable-oci-build.md) | Building Bootable OCI Images | Planned | Guidance on building custom OCI images that satisfy the Boot Contract |
| [12-console.md](./12-console.md) | VM Console | Implemented | Interactive bidirectional console via PTY, escape sequence handling, and terminal raw mode |
| [13-pause-resume.md](./13-pause-resume.md) | VM Pause and Resume | Planned | PAUSED state machine extension, vCPU freeze/unfreeze via CH API, and reconciliation |
| [15-warm-start.md](./15-warm-start.md) | VM Warm Start | Planned | Checkpoint/restore for sub-second VM creation, golden checkpoint workflow, and snapshot management |
| [16-networking.md](./16-networking.md) | CNI Networking | Planned | CNI plugin integration, bridge networking, control plane for VM network attachment |
| [17-volume-passthrough.md](./17-volume-passthrough.md) | Volume Passthrough | Planned | Host-to-guest directory sharing via virtio-fs, virtiofsd lifecycle management |

### Phase 3: Hardware and Ecosystem (docs/14)

| Doc | Title | Status | Description |
|-----|-------|--------|-------------|
| [14-device-passthrough.md](./14-device-passthrough.md) | PCI Device Passthrough | Planned | VFIO bind/unbind automation, IOMMU group validation, GPU convenience, and hotplug support |

## Future Feature Requests

The `future/` directory contains feature request specifications for features that do **not** yet have numbered design documents. Once a feature is promoted to a numbered doc, its future/ entry is removed.

| File | Title | Description |
|------|-------|-------------|
| [future/api-server.md](./future/api-server.md) | gRPC/REST API Server | Programmatic VM management, authentication, and multi-tenant support |
| [future/observability.md](./future/observability.md) | Monitoring and Logging | Prometheus metrics, structured logging, and distributed tracing |
| [future/storage-quotas.md](./future/storage-quotas.md) | Storage Quotas | Per-VM and global disk space limits for multi-tenant deployments |

See [future/README.md](./future/README.md) for details on active and superseded feature requests.

## RFCs

The `rfc/` directory contains the RFC process for proposing significant architectural or design changes to Cocoon. Use RFCs for new subsystems, breaking changes, or decisions with significant trade-offs.

- [rfc/README.md](./rfc/README.md) -- RFC process and guidelines
- [rfc/TEMPLATE.md](./rfc/TEMPLATE.md) -- Template for new RFC proposals

No formal RFCs have been created yet. The initial architecture is captured in the specification documents above (00 through 17).
