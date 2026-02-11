# Cocoon Documentation

**Cocoon** is a lightweight VM manager built on Cloud Hypervisor for managing microVMs with fast boot times and minimal resource overhead.

## ⚠️ Important: Supported Images

**Cocoon requires bootable VM images, NOT regular container images.**

✅ **Supported**:
- Cloud Hypervisor native cloud images (Ubuntu Cloud, Fedora Cloud, qcow2 format) - **Recommended**
- Bootable OCI images (custom-built with kernel, initrd, systemd, bootloader)

❌ **NOT Supported**:
- Regular container images (`ubuntu:latest`, `python:3.11`, `node:20`, etc.)
- These are application filesystems without kernel/bootloader - **will fail bootability check**

See **[00-overview.md § Supported Image Contract](./00-overview.md#️-supported-image-contract)** for details.

---

## Quick Navigation

### Phase 1: Core Functionality (Current)

Getting Cocoon up and running with OCI image support:

| Doc | Title | Status | Priority |
|-----|-------|--------|----------|
| [00](./00-overview.md) | Project Overview | ✅ Draft | - |
| [01](./01-boot-contract.md) | Boot Contract | ✅ Draft | **P0** |
| [02](./02-installation.md) | Installation & Setup | ✅ Draft | P0 |
| [03](./03-hypervisor-integration.md) | Hypervisor Integration | ✅ Draft | **P0** |
| [04](./04-oci-conversion.md) | OCI to VM Conversion | ✅ Draft | P0 |
| [05](./05-storage-management.md) | Storage Management | ✅ Draft | P0 |
| [06](./06-concurrency.md) | Concurrency & Consistency | ✅ Draft | **P0** |
| [07](./07-vm-lifecycle.md) | VM Lifecycle & State | ✅ Draft | **P0** |
| [08](./08-dependencies.md) | Dependencies & Permissions | ✅ Draft | **P0** |
| [09](./09-cli-design.md) | CLI Design | ✅ Draft | P1 |
| [10](./10-implementation-roadmap.md) | Implementation Roadmap | ✅ Draft | P1 |

**P0 = Critical** - Must be resolved before implementation starts
**P1 = Important** - Should be clarified during implementation
**P2 = Nice to have** - Can be deferred

### Phase 2: Production Features (Future)

Service exposure, networking, and operational tooling:

| Doc | Title | Status |
|-----|-------|--------|
| [future/networking](./future/networking.md) | Network Configuration | 📋 Planned |
| [future/api-server](./future/api-server.md) | gRPC/REST API Server | 📋 Planned |
| [future/observability](./future/observability.md) | Monitoring & Logging | 📋 Planned |

## Reading Path

### For Implementers

Follow this order to understand the system:

1. **[Overview](./00-overview.md)** - Understand the why and what
2. **[Boot Contract](./01-boot-contract.md)** - Critical: How OCI images become bootable VMs
3. **[Hypervisor Integration](./03-hypervisor-integration.md)** - Critical: How to manage Cloud Hypervisor processes
4. **[VM Lifecycle](./07-vm-lifecycle.md)** - Critical: State machine and metadata schema
5. **[Concurrency](./06-concurrency.md)** - Critical: Locking and consistency
6. **[Dependencies](./08-dependencies.md)** - Critical: What needs to be installed and configured

Then read the rest in any order.

### For Operators

Start here:

1. **[Installation](./02-installation.md)** - Set up Cloud Hypervisor
2. **[Dependencies](./08-dependencies.md)** - Install and configure prerequisites
3. **[CLI Design](./09-cli-design.md)** - How to use Cocoon

### For Architects

Read these for design decisions:

1. **[Overview](./00-overview.md)** - Architecture and trade-offs
2. **[Boot Contract](./01-boot-contract.md)** - Core abstraction
3. **[Storage Management](./05-storage-management.md)** - COW and caching strategy
4. **[Concurrency](./06-concurrency.md)** - Consistency guarantees

## Document Status Legend

- ✅ **Draft** - Content written, under review
- 🔄 **In Progress** - Being written
- 📋 **Planned** - Not yet started
- ⚠️ **Needs Update** - Outdated, requires revision
- ✔️ **Stable** - Reviewed and approved

## Contributing

When adding new docs:

1. Follow the numbering scheme (next available number)
2. Add entry to this README
3. Mark with appropriate priority (P0/P1/P2)
4. Update reading paths if needed

For Phase 2 docs, place in `future/` directory.

## RFC Process

For major architectural changes or feature proposals, use the RFC process:

- See [rfc/README.md](./rfc/README.md) for the RFC process
- Use [rfc/TEMPLATE.md](./rfc/TEMPLATE.md) to create new RFCs
- RFCs are numbered sequentially (001, 002, etc.)

Currently, there are no active RFCs. The initial Cocoon design is documented in the main docs (00-overview.md through 10-implementation-roadmap.md).
