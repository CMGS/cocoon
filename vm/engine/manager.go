package engine

import (
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
	"github.com/CMGS/cocoon/lock/flock"
	"github.com/CMGS/cocoon/storage"
	"github.com/CMGS/cocoon/types"
	"github.com/CMGS/cocoon/utils"
	"github.com/CMGS/cocoon/vm"
)

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

// validateVMID ensures a vm_id contains no path traversal characters.
// Valid vm_ids match: vm-{ULID} where ULID is 26 uppercase alphanumeric chars.
func validateVMID(id string) error {
	if strings.Contains(id, "/") || strings.Contains(id, "\\") || strings.Contains(id, "..") {
		return fmt.Errorf("invalid vm_id: contains path traversal characters")
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

	// Attempt boot with fallback logic based on boot strategy.
	var result *bootResult
	var bootErr error

	switch vmCfg.BootStrategy {
	case types.BootStrategyPVHThenUEFI:
		// First attempt: PVH boot using the firmware from config.
		result, bootErr = m.attemptBoot(ctx, vmID, vmCfg, vmCfg.FirmwarePath, types.BootModePVH)
		if bootErr != nil {
			log.Printf("PVH boot failed for %s, falling back to UEFI: %v", vmID, bootErr)
			// Second attempt: UEFI boot using the global UEFI firmware path.
			result, bootErr = m.attemptBoot(ctx, vmID, vmCfg, m.cfg.UEFIFirmwarePath, types.BootModeUEFI)
		}

	case types.BootStrategyUEFIOnly:
		result, bootErr = m.attemptBoot(ctx, vmID, vmCfg, vmCfg.FirmwarePath, types.BootModeUEFI)

	case types.BootStrategyPVHOnly:
		result, bootErr = m.attemptBoot(ctx, vmID, vmCfg, vmCfg.FirmwarePath, types.BootModePVH)

	default:
		// Unknown strategy: attempt PVH as the default.
		result, bootErr = m.attemptBoot(ctx, vmID, vmCfg, vmCfg.FirmwarePath, types.BootModePVH)
	}

	if bootErr != nil {
		_ = m.TransitionState(vmID, types.VMStateError, fmt.Sprintf("boot failed: %v", bootErr))
		return fmt.Errorf("boot VM %s: %w", vmID, bootErr)
	}

	// Wait for the guest to finish booting by monitoring the serial log.
	bootTimeout := time.Duration(m.cfg.BootTimeoutSeconds) * time.Second
	if bootTimeout > 0 {
		successPatterns := m.cfg.BootSuccessPatternsOrDefault()
		failurePatterns := m.cfg.BootFailurePatternsOrDefault()
		if err := waitForBoot(ctx, vmCfg.SerialLog, bootTimeout, successPatterns, failurePatterns); err != nil {
			log.Printf("boot detection failed for %s: %v", vmID, err)
			_ = m.hyper.ForceKill(vmID)
			_ = m.TransitionState(vmID, types.VMStateError, fmt.Sprintf("boot detection: %v", err))
			return fmt.Errorf("boot VM %s: %w", vmID, err)
		}
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

// Delete removes a VM and all its resources.
// Follows the transition flow: CREATED/STOPPED/ERROR -> DELETED.
// Idempotent: deleting a non-existent or DELETED VM is a no-op.
// If the VM is RUNNING, force must be true.
func (m *manager) Delete(ctx context.Context, vmID string, force bool) error {
	// Idempotent: VM does not exist -> no-op.
	meta, err := m.LoadMetadata(vmID)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return err
	}

	state := types.VMState(meta.State)

	// If running, require --force.
	if state == types.VMStateRunning {
		if !force {
			return types.ErrVMRunning
		}
		// Force stop first.
		if stopErr := m.Stop(ctx, vmID, 10*time.Second); stopErr != nil {
			// Force kill the CH process directly.
			_ = m.hyper.ForceKill(vmID)
			_ = m.TransitionState(vmID, types.VMStateError, fmt.Sprintf("force stop failed: %v", stopErr))
		}
	}

	// Force kill if process might still be alive (e.g., stuck in STARTING/STOPPING/ERROR).
	if m.hyper.IsAlive(vmID) {
		_ = m.hyper.ForceKill(vmID)
	}

	// Transition to DELETED state before removing resources.
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

	return types.BuildInspect(vmCfg, meta), nil
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

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// transitionStateWithUpdate validates a state transition and applies an
// additional mutation function to metadata before persisting.
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
		meta.ErrorCount++
	}

	if mutate != nil {
		mutate(&meta)
	}

	return utils.AtomicWriteJSON(metaPath, &meta)
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
	return &hypervisor.CHVMConfig{
		CPUs: hypervisor.CHCPUConfig{
			BootVCPUs: vmCfg.CPUs,
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
}

// resolveFirmwarePath determines the firmware file path based on the boot
// strategy and architecture. For PVH-first strategies, it returns the PVH
// firmware (hypervisor-fw). For UEFI-only, it returns the UEFI firmware
// (CLOUDHV.fd).
func resolveFirmwarePath(cfg *config.CocoonConfig, strategy types.BootStrategy, arch string) (string, error) {
	_ = arch // Reserved for future multi-arch firmware selection.

	switch strategy {
	case types.BootStrategyPVHThenUEFI, types.BootStrategyPVHOnly:
		if cfg.PVHFirmwarePath == "" {
			return "", fmt.Errorf("%w: PVH firmware path not configured", types.ErrFirmwareNotFound)
		}
		return cfg.PVHFirmwarePath, nil
	case types.BootStrategyUEFIOnly:
		if cfg.UEFIFirmwarePath == "" {
			return "", fmt.Errorf("%w: UEFI firmware path not configured", types.ErrFirmwareNotFound)
		}
		return cfg.UEFIFirmwarePath, nil
	default:
		// Unknown strategy: default to PVH firmware.
		if cfg.PVHFirmwarePath == "" {
			return "", fmt.Errorf("%w: PVH firmware path not configured", types.ErrFirmwareNotFound)
		}
		return cfg.PVHFirmwarePath, nil
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
