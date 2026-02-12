package vm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/CMGS/cocoon/config"
	hypermocks "github.com/CMGS/cocoon/hypervisor/mocks"
	"github.com/CMGS/cocoon/image"
	imgmocks "github.com/CMGS/cocoon/image/mocks"
	storemocks "github.com/CMGS/cocoon/storage/mocks"
	"github.com/CMGS/cocoon/types"
	"github.com/CMGS/cocoon/utils"
)

// ---------------------------------------------------------------------------
// Test helper: setupTestManager
// ---------------------------------------------------------------------------

type testDeps struct {
	mgr        Manager
	cfg        *config.CocoonConfig
	hyper      *hypermocks.MockClient
	refCounter *storemocks.MockReferenceCounter
	cowMgr     *storemocks.MockCOWManager
	imgMgr     *imgmocks.MockManager
}

func setupTestManager(t *testing.T) *testDeps {
	t.Helper()

	rootDir := t.TempDir()
	runtimeDir := t.TempDir()
	logDir := t.TempDir()

	cfg := config.DefaultConfig()
	// RebaseRootDir updates RootDir and re-derives BuildahRoot, firmware paths, etc.
	cfg.RebaseRootDir(rootDir)
	cfg.RuntimeDir = runtimeDir
	cfg.LogDir = logDir
	cfg.PVHFirmwarePath = filepath.Join(rootDir, "firmware", "hypervisor-fw")
	cfg.UEFIFirmwarePath = filepath.Join(rootDir, "firmware", "CLOUDHV.fd")

	if err := cfg.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}

	// Create dummy firmware files so resolveFirmwarePath won't fail.
	for _, p := range []string{cfg.PVHFirmwarePath, cfg.UEFIFirmwarePath} {
		if err := os.WriteFile(p, []byte("firmware"), 0o644); err != nil {
			t.Fatalf("write firmware: %v", err)
		}
	}

	hyper := &hypermocks.MockClient{}
	refCounter := &storemocks.MockReferenceCounter{}
	cowMgr := &storemocks.MockCOWManager{}
	imgMgr := &imgmocks.MockManager{}

	mgr := NewManager(cfg, hyper, refCounter, cowMgr, imgMgr)

	return &testDeps{
		mgr:        mgr,
		cfg:        cfg,
		hyper:      hyper,
		refCounter: refCounter,
		cowMgr:     cowMgr,
		imgMgr:     imgMgr,
	}
}

// defaultMockIdentity returns a standard ImageIdentity for tests.
func defaultMockIdentity() *image.ImageIdentity {
	return &image.ImageIdentity{
		Checksum:   "a1b2c3d4e5f6a7b8",
		Arch:       "amd64",
		FullDigest: "a1b2c3d4e5f6a7b8a1b2c3d4e5f6a7b8a1b2c3d4e5f6a7b8a1b2c3d4e5f6a7b8",
		SourceRef:  "docker.io/library/ubuntu:22.04",
		ImageType:  image.ImageTypeOCI,
	}
}

// createTestVM is a convenience helper that provisions a VM through Create
// using standard mock wiring and returns the VMConfig.
func createTestVM(t *testing.T, td *testDeps, opts *CreateOptions) *types.VMConfig {
	t.Helper()
	identity := defaultMockIdentity()

	td.imgMgr.PrepareFunc = func(_ context.Context, ref string) (*image.ImageIdentity, string, error) {
		return identity, filepath.Join(td.cfg.ImageCacheDir(), identity.BaseKey()+".qcow2"), nil
	}
	td.imgMgr.VerifyBootabilityFunc = func(_ context.Context, _ string) (*image.BootCheckResult, error) {
		return &image.BootCheckResult{Bootable: true}, nil
	}
	td.cowMgr.CreateOverlayFunc = func(baseKey, vmID, diskSize string) (string, error) {
		return filepath.Join(td.cfg.RootDir, "vms", vmID, "overlay.qcow2"), nil
	}

	vmCfg, err := td.mgr.Create(t.Context(), opts)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return vmCfg
}

// ---------------------------------------------------------------------------
// Test: Create happy path
// ---------------------------------------------------------------------------

func TestCreate_HappyPath(t *testing.T) {
	t.Parallel()
	td := setupTestManager(t)

	opts := &CreateOptions{
		Image: "docker.io/library/ubuntu:22.04",
		Name:  "test-vm",
		CPUs:  4,
	}
	vmCfg := createTestVM(t, td, opts)

	// Verify returned config fields.
	if vmCfg.Name != "test-vm" {
		t.Errorf("Name = %q, want %q", vmCfg.Name, "test-vm")
	}
	if vmCfg.CPUs != 4 {
		t.Errorf("CPUs = %d, want 4", vmCfg.CPUs)
	}
	if vmCfg.MemoryMB != td.cfg.DefaultMemoryMB {
		t.Errorf("MemoryMB = %d, want %d", vmCfg.MemoryMB, td.cfg.DefaultMemoryMB)
	}
	if vmCfg.ImageRef != "docker.io/library/ubuntu:22.04" {
		t.Errorf("ImageRef = %q, want %q", vmCfg.ImageRef, "docker.io/library/ubuntu:22.04")
	}
	if vmCfg.VMID == "" {
		t.Error("expected non-empty vm_id")
	}
	if vmCfg.BaseKey != "a1b2c3d4e5f6a7b8_amd64" {
		t.Errorf("BaseKey = %q, want %q", vmCfg.BaseKey, "a1b2c3d4e5f6a7b8_amd64")
	}
	if vmCfg.Arch != "amd64" {
		t.Errorf("Arch = %q, want %q", vmCfg.Arch, "amd64")
	}

	// Verify config.json written to disk.
	configPath := td.cfg.VMConfigPath(vmCfg.VMID)
	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("config.json not found: %v", err)
	}

	// Verify metadata.json written with CREATED state.
	metaPath := td.cfg.VMMetadataPath(vmCfg.VMID)
	var meta types.VMMetadataFile
	if err := utils.ReadJSON(metaPath, &meta); err != nil {
		t.Fatalf("read metadata.json: %v", err)
	}
	if meta.State != string(types.VMStateCreated) {
		t.Errorf("metadata state = %q, want %q", meta.State, types.VMStateCreated)
	}
	if meta.PreviousState != string(types.VMStateCreating) {
		t.Errorf("metadata previous_state = %q, want %q", meta.PreviousState, types.VMStateCreating)
	}
}

// ---------------------------------------------------------------------------
// Test: Create with non-bootable image
// ---------------------------------------------------------------------------

func TestCreate_NonBootableImage(t *testing.T) {
	t.Parallel()
	td := setupTestManager(t)

	identity := defaultMockIdentity()
	td.imgMgr.PrepareFunc = func(_ context.Context, _ string) (*image.ImageIdentity, string, error) {
		return identity, "/cache/test.qcow2", nil
	}
	td.imgMgr.VerifyBootabilityFunc = func(_ context.Context, _ string) (*image.BootCheckResult, error) {
		return &image.BootCheckResult{
			Bootable: false,
			Errors:   []string{"missing kernel"},
		}, nil
	}

	_, err := td.mgr.Create(t.Context(), &CreateOptions{
		Image: "docker.io/library/ubuntu:22.04",
	})
	if err == nil {
		t.Fatal("expected error for non-bootable image, got nil")
	}
	if !errors.Is(err, types.ErrImageNotBootable) {
		t.Errorf("expected error wrapping ErrImageNotBootable, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Test: Create with SkipVerify
// ---------------------------------------------------------------------------

func TestCreate_SkipVerify(t *testing.T) {
	t.Parallel()
	td := setupTestManager(t)

	identity := defaultMockIdentity()
	td.imgMgr.PrepareFunc = func(_ context.Context, _ string) (*image.ImageIdentity, string, error) {
		return identity, filepath.Join(td.cfg.ImageCacheDir(), identity.BaseKey()+".qcow2"), nil
	}
	// VerifyBootabilityFunc should NOT be called.
	verifyCallCount := 0
	td.imgMgr.VerifyBootabilityFunc = func(_ context.Context, _ string) (*image.BootCheckResult, error) {
		verifyCallCount++
		return &image.BootCheckResult{Bootable: false, Errors: []string{"boom"}}, nil
	}
	td.cowMgr.CreateOverlayFunc = func(baseKey, vmID, diskSize string) (string, error) {
		return filepath.Join(td.cfg.RootDir, "vms", vmID, "overlay.qcow2"), nil
	}

	vmCfg, err := td.mgr.Create(t.Context(), &CreateOptions{
		Image:      "docker.io/library/ubuntu:22.04",
		SkipVerify: true,
	})
	if err != nil {
		t.Fatalf("expected no error with SkipVerify, got %v", err)
	}
	if vmCfg == nil {
		t.Fatal("expected non-nil VMConfig")
	}
	if verifyCallCount != 0 {
		t.Errorf("VerifyBootability called %d times, want 0", verifyCallCount)
	}
}

// ---------------------------------------------------------------------------
// Test: Delete happy path
// ---------------------------------------------------------------------------

func TestDelete_HappyPath(t *testing.T) {
	t.Parallel()
	td := setupTestManager(t)

	opts := &CreateOptions{
		Image: "docker.io/library/ubuntu:22.04",
		Name:  "delete-me",
	}
	vmCfg := createTestVM(t, td, opts)

	// Delete the VM.
	err := td.mgr.Delete(t.Context(), vmCfg.VMID, false)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Verify VM directory is gone.
	vmDir := td.cfg.VMPersistDir(vmCfg.VMID)
	if _, err := os.Stat(vmDir); !os.IsNotExist(err) {
		t.Errorf("expected VM directory to be removed, got stat err: %v", err)
	}

	// Verify name index no longer contains the name.
	index, err := LoadNameIndex(td.cfg)
	if err != nil {
		t.Fatalf("LoadNameIndex: %v", err)
	}
	if _, ok := index["delete-me"]; ok {
		t.Error("expected name 'delete-me' to be removed from index")
	}
}

// ---------------------------------------------------------------------------
// Test: Delete force on running VM (simulated)
// ---------------------------------------------------------------------------

func TestDelete_ForceOnRunningVM(t *testing.T) {
	t.Parallel()
	td := setupTestManager(t)

	opts := &CreateOptions{
		Image: "docker.io/library/ubuntu:22.04",
		Name:  "force-delete",
	}
	vmCfg := createTestVM(t, td, opts)

	// Manually set state to RUNNING (simulate a running VM):
	// CREATED -> STARTING -> RUNNING
	err := td.mgr.TransitionState(vmCfg.VMID, types.VMStateStarting, "test")
	if err != nil {
		t.Fatalf("transition to STARTING: %v", err)
	}
	err = td.mgr.TransitionState(vmCfg.VMID, types.VMStateRunning, "test")
	if err != nil {
		t.Fatalf("transition to RUNNING: %v", err)
	}

	// Mock hypervisor: IsAlive returns false after ForceKill.
	td.hyper.IsAliveFunc = func(_ string) bool { return false }
	td.hyper.ForceKillFunc = func(_ string) error { return nil }
	// ShutdownFunc uses the correct signature with time.Duration.
	// No explicit mock needed here; the default nil func returns nil.

	// Without force, should fail.
	err = td.mgr.Delete(t.Context(), vmCfg.VMID, false)
	if !errors.Is(err, types.ErrVMRunning) {
		t.Fatalf("expected ErrVMRunning without force, got %v", err)
	}

	// With force, should succeed.
	err = td.mgr.Delete(t.Context(), vmCfg.VMID, true)
	if err != nil {
		t.Fatalf("expected nil error with force=true, got %v", err)
	}

	// VM directory should be gone.
	vmDir := td.cfg.VMPersistDir(vmCfg.VMID)
	if _, err := os.Stat(vmDir); !os.IsNotExist(err) {
		t.Errorf("expected VM directory to be removed after force delete")
	}
}

// ---------------------------------------------------------------------------
// Test: Delete idempotent (non-existent VM)
// ---------------------------------------------------------------------------

func TestDelete_Idempotent(t *testing.T) {
	t.Parallel()
	td := setupTestManager(t)

	// Delete a VM that does not exist.
	err := td.mgr.Delete(t.Context(), "vm-NONEXISTENT000000000000000", false)
	if err != nil {
		t.Fatalf("expected nil for non-existent VM, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Test: Inspect
// ---------------------------------------------------------------------------

func TestInspect(t *testing.T) {
	t.Parallel()
	td := setupTestManager(t)

	opts := &CreateOptions{
		Image:    "docker.io/library/ubuntu:22.04",
		Name:     "inspect-me",
		CPUs:     2,
		MemoryMB: 4096,
	}
	vmCfg := createTestVM(t, td, opts)

	inspect, err := td.mgr.Inspect(t.Context(), vmCfg.VMID)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}

	if inspect.VMID != vmCfg.VMID {
		t.Errorf("VMID = %q, want %q", inspect.VMID, vmCfg.VMID)
	}
	if inspect.Name != "inspect-me" {
		t.Errorf("Name = %q, want %q", inspect.Name, "inspect-me")
	}
	if inspect.State != types.VMStateCreated {
		t.Errorf("State = %q, want %q", inspect.State, types.VMStateCreated)
	}
	if inspect.BootConfig.CPUs != 2 {
		t.Errorf("CPUs = %d, want 2", inspect.BootConfig.CPUs)
	}
	if inspect.BootConfig.MemoryMB != 4096 {
		t.Errorf("MemoryMB = %d, want 4096", inspect.BootConfig.MemoryMB)
	}
	if inspect.Image.Ref != "docker.io/library/ubuntu:22.04" {
		t.Errorf("Image.Ref = %q, want %q", inspect.Image.Ref, "docker.io/library/ubuntu:22.04")
	}
	if inspect.Image.BaseKey != "a1b2c3d4e5f6a7b8_amd64" {
		t.Errorf("Image.BaseKey = %q, want %q", inspect.Image.BaseKey, "a1b2c3d4e5f6a7b8_amd64")
	}
}

// ---------------------------------------------------------------------------
// Test: Inspect non-existent VM
// ---------------------------------------------------------------------------

func TestInspect_NotFound(t *testing.T) {
	t.Parallel()
	td := setupTestManager(t)

	_, err := td.mgr.Inspect(t.Context(), "vm-DOESNOTEXIST000000000000")
	if err == nil {
		t.Fatal("expected error for non-existent VM, got nil")
	}
	if !errors.Is(err, types.ErrVMNotFound) {
		t.Errorf("expected ErrVMNotFound, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Test: List
// ---------------------------------------------------------------------------

func TestList(t *testing.T) {
	t.Parallel()
	td := setupTestManager(t)

	// Create two VMs.
	vm1 := createTestVM(t, td, &CreateOptions{Image: "img1", Name: "list-vm-1"})
	vm2 := createTestVM(t, td, &CreateOptions{Image: "img2", Name: "list-vm-2"})

	results, err := td.mgr.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 VMs, got %d", len(results))
	}

	// Build a set of returned vm_ids.
	ids := make(map[string]bool)
	for _, r := range results {
		ids[r.VMID] = true
	}
	if !ids[vm1.VMID] {
		t.Errorf("vm1 (%s) not found in List results", vm1.VMID)
	}
	if !ids[vm2.VMID] {
		t.Errorf("vm2 (%s) not found in List results", vm2.VMID)
	}
}

// ---------------------------------------------------------------------------
// Test: List empty
// ---------------------------------------------------------------------------

func TestList_Empty(t *testing.T) {
	t.Parallel()
	td := setupTestManager(t)

	results, err := td.mgr.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 VMs, got %d", len(results))
	}
}

// ---------------------------------------------------------------------------
// Test: ResolveVMRef by name
// ---------------------------------------------------------------------------

func TestResolveVMRef_ByName(t *testing.T) {
	t.Parallel()
	td := setupTestManager(t)

	vmCfg := createTestVM(t, td, &CreateOptions{
		Image: "docker.io/library/ubuntu:22.04",
		Name:  "my-vm",
	})

	resolved, err := td.mgr.ResolveVMRef("my-vm")
	if err != nil {
		t.Fatalf("ResolveVMRef: %v", err)
	}
	if resolved != vmCfg.VMID {
		t.Errorf("resolved = %q, want %q", resolved, vmCfg.VMID)
	}
}

// ---------------------------------------------------------------------------
// Test: ResolveVMRef by vm_id
// ---------------------------------------------------------------------------

func TestResolveVMRef_ByVMID(t *testing.T) {
	t.Parallel()
	td := setupTestManager(t)

	vmCfg := createTestVM(t, td, &CreateOptions{
		Image: "docker.io/library/ubuntu:22.04",
		Name:  "resolve-id",
	})

	resolved, err := td.mgr.ResolveVMRef(vmCfg.VMID)
	if err != nil {
		t.Fatalf("ResolveVMRef: %v", err)
	}
	if resolved != vmCfg.VMID {
		t.Errorf("resolved = %q, want %q", resolved, vmCfg.VMID)
	}
}

// ---------------------------------------------------------------------------
// Test: ResolveVMRef not found
// ---------------------------------------------------------------------------

func TestResolveVMRef_NotFound(t *testing.T) {
	t.Parallel()
	td := setupTestManager(t)

	_, err := td.mgr.ResolveVMRef("nonexistent")
	if err == nil {
		t.Fatal("expected error for non-existent name, got nil")
	}
	if !errors.Is(err, types.ErrVMNotFound) {
		t.Errorf("expected ErrVMNotFound, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Test: TransitionState valid
// ---------------------------------------------------------------------------

func TestTransitionState_Valid(t *testing.T) {
	t.Parallel()
	td := setupTestManager(t)

	vmCfg := createTestVM(t, td, &CreateOptions{
		Image: "docker.io/library/ubuntu:22.04",
		Name:  "transition-ok",
	})

	// CREATED -> STARTING is valid.
	err := td.mgr.TransitionState(vmCfg.VMID, types.VMStateStarting, "test start")
	if err != nil {
		t.Fatalf("expected valid transition, got %v", err)
	}

	// Verify metadata reflects new state.
	meta, err := td.mgr.LoadMetadata(vmCfg.VMID)
	if err != nil {
		t.Fatalf("LoadMetadata: %v", err)
	}
	if meta.State != string(types.VMStateStarting) {
		t.Errorf("state = %q, want %q", meta.State, types.VMStateStarting)
	}
	if meta.PreviousState != string(types.VMStateCreated) {
		t.Errorf("previous_state = %q, want %q", meta.PreviousState, types.VMStateCreated)
	}
}

// ---------------------------------------------------------------------------
// Test: TransitionState invalid
// ---------------------------------------------------------------------------

func TestTransitionState_Invalid(t *testing.T) {
	t.Parallel()
	td := setupTestManager(t)

	vmCfg := createTestVM(t, td, &CreateOptions{
		Image: "docker.io/library/ubuntu:22.04",
		Name:  "transition-bad",
	})

	// CREATED -> RUNNING is invalid (must go through STARTING first).
	err := td.mgr.TransitionState(vmCfg.VMID, types.VMStateRunning, "invalid")
	if err == nil {
		t.Fatal("expected error for invalid transition, got nil")
	}
	if !errors.Is(err, types.ErrInvalidTransition) {
		t.Errorf("expected ErrInvalidTransition, got: %v", err)
	}

	// Verify state did not change.
	meta, err := td.mgr.LoadMetadata(vmCfg.VMID)
	if err != nil {
		t.Fatalf("LoadMetadata: %v", err)
	}
	if meta.State != string(types.VMStateCreated) {
		t.Errorf("state = %q, want %q (should be unchanged)", meta.State, types.VMStateCreated)
	}
}

// ---------------------------------------------------------------------------
// Test: TransitionState multiple steps
// ---------------------------------------------------------------------------

func TestTransitionState_FullLifecycle(t *testing.T) {
	t.Parallel()
	td := setupTestManager(t)

	vmCfg := createTestVM(t, td, &CreateOptions{
		Image: "docker.io/library/ubuntu:22.04",
		Name:  "lifecycle",
	})

	// Walk through: CREATED -> STARTING -> RUNNING -> STOPPING -> STOPPED -> DELETED
	transitions := []types.VMState{
		types.VMStateStarting,
		types.VMStateRunning,
		types.VMStateStopping,
		types.VMStateStopped,
		types.VMStateDeleted,
	}
	for _, to := range transitions {
		err := td.mgr.TransitionState(vmCfg.VMID, to, "lifecycle test")
		if err != nil {
			t.Fatalf("transition to %s: %v", to, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Test: Create with nil options
// ---------------------------------------------------------------------------

func TestCreate_NilOptions(t *testing.T) {
	t.Parallel()
	td := setupTestManager(t)

	_, err := td.mgr.Create(t.Context(), nil)
	if err == nil {
		t.Fatal("expected error for nil options, got nil")
	}
}

// ---------------------------------------------------------------------------
// Test: Create with empty image
// ---------------------------------------------------------------------------

func TestCreate_EmptyImage(t *testing.T) {
	t.Parallel()
	td := setupTestManager(t)

	_, err := td.mgr.Create(t.Context(), &CreateOptions{Image: ""})
	if err == nil {
		t.Fatal("expected error for empty image, got nil")
	}
}

// ---------------------------------------------------------------------------
// Test: Create generates name when not provided
// ---------------------------------------------------------------------------

func TestCreate_AutoGeneratedName(t *testing.T) {
	t.Parallel()
	td := setupTestManager(t)

	vmCfg := createTestVM(t, td, &CreateOptions{
		Image: "docker.io/library/ubuntu:22.04",
		// Name is intentionally omitted.
	})

	if vmCfg.Name == "" {
		t.Error("expected auto-generated name, got empty")
	}
	if len(vmCfg.Name) < 7 {
		t.Errorf("auto-generated name too short: %q", vmCfg.Name)
	}
}

// ---------------------------------------------------------------------------
// Test: LoadConfig / LoadMetadata for a created VM
// ---------------------------------------------------------------------------

func TestLoadConfig_HappyPath(t *testing.T) {
	t.Parallel()
	td := setupTestManager(t)

	vmCfg := createTestVM(t, td, &CreateOptions{
		Image: "docker.io/library/ubuntu:22.04",
		Name:  "load-config",
	})

	loaded, err := td.mgr.LoadConfig(vmCfg.VMID)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if loaded.VMID != vmCfg.VMID {
		t.Errorf("VMID = %q, want %q", loaded.VMID, vmCfg.VMID)
	}
	if loaded.Name != "load-config" {
		t.Errorf("Name = %q, want %q", loaded.Name, "load-config")
	}
}

func TestLoadConfig_NotFound(t *testing.T) {
	t.Parallel()
	td := setupTestManager(t)

	_, err := td.mgr.LoadConfig("vm-DOESNOTEXIST000000000000")
	if err == nil {
		t.Fatal("expected error for non-existent VM, got nil")
	}
	if !errors.Is(err, types.ErrVMNotFound) {
		t.Errorf("expected ErrVMNotFound, got: %v", err)
	}
}

func TestLoadMetadata_HappyPath(t *testing.T) {
	t.Parallel()
	td := setupTestManager(t)

	vmCfg := createTestVM(t, td, &CreateOptions{
		Image: "docker.io/library/ubuntu:22.04",
		Name:  "load-meta",
	})

	meta, err := td.mgr.LoadMetadata(vmCfg.VMID)
	if err != nil {
		t.Fatalf("LoadMetadata: %v", err)
	}
	if meta.VMID != vmCfg.VMID {
		t.Errorf("VMID = %q, want %q", meta.VMID, vmCfg.VMID)
	}
	if meta.State != string(types.VMStateCreated) {
		t.Errorf("State = %q, want %q", meta.State, types.VMStateCreated)
	}
}

// ---------------------------------------------------------------------------
// Test: SaveMetadata
// ---------------------------------------------------------------------------

func TestSaveMetadata(t *testing.T) {
	t.Parallel()
	td := setupTestManager(t)

	vmCfg := createTestVM(t, td, &CreateOptions{
		Image: "docker.io/library/ubuntu:22.04",
		Name:  "save-meta",
	})

	meta, err := td.mgr.LoadMetadata(vmCfg.VMID)
	if err != nil {
		t.Fatalf("LoadMetadata: %v", err)
	}

	// Modify and save.
	meta.LastError = "test error"
	meta.ErrorCount = 1
	if err := td.mgr.SaveMetadata(meta); err != nil {
		t.Fatalf("SaveMetadata: %v", err)
	}

	// Reload and verify.
	reloaded, err := td.mgr.LoadMetadata(vmCfg.VMID)
	if err != nil {
		t.Fatalf("LoadMetadata after save: %v", err)
	}
	if reloaded.LastError != "test error" {
		t.Errorf("LastError = %q, want %q", reloaded.LastError, "test error")
	}
	if reloaded.ErrorCount != 1 {
		t.Errorf("ErrorCount = %d, want 1", reloaded.ErrorCount)
	}
}

// ---------------------------------------------------------------------------
// Test: Create applies default values
// ---------------------------------------------------------------------------

func TestCreate_DefaultValues(t *testing.T) {
	t.Parallel()
	td := setupTestManager(t)

	vmCfg := createTestVM(t, td, &CreateOptions{
		Image: "docker.io/library/ubuntu:22.04",
		Name:  "defaults",
		// CPUs, MemoryMB, DiskSize all left at zero/empty.
	})

	if vmCfg.CPUs != td.cfg.DefaultCPUs {
		t.Errorf("CPUs = %d, want default %d", vmCfg.CPUs, td.cfg.DefaultCPUs)
	}
	if vmCfg.MemoryMB != td.cfg.DefaultMemoryMB {
		t.Errorf("MemoryMB = %d, want default %d", vmCfg.MemoryMB, td.cfg.DefaultMemoryMB)
	}
	if vmCfg.DiskSize != td.cfg.DefaultDiskSize {
		t.Errorf("DiskSize = %q, want default %q", vmCfg.DiskSize, td.cfg.DefaultDiskSize)
	}
	if vmCfg.BootStrategy != types.DefaultBootStrategy {
		t.Errorf("BootStrategy = %q, want default %q", vmCfg.BootStrategy, types.DefaultBootStrategy)
	}
}

// ---------------------------------------------------------------------------
// Test: Duplicate name
// ---------------------------------------------------------------------------

func TestCreate_DuplicateName(t *testing.T) {
	t.Parallel()
	td := setupTestManager(t)

	createTestVM(t, td, &CreateOptions{
		Image: "docker.io/library/ubuntu:22.04",
		Name:  "dupe-name",
	})

	// Second create with same name should fail.
	_, err := td.mgr.Create(t.Context(), &CreateOptions{
		Image: "docker.io/library/ubuntu:22.04",
		Name:  "dupe-name",
	})
	if err == nil {
		t.Fatal("expected error for duplicate name, got nil")
	}
	if !errors.Is(err, types.ErrVMAlreadyExists) {
		t.Errorf("expected ErrVMAlreadyExists, got: %v", err)
	}
}
