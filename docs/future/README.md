# Phase 2: Future Features

This directory contains feature request specifications for Phase 2 features that do **not** yet have numbered design documents in `docs/`.

Once a feature is promoted to a numbered design doc (e.g., `docs/12-console.md`), its corresponding future/ file is removed from this directory.

## Active Feature Requests

| Doc | Title | Status | Target |
|-----|-------|--------|--------|
| [api-server.md](./api-server.md) | gRPC/REST API Server | Planned | Phase 2 |
| [observability.md](./observability.md) | Monitoring & Logging | Planned | Phase 2 |
| [storage-quotas.md](./storage-quotas.md) | Storage Quotas | Draft | Phase 2 |

## Superseded (Promoted to Design Docs)

The following feature requests have been promoted to numbered design documents and removed from this directory:

| Former File | Superseded By | Topic |
|-------------|---------------|-------|
| `console.md` | [docs/12-console.md](../12-console.md) | VM Console (Interactive TTY) |
| `device-passthrough.md` | [docs/14-device-passthrough.md](../14-device-passthrough.md) | PCI Device Passthrough (VFIO) |
| `checkpoint-restore.md` | [docs/13-pause-resume.md](../13-pause-resume.md) + [docs/15-warm-start.md](../15-warm-start.md) | Pause/Resume and Checkpoint/Restore |
| `networking.md` | [docs/16-networking.md](../16-networking.md) | CNI-Based Networking |
| `volume-passthrough.md` | [docs/17-volume-passthrough.md](../17-volume-passthrough.md) | Volume Passthrough (virtio-fs) |

## Phase 2 Scope

**Networking** (promoted to [docs/16-networking.md](../16-networking.md)):
- CNI plugin integration for VM networking
- TAP device creation and Cloud Hypervisor virtio-net integration
- Bridge, macvlan, and other CNI plugin support
- Port forwarding via CNI portmap plugin
- IPAM via host-local, dhcp, and static plugins

**API Server**:
- gRPC/REST API for programmatic VM management
- Authentication and authorization
- Multi-tenant support

**Observability**:
- Prometheus metrics exporter
- Structured logging (JSON)
- Distributed tracing integration
- Dashboard templates

**Volume Passthrough**:
- Host-to-guest directory sharing via virtio-fs
- `--volume HOST:GUEST[:ro]` flag on `create` and `run` commands
- virtiofsd process lifecycle management (spawn before VM, cleanup after stop)
- Read-only enforcement at multiple layers (virtiofsd, CH config, guest mount)

**Storage Quotas**:
- Per-VM disk space limits via qcow2 virtual size constraints
- Per-tenant aggregate quota tracking and enforcement
- Base image cache quotas with LRU/LFU eviction policies
- Soft/hard limit configuration and monitoring integration

## Why Deferred?

Phase 1 delivers a complete, production-ready CLI tool for AI Agent sandboxes without networking complexity. These features can be added incrementally without breaking existing functionality.

See `docs/00-overview.md` for Phase 1 vs Phase 2 scope.
