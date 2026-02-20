# Implementation Roadmap

**Version**: 1.0
**Status**: Historical

> **IMPORTANT -- Historical Document**: This roadmap was written during initial planning.
> Package paths throughout this document use a `pkg/` prefix (e.g., `pkg/hypervisor/`,
> `pkg/storage/`, `pkg/vm/`) which **no longer exists**. The actual packages live at
> the repository top level: `config/`, `image/`, `storage/`, `vm/`, `hypervisor/`,
> `cmd/cocoon/`, `lock/`, `types/`, `utils/`, `oci/`. Code skeletons are illustrative
> of the original plan, not the current implementation.
>
> **Historical sync note (2026-02-19, updated)**: OCI direct runtime from `create/run` is implemented for local OCI tags and registry OCI VM refs (registry refs are auto-pulled before runtime materialization). Any earlier note claiming remote runtime materialization as follow-up is superseded.
**Phase**: Phase 1
**Last Updated**: 2026-02-19

## Executive Summary

This document provides a concrete implementation roadmap for Cocoon Phase 1, synthesizing all technical specifications into an actionable development plan. It defines implementation phases, critical dependencies, and validation milestones.

**Phase 1 Scope**: Core VM management with OCI image support
**Target**: Production-ready CLI tool for lightweight VM management

## Table of Contents

1. [Implementation Principles](#1-implementation-principles)
2. [Phase Breakdown](#2-phase-breakdown)
3. [Critical Path Analysis](#3-critical-path-analysis)
4. [Detailed Implementation Order](#4-detailed-implementation-order)
5. [Testing Strategy](#5-testing-strategy)
6. [Validation Milestones](#6-validation-milestones)
7. [Risk Mitigation](#7-risk-mitigation)
8. [Phase 1 Completion Checklist](#8-phase-1-completion-checklist)

---

## 1. Implementation Principles

### 1.1 Core Principles

**P1: Interface-First Design**
- Define interfaces before implementations
- Enable testing with mocks
- Support future extensibility

**P2: Fail Fast and Loud**
- Validate dependencies at startup
- Clear error messages with actionable guidance
- No silent failures

**P3: Atomic Operations**
- Use temp files + fsync + rename pattern
- Maintain consistency even on crash
- Reference docs/06-concurrency.md for patterns

**P4: Observability Built-In**
- Structured logging from day one
- Serial console capture for debugging
- Metadata tracks all state transitions

**P5: Simplicity Over Cleverness**
- One CH process per VM (not shared daemon)
- File-based metadata (not database)
- Direct dependencies (no abstractions without clear need)

### 1.2 Non-Negotiables

These must be implemented correctly from the start:

1. **Boot Contract Compliance** (docs/01-boot-contract.md)
   - UEFI boot for cloud images (Phase 1); direct kernel boot for OCI VM images (implemented, including registry auto-pull runtime path)
   - systemd for VM initialization
   - ACPI shutdown with timeout

2. **Concurrency Safety** (docs/06-concurrency.md)
   - Global lock hierarchy
   - Atomic file operations
   - Image conversion deduplication

3. **VM Lifecycle** (docs/07-vm-lifecycle.md)
   - State machine enforcement
   - Metadata persistence
   - Crash recovery on startup

---

## 2. Phase Breakdown

### Phase 0a: Spec Freeze (Pre-Implementation Gate)

**Goal**: All P0 specifications must be finalized before entering the coding phase. Incomplete specs guarantee rework.

**Checklist** (ALL must be checked before Phase 0 begins):

- [x] **config.json schema** finalized (docs/07-vm-lifecycle.md § 5)
  - Immutable fields defined (base_key, boot_strategy), source-of-truth rules documented
- [x] **VM identifier rules** finalized (docs/07-vm-lifecycle.md § 1.4)
  - vm_id format, name uniqueness, CLI resolution, name-index.json
- [x] **Canonical filesystem layout** finalized (docs/05-storage-management.md § 2.1)
  - Single source of truth for all paths, overlay naming, references.json key format
- [x] **Image checksum identity** finalized (docs/04-oci-conversion.md § 6 + docs/05-storage-management.md)
  - Precise algorithm, multi-arch strategy, cache filename pattern
- [x] **Boot contract** stable (docs/01-boot-contract.md)
  - UEFI/direct-kernel-boot parameters, firmware paths, boot detection patterns

**Why This Gate Exists**: Starting implementation before these specs are frozen will result in:
- Components A and B disagreeing on file paths and schemas
- Reconcile/GC logic built on assumptions that change mid-development
- Migration code needed before v1.0 is even released

---

### Phase 0: Foundation (Week 1)

**Goal**: Set up project structure, dependencies, and development environment

**Deliverables**:
- Go module initialized with dependency management
- Interface definitions for all core components
- `cocoon doctor` command for dependency validation
- CI/CD pipeline skeleton

**Reference Docs**:
- docs/08-dependencies.md (complete dependency matrix)

**Validation**:
```bash
cocoon doctor              # All dependencies validated
go test ./... -v          # All tests pass (even if mocked)
```

---

### Phase 1: Hypervisor Integration (Week 2)

**Goal**: Launch and control Cloud Hypervisor processes

**Implementation Order**:

1. **Socket Management** (docs/03-hypervisor-integration.md §2)
   ```go
   // pkg/hypervisor/paths.go
   func GetVMSocketPath(vmID string) string
   func GetVMPIDPath(vmID string) string
   func SetupVMDirectory(vmID string) error
   ```

2. **Process Lifecycle** (docs/03-hypervisor-integration.md §3)
   ```go
   // pkg/hypervisor/process.go
   func LaunchCloudHypervisor(vmID string, config *VMConfig) (*exec.Cmd, error)
   func WaitForSocket(socketPath string, timeout time.Duration) error
   func ShutdownVM(vmID string, timeout time.Duration) error
   ```

3. **HTTP Client** (docs/03-hypervisor-integration.md §4)
   ```go
   // pkg/hypervisor/client.go
   type CHClient struct { ... }
   func NewCHClient(socketPath string) *CHClient
   func (c *CHClient) CreateVM(config *VMConfig) error
   func (c *CHClient) BootVM() error
   func (c *CHClient) ShutdownVM() error
   ```

4. **API Mapping** (docs/03-hypervisor-integration.md §5)
   - Implement all REST endpoints
   - Test with real Cloud Hypervisor process

**Validation**:
```bash
# Start a VM manually
cloud-hypervisor --api-socket /tmp/test.sock --cpus boot=1 --memory size=512M

# Test client
go run ./cmd/test-client/main.go
```

**Exit Criteria**:
- Can launch CH process and wait for socket
- Can send HTTP requests over Unix socket
- Can gracefully shutdown VM with ACPI + timeout
- PID file written and validated

---

### Phase 2: Storage Layer (Week 3)

**Goal**: Implement qcow2 COW storage with reference counting

**Implementation Order**:

1. **Directory Structure** (docs/05-storage-management.md §2)
   ```go
   // pkg/storage/layout.go
   func InitializeStorage() error
   func GetBaseImagePath(baseKey string) string  // baseKey = {checksum_16}_{arch}
   func GetOverlayPath(vmID string) string
   ```

2. **Reference Counter** (docs/05-storage-management.md §3)
   ```go
   // pkg/storage/refcount.go
   type ReferenceCounter interface { ... }
   func NewReferenceCounter(path string) ReferenceCounter
   // base_key = {checksum_16}_{arch}, e.g., "a1b2c3d4e5f6a7b8_amd64"
   func (r *ReferenceCounter) AddReference(baseKey, vmID, digestFull, sourceRef string) error
   func (r *ReferenceCounter) RemoveReference(baseKey, vmID string) error
   ```

3. **COW Operations** (docs/05-storage-management.md §4)
   ```go
   // pkg/storage/qcow2.go
   func CreateBaseImage(srcPath, baseKey string) error
   func CreateOverlay(baseKey, vmID string) error
   func DeleteOverlay(vmID string) error
   ```

4. **Garbage Collector** (docs/05-storage-management.md §5)
   ```go
   // pkg/storage/gc.go
   func GarbageCollect() error
   func FindOrphanedOverlays() ([]string, error)
   ```

**Validation**:
```bash
# Create base image
qemu-img create -f qcow2 base.qcow2 10G

# Create overlay
qemu-img create -f qcow2 -b base.qcow2 -F qcow2 overlay.qcow2

# Test reference counter
go test ./pkg/storage -run TestReferenceCounter -v

# Test GC
go test ./pkg/storage -run TestGarbageCollect -v
```

**Exit Criteria**:
- Can create qcow2 overlays with backing files
- Reference counter maintains accurate counts
- GC removes unused base images
- Atomic operations survive crashes (use crash injection tests)

---

### Phase 3: OCI Conversion Pipeline (Week 4-5)

**Goal**: Convert OCI images to bootable qcow2 images

**Implementation Order**:

1. **Native OCI Pull Integration** (docs/04-oci-conversion.md §2-3)
   ```go
   // pkg/image/identify.go
   func PullImage(imageRef string) error
   func MaterializeRootfs(imageRef string) (string, error)
   ```

2. **Checksum Calculation** (docs/04-oci-conversion.md §6)
   ```go
   // pkg/image/checksum.go
   func CalculateManifestChecksum(imageRef string) (string, error)
   func CheckCacheExists(checksum string) bool
   ```

3. **Rootfs to qcow2** (docs/04-oci-conversion.md §5)
   ```go
   // pkg/image/convert.go
   func ConvertRootfsToQcow2(mountPath, outputPath string, size string) error
   func MakeImageBootable(qcow2Path string, bootMode string) error
   ```

4. **Conversion Pipeline** (docs/04-oci-conversion.md §9)
   ```go
   // pkg/image/pipeline.go
   func PrepareBaseImage(imageRef string) (checksum string, path string, error)
   // Orchestrates: pull -> checksum -> (cache hit? return : convert)
   ```

**Validation**:
```bash
# Test OCI identify/pull pipeline
go test ./image/pipeline -run TestPull

# Test conversion
go run ./cmd/convert-test/main.go myorg/ubuntu-bootable:22.04

# Test bootability
cloud-hypervisor --disk path=converted.qcow2 --cpus boot=1 --memory size=1024M
```

**Exit Criteria**:
- Can pull OCI images using native go-containerregistry path
- Can calculate manifest checksums correctly
- Can convert rootfs to qcow2 with partitions
- Converted-image boot path validated in a KVM-backed E2E environment
- Cache prevents duplicate conversions

---

### Phase 4: VM Lifecycle Management (Week 6)

**Goal**: Implement complete VM state machine with metadata

**Implementation Order**:

1. **State Machine** (docs/07-vm-lifecycle.md §1-2)
   ```go
   // pkg/vm/state.go
   type VMState string
   const (...)
   func ValidateTransition(from, to VMState) error
   ```

2. **Metadata Schema** (docs/07-vm-lifecycle.md §4-5)
   ```go
   // pkg/vm/metadata.go
   type VMMetadata struct { ... }
   func LoadMetadata(vmID string) (*VMMetadata, error)
   func SaveMetadata(vmID string, metadata *VMMetadata) error
   ```

3. **VM Manager** (docs/07-vm-lifecycle.md §9)
   ```go
   // pkg/vm/manager.go
   type VMManager interface { ... }
   func (m *VMManager) Create(req *CreateRequest) (*VMMetadata, error)
   func (m *VMManager) Start(vmID string) error
   func (m *VMManager) Stop(vmID string) error
   func (m *VMManager) Delete(vmID string, force bool) error
   ```

4. **Reconciliation** (docs/07-vm-lifecycle.md §8)
   ```go
   // pkg/vm/reconcile.go
   func ReconcileOnStartup() error
   func DetectCrashedVMs() ([]string, error)
   func CleanupOrphanedSockets() error
   ```

**Validation**:
```bash
# Test state transitions
go test ./pkg/vm -run TestStateTransitions -v

# Test metadata persistence
go test ./pkg/vm -run TestMetadataPersistence -v

# Test reconciliation
go test ./pkg/vm -run TestReconciliation -v
```

**Exit Criteria**:
- State machine enforces valid transitions
- Metadata persists correctly across restarts
- Reconciliation detects and handles crashes
- All operations are idempotent

---

### Phase 5: Concurrency and Locking (Week 7)

**Goal**: Implement global lock hierarchy and atomic operations

**Implementation Order**:

1. **Lock Hierarchy** (docs/06-concurrency.md §2)
   ```go
   // pkg/lock/manager.go
   type LockManager interface { ... }
   func AcquireGlobalLock() (func(), error)
   func AcquireImageLock(checksum string) (func(), error)
   func AcquireVMLock(vmID string) (func(), error)
   ```

2. **Atomic File Operations** (docs/06-concurrency.md §4)
   ```go
   // pkg/atomic/file.go
   func WriteAtomic(path string, data []byte) error
   // Implements: tmp + fsync + rename pattern
   ```

3. **Concurrent Image Conversion** (docs/06-concurrency.md §5.1)
   ```go
   // Already handled in Phase 3, add locking
   func PrepareBaseImage(imageRef string) (string, string, error) {
       checksum := CalculateChecksum(imageRef)

       lock := AcquireImageLock(checksum)
       defer lock()

       if CheckCacheExists(checksum) {
           return checksum, GetCachedPath(checksum), nil
       }

       return ConvertImage(imageRef, checksum)
   }
   ```

**Validation**:
```bash
# Concurrent VM creation stress test
go test ./pkg/vm -run TestConcurrentCreate -count=100 -parallel=10

# Concurrent image conversion (same image)
go test ./pkg/image -run TestConcurrentConversion -count=50 -parallel=20

# Reference counter race detection
go test -race ./pkg/storage -run TestReferenceConcurrency
```

**Exit Criteria**:
- No race conditions detected with `go test -race`
- Lock hierarchy prevents deadlocks
- Concurrent operations on same image deduplicate correctly
- Reference counter remains accurate under load

---

### Phase 6: CLI Implementation (Week 8)

**Goal**: Build user-facing CLI with Docker-like commands

**Implementation Order**:

1. **CLI Framework** (docs/09-cli-design.md §2)
   ```go
   // cmd/cocoon/main.go
   app := &cli.App{
       Name: "cocoon",
       Commands: []*cli.Command{
           createCmd, startCmd, stopCmd, deleteCmd,
           inspectCmd, listCmd, logsCmd, imagePullCmd, imageListCmd,
       },
   }
   ```

2. **Core Commands** (docs/09-cli-design.md §3)
   - `cocoon create`
   - `cocoon start`
   - `cocoon stop`
   - `cocoon delete`
   - `cocoon inspect`
   - `cocoon list`

3. **Image Commands** (docs/09-cli-design.md §4)
   - `cocoon image pull`
   - `cocoon image list`
   - `cocoon image rm`

4. **Utility Commands** (docs/09-cli-design.md §5)
   - `cocoon logs`
   - `cocoon doctor`
   - `cocoon gc`

**Validation**:
```bash
# End-to-end workflow
cocoon doctor                             # Check dependencies
cocoon image pull myorg/ubuntu-bootable:22.04   # Pull bootable OCI image
cocoon create myorg/ubuntu-bootable:22.04 --name myvm  # Create VM
cocoon start myvm                         # Start VM
cocoon logs myvm                          # View serial output
cocoon inspect myvm                       # View metadata
cocoon stop myvm                          # Stop VM
cocoon delete myvm                        # Delete VM
cocoon image list                         # View cached images
cocoon image rm myorg/ubuntu-bootable:22.04   # Remove image
```

**Exit Criteria**:
- All commands documented in `cocoon help`
- Error messages provide actionable guidance
- JSON output mode works (`--format json`)
- `cocoon doctor` validates all dependencies

---

### Phase 7: Integration and Polish (Week 9)

**Goal**: End-to-end testing, documentation, and UX improvements

**Tasks**:

1. **End-to-End Tests** (docs/03-hypervisor-integration.md §8)
   ```go
   // test/e2e/vm_lifecycle_test.go
   func TestCompleteVMLifecycle(t *testing.T)
   func TestConcurrentVMCreation(t *testing.T)
   func TestCrashRecovery(t *testing.T)
   ```

2. **Performance Benchmarks**
   ```go
   // test/benchmark/performance_test.go
   func BenchmarkVMCreation(b *testing.B)
   func BenchmarkImageConversion(b *testing.B)
   func BenchmarkConcurrentVMs(b *testing.B)
   ```

3. **Documentation**
   - README.md with quickstart
   - Installation guide for major distros
   - Troubleshooting FAQ
   - Architecture diagrams

4. **UX Improvements**
   - Progress bars for image pulls
   - Better error messages
   - Shell completion scripts
   - Man pages

**Validation**:
- All e2e tests pass on clean system
- Performance meets targets (sub-second VM creation with cached image)
- Documentation reviewed by external reader

---

### Phase 8: Production Readiness (Week 10)

**Goal**: Security, stability, and deployment preparation

**Tasks**:

1. **Security Audit**
   - Dependency vulnerability scan
   - Privilege escalation review
   - Input validation audit
   - File permission review

2. **Stability Testing**
   - 24-hour soak test with 100 VMs
   - Crash injection testing
   - Disk full scenarios
   - Memory pressure testing

3. **Packaging**
   - Debian/Ubuntu `.deb` packages
   - RPM packages for Fedora/RHEL
   - Docker image for containerized use
   - GitHub releases with binaries

4. **Operations**
   - systemd service files (if daemon mode added)
   - Logrotate configurations
   - Sample monitoring configs

**Validation**:
- No memory leaks in 24-hour test
- Handles graceful shutdown (SIGTERM)
- Survives disk full condition
- All packages install cleanly

**Exit Criteria**:
- Security scan clean (no high/critical CVEs)
- Stability tests pass
- Packaged for major distros
- Production deployment guide written

---

## 3. Critical Path Analysis

### 3.1 Dependency Graph

```
Phase 0 (Foundation)
    ├─→ Phase 1 (Hypervisor)
    │       ├─→ Phase 4 (Lifecycle)
    │       │       └─→ Phase 6 (CLI)
    │       └─→ Phase 5 (Concurrency)
    │               └─→ Phase 6 (CLI)
    │
    ├─→ Phase 2 (Storage)
    │       ├─→ Phase 3 (OCI Conversion)
    │       │       └─→ Phase 6 (CLI)
    │       └─→ Phase 5 (Concurrency)
    │
    └─→ Phase 6 (CLI)
            ├─→ Phase 7 (Integration)
            └─→ Phase 8 (Production)
```

### 3.2 Critical Path Items

**Must Complete Before CLI**:
1. ✅ Hypervisor integration (Phase 1)
2. ✅ Storage layer (Phase 2)
3. ✅ OCI conversion (Phase 3)
4. ✅ VM lifecycle (Phase 4)
5. ✅ Concurrency (Phase 5)

**Blockers**:
- Phase 6 (CLI) blocked until Phases 1-5 complete
- Phase 7 (Integration) blocked until Phase 6 complete
- Phase 8 (Production) blocked until Phase 7 complete

**Parallelization Opportunities**:
- Phase 1 (Hypervisor) and Phase 2 (Storage) can be developed in parallel
- Phase 4 (Lifecycle) and Phase 3 (OCI Conversion) can overlap after their dependencies complete
- Testing can run continuously throughout all phases

---

## 4. Detailed Implementation Order

### Week 1: Foundation

**Day 1-2: Project Setup**
```bash
# Initialize Go module
go mod init github.com/CMGS/cocoon

# Create directory structure
mkdir -p cmd/cocoon pkg/{hypervisor,storage,image,vm,lock,atomic} test/{unit,integration,e2e}

# Define interfaces
touch pkg/hypervisor/interface.go
touch pkg/storage/interface.go
touch pkg/image/interface.go
touch pkg/vm/interface.go
```

**Day 3-4: Dependency Validation**
- Implement `cocoon doctor` command
- Check for cloud-hypervisor, qemu-img, libguestfs
- Validate /dev/kvm access
- Check UEFI firmware availability

**Day 5: Testing Infrastructure**
- Set up GitHub Actions CI
- Configure golangci-lint
- Set up test coverage reporting

**Milestone**: `cocoon doctor` runs and validates all dependencies

---

### Week 2: Hypervisor Integration

**Day 1: Socket Management**
```go
// pkg/hypervisor/paths.go
func SetupVMDirectory(vmID string) error {
    dir := filepath.Join("/run/cocoon/vms", vmID)
    return os.MkdirAll(dir, 0755)
}
```

**Day 2-3: Process Lifecycle**
- Launch CH with correct arguments
- Wait for socket to appear
- Monitor process health

**Day 4-5: HTTP Client**
- Implement HTTP over Unix socket
- Test all REST API endpoints
- Handle errors and retries

**Milestone**: Can launch CH, send API requests, and shutdown gracefully

---

### Week 3: Storage Layer

**Day 1: Directory Structure**
```bash
# Create storage directories
/var/lib/cocoon/
├── cache/manifests/
├── cache/images/
├── vms/
└── temp/
```

**Day 2: Reference Counter**
- Atomic file operations for references.json
- Test concurrent updates

**Day 3: COW Operations**
- qemu-img integration
- Create overlays with backing files

**Day 4-5: Garbage Collector**
- Find unused base images
- Lock-safe permanent deletion of unreferenced resources
- Test recovery scenarios

**Milestone**: 100 VMs created from single base image, GC removes unused images

---

### Week 4-5: OCI Conversion Pipeline

**Day 1-2: Native OCI Pull Integration**
```bash
# Test OCI pull/materialize operations
go test ./image/pipeline -run TestIdentifyOCI
```

**Day 3-4: Checksum and Caching**
- Calculate manifest checksum
- Check cache before conversion
- Deduplicate identical images

**Day 5-7: Rootfs to qcow2**
- Create qcow2 from directory
- Add partitions and bootloader
- Test bootability

**Day 8-10: Pipeline Integration**
- End-to-end conversion
- Handle errors and cleanup
- Performance optimization

**Milestone**: Pull bootable OCI image, convert to qcow2, boot successfully

---

### Week 6: VM Lifecycle Management

**Day 1-2: State Machine**
```go
func ValidateTransition(from, to VMState) error {
    allowed := map[VMState][]VMState{
        VMStateCreating:  {VMStateCreated, VMStateError},
        VMStateCreated:   {VMStateStarting, VMStateDeleted},
        // ...
    }
    // Validate transition
}
```

**Day 3: Metadata Persistence**
- JSON metadata schema
- Atomic writes
- Version migration

**Day 4-5: VM Manager**
- Orchestrate create/start/stop/delete
- Enforce state transitions
- Handle errors

**Milestone**: Complete VM lifecycle works, survives crashes

---

### Week 7: Concurrency and Locking

**Day 1-2: Lock Hierarchy**
```go
// Correct order per docs/06: GC (L1) > Reference (L2) > Image (L3) > VM Metadata (L4)
func CreateVM(imageRef, vmID string) error {
    // Image lock (Level 3) is acquired internally by PrepareImage.
    checksum := PrepareImage(imageRef)

    // Reference lock (Level 2) is acquired internally by AddReference.
    AddReference(checksum, vmID)

    // VM metadata lock (Level 4) is acquired for metadata writes.
    vmLock := AcquireVMLock(vmID)
    defer vmLock()

    // Create VM
}
```

**Day 3: Atomic Operations**
- Implement temp + fsync + rename pattern
- Test crash scenarios

**Day 4-5: Stress Testing**
- 100 concurrent VM creates
- 50 concurrent image conversions (same image)
- Race condition detection

**Milestone**: No races, no deadlocks, correct behavior under load

---

### Week 8: CLI Implementation

**Day 1: CLI Framework**
```go
app := &cli.App{
    Name:  "cocoon",
    Usage: "Lightweight VM management built on Cloud Hypervisor",
    Commands: []*cli.Command{...},
}
```

**Day 2-3: Core Commands**
- create, start, stop, delete
- List and inspect

**Day 4: Image Commands**
- image pull, list, rm

**Day 5: Utility Commands**
- logs, doctor, gc, firmware

**Milestone**: All CLI commands functional, help text complete

---

### Week 9: Integration and Polish

**Day 1-2: End-to-End Tests**
```go
func TestCompleteWorkflow(t *testing.T) {
    // Pull image
    RunCommand("cocoon", "image", "pull", "myorg/ubuntu-bootable:22.04")

    // Create VM
    RunCommand("cocoon", "create", "myorg/ubuntu-bootable:22.04", "--name", "test-vm")

    // Start VM
    RunCommand("cocoon", "start", "test-vm")

    // Verify running
    output := RunCommand("cocoon", "inspect", "test-vm")
    assert.Contains(t, output, `"state": "RUNNING"`)

    // Stop and delete
    RunCommand("cocoon", "stop", "test-vm")
    RunCommand("cocoon", "delete", "test-vm")
}
```

**Day 3: Performance Benchmarks**
- VM creation time (target: <1s with cached image)
- Image conversion time
- Concurrent operations throughput

**Day 4-5: Documentation and UX**
- README quickstart
- Progress indicators
- Better error messages

**Milestone**: Polished, documented, ready for external testing

---

### Week 10: Production Readiness

**Day 1-2: Security Audit**
- Dependency scan: `go list -json -m all | nancy sleuth`
- Privilege review
- Input validation

**Day 3-4: Stability Testing**
- 24-hour soak test
- Crash injection
- Resource exhaustion

**Day 5: Packaging and Release**
- Build .deb and .rpm packages
- Create GitHub release
- Write deployment guide

**Milestone**: v0.1.0 released, production-ready

---

## 5. Testing Strategy

### 5.1 Unit Tests

**Coverage Target**: 80% minimum

**Key Areas**:
```bash
# Test all packages
go test ./pkg/... -cover -v

# Race detection
go test ./pkg/... -race

# Verbose output
go test ./pkg/vm -run TestStateTransitions -v
```

**Critical Unit Tests**:
- State machine transitions (docs/07-vm-lifecycle.md §1)
- Reference counter operations (docs/05-storage-management.md §3)
- Lock hierarchy validation (docs/06-concurrency.md §2)
- Checksum calculation (docs/04-oci-conversion.md §6)

### 5.2 Integration Tests

**Requires**: Real Cloud Hypervisor installation

**Key Scenarios**:
```go
// test/integration/hypervisor_test.go
func TestLaunchAndShutdown(t *testing.T)
func TestAPIOperations(t *testing.T)
func TestCrashDetection(t *testing.T)

// test/integration/storage_test.go
func TestOverlayCreation(t *testing.T)
func TestReferenceCounter(t *testing.T)
func TestGarbageCollection(t *testing.T)

// test/integration/image_test.go
func TestImageConversion(t *testing.T)
func TestCacheDeduplication(t *testing.T)
```

**Run Integration Tests**:
```bash
go test ./test/integration/... -tags=integration -v
```

### 5.3 End-to-End Tests

**Requires**: Clean system with all dependencies

**Scenarios**:
```bash
# test/e2e/scenarios/
├── 01_basic_lifecycle.sh          # Create, start, stop, delete
├── 02_concurrent_creation.sh      # 10 VMs simultaneously
├── 03_image_deduplication.sh      # Same image, multiple VMs
├── 04_crash_recovery.sh           # Kill CH, reconcile
├── 05_disk_full.sh                # Handle ENOSPC gracefully
└── 06_upgrade_scenario.sh         # Metadata version migration
```

**Run E2E Tests**:
```bash
cd test/e2e
./run_all.sh
```

### 5.4 Performance Benchmarks

**Targets**:
- VM creation (with cached image): <1 second
- Image conversion (bootable OCI): <30 seconds
- Concurrent VM creation (10 VMs): <5 seconds
- Startup reconciliation (100 VMs): <2 seconds

**Benchmark Commands**:
```bash
go test ./test/benchmark -bench=. -benchtime=10s
```

### 5.5 Chaos Testing

**Tools**: Inject failures to test resilience

**Scenarios**:
```go
// test/chaos/inject.go
func TestDiskFullDuringConversion(t *testing.T)
func TestProcessKilledDuringBoot(t *testing.T)
func TestCorruptedMetadata(t *testing.T)
func TestOrphanedSocketCleanup(t *testing.T)
```

---

## 6. Validation Milestones

### Milestone 1: Foundation Complete
**Criteria**:
- ✅ `cocoon doctor` validates all dependencies
- ✅ CI pipeline runs on every commit
- ✅ All interfaces defined

**Validation**:
```bash
cocoon doctor
# Output:
# ✓ Cloud Hypervisor v38.0 found
# ✓ qemu-img 8.0 found
# ✓ libguestfs tools found
# ✓ /dev/kvm accessible
# ✓ UEFI firmware found at /var/lib/cocoon/firmware/CLOUDHV.fd
#
# All dependencies satisfied!
```

---

### Milestone 2: Hypervisor Control
**Criteria**:
- ✅ Can launch CH process with correct arguments
- ✅ Can communicate via HTTP over Unix socket
- ✅ Can gracefully shutdown VM
- ✅ PID file tracking works

**Validation**:
```bash
# Manual test
go run ./cmd/test-hypervisor/main.go

# Output:
# Starting Cloud Hypervisor...
# PID: 12345
# Socket: /run/cocoon/vms/test-vm/api.sock
# Sending vm.boot request...
# VM booted successfully
# Sending vm.shutdown request...
# VM shutdown gracefully
```

---

### Milestone 3: Storage Layer Works
**Criteria**:
- ✅ Can create qcow2 overlays with backing files
- ✅ Reference counter tracks usage accurately
- ✅ GC removes unused images
- ✅ Atomic operations survive crashes

**Validation**:
```bash
# Create 100 VMs from same base
for i in {1..100}; do
    cocoon create ubuntu-22.04-cloudimg --name vm-$i
done

# Check disk usage
du -sh /var/lib/cocoon/cache/images/*.qcow2
# Output: 5.0G (base image)

du -sh /var/lib/cocoon/vms/*/overlay.qcow2 | head -5
# Output: 200K per overlay

# Delete 50 VMs
for i in {1..50}; do
    cocoon delete vm-$i
done

# Run GC
cocoon image prune

# Base image still exists (50 VMs still using it)
ls /var/lib/cocoon/cache/images/*.qcow2
```

---

### Milestone 4: OCI Conversion Works
**Criteria**:
- ✅ Can pull OCI images from Docker Hub
- ✅ Can convert rootfs to bootable qcow2
- ⚠️ Converted-image boot path verified by component tests; full KVM E2E validation pending
- ✅ Checksum-based caching prevents duplicate conversions

**Validation**:
```bash
# Pull and convert bootable OCI image
time cocoon image pull myorg/ubuntu-bootable:22.04
# First run: ~30 seconds (conversion)

# Pull again (should be instant)
time cocoon image pull myorg/ubuntu-bootable:22.04
# Second run: <1 second (cache hit)

# Test bootability
cocoon create myorg/ubuntu-bootable:22.04 --name test-boot
cocoon start test-boot
cocoon logs test-boot
# Output should show systemd boot messages
```

---

### Milestone 5: VM Lifecycle Complete
**Criteria**:
- ✅ State machine enforces valid transitions
- ✅ Metadata persists across restarts
- ✅ Crash recovery works
- ✅ All operations are idempotent

**Validation**:
```bash
# Create and start VM
cocoon create myorg/ubuntu-bootable:22.04 --name myvm
cocoon start myvm
cocoon inspect myvm | jq .state
# Output: "RUNNING"

# Kill CH process manually
kill -9 $(cat /run/cocoon/vms/myvm/ch.pid)

# Doctor (reconcile + fix)
cocoon doctor --fix
cocoon inspect myvm | jq .state
# Output: "ERROR" (crash detected)

# Test idempotency
cocoon stop myvm  # Already stopped
# Output: VM already stopped (no error)
```

---

### Milestone 6: CLI Functional
**Criteria**:
- ✅ All commands work end-to-end
- ✅ Help text is clear and comprehensive
- ✅ Error messages provide actionable guidance
- ✅ JSON output mode available

**Validation**:
```bash
# Help text
cocoon help | grep "COMMANDS:"

# End-to-end workflow
cocoon doctor
cocoon image pull myorg/ubuntu-bootable:22.04
cocoon create myorg/ubuntu-bootable:22.04 --name demo --cpus 2 --memory 2048
cocoon start demo
cocoon logs demo --follow &
sleep 5
cocoon stop demo
cocoon delete demo

# JSON output
cocoon list --format json | jq .
```

---

### Milestone 7: Production Ready
**Criteria**:
- ✅ All tests pass (unit, integration, e2e)
- ✅ No race conditions detected
- ✅ 24-hour soak test passes
- ✅ Documentation complete
- ✅ Packaged for distribution

**Validation**:
```bash
# All tests
go test ./... -race -cover

# Soak test
./test/soak/run_24h.sh

# Install package
sudo dpkg -i cocoon_0.1.0_amd64.deb
cocoon version
# Output: cocoon version 0.1.0
```

---

## 7. Risk Mitigation

### Risk 1: Cloud Hypervisor Instability
**Likelihood**: Medium
**Impact**: High

**Mitigation**:
1. Pin to stable CH version (v38.0+)
2. Comprehensive integration tests
3. Detect and recover from CH crashes
4. Document known issues and workarounds

**Contingency**:
- If CH v38 has issues, fall back to v37
- If critical bugs found, contribute fixes upstream

---

### Risk 2: OCI Image Incompatibility
**Likelihood**: High (some images won't boot)
**Impact**: Medium

**Mitigation**:
1. Document boot contract requirements (docs/01-boot-contract.md)
2. Provide base image validation tool
3. Create known-good base images for common use cases
4. Clear error messages when boot fails

**Examples of Non-Bootable Images**:
- Minimal images without kernel (e.g., `alpine:3.18` base)
- Images without systemd
- Images without bootloader

**Recommended Images** (cloud images or bootable OCI):
- `ubuntu-22.04-cloudimg` (cloud image, qcow2) ✅
- `myorg/ubuntu-bootable:22.04` (bootable OCI) ✅
- `myorg/debian-bootable:12` (bootable OCI) ✅
- `myorg/fedora-bootable:39` (bootable OCI) ✅

---

### Risk 3: Performance Not Meeting Targets
**Likelihood**: Low
**Impact**: Medium

**Mitigation**:
1. Benchmark early and often
2. Profile hot paths (CPU, I/O)
3. Optimize critical sections
4. Use qcow2 compression if needed

**Performance Budget**:
- VM creation (cached): <1s (target met with COW)
- Image conversion: <30s for bootable OCI image
- Startup reconciliation: <2s for 100 VMs

---

### Risk 4: Concurrency Bugs
**Likelihood**: Medium
**Impact**: Critical (data corruption)

**Mitigation**:
1. Run all tests with `-race` flag
2. Stress test concurrent operations
3. Use proven patterns (temp + fsync + rename)
4. Code review all lock usage

**Validation**:
```bash
go test -race ./pkg/storage -run TestConcurrentRefCount -count=100
go test -race ./pkg/image -run TestConcurrentConversion -count=50
```

---

### Risk 5: Dependency Changes Breaking Build
**Likelihood**: Low
**Impact**: Medium

**Mitigation**:
1. Pin dependency versions in go.mod
2. Vendor dependencies if critical
3. CI tests against multiple CH versions
4. Document version requirements clearly

**Pinned Versions**:
```
cloud-hypervisor >= v38.0
qemu-img >= 8.0
```

---

## 8. Phase 1 Completion Checklist

> **Note**: Unchecked items in the checklists below are validation, testing, and documentation tasks —
> not missing code. "Complete" status refers to code implementation.

### P0: Critical Requirements

**Boot Contract**:
- [x] UEFI boot mode implemented (`types/boot.go`, `vm/engine/manager.go`, `hypervisor/cloudhypervisor/client.go`)
- [x] systemd boot detection implemented
- [x] Serial console capture functional (`hypervisor/cloudhypervisor/client.go`, `vm/engine/boot_detect.go`)
- [x] ACPI shutdown with configurable timeout (`vm/engine/manager.go`, `hypervisor/cloudhypervisor/client.go`)
- [x] Stop() on timeout transitions to ERROR via Shutdown(); graceful stop transitions to STOPPED. ForceKill is a separate operation invoked by Kill() (`vm/engine/manager.go`, `hypervisor/cloudhypervisor/client.go`)
- [x] `cocoon kill` provides an explicit force-kill operation independent of timeout (`vm/engine/manager.go`, `hypervisor/hypervisor.go`)

**Hypervisor Integration**:
- [x] One CH process per VM (`hypervisor/cloudhypervisor/client.go`)
- [x] Socket management correct (`hypervisor/cloudhypervisor/client.go`)
- [x] HTTP client complete with all REST endpoints (`hypervisor/cloudhypervisor/client.go`)
- [x] Process monitoring and crash detection (`vm/engine/reconcile.go`)
- [x] Graceful shutdown working (`vm/engine/manager.go`)

**Storage Layer**:
- [x] qcow2 COW overlays created correctly (`storage/local/cow.go`)
- [x] Reference counter maintains accurate counts (`storage/local/refcount.go`)
- [x] GC removes unused base images (`storage/local/gc.go`)
- [x] Atomic file operations prevent corruption (`utils/atomic.go`)
- [x] GC performs permanent deletion of unreferenced images (lock-safe check-and-delete) (`storage/local/gc.go`)

**OCI Conversion**:
- [x] Native OCI identify/pull pipeline works (`image/pipeline/identify.go`)
- [x] Manifest checksum calculated correctly (`image/pipeline/checksum.go`)
- [x] Rootfs converted to bootable qcow2 (`image/pipeline/convert_linux.go`)
- [x] Cache deduplicates identical images (`image/pipeline/manager.go`)
- [ ] Converted images boot in Cloud Hypervisor (requires E2E test on KVM host)

**VM Lifecycle**:
- [x] State machine enforces valid transitions (`types/state.go`, `types/state_test.go`)
- [x] Metadata persists correctly (`types/metadata.go`, `vm/engine/manager.go`)
- [x] All operations idempotent (`vm/engine/manager.go`, `cmd/cocoon/rm.go`)
- [x] Crash recovery working (`vm/engine/reconcile.go`)
- [x] Reconciliation handles orphaned resources (`vm/engine/reconcile.go`)

**Concurrency**:
- [x] Global lock hierarchy implemented (`lock/lock.go`, `lock/flock/flock.go`)
- [ ] No deadlocks under stress test (stress tests not yet written)
- [x] No race conditions (`go test -race ./...` clean; concurrency tests in `image/pipeline/manager_test.go`, `storage/local/refcount_test.go`, `storage/local/gc_test.go`)
- [x] Concurrent image conversion deduplicates (`image/pipeline/manager.go`)
- [x] Reference counter thread-safe (`storage/local/refcount.go` uses flock)

**CLI**:
- [x] All core commands functional (create, start, stop, delete) (`cmd/cocoon/`)
- [x] Image commands working (pull, list, rm) (`cmd/cocoon/images.go`)
- [x] Utility commands complete (logs, doctor, gc, firmware) (`cmd/cocoon/`)
- [x] Help text comprehensive (urfave/cli auto-generated)
- [x] JSON output mode available (`cmd/cocoon/output.go`)

---

### P1: Important But Not Blocking

**Documentation**:
- [x] README with quickstart (`README.md`)
- [x] Installation guide for major distros (`docs/02-installation.md`)
- [x] Architecture overview (`docs/00-overview.md`)
- [ ] Troubleshooting FAQ (documentation, not code)
- [ ] API documentation (if exposed) (documentation, not code)

**Testing**:
- [ ] Unit test coverage >80% (validation, not code)
- [ ] Integration tests pass (validation, not code)
- [ ] E2E tests cover main workflows (validation, not code)
- [ ] Performance benchmarks documented (validation, not code)
- [ ] Chaos testing validates resilience (validation, not code)

**UX Improvements**:
- [ ] Progress bars for long operations (enhancement, not blocking)
- [ ] Color-coded output (errors in red, etc.) (enhancement, not blocking)
- [ ] Shell completion scripts (enhancement, not blocking)
- [ ] Better error messages with suggestions (enhancement, not blocking)

**Packaging**:
- [ ] .deb package for Ubuntu/Debian
- [ ] .rpm package for Fedora/RHEL
- [ ] GitHub releases with binaries
- [ ] Installation instructions tested

---

### P2: Nice to Have

**Advanced Features** (can be deferred):
- [ ] `cocoon exec` command (run commands in VM)
- [ ] `cocoon cp` command (copy files to/from VM)
- [x] `cocoon console` command (interactive console) — implemented in Phase 2
- [ ] VM resource limits (CPU pinning, memory limits)
- [ ] Guest initialization helpers (virtiofs shared directories)

**Operations**:
- [ ] Prometheus metrics endpoint
- [ ] Structured JSON logging
- [ ] systemd service files
- [ ] Logrotate configs
- [ ] Sample dashboards

**Developer Tools**:
- [ ] Debug mode with verbose logging
- [ ] Dry-run mode (show what would happen)
- [ ] Force mode (skip confirmations)
- [ ] Validate mode (check image bootability)

---

## Summary

**Phase 1 Goal**: Production-ready CLI tool for managing VMs with OCI image support

**Timeline**: 10 weeks
**Critical Path**: Foundation → Hypervisor → Storage → OCI → Lifecycle → Concurrency → CLI → Integration → Production

**Success Criteria**:
1. ✅ Can pull bootable OCI images and convert to qcow2 (non-bootable images rejected at validation)
2. ✅ Can create 100+ VMs from single base image (COW efficiency)
3. ✅ VM lifecycle operations (create, start, stop, delete) work reliably
4. ✅ Crash recovery and reconciliation handle failures gracefully
5. ✅ Concurrent operations safe (no races, no deadlocks)
6. ✅ CLI provides Docker-like UX
7. ⚠️ P0 core implementation is complete; remaining validation gaps are KVM E2E boot confirmation and deadlock stress testing

**Next Steps After Phase 1**:
- Phase 2: Network configuration (TAP devices, bridges, port forwarding)
- Phase 2: gRPC/REST API server
- Phase 2: Multi-host orchestration

---

**References**:
- docs/00-overview.md - Project motivation and architecture
- docs/01-boot-contract.md - Critical: How OCI images become bootable VMs
- docs/02-installation.md - Cloud Hypervisor setup
- docs/03-hypervisor-integration.md - CH process management
- docs/04-oci-conversion.md - OCI to qcow2 pipeline
- docs/05-storage-management.md - COW and reference counting
- docs/06-concurrency.md - Locking and consistency
- docs/07-vm-lifecycle.md - State machine and metadata
- docs/08-dependencies.md - Complete dependency matrix
- docs/09-cli-design.md - CLI commands and UX

---

**End of Implementation Roadmap v1.0**
