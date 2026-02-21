package engine

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/CMGS/cocoon/config"
	"github.com/CMGS/cocoon/hypervisor"
	"github.com/CMGS/cocoon/image"
	"github.com/CMGS/cocoon/image/refcache"
	"github.com/CMGS/cocoon/lock/flock"
	"github.com/CMGS/cocoon/oci"
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

// nowRFC3339 returns the current UTC time formatted as RFC3339.
func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

// Compile-time interface check.
var _ vm.Manager = (*manager)(nil)

// manager is the concrete implementation of the Manager interface.
type manager struct {
	cfg             *config.CocoonConfig
	hyper           hypervisor.Client
	refCounter      storage.ReferenceCounter
	cowMgr          storage.COWManager
	imgMgr          image.Manager
	overlayMgr      *overlayRuntimeManager
	virtiofsMgr     *virtiofsdRuntimeManager
	registryProbeFn registryProbeFunc
	ociPullFn       func(context.Context, *config.CocoonConfig, string) (*oci.PullResult, error)
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
		cfg:             cfg,
		hyper:           hyper,
		refCounter:      refCounter,
		cowMgr:          cowMgr,
		imgMgr:          imgMgr,
		overlayMgr:      newOverlayRuntimeManager(cfg),
		virtiofsMgr:     newVirtiofsdRuntimeManager(cfg),
		registryProbeFn: oci.ProbeRegistryVMImageType,
		ociPullFn:       oci.Pull,
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

	meta.UpdatedAt = nowRFC3339()

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

func (m *manager) resolveOCIRuntimeLocalTag(ctx context.Context, resolvedImage *resolvedRuntimeImage) (string, error) {
	if resolvedImage == nil {
		return "", fmt.Errorf("resolved OCI image metadata is nil")
	}

	localTag := strings.TrimSpace(resolvedImage.LocalOCITag)
	if localTag == "" && resolvedImage.Source == runtimeImageSourceLocalOCITag {
		localTag = strings.TrimSpace(resolvedImage.PrepareRef)
	}
	if localTag != "" {
		return localTag, nil
	}

	// Auto-pull from registry if the source is a registry OCI VM ref.
	if resolvedImage.Source == runtimeImageSourceRegistry && resolvedImage.VMImageType == types.VMImageTypeOCIVM {
		pullFn := m.ociPullFn
		if pullFn == nil {
			pullFn = oci.Pull
		}
		pullResult, pullErr := pullFn(ctx, m.cfg, resolvedImage.PrepareRef)
		if pullErr != nil {
			return "", fmt.Errorf("pull OCI VM image %q: %w", resolvedImage.PrepareRef, pullErr)
		}
		localTag = strings.TrimSpace(pullResult.Ref)
		if localTag == "" {
			localTag = oci.EnsureLatestTag(resolvedImage.PrepareRef)
		}
		log.Printf("auto-pulled OCI VM image %q (digest: %s)", localTag, pullResult.ManifestDigest)
		return localTag, nil
	}

	return "", fmt.Errorf(
		"OCI VM runtime currently requires a local OCI tag; source %q is not materialized locally yet",
		resolvedImage.Source,
	)
}

func (m *manager) snapshotCachedBaseKeys(ctx context.Context) map[string]struct{} {
	keys := make(map[string]struct{})
	for img, err := range m.imgMgr.ListCached(ctx) {
		if err != nil {
			log.Printf("warning: list cached images before prepare: %v", err)
			return nil
		}
		if img != nil && img.BaseKey != "" {
			keys[img.BaseKey] = struct{}{}
		}
	}
	return keys
}

func (m *manager) resolveRuntimeImageRef(ctx context.Context, ref string) (*resolvedRuntimeImage, error) {
	return resolveRuntimeImageRefWithProbe(ctx, m.cfg, ref, m.registryProbeFn)
}

func (m *manager) shouldSkipVerifyForCacheHit(skipVerify bool, cachedBaseKeysBefore map[string]struct{}, baseKey, imageRef string) bool {
	if skipVerify || cachedBaseKeysBefore == nil {
		return false
	}
	if _, ok := cachedBaseKeysBefore[baseKey]; !ok {
		return false
	}
	verified, verifyStateErr := refcache.IsVerified(m.cfg, baseKey)
	if verifyStateErr != nil {
		log.Printf("warning: read verify state for %s: %v", baseKey, verifyStateErr)
		return false
	}
	if verified {
		log.Printf("image %s (%s): cache hit with verified record, skipping bootability verification", imageRef, baseKey)
	}
	return verified
}

func (m *manager) verifyPreparedBaseImage(ctx context.Context, imageRef, baseImagePath, baseKey string) error {
	result, verifyErr := m.imgMgr.VerifyBootability(ctx, baseImagePath)
	if verifyErr != nil {
		return fmt.Errorf("verify bootability for %s: %w", imageRef, verifyErr)
	}
	if !result.Bootable {
		return fmt.Errorf("%w: %s - %v", types.ErrImageNotBootable, imageRef, result.Errors)
	}
	for _, warning := range result.Warnings {
		log.Printf("image %s: bootability warning: %s", imageRef, warning)
	}
	if markErr := refcache.MarkVerified(m.cfg, baseKey); markErr != nil {
		log.Printf("warning: persist verify state for %s: %v", baseKey, markErr)
	}
	return nil
}

// validateBootFiles checks that the required boot files exist before launching
// the hypervisor. Returns an actionable error when a required file is missing.
func validateBootFiles(vmCfg *types.VMConfig) error {
	switch vmCfg.BootStrategy {
	case types.BootStrategyUEFI:
		if vmCfg.FirmwarePath == "" {
			return fmt.Errorf("UEFI firmware path is not configured")
		}
		if _, err := os.Stat(vmCfg.FirmwarePath); err != nil {
			return fmt.Errorf("UEFI firmware not found at %s: %w", vmCfg.FirmwarePath, err)
		}
	case types.BootStrategyDirect:
		if vmCfg.KernelPath == "" {
			return fmt.Errorf("kernel path is not configured for direct boot")
		}
		if _, err := os.Stat(vmCfg.KernelPath); err != nil {
			return fmt.Errorf("kernel not found at %s: %w", vmCfg.KernelPath, err)
		}
		if vmCfg.InitramfsPath != "" {
			if _, err := os.Stat(vmCfg.InitramfsPath); err != nil {
				return fmt.Errorf("initramfs not found at %s: %w", vmCfg.InitramfsPath, err)
			}
		}
	}
	return nil
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
		cfgCopy.BootStrategy = types.BootStrategyUEFI
	default: // Direct kernel boot
		cfgCopy.BootStrategy = types.BootStrategyDirect
	}

	// Truncate the serial log before launching CH so that waitForBoot
	// only reads output from this boot attempt, not stale content from
	// a previous run.
	if cfgCopy.SerialLog != "" {
		_ = os.Truncate(cfgCopy.SerialLog, 0)
	}

	// Step 1: Launch Cloud Hypervisor process.
	pid, err := m.hyper.Launch(ctx, vmID, &cfgCopy)
	if err != nil {
		return nil, fmt.Errorf("launch CH for %s (%s): %w", vmID, bootMode, err)
	}

	// Record PID in metadata immediately after launch (no state change).
	runtimeHypervisor := (&types.VMMetadataFile{}).HypervisorProcessName(m.cfg.CHBinary)
	if err := m.updateMetadata(vmID, func(md *types.VMMetadataFile) {
		md.ProcessPID = pid
		md.HypervisorBinary = runtimeHypervisor
		md.StartedAt = nowRFC3339()
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
func (m *manager) Start(ctx context.Context, vmID string) error {
	meta, err := m.LoadMetadata(vmID)
	if err != nil {
		return err
	}

	state := types.VMState(meta.State)

	// Healthy RUNNING state is idempotent; stale RUNNING should not be a no-op.
	if state == types.VMStateRunning {
		vmCfg, cfgErr := m.LoadConfig(vmID)
		if cfgErr != nil {
			return fmt.Errorf("load config for %s: %w", vmID, cfgErr)
		}
		if actualState := m.determineActualState(meta, vmCfg); actualState == types.VMStateRunning {
			return nil
		}
		return fmt.Errorf("VM %s metadata says RUNNING but runtime is not healthy; run 'cocoon doctor --fix' to reconcile", vmID)
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

	// Repair the overlay if the VM was previously stopped. An unclean
	// shutdown (e.g., SIGKILL) can leave leaked clusters in the qcow2
	// overlay, which causes I/O errors on the next boot.
	if state == types.VMStateStopped && vmCfg.OverlayPath != "" {
		if repairErr := m.cowMgr.RepairOverlay(ctx, vmID); repairErr != nil {
			log.Printf("warning: overlay repair for %s failed: %v", vmID, repairErr)
		}
	}

	// Transition to STARTING.
	if transErr := m.TransitionState(vmID, types.VMStateStarting, "start requested"); transErr != nil {
		return transErr
	}

	ociRuntime, err := m.setupOCIRuntimeForStart(ctx, vmID, vmCfg)
	if err != nil {
		if transErr := m.TransitionState(vmID, types.VMStateError, fmt.Sprintf("start OCI runtime failed: %v", err)); transErr != nil {
			log.Printf("warning: failed to transition %s to ERROR after OCI runtime failure: %v", vmID, transErr)
		}
		return fmt.Errorf("start OCI runtime for %s: %w", vmID, err)
	}

	rollbackOCIRuntimeOnFailure := func(trigger string, triggerErr error) {
		if vmCfg.ImageType != types.VMImageTypeOCIVM {
			return
		}
		if cleanupErr := m.cleanupOCIRuntime(vmID, nil); cleanupErr != nil {
			log.Printf("warning: rollback OCI runtime for %s after %s (%v): %v", vmID, trigger, triggerErr, cleanupErr)
		}
	}

	bootStartTime := time.Now()

	// attemptBootAndWait runs attemptBoot + waitForBoot as a single unit.
	// If the CH API calls succeed but boot-time monitoring detects a failure
	// (e.g., kernel panic), it kills the CH process and returns the error so
	// the caller handles the boot result.
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

	// Pre-boot validation: verify required boot files exist before launching CH.
	if validationErr := validateBootFiles(vmCfg); validationErr != nil {
		rollbackOCIRuntimeOnFailure("pre-boot validation failure", validationErr)
		if transErr := m.TransitionState(vmID, types.VMStateError, fmt.Sprintf("pre-boot validation failed: %v", validationErr)); transErr != nil {
			log.Printf("warning: failed to transition %s to ERROR after pre-boot validation failure: %v", vmID, transErr)
		}
		return fmt.Errorf("pre-boot validation for %s failed (run 'cocoon doctor' to check dependencies): %w", vmID, validationErr)
	}

	// Attempt boot with the configured strategy (no fallback).
	var result *bootResult
	var bootErr error

	switch vmCfg.BootStrategy {
	case types.BootStrategyUEFI:
		result, bootErr = attemptBootAndWait(vmCfg.FirmwarePath, types.BootModeUEFI)

	case types.BootStrategyDirect:
		result, bootErr = attemptBootAndWait(vmCfg.FirmwarePath, types.BootModeDirect)

	default:
		bootErr = fmt.Errorf("invalid boot strategy in config: %q", vmCfg.BootStrategy)
	}

	if bootErr != nil {
		rollbackOCIRuntimeOnFailure("boot failure", bootErr)
		if transErr := m.TransitionState(vmID, types.VMStateError, fmt.Sprintf("boot failed: %v", bootErr)); transErr != nil {
			log.Printf("warning: failed to transition %s to ERROR after boot failure: %v", vmID, transErr)
		}
		return fmt.Errorf("boot VM %s: %w", vmID, bootErr)
	}

	// Transition STARTING -> RUNNING with runtime metadata.
	runtimeHypervisor := (&types.VMMetadataFile{}).HypervisorProcessName(m.cfg.CHBinary)
	if err := m.transitionStateWithUpdate(vmID, types.VMStateRunning, "boot completed", func(md *types.VMMetadataFile) {
		md.ProcessPID = result.pid
		md.HypervisorBinary = runtimeHypervisor
		md.BootTime = time.Since(bootStartTime).Round(time.Millisecond).String()
		md.LastBootMode = string(result.bootMode)
		md.LastFirmwarePath = result.firmwarePath
		if ociRuntime != nil {
			md.VirtiofsdPID = ociRuntime.PID
			md.VirtiofsdSocket = ociRuntime.SocketPath
			md.VirtiofsdBinary = ociRuntime.ProcessName
			md.OCIOverlayMounted = true
		}
	}); err != nil {
		// Transition failed but VM is running; force kill and go to ERROR.
		if killErr := m.hyper.ForceKill(vmID); killErr != nil {
			log.Printf("warning: force kill %s after transition failure: %v", vmID, killErr)
		}
		rollbackOCIRuntimeOnFailure("run transition failure", err)
		if transErr := m.TransitionState(vmID, types.VMStateError, fmt.Sprintf("start transition failed: %v", err)); transErr != nil {
			log.Printf("warning: failed to transition %s to ERROR after run transition failure: %v", vmID, transErr)
		}
		return fmt.Errorf("transition to RUNNING for %s: %w", vmID, err)
	}

	return nil
}

func (m *manager) setupOCIRuntimeForStart(
	ctx context.Context,
	vmID string,
	vmCfg *types.VMConfig,
) (*virtiofsdRuntimeInfo, error) {
	if vmCfg.ImageType != types.VMImageTypeOCIVM {
		return nil, nil
	}
	if strings.TrimSpace(vmCfg.BaseImagePath) == "" {
		return nil, fmt.Errorf("OCI runtime entry path is empty")
	}
	// Re-pin on each start as a self-healing guard:
	// create-time pin is authoritative, but start-time idempotent pin repairs
	// missing entries caused by manual edits or crash windows in prior cleanup.
	if err := oci.AddRuntimeRef(m.cfg, vmCfg.BaseKey, vmID); err != nil {
		return nil, fmt.Errorf("pin OCI runtime cache %s for %s: %w", vmCfg.BaseKey, vmID, err)
	}

	// Read entry metadata to derive multi-lowerdir list for OverlayFS mount.
	entryMetaPath := filepath.Join(vmCfg.BaseImagePath, "meta.json")
	entryMeta, err := oci.ReadEntryMeta(entryMetaPath)
	if err != nil {
		return nil, fmt.Errorf("read OCI runtime entry metadata for %s: %w", vmCfg.BaseKey, err)
	}
	// Overlay is pre-mounted at create time. If already mounted (normal case
	// after create, or persistent across stop→start), skip the expensive mount.
	// Otherwise mount now (e.g., after host reboot where mounts are lost).
	alreadyMounted, mountCheckErr := m.overlayMgr.IsMounted(vmID)
	if mountCheckErr != nil {
		return nil, fmt.Errorf("check overlay mount for %s: %w", vmID, mountCheckErr)
	}
	if !alreadyMounted {
		// OverlayFS lowerdir: leftmost = highest priority (top layer).
		// OCI layers in meta are base-to-top, so reverse for lowerdir order.
		lowerDirs := make([]string, len(entryMeta.RootfsLayerDigests))
		for i, digest := range entryMeta.RootfsLayerDigests {
			hex, hexErr := oci.ParseSHA256Digest("sha256:" + digest)
			if hexErr != nil {
				return nil, fmt.Errorf("invalid rootfs layer digest in entry meta: %w", hexErr)
			}
			lowerDirs[len(lowerDirs)-1-i] = filepath.Join(m.cfg.OCIRuntimeLayerDir(hex), "rootfs")
		}
		if mountErr := m.overlayMgr.MountVM(vmID, lowerDirs); mountErr != nil {
			return nil, fmt.Errorf("mount OCI runtime overlay: %w", mountErr)
		}
	}

	// Post-mount validation: verify the overlay is actually mounted and the
	// merged directory is non-empty before starting virtiofsd. If virtiofsd
	// starts on an empty/unmounted directory, the guest VM gets an empty rootfs.
	if validateErr := m.validateOverlayMount(vmID); validateErr != nil {
		_ = m.overlayMgr.UnmountVM(vmID)
		return nil, fmt.Errorf("post-mount overlay validation: %w", validateErr)
	}

	socketPath := vmCfg.VirtioFSSock
	if strings.TrimSpace(socketPath) == "" {
		socketPath = m.cfg.VMOCIRootfsVirtioFSSocketPath(vmID)
	}
	runtimeInfo, err := m.virtiofsMgr.Start(ctx, vmID, m.cfg.VMOCIMergedDir(vmID), socketPath)
	if err != nil {
		_ = m.overlayMgr.UnmountVM(vmID)
		return nil, err
	}

	vmCfg.VirtioFSSock = runtimeInfo.SocketPath
	// Always override VirtioFSTag with the canonical constant so that
	// stale values from existing OCI runtime entries or VM configs on disk
	// (e.g., "/dev/root" from before the rename) are corrected at boot time.
	vmCfg.VirtioFSTag = defaultOCIRuntimeVirtioFSTag
	vmCfg.Cmdline = normalizeVirtiofsKernelCmdline(vmCfg.Cmdline, vmCfg.VirtioFSTag)

	if mdErr := m.updateMetadata(vmID, func(md *types.VMMetadataFile) {
		md.VirtiofsdPID = runtimeInfo.PID
		md.VirtiofsdSocket = runtimeInfo.SocketPath
		md.VirtiofsdBinary = runtimeInfo.ProcessName
		md.OCIOverlayMounted = true
	}); mdErr != nil {
		_ = m.virtiofsMgr.Stop(vmID, runtimeInfo.PID, runtimeInfo.ProcessName, runtimeInfo.SocketPath)
		_ = m.overlayMgr.UnmountVM(vmID)
		return nil, fmt.Errorf("record OCI runtime metadata: %w", mdErr)
	}

	return runtimeInfo, nil
}

// validateOverlayMount verifies the overlayfs mount for a VM is healthy:
// the merged directory must be mounted and contain at least one entry.
// This prevents virtiofsd from serving an empty rootfs to the guest.
func (m *manager) validateOverlayMount(vmID string) error {
	mounted, err := m.overlayMgr.IsMounted(vmID)
	if err != nil {
		return fmt.Errorf("check overlay mount status for %s: %w", vmID, err)
	}
	if !mounted {
		return fmt.Errorf("overlay for %s is not mounted after MountVM returned success", vmID)
	}

	mergedDir := m.cfg.VMOCIMergedDir(vmID)
	entries, err := os.ReadDir(mergedDir)
	if err != nil {
		return fmt.Errorf("read merged directory %s: %w", mergedDir, err)
	}
	if len(entries) == 0 {
		return fmt.Errorf("merged directory %s is empty after mount; rootfs layers may be corrupt", mergedDir)
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
		if cleanupErr := m.cleanupOCIRuntime(vmID, meta); cleanupErr != nil {
			log.Printf("warning: cleanup OCI runtime resources for %s in stopped state: %v", vmID, cleanupErr)
		}
		return nil
	}

	// If already stopping, wait for it to finish.
	if state == types.VMStateStopping {
		return m.waitForStoppingVM(ctx, vmID, timeout)
	}

	// Validate that we can stop from the current state.
	if state != types.VMStateRunning {
		return fmt.Errorf("%w: cannot stop VM in state %s", types.ErrInvalidTransition, state)
	}

	// Transition to STOPPING.
	if err := m.TransitionState(vmID, types.VMStateStopping, "stop requested"); err != nil {
		return err
	}

	// Choose shutdown strategy based on boot mode:
	//  - UEFI boot: ACPI power-button → guest systemd handles graceful shutdown.
	//  - Direct boot: skip ACPI (guest kernel likely lacks ACPI support),
	//    go straight to vm.shutdown API at the VMM level.
	var shutdownErr error
	vmCfg, cfgErr := m.LoadConfig(vmID)
	if cfgErr == nil && vmCfg.BootStrategy == types.BootStrategyDirect {
		shutdownErr = m.shutdownDirectBoot(ctx, vmID, timeout)
	} else {
		shutdownErr = m.hyper.Shutdown(ctx, vmID, timeout)
	}
	if shutdownErr != nil {
		// Shutdown failed (timeout or error); transition to ERROR.
		// LastError and ErrorCount are auto-tracked by TransitionState.
		if transErr := m.TransitionState(vmID, types.VMStateError, fmt.Sprintf("graceful stop failed: %v", shutdownErr)); transErr != nil {
			log.Printf("warning: failed to transition %s to ERROR after stop failure: %v", vmID, transErr)
		}
		return fmt.Errorf("shutdown VM %s: %w", vmID, shutdownErr)
	}

	// Transition STOPPING -> STOPPED with stopped_at and cleared PID.
	now := nowRFC3339()
	if err := m.transitionStateWithUpdate(vmID, types.VMStateStopped, "graceful stop completed", func(md *types.VMMetadataFile) {
		md.StoppedAt = now
		md.ProcessPID = 0
	}); err != nil {
		if transErr := m.TransitionState(vmID, types.VMStateError, fmt.Sprintf("stop transition failed: %v", err)); transErr != nil {
			log.Printf("warning: failed to transition %s to ERROR after stop transition failure: %v", vmID, transErr)
		}
		return err
	}

	if cleanupErr := m.cleanupOCIRuntime(vmID, nil); cleanupErr != nil {
		log.Printf("warning: cleanup OCI runtime resources for %s during stop: %v", vmID, cleanupErr)
	}

	return nil
}

// shutdownDirectBoot stops a direct-boot VM by going straight to the
// vm.shutdown API + SIGTERM, skipping the ACPI power-button that the guest
// kernel likely cannot handle without CONFIG_ACPI=y.
//
// Sequence: vm.shutdown (stop vCPUs, flush backends) → SIGTERM (graceful
// CH process exit) → SIGKILL (last resort).
func (m *manager) shutdownDirectBoot(ctx context.Context, vmID string, timeout time.Duration) error {
	socketPath := m.cfg.VMSocketPath(vmID)

	// Step 1: vm.shutdown — stop guest vCPUs and flush block backends.
	// Best-effort; if the API socket is already gone, proceed to signal.
	if err := m.hyper.ShutdownVM(ctx, socketPath); err != nil {
		log.Printf("vm.shutdown API failed for direct-boot VM %s (proceeding to SIGTERM): %v", vmID, err)
	}

	// Step 2: SIGTERM the CH process. Unlike UEFI VMs where the guest
	// initiates shutdown via ACPI and CH exits naturally, direct-boot VMs
	// need an explicit signal to terminate the CH process.
	pid, pidErr := utils.ReadPIDFile(m.cfg.VMPIDPath(vmID))
	if pidErr != nil {
		if os.IsNotExist(pidErr) {
			return nil
		}
		return fmt.Errorf("read CH PID for %s: %w", vmID, pidErr)
	}
	if !utils.IsProcessAlive(pid) {
		return nil
	}

	process, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find CH process %d for %s: %w", pid, vmID, err)
	}
	if sigErr := process.Signal(syscall.SIGTERM); sigErr != nil {
		if !utils.IsProcessAlive(pid) {
			return nil
		}
		log.Printf("SIGTERM failed for CH pid %d (%s): %v; falling back to SIGKILL", pid, vmID, sigErr)
		return m.hyper.ForceKill(vmID)
	}

	// Wait for graceful exit.
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !utils.IsProcessAlive(pid) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("shutdown canceled for %s: %w", vmID, ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}

	log.Printf("CH pid %d (%s) did not exit after SIGTERM; falling back to SIGKILL", pid, vmID)
	return m.hyper.ForceKill(vmID)
}

func (m *manager) waitForStoppingVM(ctx context.Context, vmID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		// Re-load metadata each iteration to pick up PID or state
		// changes made by other processes (e.g., reconciliation).
		freshMeta, reloadErr := m.LoadMetadata(vmID)
		if reloadErr != nil {
			return fmt.Errorf("reload metadata while waiting for stop: %w", reloadErr)
		}
		// If another process already transitioned us out of STOPPING, done.
		if types.VMState(freshMeta.State) == types.VMStateStopped {
			if cleanupErr := m.cleanupOCIRuntime(vmID, freshMeta); cleanupErr != nil {
				log.Printf("warning: cleanup OCI runtime resources for %s after external stop transition: %v", vmID, cleanupErr)
			}
			return nil
		}

		// Check both PID file (primary) and metadata PID (fallback).
		alive := m.hyper.IsAlive(vmID)
		if !alive && freshMeta.ProcessPID > 0 {
			alive = utils.ValidateProcess(freshMeta.ProcessPID, freshMeta.HypervisorProcessName(m.cfg.CHBinary))
		}
		if !alive {
			now := nowRFC3339()
			_ = m.transitionStateWithUpdate(vmID, types.VMStateStopped, "process exited during stop wait", func(md *types.VMMetadataFile) {
				md.StoppedAt = now
				md.ProcessPID = 0
			})
			if cleanupErr := m.cleanupOCIRuntime(vmID, nil); cleanupErr != nil {
				log.Printf("warning: cleanup OCI runtime resources for %s after process exit during stop wait: %v", vmID, cleanupErr)
			}
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
		if cleanupErr := m.cleanupOCIRuntime(vmID, meta); cleanupErr != nil {
			log.Printf("warning: cleanup OCI runtime resources for %s in stopped state: %v", vmID, cleanupErr)
		}
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

	// Safety net: if the PID file was already cleaned up but the CH process
	// is still running, use the PID from metadata to kill it directly.
	if meta.ProcessPID > 0 && utils.ValidateProcess(meta.ProcessPID, meta.HypervisorProcessName(m.cfg.CHBinary)) {
		log.Printf("kill %s: PID file missing but process %d still alive, killing via metadata PID", vmID, meta.ProcessPID)
		_ = utils.ForceKillProcess(meta.ProcessPID)
	}

	// Determine target state based on current state.
	now := nowRFC3339()
	var transErr error
	switch state {
	case types.VMStateRunning:
		if transErr2 := m.TransitionState(vmID, types.VMStateStopping, "force killed"); transErr2 != nil {
			log.Printf("warning: failed to transition %s to STOPPING during kill: %v", vmID, transErr2)
		}
		transErr = m.transitionStateWithUpdate(vmID, types.VMStateStopped, "force killed", func(md *types.VMMetadataFile) {
			md.StoppedAt = now
			md.ProcessPID = 0
		})
	case types.VMStateStopping:
		transErr = m.transitionStateWithUpdate(vmID, types.VMStateStopped, "force killed", func(md *types.VMMetadataFile) {
			md.StoppedAt = now
			md.ProcessPID = 0
		})
	case types.VMStateStarting:
		// STARTING -> ERROR (cannot go through STOPPING).
		transErr = m.transitionStateWithUpdate(vmID, types.VMStateError, "force killed during start", func(md *types.VMMetadataFile) {
			md.StoppedAt = now
			md.ProcessPID = 0
		})
	case types.VMStateError:
		// Clean up any zombie process
		if m.hyper.IsAlive(vmID) {
			_ = m.hyper.ForceKill(vmID)
		}
		transErr = m.transitionStateWithUpdate(vmID, types.VMStateStopped, "force killed from error state", func(md *types.VMMetadataFile) {
			md.StoppedAt = now
			md.ProcessPID = 0
		})
	default:
		return fmt.Errorf("%w: cannot kill VM in state %s", types.ErrInvalidTransition, state)
	}

	if cleanupErr := m.cleanupOCIRuntime(vmID, meta); cleanupErr != nil {
		log.Printf("warning: cleanup OCI runtime resources for %s during kill: %v", vmID, cleanupErr)
	}

	return transErr
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
	if metaPresent && meta.ProcessPID > 0 && utils.ValidateProcess(meta.ProcessPID, meta.HypervisorProcessName(m.cfg.CHBinary)) {
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

	// Best-effort cleanup: collect warnings for non-fatal failures so the
	// caller knows about residual artifacts, but don't fail the delete.
	var warnings []string

	// Stop rootfs-serving virtiofsd and unmount OCI overlay runtime resources.
	if cleanupErr := m.cleanupOCIRuntime(vmID, meta); cleanupErr != nil {
		warnings = append(warnings, fmt.Sprintf("cleanup OCI runtime: %v", cleanupErr))
	}

	// Unpin reference.
	if cfgErr == nil && vmCfg.BaseKey != "" {
		refErr := m.removeVMReference(vmCfg, vmID)
		if refErr != nil {
			message := "remove reference"
			if vmCfg.ImageType == types.VMImageTypeOCIVM {
				message = "remove OCI runtime pin"
			}
			warnings = append(warnings, fmt.Sprintf("%s %s: %v", message, vmCfg.BaseKey, refErr))
		}
	}

	// Remove COW overlay (qcow2 path only).
	if cfgErr == nil && vmCfg.ImageType != types.VMImageTypeOCIVM && strings.TrimSpace(vmCfg.OverlayPath) != "" {
		if overlayErr := m.cowMgr.RemoveOverlay(vmID); overlayErr != nil {
			warnings = append(warnings, fmt.Sprintf("remove overlay: %v", overlayErr))
		}
	}

	// Remove name from index.
	if cfgErr == nil && vmCfg.Name != "" {
		if nameErr := RemoveName(m.cfg, vmCfg.Name); nameErr != nil {
			warnings = append(warnings, fmt.Sprintf("remove name %s: %v", vmCfg.Name, nameErr))
		}
	}

	// Remove VM directories.
	vmDir := m.cfg.VMPersistDir(vmID)
	runtimeDir := m.cfg.VMRuntimeDir(vmID)
	if err := os.RemoveAll(vmDir); err != nil {
		log.Printf("warning: remove VM persist dir %s: %v", vmDir, err)
	}
	if err := os.RemoveAll(runtimeDir); err != nil {
		log.Printf("warning: remove VM runtime dir %s: %v", runtimeDir, err)
	}

	// Remove logs (live outside vmDir, under LogDir).
	_ = os.Remove(m.cfg.VMSerialLogPath(vmID))
	_ = os.Remove(m.cfg.VMCHLogPath(vmID))
	_ = os.Remove(m.cfg.VMSwtpmLogPath(vmID))
	_ = os.Remove(m.cfg.VMVirtiofsdLogPath(vmID))

	if len(warnings) > 0 {
		log.Printf("delete %s: cleanup warnings: %v", vmID, warnings)
	}

	return nil
}

func (m *manager) ensureDeletePreconditions(ctx context.Context, vmID string, force bool, meta *types.VMMetadataFile) error {
	state := types.VMState(meta.State)

	switch state {
	case types.VMStateRunning:
		if !force {
			return types.ErrVMRunning
		}
		// Attempt graceful stop first; force kill on failure.
		if stopErr := m.Stop(ctx, vmID, time.Duration(m.cfg.StopTimeoutSeconds)*time.Second); stopErr != nil {
			if killErr := m.hyper.ForceKill(vmID); killErr != nil {
				log.Printf("warning: force kill %s during delete: %v", vmID, killErr)
			}
			if transErr := m.TransitionState(vmID, types.VMStateError, fmt.Sprintf("force stop failed: %v", stopErr)); transErr != nil {
				log.Printf("warning: failed to transition %s to ERROR during delete: %v", vmID, transErr)
			}
		}

	case types.VMStateStarting, types.VMStateStopping:
		if !force {
			return fmt.Errorf("%w: VM is in %s state, use --force to delete", types.ErrInvalidTransition, state)
		}
		// Force kill any live process and move to STOPPED so the subsequent
		// STOPPED -> DELETED transition goes through normal validation.
		if killErr := m.hyper.ForceKill(vmID); killErr != nil {
			log.Printf("warning: force kill %s during delete (STARTING/STOPPING): %v", vmID, killErr)
		}
		if meta.ProcessPID > 0 && utils.ValidateProcess(meta.ProcessPID, meta.HypervisorProcessName(m.cfg.CHBinary)) {
			if killErr := utils.ForceKillProcess(meta.ProcessPID); killErr != nil {
				log.Printf("warning: force kill PID %d for %s during delete: %v", meta.ProcessPID, vmID, killErr)
			}
		}
		now := nowRFC3339()
		if transErr := m.transitionStateWithUpdate(vmID, types.VMStateStopped, "force killed for delete", func(md *types.VMMetadataFile) {
			md.StoppedAt = now
			md.ProcessPID = 0
		}); transErr != nil {
			log.Printf("warning: failed to transition %s to STOPPED during force delete: %v", vmID, transErr)
		}

	case types.VMStateError:
		// Error state with a live process: force kill it.
		hasLiveProcess := m.hyper.IsAlive(vmID) ||
			(meta.ProcessPID > 0 && utils.ValidateProcess(meta.ProcessPID, meta.HypervisorProcessName(m.cfg.CHBinary)))
		if hasLiveProcess && !force {
			return fmt.Errorf("%w: VM is in ERROR state with a live process, use --force to delete", types.ErrInvalidTransition)
		}
		if hasLiveProcess {
			if killErr := m.hyper.ForceKill(vmID); killErr != nil {
				log.Printf("warning: force kill %s in ERROR state during delete: %v", vmID, killErr)
			}
			if meta.ProcessPID > 0 {
				if killErr := utils.ForceKillProcess(meta.ProcessPID); killErr != nil {
					log.Printf("warning: force kill PID %d for %s in ERROR state: %v", meta.ProcessPID, vmID, killErr)
				}
			}
		}
		// ERROR -> DELETED is a valid transition, so no state change needed here.

	default:
		// CREATED, STOPPED, etc. — no preconditions needed.
	}
	return nil
}

func (m *manager) removeVMReference(vmCfg *types.VMConfig, vmID string) error {
	if vmCfg.ImageType == types.VMImageTypeOCIVM {
		return oci.RemoveRuntimeRef(m.cfg, vmCfg.BaseKey, vmID)
	}
	return m.refCounter.RemoveReference(vmCfg.BaseKey, vmID)
}

func (m *manager) cleanupOCIRuntime(vmID string, meta *types.VMMetadataFile) error {
	metaExists := meta != nil
	if !metaExists {
		loadedMeta, err := m.LoadMetadata(vmID)
		if err != nil {
			if !isNotFound(err) {
				return fmt.Errorf("load metadata for OCI runtime cleanup: %w", err)
			}
		} else {
			meta = loadedMeta
			metaExists = true
		}
	}

	pid := 0
	socketPath := m.cfg.VMOCIRootfsVirtioFSSocketPath(vmID)
	expectedProc := (&types.VMMetadataFile{}).VirtiofsdProcessName(m.cfg.VirtiofsdBinary)
	needsMetadataClear := false
	if meta != nil {
		pid = meta.VirtiofsdPID
		if strings.TrimSpace(meta.VirtiofsdSocket) != "" {
			socketPath = meta.VirtiofsdSocket
		}
		expectedProc = meta.VirtiofsdProcessName(m.cfg.VirtiofsdBinary)
		needsMetadataClear = meta.VirtiofsdPID > 0 || strings.TrimSpace(meta.VirtiofsdSocket) != "" || meta.OCIOverlayMounted
	}

	var cleanupErr error

	if err := m.virtiofsMgr.Stop(vmID, pid, expectedProc, socketPath); err != nil {
		cleanupErr = errors.Join(cleanupErr, fmt.Errorf("stop virtiofsd: %w", err))
	}

	if err := m.overlayMgr.UnmountVM(vmID); err != nil {
		cleanupErr = errors.Join(cleanupErr, fmt.Errorf("unmount overlay: %w", err))
	}

	if metaExists && needsMetadataClear {
		if err := m.updateMetadata(vmID, func(md *types.VMMetadataFile) {
			md.VirtiofsdPID = 0
			md.VirtiofsdSocket = ""
			md.OCIOverlayMounted = false
		}); err != nil && !isNotFound(err) {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("clear OCI runtime metadata: %w", err))
		}
	}

	return cleanupErr
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
	actualState := m.determineActualState(meta, vmCfg)
	if actualState != inspect.State {
		inspect.State = actualState
	}
	if excerpt, readErr := readSerialLogExcerpt(vmCfg.SerialLog, 100); readErr == nil {
		inspect.Hypervisor.SerialLogExcerpt = excerpt
	}

	// Populate console PTY path from live CH API (only for running VMs).
	if types.VMState(meta.State) == types.VMStateRunning && m.hyper.IsAlive(vmID) {
		if ptyPath, ptyErr := m.hyper.GetConsolePTYPath(ctx, vmCfg.SocketPath); ptyErr == nil && ptyPath != "" {
			inspect.Hypervisor.ConsolePTY = ptyPath
		}
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

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() == 0 {
		return nil, nil
	}

	const (
		readChunkSize = int64(64 * 1024)
		maxTailBytes  = int64(8 * 1024 * 1024)
	)

	var (
		offset       = info.Size()
		totalRead    int64
		newlineCount int
		chunks       [][]byte
	)

	for offset > 0 && newlineCount <= maxLines && totalRead < maxTailBytes {
		n := readChunkSize
		n = min(n, offset)
		offset -= n

		buf := make([]byte, n)
		readN, readErr := f.ReadAt(buf, offset)
		if readErr != nil && readErr != io.EOF {
			return nil, readErr
		}
		buf = buf[:readN]
		chunks = append(chunks, buf)
		totalRead += int64(readN)
		newlineCount += bytes.Count(buf, []byte{'\n'})
	}

	var tail bytes.Buffer
	tail.Grow(int(totalRead))
	for i := len(chunks) - 1; i >= 0; i-- {
		if _, writeErr := tail.Write(chunks[i]); writeErr != nil {
			return nil, writeErr
		}
	}

	content := strings.TrimRight(tail.String(), "\n")
	if content == "" {
		return nil, nil
	}

	lines := strings.Split(content, "\n")
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	if offset > 0 && len(lines) > 0 {
		lines[0] = "..." + lines[0]
	}
	return lines, nil
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// withMetadataLock executes fn while holding the VM metadata lock.
func (m *manager) withMetadataLock(vmID string, fn func(*types.VMMetadataFile) error) error {
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

	if err := fn(&meta); err != nil {
		return err
	}

	meta.UpdatedAt = nowRFC3339()
	return utils.AtomicWriteJSON(metaPath, &meta)
}

// transitionStateWithUpdate validates a state transition and applies an
// additional mutation function to metadata before persisting.
func (m *manager) transitionStateWithUpdate(vmID string, to types.VMState, reason string, mutate func(*types.VMMetadataFile)) error {
	return m.withMetadataLock(vmID, func(meta *types.VMMetadataFile) error {
		from := types.VMState(meta.State)
		if err := types.ValidateTransition(from, to); err != nil {
			return fmt.Errorf("%w: %s -> %s (%s)", types.ErrInvalidTransition, from, to, reason)
		}

		meta.PreviousState = meta.State
		meta.State = string(to)

		// Auto-track errors: when entering ERROR, record reason and increment count.
		if to == types.VMStateError {
			meta.LastError = reason
			meta.LastErrorType = string(classifyError(reason))
			meta.LastErrorAt = nowRFC3339()
			if meta.ErrorCount < 1000 {
				meta.ErrorCount++
			}
		}

		if mutate != nil {
			mutate(meta)
		}
		return nil
	})
}

// UpdateMetadata applies a mutation function to metadata without performing
// a state transition, atomically under flock. Implements the vm.Manager interface.
func (m *manager) UpdateMetadata(vmID string, mutate func(*types.VMMetadataFile)) error {
	return m.updateMetadata(vmID, mutate)
}

// updateMetadata applies a mutation function to metadata without performing
// a state transition.
func (m *manager) updateMetadata(vmID string, mutate func(*types.VMMetadataFile)) error {
	return m.withMetadataLock(vmID, func(meta *types.VMMetadataFile) error {
		if mutate != nil {
			mutate(meta)
		}
		return nil
	})
}

// buildCHVMConfig converts a types.VMConfig to the Cloud Hypervisor REST API
// request body (hypervisor.CHVMConfig) for PUT /api/v1/vm.create.
func buildCHVMConfig(vmCfg *types.VMConfig) *hypervisor.CHVMConfig {
	virtiofsEnabled := strings.TrimSpace(vmCfg.VirtioFSTag) != "" && strings.TrimSpace(vmCfg.VirtioFSSock) != ""
	cmdline := vmCfg.Cmdline
	virtiofsTag := ""
	if virtiofsEnabled {
		virtiofsTag = defaultOCIRuntimeVirtioFSTag
		cmdline = normalizeVirtiofsKernelCmdline(cmdline, virtiofsTag)
	}

	cfg := &hypervisor.CHVMConfig{
		CPUs: hypervisor.CHCPUConfig{
			BootVCPUs: vmCfg.CPUs,
			MaxVCPUs:  vmCfg.CPUs,
		},
		Memory: hypervisor.CHMemoryConfig{
			// CH expects memory size in bytes; VMConfig stores MB.
			Size: vmCfg.MemoryMB * 1024 * 1024,
		},
		Serial: hypervisor.CHSerialConfig{
			Mode: "File",
			File: vmCfg.SerialLog,
		},
		Console: hypervisor.CHConsoleConfig{
			Mode: "Pty",
		},
	}
	if strings.TrimSpace(vmCfg.OverlayPath) != "" {
		cfg.Disks = append(cfg.Disks, hypervisor.CHDiskConfig{
			Path:         vmCfg.OverlayPath,
			ReadOnly:     false,
			ImageType:    "Qcow2",
			BackingFiles: true,
		})
	}

	// Build payload based on boot strategy.
	// Direct kernel boot uses payload.kernel + payload.initramfs + payload.cmdline.
	// UEFI boot uses payload.firmware (CLOUDHV.fd).
	if vmCfg.KernelPath != "" {
		cfg.Payload = &hypervisor.CHPayloadConfig{
			Kernel:    vmCfg.KernelPath,
			Initramfs: vmCfg.InitramfsPath,
			Cmdline:   cmdline,
		}
	} else if vmCfg.FirmwarePath != "" {
		cfg.Payload = &hypervisor.CHPayloadConfig{
			Firmware: vmCfg.FirmwarePath,
		}
	}

	// Phase 2 virtiofs rootfs path uses CH fs[].
	if virtiofsEnabled {
		cfg.Fs = append(cfg.Fs, hypervisor.CHFsConfig{
			Tag:    virtiofsTag,
			Socket: vmCfg.VirtioFSSock,
		})
		// Cloud Hypervisor requires shared memory (or huge pages) for vhost-user.
		// virtio-fs is a vhost-user device, so ensure memory.shared is enabled.
		cfg.Memory.Shared = true
	}

	// TPM socket is passed via REST payload, not CLI flags.
	if vmCfg.TPMSocketPath != "" {
		cfg.TPM = &hypervisor.CHTPMConfig{
			Socket: vmCfg.TPMSocketPath,
		}
	}

	return cfg
}

// resolveUEFIFirmwarePath returns a usable UEFI firmware path. It first checks
// the configured primary path. If the primary is missing or empty, it probes
// the deprecated system OVMF fallback paths.
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
// strategy. For UEFI, it returns the UEFI firmware (CLOUDHV.fd) with
// fallback to system OVMF paths. For direct kernel boot, no firmware is
// needed (kernel/initramfs come from the OCI image).
func resolveFirmwarePath(cfg *config.CocoonConfig, strategy types.BootStrategy) (string, error) {
	switch strategy {
	case types.BootStrategyDirect:
		// Direct kernel boot does not use firmware; kernel path is resolved separately.
		return "", nil
	case types.BootStrategyUEFI:
		return resolveUEFIFirmwarePath(cfg)
	default:
		return "", fmt.Errorf("invalid boot strategy: %q", strategy)
	}
}

// generateDefaultName creates a name like "cocoon-a3f7b2c1d9e0f1a2".
// Uses 8 random bytes (64-bit entropy) to keep collision probability low.
func generateDefaultName() string {
	b := make([]byte, 8)
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
