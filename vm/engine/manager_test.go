package engine

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/CMGS/cocoon/config"
	"github.com/CMGS/cocoon/hypervisor"
	hypermocks "github.com/CMGS/cocoon/hypervisor/mocks"
	"github.com/CMGS/cocoon/image"
	imgmocks "github.com/CMGS/cocoon/image/mocks"
	"github.com/CMGS/cocoon/image/refcache"
	"github.com/CMGS/cocoon/oci"
	storemocks "github.com/CMGS/cocoon/storage/mocks"
	"github.com/CMGS/cocoon/types"
	"github.com/CMGS/cocoon/utils"
	"github.com/CMGS/cocoon/vm"
)

// ---------------------------------------------------------------------------
// Test helper: setupTestManager
// ---------------------------------------------------------------------------

type testDeps struct {
	mgr        vm.Manager
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
	cfg.UEFIFirmwarePath = filepath.Join(rootDir, "firmware", "CLOUDHV.fd")

	if err := cfg.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}

	// Create dummy firmware file so resolveFirmwarePath won't fail.
	if err := os.WriteFile(cfg.UEFIFirmwarePath, []byte("firmware"), 0o644); err != nil {
		t.Fatalf("write firmware: %v", err)
	}

	hyper := &hypermocks.MockClient{}
	refCounter := &storemocks.MockReferenceCounter{}
	cowMgr := &storemocks.MockCOWManager{}
	imgMgr := &imgmocks.MockManager{}

	mgr := New(cfg, hyper, refCounter, cowMgr, imgMgr)
	concreteMgr, ok := mgr.(*manager)
	if !ok {
		t.Fatalf("unexpected manager type %T", mgr)
	}
	// Keep manager tests hermetic: never depend on host skopeo/network.
	concreteMgr.registryProbeRawFn = resolverProbeStub

	return &testDeps{
		mgr:        concreteMgr,
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
func createTestVM(t *testing.T, td *testDeps, opts *vm.CreateOptions) *types.VMConfig {
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

type initramfsTestEntry struct {
	Name string
	Data []byte
}

func buildTestNewcCPIO(entries []initramfsTestEntry) []byte {
	var buf bytes.Buffer
	ino := uint32(1)
	for _, e := range entries {
		writeTestCPIONewcEntry(&buf, ino, e.Name, e.Data)
		ino++
	}
	writeTestCPIONewcEntry(&buf, 0, "TRAILER!!!", nil)
	return buf.Bytes()
}

func writeTestCPIONewcEntry(w *bytes.Buffer, ino uint32, name string, data []byte) {
	nameWithNul := name + "\x00"
	namesize := len(nameWithNul)
	filesize := len(data)

	hdr := fmt.Sprintf("070701"+
		"%08X"+ // ino
		"%08X"+ // mode
		"%08X"+ // uid
		"%08X"+ // gid
		"%08X"+ // nlink
		"%08X"+ // mtime
		"%08X"+ // filesize
		"%08X"+ // devmajor
		"%08X"+ // devminor
		"%08X"+ // rdevmajor
		"%08X"+ // rdevminor
		"%08X"+ // namesize
		"%08X", // check
		ino,
		0o100644,  // mode
		uint32(0), // uid
		uint32(0), // gid
		uint32(1), // nlink
		uint32(0), // mtime
		uint32(filesize),
		uint32(0), // devmajor
		uint32(0), // devminor
		uint32(0), // rdevmajor
		uint32(0), // rdevminor
		uint32(namesize),
		uint32(0), // check
	)

	w.WriteString(hdr)
	w.WriteString(nameWithNul)

	headerPlusName := 110 + namesize
	if pad := (headerPlusName + 3) &^ 3; pad > headerPlusName {
		w.Write(make([]byte, pad-headerPlusName))
	}

	w.Write(data)
	if pad := (filesize + 3) &^ 3; pad > filesize {
		w.Write(make([]byte, pad-filesize))
	}
}

// ---------------------------------------------------------------------------
// Test: Create happy path
// ---------------------------------------------------------------------------

func TestCreate_HappyPath(t *testing.T) {
	t.Parallel()
	td := setupTestManager(t)

	opts := &vm.CreateOptions{
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
	if vmCfg.ImageType != types.VMImageTypeQCOW2 {
		t.Errorf("ImageType = %q, want %q", vmCfg.ImageType, types.VMImageTypeQCOW2)
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

	_, err := td.mgr.Create(t.Context(), &vm.CreateOptions{
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

	vmCfg, err := td.mgr.Create(t.Context(), &vm.CreateOptions{
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

func TestCreate_DefaultSkipVerifyOnCacheHit(t *testing.T) {
	t.Parallel()
	td := setupTestManager(t)

	identity := defaultMockIdentity()
	basePath := filepath.Join(td.cfg.ImageCacheDir(), identity.BaseKey()+".qcow2")
	if err := refcache.MarkVerified(td.cfg, identity.BaseKey()); err != nil {
		t.Fatalf("MarkVerified: %v", err)
	}
	td.imgMgr.ListCachedFunc = func(_ context.Context) ([]*image.CachedImage, error) {
		return []*image.CachedImage{
			{BaseKey: identity.BaseKey(), Path: basePath},
		}, nil
	}
	td.imgMgr.PrepareFunc = func(_ context.Context, _ string) (*image.ImageIdentity, string, error) {
		return identity, basePath, nil
	}
	verifyCallCount := 0
	td.imgMgr.VerifyBootabilityFunc = func(_ context.Context, _ string) (*image.BootCheckResult, error) {
		verifyCallCount++
		return &image.BootCheckResult{Bootable: true}, nil
	}
	td.cowMgr.CreateOverlayFunc = func(baseKey, vmID, diskSize string) (string, error) {
		return filepath.Join(td.cfg.RootDir, "vms", vmID, "overlay.qcow2"), nil
	}

	vmCfg, err := td.mgr.Create(t.Context(), &vm.CreateOptions{
		Image: "docker.io/library/ubuntu:22.04",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if vmCfg == nil {
		t.Fatal("expected non-nil VMConfig")
	}
	if verifyCallCount != 0 {
		t.Errorf("VerifyBootability called %d times, want 0", verifyCallCount)
	}
}

func TestCreate_CacheHitWithoutVerifiedStateRunsVerify(t *testing.T) {
	t.Parallel()
	td := setupTestManager(t)

	identity := defaultMockIdentity()
	basePath := filepath.Join(td.cfg.ImageCacheDir(), identity.BaseKey()+".qcow2")
	td.imgMgr.ListCachedFunc = func(_ context.Context) ([]*image.CachedImage, error) {
		return []*image.CachedImage{
			{BaseKey: identity.BaseKey(), Path: basePath},
		}, nil
	}
	td.imgMgr.PrepareFunc = func(_ context.Context, _ string) (*image.ImageIdentity, string, error) {
		return identity, basePath, nil
	}
	verifyCallCount := 0
	td.imgMgr.VerifyBootabilityFunc = func(_ context.Context, _ string) (*image.BootCheckResult, error) {
		verifyCallCount++
		return &image.BootCheckResult{Bootable: true}, nil
	}
	td.cowMgr.CreateOverlayFunc = func(baseKey, vmID, diskSize string) (string, error) {
		return filepath.Join(td.cfg.RootDir, "vms", vmID, "overlay.qcow2"), nil
	}

	vmCfg, err := td.mgr.Create(t.Context(), &vm.CreateOptions{
		Image: "docker.io/library/ubuntu:22.04",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if vmCfg == nil {
		t.Fatal("expected non-nil VMConfig")
	}
	if verifyCallCount != 1 {
		t.Errorf("VerifyBootability called %d times, want 1", verifyCallCount)
	}
}

func TestCreate_DefaultVerifyOnCacheMiss(t *testing.T) {
	t.Parallel()
	td := setupTestManager(t)

	identity := defaultMockIdentity()
	td.imgMgr.ListCachedFunc = func(_ context.Context) ([]*image.CachedImage, error) {
		return nil, nil
	}
	td.imgMgr.PrepareFunc = func(_ context.Context, _ string) (*image.ImageIdentity, string, error) {
		return identity, filepath.Join(td.cfg.ImageCacheDir(), identity.BaseKey()+".qcow2"), nil
	}
	verifyCallCount := 0
	td.imgMgr.VerifyBootabilityFunc = func(_ context.Context, _ string) (*image.BootCheckResult, error) {
		verifyCallCount++
		return &image.BootCheckResult{Bootable: true}, nil
	}
	td.cowMgr.CreateOverlayFunc = func(baseKey, vmID, diskSize string) (string, error) {
		return filepath.Join(td.cfg.RootDir, "vms", vmID, "overlay.qcow2"), nil
	}

	vmCfg, err := td.mgr.Create(t.Context(), &vm.CreateOptions{
		Image: "docker.io/library/ubuntu:22.04",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if vmCfg == nil {
		t.Fatal("expected non-nil VMConfig")
	}
	if verifyCallCount != 1 {
		t.Errorf("VerifyBootability called %d times, want 1", verifyCallCount)
	}
}

func TestCreate_InvalidBootStrategy(t *testing.T) {
	t.Parallel()
	td := setupTestManager(t)

	identity := defaultMockIdentity()
	td.imgMgr.PrepareFunc = func(_ context.Context, _ string) (*image.ImageIdentity, string, error) {
		return identity, filepath.Join(td.cfg.ImageCacheDir(), identity.BaseKey()+".qcow2"), nil
	}
	td.imgMgr.VerifyBootabilityFunc = func(_ context.Context, _ string) (*image.BootCheckResult, error) {
		return &image.BootCheckResult{Bootable: true}, nil
	}

	_, err := td.mgr.Create(t.Context(), &vm.CreateOptions{
		Image:        "docker.io/library/ubuntu:22.04",
		BootStrategy: types.BootStrategy("invalid_strategy"),
	})
	if err == nil {
		t.Fatal("expected error for invalid boot strategy, got nil")
	}
	if got, want := err.Error(), `invalid boot strategy: "invalid_strategy"`; !strings.Contains(got, want) {
		t.Fatalf("Create error = %q, want substring %q", got, want)
	}
}

func TestCreate_LocalOCITagRequiresMaterializedLayout(t *testing.T) {
	t.Parallel()
	td := setupTestManager(t)

	store := oci.NewStore(td.cfg)
	if err := store.SaveTag("demo:latest", filepath.Join(t.TempDir(), "layout"), "sha256:1111"); err != nil {
		t.Fatalf("SaveTag: %v", err)
	}

	prepareCalled := false
	td.imgMgr.PrepareFunc = func(_ context.Context, _ string) (*image.ImageIdentity, string, error) {
		prepareCalled = true
		return defaultMockIdentity(), filepath.Join(td.cfg.ImageCacheDir(), "unused.qcow2"), nil
	}

	_, err := td.mgr.Create(t.Context(), &vm.CreateOptions{
		Image: "demo",
	})
	if err == nil {
		t.Fatal("expected OCI runtime create error, got nil")
	}
	if !strings.Contains(err.Error(), "OCI runtime preflight") && !strings.Contains(err.Error(), "local OCI layout") {
		t.Fatalf("unexpected error: %v", err)
	}
	if prepareCalled {
		t.Fatal("imgMgr.Prepare should not be called for unresolved OCI runtime path")
	}
}

func TestResolveOCIRuntimeLocalTag_RegistryPullNormalizesLatest(t *testing.T) {
	t.Parallel()
	td := setupTestManager(t)

	mgrImpl, ok := td.mgr.(*manager)
	if !ok {
		t.Fatalf("unexpected manager type %T", td.mgr)
	}

	var pulledRef string
	mgrImpl.ociPullFn = func(_ context.Context, _ *config.CocoonConfig, ref string) (*oci.PullResult, error) {
		pulledRef = ref
		return &oci.PullResult{
			Ref:            oci.EnsureLatestTag(ref),
			ManifestDigest: "sha256:deadbeef",
		}, nil
	}

	localTag, err := mgrImpl.resolveOCIRuntimeLocalTag(t.Context(), &resolvedRuntimeImage{
		PrepareRef:  "example.com/repo",
		Source:      runtimeImageSourceRegistry,
		VMImageType: types.VMImageTypeOCIVM,
	})
	if err != nil {
		t.Fatalf("resolveOCIRuntimeLocalTag: %v", err)
	}
	if pulledRef != "example.com/repo" {
		t.Fatalf("pull ref = %q, want %q", pulledRef, "example.com/repo")
	}
	if localTag != "example.com/repo:latest" {
		t.Fatalf("local tag = %q, want %q", localTag, "example.com/repo:latest")
	}
}

func TestValidateOCIRuntimeInitramfsVirtiofs_Found(t *testing.T) {
	t.Parallel()

	cpioData := buildTestNewcCPIO([]initramfsTestEntry{
		{Name: "lib/modules/6.8.0/kernel/fs/fuse/virtiofs.ko", Data: []byte("ELF-fake")},
	})
	var gzBuf bytes.Buffer
	gzw := gzip.NewWriter(&gzBuf)
	if _, err := gzw.Write(cpioData); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}

	initrdPath := filepath.Join(t.TempDir(), "initrd.img")
	if err := os.WriteFile(initrdPath, gzBuf.Bytes(), 0o644); err != nil {
		t.Fatalf("write initrd: %v", err)
	}

	if err := validateOCIRuntimeInitramfsVirtiofs(initrdPath); err != nil {
		t.Fatalf("validateOCIRuntimeInitramfsVirtiofs: %v", err)
	}
}

func TestValidateOCIRuntimeInitramfsVirtiofs_NotFound(t *testing.T) {
	t.Parallel()

	cpioData := buildTestNewcCPIO([]initramfsTestEntry{
		{Name: "lib/modules/6.8.0/kernel/fs/ext4/ext4.ko", Data: []byte("ELF-fake")},
	})
	initrdPath := filepath.Join(t.TempDir(), "initrd.img")
	if err := os.WriteFile(initrdPath, cpioData, 0o644); err != nil {
		t.Fatalf("write initrd: %v", err)
	}

	err := validateOCIRuntimeInitramfsVirtiofs(initrdPath)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "virtiofs module not found") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateOCIRuntimeInitramfsVirtiofs_InconclusiveFails(t *testing.T) {
	t.Parallel()

	initrdPath := filepath.Join(t.TempDir(), "initrd.img")
	if err := os.WriteFile(initrdPath, []byte{0xde, 0xad, 0xbe, 0xef, 0x00, 0x01}, 0o644); err != nil {
		t.Fatalf("write initrd: %v", err)
	}

	err := validateOCIRuntimeInitramfsVirtiofs(initrdPath)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "virtiofs module detection failed") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Test: Delete happy path
// ---------------------------------------------------------------------------

func TestDelete_HappyPath(t *testing.T) {
	t.Parallel()
	td := setupTestManager(t)

	opts := &vm.CreateOptions{
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

func TestDelete_RemovesAllLogFiles(t *testing.T) {
	t.Parallel()
	td := setupTestManager(t)

	vmCfg := createTestVM(t, td, &vm.CreateOptions{
		Image: "docker.io/library/ubuntu:22.04",
		Name:  "delete-log-cleanup",
	})

	logPaths := []string{
		td.cfg.VMSerialLogPath(vmCfg.VMID),
		td.cfg.VMCHLogPath(vmCfg.VMID),
		td.cfg.VMSwtpmLogPath(vmCfg.VMID),
		td.cfg.VMVirtiofsdLogPath(vmCfg.VMID),
	}
	for _, path := range logPaths {
		if err := os.WriteFile(path, []byte("log"), 0o644); err != nil { //nolint:gosec // test fixture
			t.Fatalf("write log %s: %v", path, err)
		}
	}

	if err := td.mgr.Delete(t.Context(), vmCfg.VMID, false); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	for _, path := range logPaths {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("expected log file %s removed, stat err=%v", path, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Test: Delete force on running VM (simulated)
// ---------------------------------------------------------------------------

func TestDelete_ForceOnRunningVM(t *testing.T) {
	t.Parallel()
	td := setupTestManager(t)

	opts := &vm.CreateOptions{
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

func TestDelete_MissingMetadata_StillCleansResources(t *testing.T) {
	t.Parallel()
	td := setupTestManager(t)

	vmCfg := createTestVM(t, td, &vm.CreateOptions{
		Image: "docker.io/library/ubuntu:22.04",
		Name:  "missing-metadata-delete",
	})

	// Simulate metadata loss/corruption.
	if err := os.Remove(td.cfg.VMMetadataPath(vmCfg.VMID)); err != nil {
		t.Fatalf("remove metadata.json: %v", err)
	}

	if err := td.mgr.Delete(t.Context(), vmCfg.VMID, false); err != nil {
		t.Fatalf("Delete with missing metadata: %v", err)
	}

	if _, err := os.Stat(td.cfg.VMPersistDir(vmCfg.VMID)); !os.IsNotExist(err) {
		t.Fatalf("expected VM dir removed, got stat err: %v", err)
	}

	index, err := LoadNameIndex(td.cfg)
	if err != nil {
		t.Fatalf("LoadNameIndex: %v", err)
	}
	if _, ok := index["missing-metadata-delete"]; ok {
		t.Fatal("expected name removed from index")
	}
}

func TestDelete_MissingMetadata_RequiresForceWhenAlive(t *testing.T) {
	t.Parallel()
	td := setupTestManager(t)

	vmCfg := createTestVM(t, td, &vm.CreateOptions{
		Image: "docker.io/library/ubuntu:22.04",
		Name:  "missing-meta-running",
	})

	if err := os.Remove(td.cfg.VMMetadataPath(vmCfg.VMID)); err != nil {
		t.Fatalf("remove metadata.json: %v", err)
	}

	td.hyper.IsAliveFunc = func(_ string) bool { return true }
	td.hyper.ForceKillFunc = func(_ string) error { return nil }

	err := td.mgr.Delete(t.Context(), vmCfg.VMID, false)
	if !errors.Is(err, types.ErrVMRunning) {
		t.Fatalf("expected ErrVMRunning, got %v", err)
	}

	if err := td.mgr.Delete(t.Context(), vmCfg.VMID, true); err != nil {
		t.Fatalf("force delete with missing metadata: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Test: Inspect
// ---------------------------------------------------------------------------

func TestInspect(t *testing.T) {
	t.Parallel()
	td := setupTestManager(t)

	opts := &vm.CreateOptions{
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

func TestInspect_SerialLogExcerpt(t *testing.T) {
	t.Parallel()
	td := setupTestManager(t)

	vmCfg := createTestVM(t, td, &vm.CreateOptions{
		Image: "docker.io/library/ubuntu:22.04",
		Name:  "inspect-log",
	})

	// Write >100 lines and assert inspect returns the last 100 lines.
	lines := make([]byte, 0, 4096)
	for i := 0; i < 120; i++ {
		lines = append(lines, []byte(fmt.Sprintf("line-%03d\n", i))...)
	}
	if err := os.WriteFile(vmCfg.SerialLog, lines, 0o644); err != nil { //nolint:gosec // test fixture
		t.Fatalf("WriteFile serial log: %v", err)
	}

	inspect, err := td.mgr.Inspect(t.Context(), vmCfg.VMID)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}

	excerpt := inspect.Hypervisor.SerialLogExcerpt
	if len(excerpt) != 100 {
		t.Fatalf("excerpt length=%d, want 100", len(excerpt))
	}
	if excerpt[0] != "line-020" {
		t.Fatalf("excerpt[0]=%q, want %q", excerpt[0], "line-020")
	}
	if excerpt[len(excerpt)-1] != "line-119" {
		t.Fatalf("excerpt[last]=%q, want %q", excerpt[len(excerpt)-1], "line-119")
	}
}

func TestInspect_UsesActualStateWhenMetadataIsStale(t *testing.T) {
	t.Parallel()
	td := setupTestManager(t)

	vmCfg := createTestVM(t, td, &vm.CreateOptions{
		Image: "docker.io/library/ubuntu:22.04",
		Name:  "inspect-stale-running",
	})

	if err := td.mgr.TransitionState(vmCfg.VMID, types.VMStateStarting, "test"); err != nil {
		t.Fatalf("transition to STARTING: %v", err)
	}
	if err := td.mgr.TransitionState(vmCfg.VMID, types.VMStateRunning, "test"); err != nil {
		t.Fatalf("transition to RUNNING: %v", err)
	}

	inspect, err := td.mgr.Inspect(t.Context(), vmCfg.VMID)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if inspect.State != types.VMStateError {
		t.Fatalf("Inspect state = %q, want %q for stale RUNNING metadata", inspect.State, types.VMStateError)
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
	vm1 := createTestVM(t, td, &vm.CreateOptions{Image: "img1", Name: "list-vm-1"})
	vm2 := createTestVM(t, td, &vm.CreateOptions{Image: "img2", Name: "list-vm-2"})

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

	vmCfg := createTestVM(t, td, &vm.CreateOptions{
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

	vmCfg := createTestVM(t, td, &vm.CreateOptions{
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

	vmCfg := createTestVM(t, td, &vm.CreateOptions{
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

	vmCfg := createTestVM(t, td, &vm.CreateOptions{
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

	vmCfg := createTestVM(t, td, &vm.CreateOptions{
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

	_, err := td.mgr.Create(t.Context(), &vm.CreateOptions{Image: ""})
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

	vmCfg := createTestVM(t, td, &vm.CreateOptions{
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

	vmCfg := createTestVM(t, td, &vm.CreateOptions{
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

	vmCfg := createTestVM(t, td, &vm.CreateOptions{
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

	vmCfg := createTestVM(t, td, &vm.CreateOptions{
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

	vmCfg := createTestVM(t, td, &vm.CreateOptions{
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
	if vmCfg.ImageType != types.VMImageTypeQCOW2 {
		t.Errorf("ImageType = %q, want default %q", vmCfg.ImageType, types.VMImageTypeQCOW2)
	}
}

// ---------------------------------------------------------------------------
// Test: Duplicate name
// ---------------------------------------------------------------------------

func TestCreate_DuplicateName(t *testing.T) {
	t.Parallel()
	td := setupTestManager(t)

	createTestVM(t, td, &vm.CreateOptions{
		Image: "docker.io/library/ubuntu:22.04",
		Name:  "dupe-name",
	})

	// Second create with same name should fail.
	_, err := td.mgr.Create(t.Context(), &vm.CreateOptions{
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

func TestStart_InvalidBootStrategyInConfig(t *testing.T) {
	t.Parallel()
	td := setupTestManager(t)

	td.cfg.BootTimeoutSeconds = 0

	v := createTestVM(t, td, &vm.CreateOptions{
		Image: "docker.io/library/ubuntu:22.04",
		Name:  "start-invalid-strategy",
	})

	launchCalls := 0
	td.hyper.LaunchFunc = func(_ context.Context, _ string, _ *types.VMConfig) (int, error) {
		launchCalls++
		return 1234, nil
	}

	v.BootStrategy = types.BootStrategy("invalid_strategy")
	if err := utils.AtomicWriteJSON(td.cfg.VMConfigPath(v.VMID), v); err != nil {
		t.Fatalf("write config.json: %v", err)
	}

	err := td.mgr.Start(t.Context(), v.VMID)
	if err == nil {
		t.Fatal("Start() error = nil, want non-nil")
	}
	if got, want := err.Error(), `invalid boot strategy in config: "invalid_strategy"`; !strings.Contains(got, want) {
		t.Fatalf("Start error = %q, want substring %q", got, want)
	}
	if launchCalls != 0 {
		t.Fatalf("Launch calls = %d, want 0 for invalid strategy", launchCalls)
	}

	meta, loadErr := td.mgr.LoadMetadata(v.VMID)
	if loadErr != nil {
		t.Fatalf("LoadMetadata: %v", loadErr)
	}
	if meta.State != string(types.VMStateError) {
		t.Fatalf("metadata state = %q, want %q", meta.State, types.VMStateError)
	}
}

func TestStart_RefreshesHypervisorBinaryInMetadata(t *testing.T) {
	t.Parallel()
	td := setupTestManager(t)

	td.cfg.BootTimeoutSeconds = 0
	td.cfg.CHBinary = "/opt/bin/new-hypervisor"

	v := createTestVM(t, td, &vm.CreateOptions{
		Image: "docker.io/library/ubuntu:22.04",
		Name:  "start-refresh-hypervisor-binary",
	})

	// Simulate metadata from an older run/config.
	if err := td.mgr.UpdateMetadata(v.VMID, func(md *types.VMMetadataFile) {
		md.HypervisorBinary = "old-hypervisor"
	}); err != nil {
		t.Fatalf("UpdateMetadata before start: %v", err)
	}

	td.hyper.LaunchFunc = func(_ context.Context, _ string, _ *types.VMConfig) (int, error) {
		return 4242, nil
	}
	td.hyper.CreateVMFunc = func(_ context.Context, _ string, _ *hypervisor.CHVMConfig) error {
		return nil
	}
	td.hyper.BootVMFunc = func(_ context.Context, _ string) error {
		return nil
	}

	if err := td.mgr.Start(t.Context(), v.VMID); err != nil {
		t.Fatalf("Start: %v", err)
	}

	meta, err := td.mgr.LoadMetadata(v.VMID)
	if err != nil {
		t.Fatalf("LoadMetadata: %v", err)
	}
	if meta.State != string(types.VMStateRunning) {
		t.Fatalf("metadata state=%q, want %q", meta.State, types.VMStateRunning)
	}
	if meta.HypervisorBinary != "new-hypervisor" {
		t.Fatalf("hypervisor_binary=%q, want %q", meta.HypervisorBinary, "new-hypervisor")
	}
}

func TestStart_StaleRunningMetadataReturnsActionableError(t *testing.T) {
	t.Parallel()
	td := setupTestManager(t)

	vmCfg := createTestVM(t, td, &vm.CreateOptions{
		Image: "docker.io/library/ubuntu:22.04",
		Name:  "start-stale-running",
	})

	if err := td.mgr.TransitionState(vmCfg.VMID, types.VMStateStarting, "test"); err != nil {
		t.Fatalf("transition to STARTING: %v", err)
	}
	if err := td.mgr.TransitionState(vmCfg.VMID, types.VMStateRunning, "test"); err != nil {
		t.Fatalf("transition to RUNNING: %v", err)
	}

	err := td.mgr.Start(t.Context(), vmCfg.VMID)
	if err == nil {
		t.Fatal("expected stale running start error, got nil")
	}
	if !strings.Contains(err.Error(), "metadata says RUNNING but runtime is not healthy") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestStart_OCIRuntimeMetadataWiring(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" {
		t.Skip("OCI runtime virtiofsd lifecycle is Linux-only")
	}
	td := setupTestManager(t)

	td.cfg.BootTimeoutSeconds = 0

	v := createTestVM(t, td, &vm.CreateOptions{
		Image: "docker.io/library/ubuntu:22.04",
		Name:  "start-oci-runtime-metadata",
	})

	v.ImageType = types.VMImageTypeOCIVM
	v.VirtioFSTag = "/dev/root"
	v.VirtioFSSock = td.cfg.VMOCIRootfsVirtioFSSocketPath(v.VMID)
	if err := utils.AtomicWriteJSON(td.cfg.VMConfigPath(v.VMID), v); err != nil {
		t.Fatalf("write config.json: %v", err)
	}

	mgrImpl, ok := td.mgr.(*manager)
	if !ok {
		t.Fatal("manager implementation type assertion failed")
	}
	mgrImpl.overlayMgr.mountFn = func(string, string, string, uintptr, string) error { return nil }
	mgrImpl.overlayMgr.readMountInfoFn = func() ([]byte, error) { return []byte(""), nil }
	vfsMgr := newVirtiofsdRuntimeManager(td.cfg)
	vfsMgr.resolveBinaryFn = func(string) (string, error) { return "/usr/bin/virtiofsd", nil }
	vfsMgr.launchFn = func(context.Context, string, []string, string) (int, error) { return 7788, nil }
	vfsMgr.waitReadyFn = func(context.Context, string, int, time.Duration) error { return nil }
	vfsMgr.stopFn = func(int, string, time.Duration) error { return nil }
	vfsMgr.removeFn = func(string) error { return nil }
	mgrImpl.virtiofsMgr = vfsMgr

	td.hyper.LaunchFunc = func(_ context.Context, _ string, _ *types.VMConfig) (int, error) {
		return 4242, nil
	}
	td.hyper.CreateVMFunc = func(_ context.Context, _ string, vmCfg *hypervisor.CHVMConfig) error {
		if len(vmCfg.Fs) != 1 {
			t.Fatalf("fs entries = %d, want 1", len(vmCfg.Fs))
		}
		return nil
	}
	td.hyper.BootVMFunc = func(_ context.Context, _ string) error {
		return nil
	}

	if err := td.mgr.Start(t.Context(), v.VMID); err != nil {
		t.Fatalf("Start: %v", err)
	}

	meta, err := td.mgr.LoadMetadata(v.VMID)
	if err != nil {
		t.Fatalf("LoadMetadata: %v", err)
	}
	if meta.State != string(types.VMStateRunning) {
		t.Fatalf("state = %q, want %q", meta.State, types.VMStateRunning)
	}
	if meta.VirtiofsdPID != 7788 {
		t.Fatalf("virtiofsd_pid = %d, want 7788", meta.VirtiofsdPID)
	}
	if meta.VirtiofsdSocket != v.VirtioFSSock {
		t.Fatalf("virtiofsd_socket = %q, want %q", meta.VirtiofsdSocket, v.VirtioFSSock)
	}
	if meta.VirtiofsdBinary != "virtiofsd" {
		t.Fatalf("virtiofsd_binary = %q, want %q", meta.VirtiofsdBinary, "virtiofsd")
	}

	refs, err := oci.GetRuntimeRefs(td.cfg, v.BaseKey)
	if err != nil {
		t.Fatalf("GetRuntimeRefs: %v", err)
	}
	if len(refs) != 1 || refs[0] != v.VMID {
		t.Fatalf("runtime refs = %v, want [%s]", refs, v.VMID)
	}
}

func TestStart_OCIRuntimeCleanupOnBootFailure(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" {
		t.Skip("OCI runtime virtiofsd lifecycle is Linux-only")
	}
	td := setupTestManager(t)

	td.cfg.BootTimeoutSeconds = 0

	v := createTestVM(t, td, &vm.CreateOptions{
		Image: "docker.io/library/ubuntu:22.04",
		Name:  "start-oci-runtime-cleanup-failure",
	})

	v.ImageType = types.VMImageTypeOCIVM
	v.VirtioFSTag = "/dev/root"
	v.VirtioFSSock = td.cfg.VMOCIRootfsVirtioFSSocketPath(v.VMID)
	if err := utils.AtomicWriteJSON(td.cfg.VMConfigPath(v.VMID), v); err != nil {
		t.Fatalf("write config.json: %v", err)
	}

	mgrImpl, ok := td.mgr.(*manager)
	if !ok {
		t.Fatal("manager implementation type assertion failed")
	}
	mgrImpl.overlayMgr.mountFn = func(string, string, string, uintptr, string) error { return nil }
	mgrImpl.overlayMgr.readMountInfoFn = func() ([]byte, error) { return []byte(""), nil }
	stopCalled := false
	vfsMgr := newVirtiofsdRuntimeManager(td.cfg)
	vfsMgr.resolveBinaryFn = func(string) (string, error) { return "/usr/bin/virtiofsd", nil }
	vfsMgr.launchFn = func(context.Context, string, []string, string) (int, error) { return 9900, nil }
	vfsMgr.waitReadyFn = func(context.Context, string, int, time.Duration) error { return nil }
	vfsMgr.stopFn = func(pid int, _ string, _ time.Duration) error {
		stopCalled = true
		if pid != 9900 {
			t.Fatalf("stop pid = %d, want 9900", pid)
		}
		return nil
	}
	vfsMgr.removeFn = func(string) error { return nil }
	mgrImpl.virtiofsMgr = vfsMgr

	td.hyper.LaunchFunc = func(_ context.Context, _ string, _ *types.VMConfig) (int, error) {
		return 0, errors.New("launch failed")
	}

	err := td.mgr.Start(t.Context(), v.VMID)
	if err == nil {
		t.Fatal("expected Start error, got nil")
	}
	if !strings.Contains(err.Error(), "launch failed") {
		t.Fatalf("Start error = %v, want launch failure", err)
	}
	if !stopCalled {
		t.Fatal("expected virtiofs stopFn to be called on boot failure")
	}

	meta, loadErr := td.mgr.LoadMetadata(v.VMID)
	if loadErr != nil {
		t.Fatalf("LoadMetadata: %v", loadErr)
	}
	if meta.State != string(types.VMStateError) {
		t.Fatalf("state = %q, want %q", meta.State, types.VMStateError)
	}
	if meta.VirtiofsdPID != 0 {
		t.Fatalf("virtiofsd_pid = %d, want 0", meta.VirtiofsdPID)
	}
	if meta.VirtiofsdSocket != "" {
		t.Fatalf("virtiofsd_socket = %q, want empty", meta.VirtiofsdSocket)
	}
}

func TestStop_CleansOCIRuntimeMetadata(t *testing.T) {
	t.Parallel()
	td := setupTestManager(t)

	v := createTestVM(t, td, &vm.CreateOptions{
		Image: "docker.io/library/ubuntu:22.04",
		Name:  "stop-cleans-oci-runtime",
	})

	v.ImageType = types.VMImageTypeOCIVM
	v.VirtioFSTag = "/dev/root"
	v.VirtioFSSock = td.cfg.VMOCIRootfsVirtioFSSocketPath(v.VMID)
	if err := utils.AtomicWriteJSON(td.cfg.VMConfigPath(v.VMID), v); err != nil {
		t.Fatalf("write config.json: %v", err)
	}

	if err := td.mgr.UpdateMetadata(v.VMID, func(md *types.VMMetadataFile) {
		md.State = string(types.VMStateRunning)
		md.PreviousState = string(types.VMStateCreated)
		md.ProcessPID = 100
		md.VirtiofsdPID = 200
		md.VirtiofsdSocket = v.VirtioFSSock
		md.VirtiofsdBinary = "virtiofsd"
		md.OCIOverlayMounted = true
	}); err != nil {
		t.Fatalf("UpdateMetadata: %v", err)
	}

	mgrImpl, ok := td.mgr.(*manager)
	if !ok {
		t.Fatal("manager implementation type assertion failed")
	}
	stopCalled := false
	vfsMgr := newVirtiofsdRuntimeManager(td.cfg)
	vfsMgr.stopFn = func(pid int, expectedProc string, _ time.Duration) error {
		stopCalled = true
		if pid != 200 {
			t.Fatalf("virtiofs stop pid = %d, want 200", pid)
		}
		if expectedProc != "virtiofsd" {
			t.Fatalf("virtiofs expected proc = %q, want virtiofsd", expectedProc)
		}
		return nil
	}
	vfsMgr.removeFn = func(string) error { return nil }
	mgrImpl.virtiofsMgr = vfsMgr

	td.hyper.ShutdownFunc = func(context.Context, string, time.Duration) error { return nil }

	if err := td.mgr.Stop(t.Context(), v.VMID, 2*time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !stopCalled {
		t.Fatal("expected virtiofs stopFn to be called")
	}

	meta, err := td.mgr.LoadMetadata(v.VMID)
	if err != nil {
		t.Fatalf("LoadMetadata: %v", err)
	}
	if meta.State != string(types.VMStateStopped) {
		t.Fatalf("state = %q, want %q", meta.State, types.VMStateStopped)
	}
	if meta.VirtiofsdPID != 0 {
		t.Fatalf("virtiofsd_pid = %d, want 0", meta.VirtiofsdPID)
	}
	if meta.VirtiofsdSocket != "" {
		t.Fatalf("virtiofsd_socket = %q, want empty", meta.VirtiofsdSocket)
	}
	if meta.OCIOverlayMounted {
		t.Fatal("oci_overlay_mounted = true, want false")
	}
}

func TestStop_StoppingStateCleansOCIRuntimeMetadata(t *testing.T) {
	t.Parallel()
	td := setupTestManager(t)

	v := createTestVM(t, td, &vm.CreateOptions{
		Image: "docker.io/library/ubuntu:22.04",
		Name:  "stop-stopping-cleans-oci-runtime",
	})

	v.ImageType = types.VMImageTypeOCIVM
	v.VirtioFSTag = "/dev/root"
	v.VirtioFSSock = td.cfg.VMOCIRootfsVirtioFSSocketPath(v.VMID)
	if err := utils.AtomicWriteJSON(td.cfg.VMConfigPath(v.VMID), v); err != nil {
		t.Fatalf("write config.json: %v", err)
	}

	if err := td.mgr.UpdateMetadata(v.VMID, func(md *types.VMMetadataFile) {
		md.State = string(types.VMStateStopping)
		md.PreviousState = string(types.VMStateRunning)
		md.ProcessPID = 100
		md.VirtiofsdPID = 200
		md.VirtiofsdSocket = v.VirtioFSSock
		md.VirtiofsdBinary = "virtiofsd"
		md.OCIOverlayMounted = true
	}); err != nil {
		t.Fatalf("UpdateMetadata: %v", err)
	}

	mgrImpl, ok := td.mgr.(*manager)
	if !ok {
		t.Fatal("manager implementation type assertion failed")
	}
	stopCalled := false
	vfsMgr := newVirtiofsdRuntimeManager(td.cfg)
	vfsMgr.stopFn = func(pid int, expectedProc string, _ time.Duration) error {
		stopCalled = true
		if pid != 200 {
			t.Fatalf("virtiofs stop pid = %d, want 200", pid)
		}
		if expectedProc != "virtiofsd" {
			t.Fatalf("virtiofs expected proc = %q, want virtiofsd", expectedProc)
		}
		return nil
	}
	vfsMgr.removeFn = func(string) error { return nil }
	mgrImpl.virtiofsMgr = vfsMgr

	td.hyper.IsAliveFunc = func(string) bool { return false }

	if err := td.mgr.Stop(t.Context(), v.VMID, 2*time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !stopCalled {
		t.Fatal("expected virtiofs stopFn to be called for STOPPING->STOPPED path")
	}

	meta, err := td.mgr.LoadMetadata(v.VMID)
	if err != nil {
		t.Fatalf("LoadMetadata: %v", err)
	}
	if meta.State != string(types.VMStateStopped) {
		t.Fatalf("state = %q, want %q", meta.State, types.VMStateStopped)
	}
	if meta.VirtiofsdPID != 0 {
		t.Fatalf("virtiofsd_pid = %d, want 0", meta.VirtiofsdPID)
	}
	if meta.VirtiofsdSocket != "" {
		t.Fatalf("virtiofsd_socket = %q, want empty", meta.VirtiofsdSocket)
	}
	if meta.OCIOverlayMounted {
		t.Fatal("oci_overlay_mounted = true, want false")
	}
}

func TestDelete_OCIRuntimeUnpinsRuntimeCache(t *testing.T) {
	t.Parallel()
	td := setupTestManager(t)

	v := createTestVM(t, td, &vm.CreateOptions{
		Image: "docker.io/library/ubuntu:22.04",
		Name:  "delete-oci-unpin-runtime",
	})

	runtimeKey := "runtime-pin-delete"
	v.ImageType = types.VMImageTypeOCIVM
	v.BaseKey = runtimeKey
	v.BaseImagePath = td.cfg.OCIRuntimeRootfsDir(runtimeKey)
	v.OverlayPath = ""
	if err := os.MkdirAll(v.BaseImagePath, 0o755); err != nil {
		t.Fatalf("mkdir runtime rootfs: %v", err)
	}
	if err := utils.AtomicWriteJSON(td.cfg.VMConfigPath(v.VMID), v); err != nil {
		t.Fatalf("write config.json: %v", err)
	}

	if err := oci.AddRuntimeRef(td.cfg, runtimeKey, v.VMID); err != nil {
		t.Fatalf("AddRuntimeRef: %v", err)
	}

	if err := td.mgr.Delete(t.Context(), v.VMID, false); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	refs, err := oci.GetRuntimeRefs(td.cfg, runtimeKey)
	if err != nil {
		t.Fatalf("GetRuntimeRefs: %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("runtime refs = %v, want []", refs)
	}
}

func TestBuildCHVMConfig_SetsMaxVCPUs(t *testing.T) {
	t.Parallel()

	vmCfg := &types.VMConfig{
		CPUs:        4,
		MemoryMB:    2048,
		OverlayPath: "/tmp/overlay.qcow2",
		SerialLog:   "/tmp/serial.log",
	}

	chCfg := buildCHVMConfig(vmCfg)
	if chCfg.CPUs.BootVCPUs != 4 {
		t.Fatalf("boot_vcpus = %d, want 4", chCfg.CPUs.BootVCPUs)
	}
	if chCfg.CPUs.MaxVCPUs != 4 {
		t.Fatalf("max_vcpus = %d, want 4", chCfg.CPUs.MaxVCPUs)
	}
}

func TestBuildCHVMConfig_DirectKernelBoot(t *testing.T) {
	t.Parallel()

	vmCfg := &types.VMConfig{
		CPUs:          2,
		MemoryMB:      1024,
		OverlayPath:   "/tmp/overlay.qcow2",
		SerialLog:     "/tmp/serial.log",
		BootStrategy:  types.BootStrategyDirect,
		KernelPath:    "/boot/vmlinuz",
		InitramfsPath: "/boot/initrd.img",
		Cmdline:       "console=ttyS0 root=/dev/vda1",
	}

	chCfg := buildCHVMConfig(vmCfg)
	if chCfg.Payload == nil {
		t.Fatal("expected Payload to be set for direct kernel boot")
	}
	if chCfg.Payload.Kernel != "/boot/vmlinuz" {
		t.Fatalf("payload.kernel = %q, want /boot/vmlinuz", chCfg.Payload.Kernel)
	}
	if chCfg.Payload.Initramfs != "/boot/initrd.img" {
		t.Fatalf("payload.initramfs = %q, want /boot/initrd.img", chCfg.Payload.Initramfs)
	}
	if chCfg.Payload.Cmdline != "console=ttyS0 root=/dev/vda1" {
		t.Fatalf("payload.cmdline = %q, want \"console=ttyS0 root=/dev/vda1\"", chCfg.Payload.Cmdline)
	}
	if chCfg.Payload.Firmware != "" {
		t.Fatalf("payload.firmware should be empty for direct kernel boot, got %q", chCfg.Payload.Firmware)
	}
	if len(chCfg.Fs) != 0 {
		t.Fatalf("fs should be empty for config without virtiofs, got %+v", chCfg.Fs)
	}
	if chCfg.Memory.Shared {
		t.Fatal("memory.shared should be false when virtiofs is not configured")
	}
}

func TestBuildCHVMConfig_DirectKernelBootWithVirtioFS(t *testing.T) {
	t.Parallel()

	vmCfg := &types.VMConfig{
		CPUs:          2,
		MemoryMB:      1024,
		OverlayPath:   "/tmp/overlay.qcow2",
		SerialLog:     "/tmp/serial.log",
		BootStrategy:  types.BootStrategyDirect,
		KernelPath:    "/boot/vmlinuz",
		InitramfsPath: "/boot/initrd.img",
		Cmdline:       "root=/dev/root rootfstype=virtiofs rw",
		VirtioFSTag:   "/dev/root",
		VirtioFSSock:  "/var/lib/cocoon/vms/vm-test/virtiofsd.sock",
	}

	chCfg := buildCHVMConfig(vmCfg)
	if len(chCfg.Fs) != 1 {
		t.Fatalf("fs entries = %d, want 1", len(chCfg.Fs))
	}
	if chCfg.Fs[0].Tag != "/dev/root" {
		t.Fatalf("fs[0].tag = %q, want /dev/root", chCfg.Fs[0].Tag)
	}
	if chCfg.Fs[0].Socket != "/var/lib/cocoon/vms/vm-test/virtiofsd.sock" {
		t.Fatalf("fs[0].socket = %q, want /var/lib/cocoon/vms/vm-test/virtiofsd.sock", chCfg.Fs[0].Socket)
	}
	if chCfg.Payload.Cmdline != "console=ttyS0 console=hvc0 root=/dev/root rootfstype=virtiofs rw" {
		t.Fatalf("payload.cmdline = %q, want console=ttyS0 console=hvc0 root=/dev/root rootfstype=virtiofs rw", chCfg.Payload.Cmdline)
	}
	if !chCfg.Memory.Shared {
		t.Fatal("memory.shared should be true when virtiofs is configured")
	}
}

func TestBuildCHVMConfig_UEFIFirmwareInPayload(t *testing.T) {
	t.Parallel()

	vmCfg := &types.VMConfig{
		CPUs:         2,
		MemoryMB:     1024,
		OverlayPath:  "/tmp/overlay.qcow2",
		SerialLog:    "/tmp/serial.log",
		BootStrategy: types.BootStrategyUEFI,
		FirmwarePath: "/usr/share/CLOUDHV.fd",
	}

	chCfg := buildCHVMConfig(vmCfg)
	if chCfg.Payload == nil {
		t.Fatal("expected Payload to be set for UEFI boot")
	}
	if chCfg.Payload.Firmware != "/usr/share/CLOUDHV.fd" {
		t.Fatalf("payload.firmware = %q, want /usr/share/CLOUDHV.fd", chCfg.Payload.Firmware)
	}
	if chCfg.Payload.Kernel != "" {
		t.Fatalf("payload.kernel should be empty for UEFI, got %q", chCfg.Payload.Kernel)
	}
}

func TestBuildCHVMConfig_NoFirmwareNoPayload(t *testing.T) {
	t.Parallel()

	vmCfg := &types.VMConfig{
		CPUs:        2,
		MemoryMB:    1024,
		OverlayPath: "/tmp/overlay.qcow2",
		SerialLog:   "/tmp/serial.log",
	}

	chCfg := buildCHVMConfig(vmCfg)
	if chCfg.Payload != nil {
		t.Fatalf("expected Payload to be nil when no firmware, got %+v", chCfg.Payload)
	}
}
