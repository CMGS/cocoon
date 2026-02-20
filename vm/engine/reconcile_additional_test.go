package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/CMGS/cocoon/config"
	hypermocks "github.com/CMGS/cocoon/hypervisor/mocks"
	imgmocks "github.com/CMGS/cocoon/image/mocks"
	"github.com/CMGS/cocoon/oci"
	"github.com/CMGS/cocoon/storage/local"
	storemocks "github.com/CMGS/cocoon/storage/mocks"
	"github.com/CMGS/cocoon/types"
	"github.com/CMGS/cocoon/utils"
	"github.com/CMGS/cocoon/vm"
)

func setupReconcileManager(t *testing.T) (*manager, *config.CocoonConfig) {
	t.Helper()

	rootDir := t.TempDir()
	runtimeDir := t.TempDir()
	logDir := t.TempDir()

	cfg := config.DefaultConfig()
	cfg.RebaseRootDir(rootDir)
	cfg.RuntimeDir = runtimeDir
	cfg.LogDir = logDir

	if err := cfg.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}

	hyper := &hypermocks.MockClient{}
	refCounter := local.NewReferenceCounter(cfg)
	cowMgr := &storemocks.MockCOWManager{}
	imgMgr := &imgmocks.MockManager{}

	mgr, ok := New(cfg, hyper, refCounter, cowMgr, imgMgr).(*manager)
	if !ok {
		t.Fatal("type assertion to *manager failed")
	}

	return mgr, cfg
}

func writeTestVMArtifacts(t *testing.T, cfg *config.CocoonConfig, vmID, name, baseKey string) {
	t.Helper()

	if err := os.MkdirAll(cfg.VMPersistDir(vmID), 0o755); err != nil { //nolint:gosec // test fixture directory
		t.Fatalf("mkdir vm dir: %v", err)
	}
	if err := os.MkdirAll(cfg.VMRuntimeDir(vmID), 0o755); err != nil { //nolint:gosec // test fixture directory
		t.Fatalf("mkdir vm runtime dir: %v", err)
	}
	if err := os.WriteFile(cfg.VMOverlayPath(vmID), []byte("overlay"), 0o644); err != nil { //nolint:gosec // test fixture file
		t.Fatalf("write overlay: %v", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	vmCfg := &types.VMConfig{
		VMID:           vmID,
		Name:           name,
		ImageRef:       "docker.io/library/ubuntu:22.04",
		BaseKey:        baseKey,
		BaseDigestFull: "a1b2c3d4e5f6a7b8a1b2c3d4e5f6a7b8a1b2c3d4e5f6a7b8a1b2c3d4e5f6a7b8",
		Arch:           "amd64",
		BootStrategy:   types.DefaultBootStrategy,
		FirmwarePath:   cfg.UEFIFirmwarePath,
		CPUs:           2,
		MemoryMB:       2048,
		DiskSize:       "10G",
		BaseImagePath:  cfg.BaseImagePath(baseKey),
		OverlayPath:    cfg.VMOverlayPath(vmID),
		SerialLog:      cfg.VMSerialLogPath(vmID),
		SocketPath:     cfg.VMSocketPath(vmID),
		CreatedAt:      now,
		SchemaVersion:  types.CurrentConfigSchemaVersion,
	}
	if err := utils.AtomicWriteJSON(cfg.VMConfigPath(vmID), vmCfg); err != nil {
		t.Fatalf("write config.json: %v", err)
	}

	meta := &types.VMMetadataFile{
		VMID:          vmID,
		State:         string(types.VMStateCreated),
		PreviousState: string(types.VMStateCreating),
		UpdatedAt:     now,
		SchemaVersion: types.CurrentMetadataSchemaVersion,
	}
	if err := utils.AtomicWriteJSON(cfg.VMMetadataPath(vmID), meta); err != nil {
		t.Fatalf("write metadata.json: %v", err)
	}
}

func writeTestOCIVMArtifacts(t *testing.T, cfg *config.CocoonConfig, vmID, name string, state types.VMState) {
	t.Helper()

	if err := os.MkdirAll(cfg.VMPersistDir(vmID), 0o755); err != nil { //nolint:gosec // test fixture directory
		t.Fatalf("mkdir vm dir: %v", err)
	}
	if err := os.MkdirAll(cfg.VMRuntimeDir(vmID), 0o755); err != nil { //nolint:gosec // test fixture directory
		t.Fatalf("mkdir vm runtime dir: %v", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	baseKey := "oci-runtime-key"

	// Create per-layer cache directories and entry metadata.
	const (
		kernelDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		rootfsDigest = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	kernelLayerDir := filepath.Join(cfg.OCIRuntimeLayerDir(kernelDigest), "kernel")
	rootfsLayerDir := filepath.Join(cfg.OCIRuntimeLayerDir(rootfsDigest), "rootfs")
	if err := os.MkdirAll(kernelLayerDir, 0o755); err != nil { //nolint:gosec // test fixture
		t.Fatalf("mkdir kernel layer dir: %v", err)
	}
	if err := os.MkdirAll(rootfsLayerDir, 0o755); err != nil { //nolint:gosec // test fixture
		t.Fatalf("mkdir rootfs layer dir: %v", err)
	}

	entryDir := cfg.OCIRuntimeEntryDir(baseKey)
	if err := os.MkdirAll(entryDir, 0o755); err != nil { //nolint:gosec // test fixture
		t.Fatalf("mkdir entry dir: %v", err)
	}
	entryMeta := &oci.OCIRuntimeEntryMeta{
		ManifestDigest:     "sha256:00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff",
		KernelLayerDigest:  kernelDigest,
		RootfsLayerDigests: []string{rootfsDigest},
		Arch:               "amd64",
		VirtioFSTag:        "cocoon-rootfs",
	}
	if err := oci.WriteEntryMeta(cfg.OCIRuntimeEntryMetaPath(baseKey), entryMeta); err != nil {
		t.Fatalf("write entry meta: %v", err)
	}

	vmCfg := &types.VMConfig{
		VMID:           vmID,
		Name:           name,
		ImageRef:       "local-oci:latest",
		BaseKey:        baseKey,
		BaseDigestFull: "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff",
		ImageType:      types.VMImageTypeOCIVM,
		Arch:           "amd64",
		BootStrategy:   types.BootStrategyDirect,
		BaseImagePath:  entryDir,
		KernelPath:     filepath.Join(kernelLayerDir, "vmlinuz"),
		InitramfsPath:  filepath.Join(kernelLayerDir, "initrd.img"),
		Cmdline:        "root=cocoon-rootfs rootfstype=virtiofs rw",
		VirtioFSTag:    "cocoon-rootfs",
		VirtioFSSock:   cfg.VMOCIRootfsVirtioFSSocketPath(vmID),
		SocketPath:     cfg.VMSocketPath(vmID),
		SerialLog:      cfg.VMSerialLogPath(vmID),
		CreatedAt:      now,
		SchemaVersion:  types.CurrentConfigSchemaVersion,
	}
	if err := utils.AtomicWriteJSON(cfg.VMConfigPath(vmID), vmCfg); err != nil {
		t.Fatalf("write config.json: %v", err)
	}

	meta := &types.VMMetadataFile{
		VMID:          vmID,
		State:         string(state),
		PreviousState: string(types.VMStateCreating),
		UpdatedAt:     now,
		SchemaVersion: types.CurrentMetadataSchemaVersion,
	}
	if err := utils.AtomicWriteJSON(cfg.VMMetadataPath(vmID), meta); err != nil {
		t.Fatalf("write metadata.json: %v", err)
	}
}

func hasIssue(issues []vm.Inconsistency, incType vm.InconsistencyType) bool {
	for _, issue := range issues {
		if issue.Type == incType {
			return true
		}
	}
	return false
}

func TestReconcile_FixMissingReference(t *testing.T) {
	t.Parallel()

	mgr, cfg := setupReconcileManager(t)
	baseKey := "a1b2c3d4e5f6a7b8_amd64"
	vmID := "vm-01HABC9D8E7F6G5H4J3K2M1N0P"
	writeTestVMArtifacts(t, cfg, vmID, "missing-ref", baseKey)

	if err := utils.AtomicWriteJSON(cfg.ReferencesFile(), types.ReferencesFile{}); err != nil {
		t.Fatalf("write empty references.json: %v", err)
	}

	issues, err := mgr.Reconcile(context.Background(), false, false)
	if err != nil {
		t.Fatalf("Reconcile dry-run: %v", err)
	}
	if !hasIssue(issues, vm.InconsistencyMissingReference) {
		t.Fatalf("expected %s issue in dry-run", vm.InconsistencyMissingReference)
	}

	issues, err = mgr.Reconcile(context.Background(), true, false)
	if err != nil {
		t.Fatalf("Reconcile --fix: %v", err)
	}
	if !hasIssue(issues, vm.InconsistencyMissingReference) {
		t.Fatalf("expected %s issue in fix run", vm.InconsistencyMissingReference)
	}

	refs, err := mgr.refCounter.GetReferences(baseKey)
	if err != nil {
		t.Fatalf("GetReferences: %v", err)
	}
	found := false
	for _, ref := range refs {
		if ref == vmID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected vmID %s to be restored into references.json", vmID)
	}
}

func TestReconcile_FixDanglingReference(t *testing.T) {
	t.Parallel()

	mgr, cfg := setupReconcileManager(t)
	baseKey := "b1b2c3d4e5f6a7b8_amd64"
	vmID := "vm-01HABC9D8E7F6G5H4J3K2M1N0Q"
	writeTestVMArtifacts(t, cfg, vmID, "dangling-ref", baseKey)

	refs := types.ReferencesFile{
		baseKey: {
			Path:       cfg.BaseImagePath(baseKey),
			DigestFull: "b1b2c3d4e5f6a7b8b1b2c3d4e5f6a7b8b1b2c3d4e5f6a7b8b1b2c3d4e5f6a7b8",
			SourceRef:  "docker.io/library/ubuntu:22.04",
			Refs:       []string{vmID, "vm-DOES-NOT-EXIST"},
			CreatedAt:  time.Now().UTC().Format(time.RFC3339),
		},
	}
	if err := utils.AtomicWriteJSON(cfg.ReferencesFile(), refs); err != nil {
		t.Fatalf("write references.json: %v", err)
	}

	issues, err := mgr.Reconcile(context.Background(), true, false)
	if err != nil {
		t.Fatalf("Reconcile --fix: %v", err)
	}
	if !hasIssue(issues, vm.InconsistencyDanglingReference) {
		t.Fatalf("expected %s issue", vm.InconsistencyDanglingReference)
	}

	updatedRefs := make(types.ReferencesFile)
	if err := utils.ReadJSON(cfg.ReferencesFile(), &updatedRefs); err != nil {
		t.Fatalf("read references.json: %v", err)
	}
	entry := updatedRefs[baseKey]
	if entry == nil {
		t.Fatalf("expected base key %s to remain present", baseKey)
	}
	for _, ref := range entry.Refs {
		if ref == "vm-DOES-NOT-EXIST" {
			t.Fatalf("expected dangling vmID to be removed from references")
		}
	}
}

func TestReconcile_OrphanOverlayFixedByDoctorFix(t *testing.T) {
	t.Parallel()

	mgr, cfg := setupReconcileManager(t)
	vmID := "vm-01HABC9D8E7F6G5H4J3K2M1N0R"

	if err := os.MkdirAll(cfg.VMPersistDir(vmID), 0o755); err != nil { //nolint:gosec // test fixture directory
		t.Fatalf("mkdir vm dir: %v", err)
	}
	overlayPath := cfg.VMOverlayPath(vmID)
	if err := os.WriteFile(overlayPath, []byte("overlay"), 0o644); err != nil { //nolint:gosec // test fixture file
		t.Fatalf("write overlay: %v", err)
	}

	issues, err := mgr.Reconcile(context.Background(), true, false)
	if err != nil {
		t.Fatalf("Reconcile --fix: %v", err)
	}
	if !hasIssue(issues, vm.InconsistencyOrphanedOverlay) {
		t.Fatalf("expected %s issue", vm.InconsistencyOrphanedOverlay)
	}

	if _, err := os.Stat(cfg.VMPersistDir(vmID)); !os.IsNotExist(err) {
		t.Fatalf("expected orphan VM dir to be removed after --fix")
	}
	if _, err := os.Stat(overlayPath); !os.IsNotExist(err) {
		t.Fatalf("expected orphan overlay to be removed from original location")
	}
}

func TestReconcile_NameIndexDryRunDoesNotMutate(t *testing.T) {
	t.Parallel()

	mgr, cfg := setupReconcileManager(t)
	baseKey := "c1b2c3d4e5f6a7b8_amd64"
	vmID := "vm-01HABC9D8E7F6G5H4J3K2M1N0S"
	writeTestVMArtifacts(t, cfg, vmID, "name-index-target", baseKey)

	if err := utils.AtomicWriteJSON(cfg.ReferencesFile(), types.ReferencesFile{}); err != nil {
		t.Fatalf("write references.json: %v", err)
	}
	if err := utils.AtomicWriteJSON(cfg.NameIndexFile(), vm.NameIndex{"stale-name": "vm-stale"}); err != nil {
		t.Fatalf("write stale name-index.json: %v", err)
	}

	issues, err := mgr.Reconcile(context.Background(), false, false)
	if err != nil {
		t.Fatalf("Reconcile dry-run: %v", err)
	}
	if !hasIssue(issues, vm.InconsistencyNameIndexStale) {
		t.Fatalf("expected %s issue", vm.InconsistencyNameIndexStale)
	}

	index, err := LoadNameIndex(cfg)
	if err != nil {
		t.Fatalf("LoadNameIndex: %v", err)
	}
	if _, ok := index["stale-name"]; !ok {
		t.Fatalf("expected dry-run reconcile to keep existing stale index unchanged")
	}

	if _, err := mgr.Reconcile(context.Background(), true, false); err != nil {
		t.Fatalf("Reconcile --fix: %v", err)
	}
	fixedIndex, err := LoadNameIndex(cfg)
	if err != nil {
		t.Fatalf("LoadNameIndex fixed: %v", err)
	}
	if fixedIndex["name-index-target"] != vmID {
		t.Fatalf("expected name-index rebuilt with %q -> %q", "name-index-target", vmID)
	}
}

func TestBuildOrphanScanProcessNames(t *testing.T) {
	t.Parallel()

	got := buildOrphanScanProcessNames([]string{
		" cloud-hypervisor ",
		"",
		"my-hypervisor",
		"cloud-hypervisor",
		"swtpm",
	})

	seen := make(map[string]struct{}, len(got))
	for _, name := range got {
		seen[name] = struct{}{}
	}

	for _, want := range []string{"cloud-hypervisor", "my-hypervisor", "swtpm"} {
		if _, ok := seen[want]; !ok {
			t.Fatalf("process name %q missing from %v", want, got)
		}
	}
	if len(seen) != 3 {
		t.Fatalf("expected unique process names, got %v", got)
	}
}

func TestReconcileStateTimeouts_FromConfig(t *testing.T) {
	t.Parallel()

	mgr, _ := setupReconcileManager(t)
	mgr.cfg.BootTimeoutSeconds = 77
	mgr.cfg.StopTimeoutSeconds = 23

	if got := mgr.startingStateTimeout(); got != 77*time.Second {
		t.Fatalf("startingStateTimeout=%s, want %s", got, 77*time.Second)
	}
	if got := mgr.stoppingStateTimeout(); got != 23*time.Second {
		t.Fatalf("stoppingStateTimeout=%s, want %s", got, 23*time.Second)
	}
}

func TestReconcile_DetectsOCIRuntimeMismatch(t *testing.T) {
	t.Parallel()

	mgr, cfg := setupReconcileManager(t)
	vmID := "vm-01HABC9D8E7F6G5H4J3K2M1N0T"
	writeTestOCIVMArtifacts(t, cfg, vmID, "oci-runtime-mismatch", types.VMStateRunning)

	if err := mgr.updateMetadata(vmID, func(md *types.VMMetadataFile) {
		md.VirtiofsdPID = 0
		md.OCIOverlayMounted = false
	}); err != nil {
		t.Fatalf("update metadata: %v", err)
	}

	issues, err := mgr.Reconcile(context.Background(), false, false)
	if err != nil {
		t.Fatalf("Reconcile dry-run: %v", err)
	}
	if !hasIssue(issues, vm.InconsistencyOCIRuntimeOverlay) {
		t.Fatalf("expected %s issue", vm.InconsistencyOCIRuntimeOverlay)
	}
	if !hasIssue(issues, vm.InconsistencyOCIRuntimeVirtio) {
		t.Fatalf("expected %s issue", vm.InconsistencyOCIRuntimeVirtio)
	}
}

func TestReconcile_FixStoppedOCIRuntimeLeak(t *testing.T) {
	t.Parallel()

	mgr, cfg := setupReconcileManager(t)
	vmID := "vm-01HABC9D8E7F6G5H4J3K2M1N0U"
	writeTestOCIVMArtifacts(t, cfg, vmID, "oci-runtime-leak", types.VMStateStopped)

	socketPath := cfg.VMOCIRootfsVirtioFSSocketPath(vmID)
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o755); err != nil { //nolint:gosec // test fixture directory
		t.Fatalf("mkdir runtime dir: %v", err)
	}
	if err := os.WriteFile(socketPath, []byte(""), 0o644); err != nil { //nolint:gosec // test fixture file
		t.Fatalf("write socket fixture: %v", err)
	}

	if err := mgr.updateMetadata(vmID, func(md *types.VMMetadataFile) {
		md.VirtiofsdPID = 12345
		md.VirtiofsdBinary = "virtiofsd"
		md.VirtiofsdSocket = socketPath
		md.OCIOverlayMounted = true
	}); err != nil {
		t.Fatalf("update metadata: %v", err)
	}

	issues, err := mgr.Reconcile(context.Background(), true, false)
	if err != nil {
		t.Fatalf("Reconcile --fix: %v", err)
	}
	if !hasIssue(issues, vm.InconsistencyOCIRuntimeOverlay) {
		t.Fatalf("expected %s issue", vm.InconsistencyOCIRuntimeOverlay)
	}
	if !hasIssue(issues, vm.InconsistencyOCIRuntimeVirtio) {
		t.Fatalf("expected %s issue", vm.InconsistencyOCIRuntimeVirtio)
	}

	meta, err := mgr.LoadMetadata(vmID)
	if err != nil {
		t.Fatalf("LoadMetadata: %v", err)
	}
	if meta.VirtiofsdPID != 0 {
		t.Fatalf("virtiofsd_pid=%d, want 0", meta.VirtiofsdPID)
	}
	if meta.VirtiofsdSocket != "" {
		t.Fatalf("virtiofsd_socket=%q, want empty", meta.VirtiofsdSocket)
	}
	if meta.OCIOverlayMounted {
		t.Fatal("oci_overlay_mounted=true, want false")
	}

	if _, err := os.Stat(socketPath); !os.IsNotExist(err) {
		t.Fatalf("expected virtiofs socket removed, stat err=%v", err)
	}
}

func TestReconcile_FixStateMismatchAlsoCleansOCIRuntimeLeak(t *testing.T) {
	t.Parallel()

	mgr, cfg := setupReconcileManager(t)
	vmID := "vm-01HABC9D8E7F6G5H4J3K2M1N0X"
	writeTestOCIVMArtifacts(t, cfg, vmID, "oci-runtime-state-mismatch", types.VMStateStopping)

	socketPath := cfg.VMOCIRootfsVirtioFSSocketPath(vmID)
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o755); err != nil { //nolint:gosec // test fixture directory
		t.Fatalf("mkdir runtime dir: %v", err)
	}
	if err := os.WriteFile(socketPath, []byte(""), 0o644); err != nil { //nolint:gosec // test fixture file
		t.Fatalf("write socket fixture: %v", err)
	}

	if err := mgr.updateMetadata(vmID, func(md *types.VMMetadataFile) {
		md.ProcessPID = 0
		md.VirtiofsdPID = 12345
		md.VirtiofsdBinary = "virtiofsd"
		md.VirtiofsdSocket = socketPath
		md.OCIOverlayMounted = true
	}); err != nil {
		t.Fatalf("update metadata: %v", err)
	}

	issues, err := mgr.Reconcile(context.Background(), true, false)
	if err != nil {
		t.Fatalf("Reconcile --fix: %v", err)
	}
	if !hasIssue(issues, vm.InconsistencyStateMismatch) {
		t.Fatalf("expected %s issue", vm.InconsistencyStateMismatch)
	}

	meta, err := mgr.LoadMetadata(vmID)
	if err != nil {
		t.Fatalf("LoadMetadata: %v", err)
	}
	if meta.State != string(types.VMStateStopped) {
		t.Fatalf("state=%q, want %q", meta.State, types.VMStateStopped)
	}
	if meta.VirtiofsdPID != 0 {
		t.Fatalf("virtiofsd_pid=%d, want 0", meta.VirtiofsdPID)
	}
	if meta.VirtiofsdSocket != "" {
		t.Fatalf("virtiofsd_socket=%q, want empty", meta.VirtiofsdSocket)
	}
	if meta.OCIOverlayMounted {
		t.Fatal("oci_overlay_mounted=true, want false")
	}
	if _, err := os.Stat(socketPath); !os.IsNotExist(err) {
		t.Fatalf("expected virtiofs socket removed, stat err=%v", err)
	}
}

func TestReconcile_FixMissingOCIRuntimePin(t *testing.T) {
	t.Parallel()

	mgr, cfg := setupReconcileManager(t)
	vmID := "vm-01HABC9D8E7F6G5H4J3K2M1N0V"
	writeTestOCIVMArtifacts(t, cfg, vmID, "oci-runtime-pin-missing", types.VMStateCreated)

	issues, err := mgr.Reconcile(context.Background(), true, false)
	if err != nil {
		t.Fatalf("Reconcile --fix: %v", err)
	}
	if !hasIssue(issues, vm.InconsistencyMissingOCIRuntimePin) {
		t.Fatalf("expected %s issue", vm.InconsistencyMissingOCIRuntimePin)
	}

	refs, err := oci.GetRuntimeRefs(cfg, "oci-runtime-key")
	if err != nil {
		t.Fatalf("GetRuntimeRefs: %v", err)
	}
	found := false
	for _, ref := range refs {
		if ref == vmID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected vmID %s to be restored in oci-runtime-refs.json", vmID)
	}
}

func TestReconcile_FixDanglingRuntimePinAndOrphanRuntimeCache(t *testing.T) {
	t.Parallel()

	mgr, cfg := setupReconcileManager(t)
	runtimeKey := "deadbeefcafefeed"

	// Runtime cache exists but pin points to a missing VM.
	runtimeDir := cfg.OCIRuntimeEntryDir(runtimeKey)
	if err := os.MkdirAll(filepath.Join(runtimeDir, "rootfs"), 0o755); err != nil { //nolint:gosec // test fixture directory
		t.Fatalf("mkdir runtime cache: %v", err)
	}
	old := time.Now().Add(-10 * time.Minute)
	_ = os.Chtimes(runtimeDir, old, old)

	if err := oci.AddRuntimeRef(cfg, runtimeKey, "vm-DOES-NOT-EXIST"); err != nil {
		t.Fatalf("AddRuntimeRef: %v", err)
	}

	issues, err := mgr.Reconcile(context.Background(), true, false)
	if err != nil {
		t.Fatalf("Reconcile --fix: %v", err)
	}
	if !hasIssue(issues, vm.InconsistencyDanglingOCIRuntimePin) {
		t.Fatalf("expected %s issue", vm.InconsistencyDanglingOCIRuntimePin)
	}
	if !hasIssue(issues, vm.InconsistencyOrphanedOCIRuntime) {
		t.Fatalf("expected %s issue", vm.InconsistencyOrphanedOCIRuntime)
	}

	refs, err := oci.GetRuntimeRefs(cfg, runtimeKey)
	if err != nil {
		t.Fatalf("GetRuntimeRefs: %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("expected dangling runtime refs to be removed, got %v", refs)
	}
	if _, err := os.Stat(runtimeDir); !os.IsNotExist(err) {
		t.Fatalf("expected orphan runtime cache dir removed, stat err=%v", err)
	}
}
