package engine

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/CMGS/cocoon/config"
	"github.com/CMGS/cocoon/hypervisor"
	"github.com/CMGS/cocoon/image"
	"github.com/CMGS/cocoon/image/refcache"
	"github.com/CMGS/cocoon/lock/flock"
	"github.com/CMGS/cocoon/storage"
	"github.com/CMGS/cocoon/types"
	"github.com/CMGS/cocoon/utils"
	"github.com/CMGS/cocoon/vm"
)

// Lock hierarchy (must acquire in this order to prevent deadlock):
//
//	Level 1: GC Lock (storage/local/gc.go)
//	Level 2: References Lock / Name Index Lock
//	Level 3: Image Conversion Lock (per-checksum)
//	Level 4: VM Metadata Lock (per-VM)
//
// IMPORTANT: flock-based locks are NOT reentrant. A goroutine that holds
// a lock MUST NOT attempt to acquire the same lock again. All current
// call paths are verified to be non-recursive.

// Compile-time interface check.
var _ vm.Manager = (*manager)(nil)

// manager is the concrete implementation of the Manager interface.
type manager struct {
	cfg        *config.CocoonConfig
	hyper      hypervisor.Client
	refCounter storage.ReferenceCounter
	cowMgr     storage.COWManager
	imgMgr     image.Manager
}

// New creates a new VM manager backed by the given configuration and dependencies.
func New(
	cfg *config.CocoonConfig,
	hyper hypervisor.Client,
	refCounter storage.ReferenceCounter,
	cowMgr storage.COWManager,
	imgMgr image.Manager,
) vm.Manager {
	return &manager{
		cfg:        cfg,
		hyper:      hyper,
		refCounter: refCounter,
		cowMgr:     cowMgr,
		imgMgr:     imgMgr,
	}
}

// ---------------------------------------------------------------------------
// Name resolution
// ---------------------------------------------------------------------------

// ResolveVMRef resolves a user-provided reference to a vm_id.
// If ref starts with "vm-", it is treated as a direct vm_id and validated
// by checking for the existence of config.json. Otherwise, the name index
// is consulted.
func (m *manager) ResolveVMRef(ref string) (string, error) {
	if strings.HasPrefix(ref, "vm-") {
		// Sanitize: reject path traversal characters in the user-provided
		// vm_id to prevent escaping the VM directory.
		if err := validateVMID(ref); err != nil {
			return "", fmt.Errorf("%w: %s", types.ErrVMNotFound, ref)
		}
		// Direct vm_id lookup: verify config.json exists.
		configPath := m.cfg.VMConfigPath(ref)
		if _, err := os.Stat(configPath); err != nil {
			if os.IsNotExist(err) {
				return "", fmt.Errorf("%w: %s", types.ErrVMNotFound, ref)
			}
			return "", fmt.Errorf("stat config for %s: %w", ref, err)
		}
		return ref, nil
	}

	// Name index lookup.
	index, err := LoadNameIndex(m.cfg)
	if err != nil {
		return "", fmt.Errorf("load name index: %w", err)
	}

	vmID, ok := index[ref]
	if !ok {
		return "", fmt.Errorf("%w: %s", types.ErrVMNotFound, ref)
	}
	return vmID, nil
}

// validateVMID verifies that a vm_id matches the canonical format vm-{ULID}.
// ULID is exactly 26 characters from the Crockford Base32 alphabet.
func validateVMID(id string) error {
	const ulidLen = 26
	if !strings.HasPrefix(id, "vm-") {
		return fmt.Errorf("invalid vm_id: must start with vm-")
	}
	ulidPart := id[3:]
	if len(ulidPart) != ulidLen {
		return fmt.Errorf("invalid vm_id: ULID part must be %d characters, got %d", ulidLen, len(ulidPart))
	}
	// Validate against ULID's Crockford Base32 character set (0-9, A-Z).
	// oklog/ulid encodes using uppercase; we accept both cases.
	_, err := ulid.ParseStrict(ulidPart)
	if err != nil {
		return fmt.Errorf("invalid vm_id: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Config / Metadata persistence
// ---------------------------------------------------------------------------

// LoadConfig reads a VM's immutable config.json.
func (m *manager) LoadConfig(vmID string) (*types.VMConfig, error) {
	configPath := m.cfg.VMConfigPath(vmID)
	var cfg types.VMConfig
	if err := utils.ReadJSON(configPath, &cfg); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", types.ErrVMNotFound, vmID)
		}
		return nil, fmt.Errorf("read config for %s: %w", vmID, err)
	}
	return &cfg, nil
}

// LoadMetadata reads a VM's mutable metadata.json.
// Reads are lock-free because metadata.json is always atomically replaced.
func (m *manager) LoadMetadata(vmID string) (*types.VMMetadataFile, error) {
	metaPath := m.cfg.VMMetadataPath(vmID)
	var meta types.VMMetadataFile
	if err := utils.ReadJSON(metaPath, &meta); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", types.ErrVMNotFound, vmID)
		}
		return nil, fmt.Errorf("read metadata for %s: %w", vmID, err)
	}
	return &meta, nil
}

// SaveMetadata persists a VM's metadata.json atomically under flock.
// Uses metadata.lock (Level 4) to serialize concurrent writers.
func (m *manager) SaveMetadata(meta *types.VMMetadataFile) error {
	lockPath := m.cfg.VMMetadataLock(meta.VMID)
	fl := flock.New(lockPath)
	if err := fl.Lock(); err != nil {
		return fmt.Errorf("acquire metadata lock for %s: %w", meta.VMID, err)
	}
	defer fl.Unlock() //nolint:errcheck

	meta.UpdatedAt = time.Now().UTC().Format(time.RFC3339)

	metaPath := m.cfg.VMMetadataPath(meta.VMID)
	return utils.AtomicWriteJSON(metaPath, meta)
}

// ---------------------------------------------------------------------------
// State management
// ---------------------------------------------------------------------------

// TransitionState validates a state transition and persists it atomically.
// The previous state is recorded in metadata for auditing.
// When transitioning to ERROR, LastError and ErrorCount are automatically updated.
func (m *manager) TransitionState(vmID string, to types.VMState, reason string) error {
	return m.transitionStateWithUpdate(vmID, to, reason, nil)
}

// ---------------------------------------------------------------------------
// CRUD operations
// ---------------------------------------------------------------------------

// Create provisions a new VM: generates an ID, prepares the image, creates
// the overlay, writes config.json and metadata.json, pins the reference,
// registers the name, and transitions CREATING -> CREATED.
func (m *manager) Create(ctx context.Context, opts *vm.CreateOptions) (*types.VMConfig, error) {
	if opts == nil || opts.Image == "" {
		return nil, fmt.Errorf("image is required")
	}

	// Apply defaults.
	cpus := opts.CPUs
	if cpus <= 0 {
		cpus = m.cfg.DefaultCPUs
	}
	memoryMB := opts.MemoryMB
	if memoryMB <= 0 {
		memoryMB = m.cfg.DefaultMemoryMB
	}
	diskSize := opts.DiskSize
	if diskSize == "" {
		diskSize = m.cfg.DefaultDiskSize
	}
	bootStrategy := opts.BootStrategy
	if bootStrategy == "" {
		bootStrategy = types.DefaultBootStrategy
	}

	// Generate name if not provided.
	name := opts.Name
	if name == "" {
		name = generateDefaultName()
	}

	// Generate vm_id.
	vmID := "vm-" + ulid.MustNew(ulid.Now(), rand.Reader).String()

	now := time.Now().UTC().Format(time.RFC3339)

	// Step 1: Prepare image (pull + convert + cache).
	identity, baseImagePath, err := m.imgMgr.Prepare(ctx, opts.Image)
	if err != nil {
		return nil, fmt.Errorf("prepare image %s: %w", opts.Image, err)
	}

	// Step 1b: Verify bootability (unless skipped).
	if !opts.SkipVerify {
		result, verifyErr := m.imgMgr.VerifyBootability(ctx, baseImagePath)
		if verifyErr != nil {
			return nil, fmt.Errorf("verify bootability for %s: %w", opts.Image, verifyErr)
		}
		if !result.Bootable {
			return nil, fmt.Errorf("%w: %s - %v", types.ErrImageNotBootable, opts.Image, result.Errors)
		}
		for _, w := range result.Warnings {
			log.Printf("image %s: bootability warning: %s", opts.Image, w)
		}
	}

	baseKey := identity.BaseKey()
	if idxErr := refcache.Upsert(m.cfg, opts.Image, baseKey, identity.FullDigest); idxErr != nil {
		log.Printf("warning: update manifest cache for %q: %v", opts.Image, idxErr)
	}

	// Resolve firmware path based on boot strategy and arch.
	firmwarePath, err := resolveFirmwarePath(m.cfg, bootStrategy, identity.Arch)
	if err != nil {
		return nil, fmt.Errorf("resolve firmware: %w", err)
	}

	// Build immutable config.
	vmCfg := &types.VMConfig{
		VMID:           vmID,
		Name:           name,
		ImageRef:       opts.Image,
		BaseKey:        baseKey,
		BaseDigestFull: identity.FullDigest,
		Arch:           identity.Arch,
		BootStrategy:   bootStrategy,
		FirmwarePath:   firmwarePath,
		CPUs:           cpus,
		MemoryMB:       memoryMB,
		DiskSize:       diskSize,
		BaseImagePath:  baseImagePath,
		OverlayPath:    m.cfg.VMOverlayPath(vmID),
		SocketPath:     m.cfg.VMSocketPath(vmID),
		SerialLog:      m.cfg.VMSerialLogPath(vmID),
		CreatedAt:      now,
		SchemaVersion:  types.CurrentConfigSchemaVersion,
	}

	// Step 2: Create VM directory, write config + metadata, then pin reference.
	// Pin MUST happen before overlay creation to prevent GC from collecting
	// the base image during the (slow) overlay step (docs/09 "pin-first" flow).
	// Metadata must exist before pin so reconciliation can find the VM if we
	// crash after pinning.
	vmDir := m.cfg.VMPersistDir(vmID)
	if err := os.MkdirAll(vmDir, 0o755); err != nil { //nolint:gosec // G301: VM directory needs world-readable access for CH process
		return nil, fmt.Errorf("create VM directory: %w", err)
	}

	// Write config.json (immutable, written once).
	configPath := m.cfg.VMConfigPath(vmID)
	if err := utils.AtomicWriteJSON(configPath, vmCfg); err != nil {
		_ = os.RemoveAll(vmDir)
		return nil, fmt.Errorf("write config.json: %w", err)
	}

	// Write initial metadata.json in CREATING state.
	meta := &types.VMMetadataFile{
		VMID:          vmID,
		State:         string(types.VMStateCreating),
		PreviousState: "",
		UpdatedAt:     now,
		SchemaVersion: types.CurrentMetadataSchemaVersion,
	}
	metaPath := m.cfg.VMMetadataPath(vmID)
	if err := utils.AtomicWriteJSON(metaPath, meta); err != nil {
		_ = os.RemoveAll(vmDir)
		return nil, fmt.Errorf("write metadata.json: %w", err)
	}

	// Pin reference: protects base image from GC during subsequent slow steps.
	if err := m.refCounter.AddReference(baseKey, vmID, identity.FullDigest, opts.Image); err != nil {
		_ = os.RemoveAll(vmDir)
		return nil, fmt.Errorf("pin reference for %s: %w", vmID, err)
	}

	// Step 3: Create the COW overlay (slow, requires base image on disk).
	if _, err := m.cowMgr.CreateOverlay(baseKey, vmID, diskSize); err != nil {
		_ = m.refCounter.RemoveReference(baseKey, vmID)
		_ = os.RemoveAll(vmDir)
		return nil, fmt.Errorf("create overlay for %s: %w", vmID, err)
	}

	// Register name in the index (atomically checks uniqueness under lock).
	if err := AddName(m.cfg, name, vmID); err != nil {
		_ = m.refCounter.RemoveReference(baseKey, vmID)
		_ = m.cowMgr.RemoveOverlay(vmID)
		_ = os.RemoveAll(vmDir)
		return nil, fmt.Errorf("%w: %s", types.ErrVMAlreadyExists, err)
	}

	// Transition CREATING -> CREATED.
	if err := m.TransitionState(vmID, types.VMStateCreated, "creation completed"); err != nil {
		// On transition failure, clean up everything.
		_ = RemoveName(m.cfg, name)
		_ = m.refCounter.RemoveReference(baseKey, vmID)
		_ = m.cowMgr.RemoveOverlay(vmID)
		_ = os.RemoveAll(vmDir)
		return nil, fmt.Errorf("transition to CREATED: %w", err)
	}

	return vmCfg, nil
}

// bootResult holds the outcome of a successful boot attempt.
type bootResult struct {
	pid          int
	bootMode     types.BootMode
	firmwarePath string
}

// attemptBoot launches a Cloud Hypervisor process, configures the VM, and boots
// the guest using the specified firmware and boot mode. It operates on a copy of
// vmCfg so the on-disk config.json is never modified.
//
// On success it returns a bootResult with the CH process PID and the actual boot
// parameters. On any failure after the CH process has been launched, it force-kills
// the process before returning the error.
func (m *manager) attemptBoot(ctx context.Context, vmID string, vmCfg *types.VMConfig, firmwarePath string, bootMode types.BootMode) (*bootResult, error) {
	// Work on a shallow copy so we never mutate the caller's config.
	cfgCopy := *vmCfg
	cfgCopy.FirmwarePath = firmwarePath

	// Map boot mode to the strategy the hypervisor client expects.
	switch bootMode {
	case types.BootModeUEFI:
		cfgCopy.BootStrategy = types.BootStrategyUEFIOnly
	default: // PVH
		cfgCopy.BootStrategy = types.BootStrategyPVHOnly
	}

	// Step 1: Launch Cloud Hypervisor process.
	pid, err := m.hyper.Launch(ctx, vmID, &cfgCopy)
	if err != nil {
		return nil, fmt.Errorf("launch CH for %s (%s): %w", vmID, bootMode, err)
	}

	// Record PID in metadata immediately after launch (no state change).
	if err := m.updateMetadata(vmID, func(md *types.VMMetadataFile) {
		md.ProcessPID = pid
		md.StartedAt = time.Now().UTC().Format(time.RFC3339)
	}); err != nil {
		_ = m.hyper.ForceKill(vmID)
		return nil, fmt.Errorf("record PID for %s: %w", vmID, err)
	}

	// Step 2: Configure the VM via CH REST API.
	chVMCfg := buildCHVMConfig(&cfgCopy)
	if err := m.hyper.CreateVM(ctx, cfgCopy.SocketPath, chVMCfg); err != nil {
		_ = m.hyper.ForceKill(vmID)
		return nil, fmt.Errorf("create VM config for %s (%s): %w", vmID, bootMode, err)
	}

	// Step 3: Boot the guest.
	if err := m.hyper.BootVM(ctx, cfgCopy.SocketPath); err != nil {
		_ = m.hyper.ForceKill(vmID)
		return nil, fmt.Errorf("boot VM %s (%s): %w", vmID, bootMode, err)
	}

	return &bootResult{
		pid:          pid,
		bootMode:     bootMode,
		firmwarePath: firmwarePath,
	}, nil
}

type bootFailureReason int

const (
	bootFailureUnknown bootFailureReason = iota

	// Recoverable: retry with UEFI may succeed.
	bootFailureFirmwareNotFound
	bootFailurePVHProtocol
	bootFailureDiskDiscovery
	bootFailureBootloaderCompat
	bootFailureKernelPanic

	// Non-recoverable: fallback won't help.
	bootFailureKVMAccess
	bootFailureResourceExhaustion
	bootFailureDiskAccess
	bootFailureSocketConflict
	bootFailureInvalidConfig
)

// classifyBootFailure returns a normalized failure reason from wrapped error text.
func classifyBootFailure(err error) bootFailureReason {
	if err == nil {
		return bootFailureUnknown
	}

	msg := strings.ToLower(err.Error())

	// Recoverable conditions.
	switch {
	case strings.Contains(msg, "failed to load firmware"),
		strings.Contains(msg, "firmware: no such file"):
		return bootFailureFirmwareNotFound
	case strings.Contains(msg, "pvh entry point not found"),
		strings.Contains(msg, "invalid pvh header"):
		return bootFailurePVHProtocol
	case strings.Contains(msg, "no bootable device"),
		strings.Contains(msg, "failed to find efi system partition"),
		strings.Contains(msg, "virtio-blk: no bootable partitions"):
		return bootFailureDiskDiscovery
	case strings.Contains(msg, "failed to load boot loader"),
		strings.Contains(msg, "unsupported boot loader format"):
		return bootFailureBootloaderCompat
	case strings.Contains(msg, "kernel panic"),
		strings.Contains(msg, "unable to mount root fs"),
		strings.Contains(msg, "vfs: cannot open root device"):
		return bootFailureKernelPanic
	}

	// Non-recoverable conditions.
	switch {
	case strings.Contains(msg, "failed to open /dev/kvm"),
		strings.Contains(msg, "kvm not available"):
		return bootFailureKVMAccess
	case strings.Contains(msg, "cannot allocate memory"),
		strings.Contains(msg, "out of memory"):
		return bootFailureResourceExhaustion
	case strings.Contains(msg, "failed to open disk"),
		(strings.Contains(msg, "permission denied") && strings.Contains(msg, "disk")):
		return bootFailureDiskAccess
	case strings.Contains(msg, "failed to bind socket"),
		strings.Contains(msg, "address already in use"):
		return bootFailureSocketConflict
	case strings.Contains(msg, "invalid parameter"),
		strings.Contains(msg, "failed to parse"):
		return bootFailureInvalidConfig
	}

	return bootFailureUnknown
}

// shouldFallbackToUEFI determines whether a PVH boot failure should trigger
// automatic UEFI retry.
func shouldFallbackToUEFI(err error) bool {
	switch classifyBootFailure(err) {
	case bootFailureFirmwareNotFound,
		bootFailurePVHProtocol,
		bootFailureDiskDiscovery,
		bootFailureBootloaderCompat,
		bootFailureKernelPanic:
		return true
	default:
		return false
	}
}

// Start launches the Cloud Hypervisor process for a VM.
// Follows the transition flow: CREATED/STOPPED -> STARTING -> RUNNING.
// Idempotent: starting a RUNNING VM is a no-op.
//
// When BootStrategy is pvh_then_uefi and the PVH boot fails, Start
// automatically retries with UEFI firmware before transitioning to ERROR.
func (m *manager) Start(ctx context.Context, vmID string) error {
	meta, err := m.LoadMetadata(vmID)
	if err != nil {
		return err
	}

	state := types.VMState(meta.State)

	// Idempotent: already running -> no-op.
	if state == types.VMStateRunning {
		return nil
	}

	// Reject if already starting.
	if state == types.VMStateStarting {
		return fmt.Errorf("VM %s is already starting", vmID)
	}

	// Validate that we can start from the current state.
	if state != types.VMStateCreated && state != types.VMStateStopped {
		return fmt.Errorf("%w: cannot start VM in state %s", types.ErrInvalidTransition, state)
	}

	// Load config for paths and boot settings.
	vmCfg, err := m.LoadConfig(vmID)
	if err != nil {
		return fmt.Errorf("load config for %s: %w", vmID, err)
	}

	// Transition to STARTING.
	if transErr := m.TransitionState(vmID, types.VMStateStarting, "start requested"); transErr != nil {
		return transErr
	}

	bootStartTime := time.Now()

	// attemptBootAndWait runs attemptBoot + waitForBoot as a single unit.
	// If the CH API calls succeed but boot-time monitoring detects a failure
	// (e.g., kernel panic), it kills the CH process and returns the error so
	// the fallback logic can retry with a different boot mode.
	attemptBootAndWait := func(firmwarePath string, bootMode types.BootMode) (*bootResult, error) {
		res, err := m.attemptBoot(ctx, vmID, vmCfg, firmwarePath, bootMode)
		if err != nil {
			return nil, err
		}

		// Monitor serial log for boot success/failure.
		bootTimeout := time.Duration(m.cfg.BootTimeoutSeconds) * time.Second
		if bootTimeout > 0 {
			successPatterns := m.cfg.BootSuccessPatternsOrDefault()
			failurePatterns := m.cfg.BootFailurePatternsOrDefault()
			if waitErr := waitForBoot(ctx, vmCfg.SerialLog, bootTimeout, successPatterns, failurePatterns); waitErr != nil {
				log.Printf("boot detection failed for %s (%s): %v", vmID, bootMode, waitErr)
				_ = m.hyper.ForceKill(vmID)
				return nil, fmt.Errorf("boot failure detected: %w", waitErr)
			}
		}

		return res, nil
	}

	// Attempt boot with fallback logic based on boot strategy.
	var result *bootResult
	var bootErr error

	switch vmCfg.BootStrategy {
	case types.BootStrategyPVHThenUEFI:
		// First attempt: PVH boot using the firmware from config.
		result, bootErr = attemptBootAndWait(vmCfg.FirmwarePath, types.BootModePVH)
		if bootErr != nil && shouldFallbackToUEFI(bootErr) {
			log.Printf("PVH boot failed for %s, falling back to UEFI: %v", vmID, bootErr)
			// Second attempt: UEFI boot using resolved firmware path (with system OVMF fallback).
			uefiFW, fwErr := resolveUEFIFirmwarePath(m.cfg)
			if fwErr != nil {
				bootErr = fmt.Errorf("UEFI fallback: %w", fwErr)
			} else {
				result, bootErr = attemptBootAndWait(uefiFW, types.BootModeUEFI)
			}
		}

	case types.BootStrategyUEFIOnly:
		result, bootErr = attemptBootAndWait(vmCfg.FirmwarePath, types.BootModeUEFI)

	case types.BootStrategyPVHOnly:
		result, bootErr = attemptBootAndWait(vmCfg.FirmwarePath, types.BootModePVH)

	default:
		bootErr = fmt.Errorf("invalid boot strategy in config: %q", vmCfg.BootStrategy)
	}

	if bootErr != nil {
		_ = m.TransitionState(vmID, types.VMStateError, fmt.Sprintf("boot failed: %v", bootErr))
		return fmt.Errorf("boot VM %s: %w", vmID, bootErr)
	}

	// Transition STARTING -> RUNNING with runtime metadata.
	if err := m.transitionStateWithUpdate(vmID, types.VMStateRunning, "boot completed", func(md *types.VMMetadataFile) {
		md.ProcessPID = result.pid
		md.BootTime = time.Since(bootStartTime).Round(time.Millisecond).String()
		md.LastBootMode = string(result.bootMode)
		md.LastFirmwarePath = result.firmwarePath
	}); err != nil {
		// Transition failed but VM is running; force kill and go to ERROR.
		_ = m.hyper.ForceKill(vmID)
		_ = m.TransitionState(vmID, types.VMStateError, fmt.Sprintf("start transition failed: %v", err))
		return fmt.Errorf("transition to RUNNING for %s: %w", vmID, err)
	}

	return nil
}

// Stop sends a shutdown signal to the VM and waits for graceful stop.
// Follows the transition flow: RUNNING -> STOPPING -> STOPPED.
// Idempotent: stopping a STOPPED VM is a no-op.
func (m *manager) Stop(ctx context.Context, vmID string, timeout time.Duration) error {
	meta, err := m.LoadMetadata(vmID)
	if err != nil {
		return err
	}

	state := types.VMState(meta.State)

	// Idempotent: already stopped -> no-op.
	if state == types.VMStateStopped {
		return nil
	}

	// If already stopping, wait for it to finish.
	if state == types.VMStateStopping {
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			if !m.hyper.IsAlive(vmID) {
				now := time.Now().UTC().Format(time.RFC3339)
				_ = m.transitionStateWithUpdate(vmID, types.VMStateStopped, "process exited during stop wait", func(md *types.VMMetadataFile) {
					md.StoppedAt = now
					md.ProcessPID = 0
				})
				return nil
			}
			select {
			case <-ctx.Done():
				return fmt.Errorf("context canceled while waiting for VM %s to stop: %w", vmID, ctx.Err())
			case <-time.After(250 * time.Millisecond):
			}
		}
		return fmt.Errorf("VM %s still stopping after %v timeout", vmID, timeout)
	}

	// Validate that we can stop from the current state.
	if state != types.VMStateRunning {
		return fmt.Errorf("%w: cannot stop VM in state %s", types.ErrInvalidTransition, state)
	}

	// Transition to STOPPING.
	if err := m.TransitionState(vmID, types.VMStateStopping, "stop requested"); err != nil {
		return err
	}

	// Graceful shutdown via CH API: sends ACPI shutdown + waits for process exit.
	if err := m.hyper.Shutdown(ctx, vmID, timeout); err != nil {
		// Shutdown failed (timeout or error); transition to ERROR.
		// LastError and ErrorCount are auto-tracked by TransitionState.
		_ = m.TransitionState(vmID, types.VMStateError, fmt.Sprintf("graceful stop failed: %v", err))
		return fmt.Errorf("shutdown VM %s: %w", vmID, err)
	}

	// Transition STOPPING -> STOPPED with stopped_at and cleared PID.
	now := time.Now().UTC().Format(time.RFC3339)
	if err := m.transitionStateWithUpdate(vmID, types.VMStateStopped, "graceful stop completed", func(md *types.VMMetadataFile) {
		md.StoppedAt = now
		md.ProcessPID = 0
	}); err != nil {
		_ = m.TransitionState(vmID, types.VMStateError, fmt.Sprintf("stop transition failed: %v", err))
		return err
	}

	return nil
}

// Kill force-terminates a VM via SIGKILL and updates metadata atomically.
// Follows the transition flow:
//   - RUNNING  -> STOPPING -> STOPPED
//   - STOPPING -> STOPPED
//   - STARTING -> ERROR (cannot go through STOPPING)
//
// Idempotent: killing a STOPPED VM is a no-op.
func (m *manager) Kill(ctx context.Context, vmID string) error {
	meta, err := m.LoadMetadata(vmID)
	if err != nil {
		return err
	}

	state := types.VMState(meta.State)

	// Idempotent: already stopped -> no-op.
	if state == types.VMStateStopped {
		return nil
	}

	// Force kill the CH process.
	if err := m.hyper.ForceKill(vmID); err != nil {
		// If PID is stale/reused, process is already gone — continue cleanup.
		if !m.hyper.IsAlive(vmID) {
			log.Printf("kill %s: ForceKill error (process already gone): %v", vmID, err)
		} else {
			return fmt.Errorf("force kill %s: %w", vmID, err)
		}
	}

	// Determine target state based on current state.
	now := time.Now().UTC().Format(time.RFC3339)
	switch state {
	case types.VMStateRunning:
		_ = m.TransitionState(vmID, types.VMStateStopping, "force killed")
		return m.transitionStateWithUpdate(vmID, types.VMStateStopped, "force killed", func(md *types.VMMetadataFile) {
			md.StoppedAt = now
			md.ProcessPID = 0
		})
	case types.VMStateStopping:
		return m.transitionStateWithUpdate(vmID, types.VMStateStopped, "force killed", func(md *types.VMMetadataFile) {
			md.StoppedAt = now
			md.ProcessPID = 0
		})
	case types.VMStateStarting:
		// STARTING -> ERROR (cannot go through STOPPING).
		return m.transitionStateWithUpdate(vmID, types.VMStateError, "force killed during start", func(md *types.VMMetadataFile) {
			md.StoppedAt = now
			md.ProcessPID = 0
		})
	case types.VMStateError:
		// Clean up any zombie process
		if m.hyper.IsAlive(vmID) {
			_ = m.hyper.ForceKill(vmID)
		}
		return m.transitionStateWithUpdate(vmID, types.VMStateStopped, "force killed from error state", func(md *types.VMMetadataFile) {
			md.StoppedAt = now
			md.ProcessPID = 0
		})
	default:
		return fmt.Errorf("%w: cannot kill VM in state %s", types.ErrInvalidTransition, state)
	}
}

// Delete removes a VM and all its resources.
// Follows the transition flow: CREATED/STOPPED/ERROR -> DELETED.
// Idempotent: deleting a non-existent or DELETED VM is a no-op.
// If the VM is RUNNING, force must be true.
func (m *manager) Delete(ctx context.Context, vmID string, force bool) error {
	// Best effort load of metadata. If metadata is missing but other VM
	// artifacts still exist, continue with cleanup to satisfy delete semantics.
	meta, err := m.LoadMetadata(vmID)
	metaPresent := true
	if err != nil {
		if isNotFound(err) {
			metaPresent = false
		} else {
			return err
		}
	}

	// When metadata is missing but the VM process is still alive, require force.
	if !metaPresent && m.hyper.IsAlive(vmID) && !force {
		return types.ErrVMRunning
	}

	if metaPresent {
		if err := m.ensureDeletePreconditions(ctx, vmID, force, meta); err != nil {
			return err
		}
	}

	// Force kill if process might still be alive (e.g., stuck in STARTING/STOPPING/ERROR).
	if m.hyper.IsAlive(vmID) {
		_ = m.hyper.ForceKill(vmID)
	}

	// Safety net: if the PID file was already cleaned up (e.g., by a prior
	// failed ForceKill) but the CH process is still running, use the PID
	// from metadata to kill it directly.
	if metaPresent && meta.ProcessPID > 0 && utils.ValidateProcess(meta.ProcessPID, "cloud-hypervisor") {
		_ = utils.ForceKillProcess(meta.ProcessPID)
	}

	// Transition to DELETED state before removing resources when metadata exists.
	if metaPresent {
		if transErr := m.TransitionState(vmID, types.VMStateDeleted, "delete requested"); transErr != nil {
			if !force {
				return fmt.Errorf("transition to DELETED: %w", transErr)
			}
			// Force: write DELETED state directly, bypassing validation.
			_ = m.updateMetadata(vmID, func(md *types.VMMetadataFile) {
				md.PreviousState = md.State
				md.State = string(types.VMStateDeleted)
			})
		}
	}

	// Load config to get the name and baseKey for cleanup.
	vmCfg, cfgErr := m.LoadConfig(vmID)

	// Unpin reference: remove this VM from the base image's reference list.
	if cfgErr == nil && vmCfg.BaseKey != "" {
		_ = m.refCounter.RemoveReference(vmCfg.BaseKey, vmID)
	}

	// Remove COW overlay.
	_ = m.cowMgr.RemoveOverlay(vmID)

	// Remove name from index.
	if cfgErr == nil && vmCfg.Name != "" {
		_ = RemoveName(m.cfg, vmCfg.Name)
	}

	// Remove VM directories.
	vmDir := m.cfg.VMPersistDir(vmID)
	runtimeDir := m.cfg.VMRuntimeDir(vmID)
	_ = os.RemoveAll(vmDir)
	_ = os.RemoveAll(runtimeDir)

	// Remove serial log (lives outside vmDir, under LogDir).
	_ = os.Remove(m.cfg.VMSerialLogPath(vmID))

	return nil
}

func (m *manager) ensureDeletePreconditions(ctx context.Context, vmID string, force bool, meta *types.VMMetadataFile) error {
	state := types.VMState(meta.State)

	// If running, require --force and attempt graceful stop first.
	if state != types.VMStateRunning {
		return nil
	}
	if !force {
		return types.ErrVMRunning
	}
	if stopErr := m.Stop(ctx, vmID, 10*time.Second); stopErr != nil {
		// Force kill the CH process directly.
		_ = m.hyper.ForceKill(vmID)
		_ = m.TransitionState(vmID, types.VMStateError, fmt.Sprintf("force stop failed: %v", stopErr))
	}
	return nil
}

// Inspect returns a merged view of config.json and metadata.json.
func (m *manager) Inspect(ctx context.Context, vmID string) (*types.VMInspect, error) {
	vmCfg, err := m.LoadConfig(vmID)
	if err != nil {
		return nil, err
	}

	meta, err := m.LoadMetadata(vmID)
	if err != nil {
		return nil, err
	}

	inspect := types.BuildInspect(vmCfg, meta)
	if excerpt, readErr := readSerialLogExcerpt(vmCfg.SerialLog, 100); readErr == nil {
		inspect.Hypervisor.SerialLogExcerpt = excerpt
	}
	return inspect, nil
}

// List returns inspect views for all VMs.
func (m *manager) List(ctx context.Context) ([]*types.VMInspect, error) {
	entries, err := os.ReadDir(m.cfg.VMDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read VM directory: %w", err)
	}

	var results []*types.VMInspect
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		vmID := entry.Name()
		inspect, err := m.Inspect(ctx, vmID)
		if err != nil {
			// Skip VMs with missing/corrupt data.
			continue
		}
		results = append(results, inspect)
	}

	return results, nil
}

func readSerialLogExcerpt(path string, maxLines int) ([]string, error) {
	if maxLines <= 0 {
		return nil, nil
	}
	f, err := os.Open(path) //nolint:gosec // G304: path comes from persisted VM config
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck

	scanner := bufio.NewScanner(f)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	lines := make([]string, 0, maxLines)
	for scanner.Scan() {
		line := scanner.Text()
		if len(lines) < maxLines {
			lines = append(lines, line)
			continue
		}
		copy(lines, lines[1:])
		lines[maxLines-1] = line
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return lines, nil
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// transitionStateWithUpdate validates a state transition and applies an
// additional mutation function to metadata before persisting.
//
// Callers MUST NOT already hold the VM metadata lock (Level 4). This method
// acquires it internally. Calling while holding the lock will deadlock because
// flock is not reentrant.
func (m *manager) transitionStateWithUpdate(vmID string, to types.VMState, reason string, mutate func(*types.VMMetadataFile)) error {
	lockPath := m.cfg.VMMetadataLock(vmID)
	fl := flock.New(lockPath)
	if err := fl.Lock(); err != nil {
		return fmt.Errorf("acquire metadata lock for %s: %w", vmID, err)
	}
	defer fl.Unlock() //nolint:errcheck

	metaPath := m.cfg.VMMetadataPath(vmID)
	var meta types.VMMetadataFile
	if err := utils.ReadJSON(metaPath, &meta); err != nil {
		return fmt.Errorf("read metadata for %s: %w", vmID, err)
	}

	from := types.VMState(meta.State)
	if err := types.ValidateTransition(from, to); err != nil {
		return fmt.Errorf("%w: %s -> %s (%s)", types.ErrInvalidTransition, from, to, reason)
	}

	meta.PreviousState = meta.State
	meta.State = string(to)
	meta.UpdatedAt = time.Now().UTC().Format(time.RFC3339)

	// Auto-track errors: when entering ERROR, record reason and increment count.
	if to == types.VMStateError {
		meta.LastError = reason
		meta.LastErrorAt = time.Now().UTC().Format(time.RFC3339)
		meta.ErrorCount++
	}

	if mutate != nil {
		mutate(&meta)
	}

	return utils.AtomicWriteJSON(metaPath, &meta)
}

// UpdateMetadata applies a mutation function to metadata without performing
// a state transition, atomically under flock. Implements the vm.Manager interface.
func (m *manager) UpdateMetadata(vmID string, mutate func(*types.VMMetadataFile)) error {
	return m.updateMetadata(vmID, mutate)
}

// updateMetadata applies a mutation function to metadata without performing
// a state transition. Used for recording runtime information (e.g., PID)
// within the same state.
func (m *manager) updateMetadata(vmID string, mutate func(*types.VMMetadataFile)) error {
	lockPath := m.cfg.VMMetadataLock(vmID)
	fl := flock.New(lockPath)
	if err := fl.Lock(); err != nil {
		return fmt.Errorf("acquire metadata lock for %s: %w", vmID, err)
	}
	defer fl.Unlock() //nolint:errcheck

	metaPath := m.cfg.VMMetadataPath(vmID)
	var meta types.VMMetadataFile
	if err := utils.ReadJSON(metaPath, &meta); err != nil {
		return fmt.Errorf("read metadata for %s: %w", vmID, err)
	}

	meta.UpdatedAt = time.Now().UTC().Format(time.RFC3339)

	if mutate != nil {
		mutate(&meta)
	}

	return utils.AtomicWriteJSON(metaPath, &meta)
}

// buildCHVMConfig converts a types.VMConfig to the Cloud Hypervisor REST API
// request body (hypervisor.CHVMConfig) for PUT /api/v1/vm.create.
func buildCHVMConfig(vmCfg *types.VMConfig) *hypervisor.CHVMConfig {
	cfg := &hypervisor.CHVMConfig{
		CPUs: hypervisor.CHCPUConfig{
			BootVCPUs: vmCfg.CPUs,
			MaxVCPUs:  vmCfg.CPUs,
		},
		Memory: hypervisor.CHMemoryConfig{
			// CH expects memory size in bytes; VMConfig stores MB.
			Size: vmCfg.MemoryMB * 1024 * 1024,
		},
		Disks: []hypervisor.CHDiskConfig{
			{
				Path:     vmCfg.OverlayPath,
				ReadOnly: false,
			},
		},
		Serial: hypervisor.CHSerialConfig{
			Mode: "File",
			File: vmCfg.SerialLog,
		},
		Console: hypervisor.CHConsoleConfig{
			Mode: "Off",
		},
	}

	// Include firmware in the REST payload so CH is launched in pure daemon
	// mode (no --firmware/--kernel CLI flags). Both PVH (hypervisor-fw) and
	// UEFI (CLOUDHV.fd) use the payload.kernel field, matching the CH
	// quickstart pattern (--kernel). CH auto-detects the file type (ELF for
	// PVH, PE for UEFI) and sets up the boot protocol accordingly.
	// payload.firmware is a legacy field that doesn't set up PVH start_info.
	if vmCfg.FirmwarePath != "" {
		cfg.Payload = &hypervisor.CHPayloadConfig{
			Kernel: vmCfg.FirmwarePath,
		}
	}

	return cfg
}

// resolveUEFIFirmwarePath returns a usable UEFI firmware path. It first checks
// the configured primary path. If the primary is missing or empty, it probes
// the deprecated system OVMF fallback paths. PVH firmware has no fallback.
func resolveUEFIFirmwarePath(cfg *config.CocoonConfig) (string, error) {
	// Try the configured primary path first.
	if cfg.UEFIFirmwarePath != "" {
		if _, err := os.Stat(cfg.UEFIFirmwarePath); err == nil {
			return cfg.UEFIFirmwarePath, nil
		}
	}

	// Probe deprecated system OVMF fallback paths.
	for _, path := range config.UEFIFallbackPaths {
		if _, err := os.Stat(path); err == nil {
			log.Printf("WARNING: Using system OVMF at %s (deprecated; install CLOUDHV.fd via 'cocoon firmware install')", path)
			return path, nil
		}
	}

	// Build a helpful error listing all candidate paths.
	candidates := []string{cfg.UEFIFirmwarePath}
	candidates = append(candidates, config.UEFIFallbackPaths...)
	return "", fmt.Errorf("%w: UEFI firmware not found at any candidate path: %v", types.ErrFirmwareNotFound, candidates)
}

// resolveFirmwarePath determines the firmware file path based on the boot
// strategy and architecture. For PVH-first strategies, it returns the PVH
// firmware (hypervisor-fw). For UEFI-only, it returns the UEFI firmware
// (CLOUDHV.fd) with fallback to system OVMF paths.
func resolveFirmwarePath(cfg *config.CocoonConfig, strategy types.BootStrategy, arch string) (string, error) {
	_ = arch // Reserved for future multi-arch firmware selection.

	switch strategy {
	case types.BootStrategyPVHThenUEFI, types.BootStrategyPVHOnly:
		if cfg.PVHFirmwarePath == "" {
			return "", fmt.Errorf("%w: PVH firmware path not configured", types.ErrFirmwareNotFound)
		}
		return cfg.PVHFirmwarePath, nil
	case types.BootStrategyUEFIOnly:
		return resolveUEFIFirmwarePath(cfg)
	default:
		return "", fmt.Errorf("invalid boot strategy: %q", strategy)
	}
}

// generateDefaultName creates a name like "cocoon-a3f7b2c1".
func generateDefaultName() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "cocoon-" + hex.EncodeToString(b)
}

// isNotFound checks whether an error wraps types.ErrVMNotFound or os.ErrNotExist.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, types.ErrVMNotFound)
}
