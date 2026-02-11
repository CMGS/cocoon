# Phase 2: Future Features

This directory contains specifications for Phase 2 features that are explicitly deferred from Phase 1.

## Planned Documents

| Doc | Title | Status | Target |
|-----|-------|--------|--------|
| [networking.md](./networking.md) | Network Configuration | 📋 Planned | Phase 2 |
| [api-server.md](./api-server.md) | gRPC/REST API Server | 📋 Planned | Phase 2 |
| [observability.md](./observability.md) | Monitoring & Logging | 📋 Planned | Phase 2 |

## Phase 2 Scope

**Networking**:
- TAP/bridge configuration for VM networking
- Network isolation and policies
- Port forwarding and service exposure

**API Server**:
- gRPC/REST API for programmatic VM management
- Authentication and authorization
- Multi-tenant support

**Observability**:
- Prometheus metrics exporter
- Structured logging (JSON)
- Distributed tracing integration
- Dashboard templates

## Why Deferred?

Phase 1 delivers a complete, production-ready CLI tool for AI Agent sandboxes without networking complexity. These features can be added incrementally without breaking existing functionality.

See `docs/00-overview.md` for Phase 1 vs Phase 2 分scope.
