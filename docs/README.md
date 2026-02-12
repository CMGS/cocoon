# Cocoon Design Documents

This directory contains design documents, specifications, and RFCs for Cocoon. These are living documents that evolve with the project.

## Specifications

| File | Title | Description |
|------|-------|-------------|
| [00-overview.md](./00-overview.md) | Project Overview | High-level architecture, supported image contract, and deployment strategy |
| [01-boot-contract.md](./01-boot-contract.md) | Boot Contract Specification | Defines boot modes (PVH/UEFI), guest initialization, I/O mechanisms, and image requirements |
| [02-installation.md](./02-installation.md) | Installation | Cloud Hypervisor installation, KVM setup, and host prerequisites |
| [03-hypervisor-integration.md](./03-hypervisor-integration.md) | Cloud Hypervisor Integration | Process model, socket management, HTTP API integration, and crash recovery |
| [04-oci-conversion.md](./04-oci-conversion.md) | OCI to qcow2 Conversion | Pipeline for converting OCI images into bootable qcow2 disk images |
| [05-storage-management.md](./05-storage-management.md) | Storage Management | Directory layout, copy-on-write optimization, reference counting, and garbage collection |
| [06-concurrency.md](./06-concurrency.md) | Concurrency Design | Lock hierarchy, atomic operations, crash consistency, and deadlock prevention |
| [07-vm-lifecycle.md](./07-vm-lifecycle.md) | VM Lifecycle Management | State machine, identifier rules, metadata schema, idempotency, and reconciliation |
| [08-dependencies.md](./08-dependencies.md) | Dependencies and Requirements | External tools, version requirements, installation instructions, and permissions |
| [09-cli-design.md](./09-cli-design.md) | CLI Design and Commands | Command structure, flags, output formats, and supported image types |
| [10-implementation-roadmap.md](./10-implementation-roadmap.md) | Implementation Roadmap | Phase 1 development plan, critical path, testing strategy, and validation milestones |
| [11-bootable-oci-build.md](./11-bootable-oci-build.md) | Building Bootable OCI Images | Guidance on building custom OCI images that satisfy the Boot Contract (planned) |

## Future

The `future/` directory contains specifications for features planned for Phase 2. These are explicitly deferred from Phase 1.

| File | Title | Description |
|------|-------|-------------|
| [future/networking.md](./future/networking.md) | Network Configuration | TAP/bridge networking, port forwarding, and network isolation |
| [future/api-server.md](./future/api-server.md) | gRPC/REST API Server | Programmatic VM management, authentication, and multi-tenant support |
| [future/observability.md](./future/observability.md) | Monitoring and Logging | Prometheus metrics, structured logging, and distributed tracing |
| [future/storage-quotas.md](./future/storage-quotas.md) | Storage Quotas | Per-VM and global disk space limits for multi-tenant deployments |

## RFCs

The `rfc/` directory contains the RFC process for proposing significant architectural or design changes to Cocoon. Use RFCs for new subsystems, breaking changes, or decisions with significant trade-offs.

- [rfc/README.md](./rfc/README.md) -- RFC process and guidelines
- [rfc/TEMPLATE.md](./rfc/TEMPLATE.md) -- Template for new RFC proposals

No formal RFCs have been created yet. The initial architecture is captured in the specification documents above (00 through 11).
