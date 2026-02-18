package engine

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/CMGS/cocoon/config"
	hypermocks "github.com/CMGS/cocoon/hypervisor/mocks"
	imgmocks "github.com/CMGS/cocoon/image/mocks"
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
