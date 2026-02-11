# Storage Quotas

**Status**: 📋 Planned for Phase 2
**Priority**: P2

## Overview

Storage quotas enable administrators to limit disk space usage for VMs, preventing resource exhaustion and supporting multi-tenant deployments. This feature is explicitly deferred to Phase 2 as it adds complexity to the storage management system.

## Motivation

Without quotas, VMs can:
- Consume unlimited disk space through overlay growth
- Exhaust host storage and impact other VMs
- Create security/stability risks in multi-tenant environments

## Proposed Features

### Per-VM Quotas

Limit the maximum size of individual VM overlay disks:

```yaml
# cocoon.yaml
quotas:
  per_vm_default: "10G"
  per_vm_max: "100G"
```

**Implementation approach**:
- Use qcow2 virtual size limits when creating overlays
- Monitor overlay disk usage via `qemu-img info`
- Reject writes when quota exceeded

### Per-Tenant Quotas

Limit total disk space usage across all VMs owned by a tenant:

```yaml
quotas:
  per_tenant:
    tenant-a: "500G"
    tenant-b: "1T"
```

**Implementation approach**:
- Track cumulative overlay sizes per tenant
- Check quota before VM creation
- Enforce during storage allocation

### Base Image Cache Quotas

Limit total size of cached base images:

```yaml
quotas:
  base_image_cache: "1T"
  cache_eviction_policy: "lru"  # or "lfu"
```

**Implementation approach**:
- Track total cache directory size
- Evict least-recently-used base images when limit exceeded
- Preserve images with active VM references

## Design Considerations

### Soft vs Hard Limits

**Soft limits**: Generate warnings but allow continued operation
**Hard limits**: Enforce strict boundaries and reject operations

Recommended approach: Support both with configurable thresholds

### Quota Enforcement Points

1. **VM Creation**: Check quotas before creating overlay
2. **Runtime Monitoring**: Periodic checks of actual disk usage
3. **Garbage Collection**: Account for quota usage during cleanup

### Quota Storage

Store quota configuration and usage metrics:

```json
// /var/lib/cocoon/quotas.json
{
  "per_vm": {
    "vm-001": {
      "limit": "10G",
      "used": "2.5G",
      "last_checked": "2024-02-11T10:30:00Z"
    }
  },
  "per_tenant": {
    "tenant-a": {
      "limit": "500G",
      "used": "350G",
      "vm_count": 42
    }
  },
  "cache": {
    "limit": "1T",
    "used": "800G",
    "image_count": 127
  }
}
```

## Implementation Phases

### Phase 2.1: Basic Per-VM Quotas

- Set virtual size limits on overlay creation
- Monitor and report quota usage
- Reject VM creation when quota exceeded

### Phase 2.2: Tenant Quotas

- Multi-tenant tracking and enforcement
- Tenant-specific quota configuration
- Aggregated usage reporting

### Phase 2.3: Cache Management

- Base image cache quotas
- LRU/LFU eviction policies
- Smart cache warming

## Integration with Storage Management

Quotas integrate with existing storage components:

**Reference Counter**: Track quota usage per base image
**Garbage Collector**: Free quota space when cleaning up
**COW Manager**: Enforce limits during overlay creation

See [05-storage-management.md](../05-storage-management.md) for core storage architecture.

## CLI Commands

Proposed quota management commands:

```bash
# Set VM quota
cocoon quota set --vm vm-001 --limit 20G

# View quota status
cocoon quota status --vm vm-001

# Set tenant quota
cocoon quota set --tenant tenant-a --limit 1T

# List quota usage
cocoon quota list

# Enforce quotas (check and report violations)
cocoon quota enforce
```

## Monitoring and Alerts

Quota metrics for observability:

- `cocoon_quota_limit_bytes{type="vm|tenant|cache"}`
- `cocoon_quota_used_bytes{type="vm|tenant|cache"}`
- `cocoon_quota_utilization_ratio{type="vm|tenant|cache"}`
- `cocoon_quota_violations_total{type="vm|tenant|cache"}`

Alert on:
- Quota utilization > 80% (warning)
- Quota utilization > 95% (critical)
- Quota violation attempts

## Why Phase 2?

**Reasons for deferral**:
1. **Complexity**: Adds significant code and testing burden
2. **Monitoring overhead**: Requires periodic disk usage checks
3. **Phase 1 sufficiency**: Basic storage management is adequate for single-tenant use
4. **Incremental addition**: Can be added without breaking existing functionality

Phase 1 provides complete VM lifecycle management with efficient storage. Quotas enhance multi-tenant deployments but aren't required for core functionality.

## References

- [05-storage-management.md](../05-storage-management.md) - Core storage architecture
- [06-concurrency.md](../06-concurrency.md) - Consistency guarantees for quota updates
- [future/api-server.md](./api-server.md) - API endpoints for quota management (Phase 2)

---

**Note**: This document is a design sketch for Phase 2. Implementation details will be refined based on Phase 1 experience and production requirements.
