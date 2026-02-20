package engine

import (
	"context"
	"crypto/rand"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/oklog/ulid/v2"

	"github.com/CMGS/cocoon/image/refcache"
	"github.com/CMGS/cocoon/oci"
	"github.com/CMGS/cocoon/types"
	"github.com/CMGS/cocoon/utils"
	"github.com/CMGS/cocoon/vm"
)

// imagePreparation captures the strategy-specific results of preparing a base
// image (qcow2 or OCI VM). It carries all the data needed to build the VMConfig
// and provides hooks for pinning, storage setup, and rollback.
type imagePreparation struct {
	BaseKey        string
	BaseDigestFull string
	Arch           string
	BaseImagePath  string

	// OCI VM direct-boot fields (zero-value for qcow2 path).
	KernelPath    string
	InitramfsPath string
	Cmdline       string
	VirtioFSTag   string

	// pinRef pins the base image for GC protection (qcow2: AddReference, OCI: AddRuntimeRef).
	pinRef func(vmID string) error
	// unpinRef removes the GC pin on rollback.
	unpinRef func(vmID string)
	// postPin runs after pinning — verify + refcache for qcow2; no-op for OCI VM.
	postPin func(ctx context.Context) error
	// setupStorage creates the overlay (qcow2) or workspace (OCI VM).
	setupStorage func(ctx context.Context, vmID, diskSize string) error
	// cleanupStorage removes storage artifacts on rollback.
	cleanupStorage func(vmID string)
}

// resolveBootStrategy determines the boot strategy for the given image type,
// validating that the requested strategy is compatible.
func resolveBootStrategy(requested types.BootStrategy, imageRef string, imageType types.VMImageType) (types.BootStrategy, error) {
	if imageType == types.VMImageTypeOCIVM {
		if requested == "" {
			return types.BootStrategyDirect, nil
		}
		if requested != types.BootStrategyDirect {
			return "", fmt.Errorf("OCI VM image %q requires --boot-strategy=%s (got %q)", imageRef, types.BootStrategyDirect, requested)
		}
		return requested, nil
	}
	if requested == "" {
		return types.DefaultBootStrategy, nil
	}
	if requested == types.BootStrategyDirect {
		return "", fmt.Errorf("direct kernel boot requires an OCI VM image reference")
	}
	return requested, nil
}

// Create provisions a new VM: generates an ID, prepares the image, creates
// the storage (overlay or workspace), writes config.json and metadata.json,
// pins the reference, registers the name, and transitions CREATING -> CREATED.
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

	resolvedImage, err := m.resolveRuntimeImageRef(ctx, opts.Image)
	if err != nil {
		return nil, fmt.Errorf("resolve image reference %q: %w", opts.Image, err)
	}

	bootStrategy, err := resolveBootStrategy(opts.BootStrategy, opts.Image, resolvedImage.VMImageType)
	if err != nil {
		return nil, err
	}

	// Prepare the base image using strategy-specific logic.
	var prep *imagePreparation
	if resolvedImage.VMImageType == types.VMImageTypeOCIVM {
		prep, err = m.prepareOCIVMImage(ctx, opts, resolvedImage)
	} else {
		prep, err = m.prepareQCOW2Image(ctx, opts, resolvedImage)
	}
	if err != nil {
		return nil, err
	}

	return m.provisionVM(ctx, opts, resolvedImage, prep, bootStrategy, cpus, memoryMB, diskSize) //nolint:wrapcheck // error wrapping handled by callers
}

// provisionVM performs the remaining VM provisioning steps after image
// preparation: generates an ID, pins references, writes config/metadata,
// creates storage, registers name, and transitions to CREATED.
func (m *manager) provisionVM(
	ctx context.Context,
	opts *vm.CreateOptions,
	resolvedImage *resolvedRuntimeImage,
	prep *imagePreparation,
	bootStrategy types.BootStrategy,
	cpus int,
	memoryMB int64,
	diskSize string,
) (*types.VMConfig, error) {
	// Generate name and VM ID.
	name := opts.Name
	if name == "" {
		name = generateDefaultName()
	}
	vmID := "vm-" + ulid.MustNew(ulid.Now(), rand.Reader).String()
	now := nowRFC3339()

	// Pin reference for GC protection.
	pinned := false
	createSucceeded := false
	defer func() {
		if !createSucceeded && pinned {
			prep.unpinRef(vmID)
		}
	}()

	if pinErr := prep.pinRef(vmID); pinErr != nil {
		return nil, fmt.Errorf("pin reference for %s: %w", vmID, pinErr)
	}
	pinned = true

	// Post-pin hook (verify + refcache for qcow2, no-op for OCI VM).
	if postErr := prep.postPin(ctx); postErr != nil {
		return nil, postErr
	}

	// Resolve firmware path based on boot strategy.
	firmwarePath, fwErr := resolveFirmwarePath(m.cfg, bootStrategy)
	if fwErr != nil {
		return nil, fmt.Errorf("resolve firmware: %w", fwErr)
	}

	// Resolve TPM socket path only if TPM is explicitly enabled.
	var tpmSocketPath string
	if opts.EnableTPM {
		tpmSocketPath = m.cfg.VMTPMSocketPath(vmID)
	}

	// Derive overlay/virtiofs paths depending on image type.
	overlayPath := ""
	virtioFSSock := ""
	if resolvedImage.VMImageType == types.VMImageTypeOCIVM {
		virtioFSSock = m.cfg.VMOCIRootfsVirtioFSSocketPath(vmID)
	} else {
		overlayPath = m.cfg.VMOverlayPath(vmID)
	}

	// Build immutable config.
	vmCfg := &types.VMConfig{
		VMID:           vmID,
		Name:           name,
		ImageRef:       opts.Image,
		BaseKey:        prep.BaseKey,
		BaseDigestFull: prep.BaseDigestFull,
		Arch:           prep.Arch,
		ImageType:      resolvedImage.VMImageType,
		BootStrategy:   bootStrategy,
		FirmwarePath:   firmwarePath,
		KernelPath:     prep.KernelPath,
		InitramfsPath:  prep.InitramfsPath,
		Cmdline:        prep.Cmdline,
		VirtioFSTag:    prep.VirtioFSTag,
		VirtioFSSock:   virtioFSSock,
		CPUs:           cpus,
		MemoryMB:       memoryMB,
		DiskSize:       diskSize,
		BaseImagePath:  prep.BaseImagePath,
		OverlayPath:    overlayPath,
		SocketPath:     m.cfg.VMSocketPath(vmID),
		TPMSocketPath:  tpmSocketPath,
		SerialLog:      m.cfg.VMSerialLogPath(vmID),
		CreatedAt:      now,
		SchemaVersion:  types.CurrentConfigSchemaVersion,
	}

	// Create VM directory, write config + metadata.
	// Metadata must exist before storage creation so reconciliation can
	// find the VM if we crash after pinning.
	vmDir := m.cfg.VMPersistDir(vmID)
	if mkdirErr := os.MkdirAll(vmDir, 0o700); mkdirErr != nil { //nolint:gosec // G301: restricted to owner — VM persistent state
		return nil, fmt.Errorf("create VM directory: %w", mkdirErr)
	}

	vmDirCreated := true
	defer func() {
		if !createSucceeded && vmDirCreated {
			_ = os.RemoveAll(vmDir)
		}
	}()

	// Write config.json (immutable, written once).
	configPath := m.cfg.VMConfigPath(vmID)
	if writeErr := utils.AtomicWriteJSON(configPath, vmCfg); writeErr != nil {
		return nil, fmt.Errorf("write config.json: %w", writeErr)
	}

	// Write initial metadata.json in CREATING state.
	runtimeHypervisor := (&types.VMMetadataFile{}).HypervisorProcessName(m.cfg.CHBinary)
	runtimeVirtiofsd := (&types.VMMetadataFile{}).VirtiofsdProcessName(m.cfg.VirtiofsdBinary)
	meta := &types.VMMetadataFile{
		VMID:             vmID,
		State:            string(types.VMStateCreating),
		PreviousState:    "",
		HypervisorBinary: runtimeHypervisor,
		VirtiofsdBinary:  runtimeVirtiofsd,
		UpdatedAt:        now,
		SchemaVersion:    types.CurrentMetadataSchemaVersion,
	}
	metaPath := m.cfg.VMMetadataPath(vmID)
	if writeErr := utils.AtomicWriteJSON(metaPath, meta); writeErr != nil {
		return nil, fmt.Errorf("write metadata.json: %w", writeErr)
	}

	// Create storage: COW overlay (qcow2) or OCI runtime workspace.
	storageCreated := false
	defer func() {
		if !createSucceeded && storageCreated {
			prep.cleanupStorage(vmID)
		}
	}()

	if storageErr := prep.setupStorage(ctx, vmID, diskSize); storageErr != nil {
		return nil, storageErr
	}
	storageCreated = true

	// Register name in the index (atomically checks uniqueness under lock).
	nameRegistered := false
	defer func() {
		if !createSucceeded && nameRegistered {
			_ = RemoveName(m.cfg, name)
		}
	}()

	if nameErr := AddName(m.cfg, name, vmID); nameErr != nil {
		return nil, fmt.Errorf("%w: %s", types.ErrVMAlreadyExists, nameErr)
	}
	nameRegistered = true

	// Transition CREATING -> CREATED.
	if transErr := m.TransitionState(vmID, types.VMStateCreated, "creation completed"); transErr != nil {
		return nil, fmt.Errorf("transition to CREATED: %w", transErr)
	}

	createSucceeded = true
	return vmCfg, nil
}

// prepareQCOW2Image prepares a standard container/cloud image for UEFI boot.
// Returns an imagePreparation with qcow2-specific hooks.
func (m *manager) prepareQCOW2Image(
	ctx context.Context,
	opts *vm.CreateOptions,
	resolvedImage *resolvedRuntimeImage,
) (*imagePreparation, error) {
	cachedBaseKeysBefore := m.snapshotCachedBaseKeys(ctx)

	identity, baseImagePath, err := m.imgMgr.Prepare(ctx, resolvedImage.PrepareRef)
	if err != nil {
		return nil, fmt.Errorf("prepare image %s: %w", opts.Image, err)
	}

	baseKey := identity.BaseKey()

	return &imagePreparation{
		BaseKey:        baseKey,
		BaseDigestFull: identity.FullDigest,
		Arch:           identity.Arch,
		BaseImagePath:  baseImagePath,

		pinRef: func(vmID string) error {
			return m.refCounter.AddReference(baseKey, vmID, identity.FullDigest, opts.Image)
		},
		unpinRef: func(vmID string) {
			_ = m.refCounter.RemoveReference(baseKey, vmID)
		},
		postPin: func(ctx context.Context) error {
			skipVerifyForCacheHit := m.shouldSkipVerifyForCacheHit(opts.SkipVerify, cachedBaseKeysBefore, baseKey, opts.Image)
			if !opts.SkipVerify && !skipVerifyForCacheHit {
				if verifyErr := m.verifyPreparedBaseImage(ctx, opts.Image, baseImagePath, baseKey); verifyErr != nil {
					return verifyErr
				}
			}
			if idxErr := refcache.Upsert(m.cfg, opts.Image, baseKey, identity.FullDigest); idxErr != nil {
				log.Printf("warning: update manifest cache for %q: %v", opts.Image, idxErr)
			}
			return nil
		},
		setupStorage: func(ctx context.Context, vmID, diskSize string) error {
			if _, err := m.cowMgr.CreateOverlay(ctx, baseKey, vmID, diskSize); err != nil {
				return fmt.Errorf("create overlay for %s: %w", vmID, err)
			}
			return nil
		},
		cleanupStorage: func(vmID string) {
			_ = m.cowMgr.RemoveOverlay(vmID)
		},
	}, nil
}

// prepareOCIVMImage prepares an OCI VM image for direct kernel boot.
// Returns an imagePreparation with OCI-specific hooks.
func (m *manager) prepareOCIVMImage(
	ctx context.Context,
	_ *vm.CreateOptions,
	resolvedImage *resolvedRuntimeImage,
) (*imagePreparation, error) {
	if err := m.ensureOCIRuntimePreflight(ctx); err != nil {
		return nil, fmt.Errorf("OCI runtime preflight: %w", err)
	}

	localTag, err := m.resolveOCIRuntimeLocalTag(ctx, resolvedImage)
	if err != nil {
		return nil, err
	}

	runtimeSpec, err := prepareLocalOCIRuntime(ctx, m.cfg, localTag)
	if err != nil {
		return nil, err
	}

	baseDigest := strings.TrimPrefix(runtimeSpec.ManifestDigest, "sha256:")

	return &imagePreparation{
		BaseKey:        runtimeSpec.RuntimeKey,
		BaseDigestFull: baseDigest,
		Arch:           runtimeSpec.Arch,
		BaseImagePath:  m.cfg.OCIRuntimeEntryDir(runtimeSpec.RuntimeKey),
		KernelPath:     runtimeSpec.KernelPath,
		InitramfsPath:  runtimeSpec.InitramfsPath,
		Cmdline:        runtimeSpec.Cmdline,
		VirtioFSTag:    runtimeSpec.VirtioFSTag,

		pinRef: func(vmID string) error {
			return oci.AddRuntimeRef(m.cfg, runtimeSpec.RuntimeKey, vmID)
		},
		unpinRef: func(vmID string) {
			if unpinErr := oci.RemoveRuntimeRef(m.cfg, runtimeSpec.RuntimeKey, vmID); unpinErr != nil {
				log.Printf("warning: rollback OCI runtime pin for %s (%s): %v", vmID, runtimeSpec.RuntimeKey, unpinErr)
			}
		},
		postPin: func(_ context.Context) error {
			return nil // OCI VM images have no post-pin verification.
		},
		setupStorage: func(_ context.Context, vmID, _ string) error {
			if err := m.overlayMgr.EnsureWorkspace(vmID); err != nil {
				return fmt.Errorf("create OCI runtime workspace for %s: %w", vmID, err)
			}
			return nil
		},
		cleanupStorage: func(_ string) {
			// Workspace is inside vmDir which gets cleaned up by the vmDir defer.
		},
	}, nil
}
