package oci

import "testing"

func TestRuntimeRefs_AddGetRemove(t *testing.T) {
	t.Parallel()

	cfg := testConfig(t)
	runtimeKey := "0011223344556677"
	vmID := "vm-01HF00TESTVMREF0000000000"

	if err := AddRuntimeRef(cfg, runtimeKey, vmID); err != nil {
		t.Fatalf("AddRuntimeRef: %v", err)
	}
	// Idempotent add.
	if err := AddRuntimeRef(cfg, runtimeKey, vmID); err != nil {
		t.Fatalf("AddRuntimeRef (idempotent): %v", err)
	}

	refs, err := GetRuntimeRefs(cfg, runtimeKey)
	if err != nil {
		t.Fatalf("GetRuntimeRefs: %v", err)
	}
	if len(refs) != 1 || refs[0] != vmID {
		t.Fatalf("refs=%v, want [%s]", refs, vmID)
	}

	referenced, err := IsRuntimeReferenced(cfg, runtimeKey)
	if err != nil {
		t.Fatalf("IsRuntimeReferenced: %v", err)
	}
	if !referenced {
		t.Fatal("expected runtime key to be referenced")
	}

	if err := RemoveRuntimeRef(cfg, runtimeKey, vmID); err != nil {
		t.Fatalf("RemoveRuntimeRef: %v", err)
	}
	// Idempotent remove.
	if err := RemoveRuntimeRef(cfg, runtimeKey, vmID); err != nil {
		t.Fatalf("RemoveRuntimeRef (idempotent): %v", err)
	}

	refs, err = GetRuntimeRefs(cfg, runtimeKey)
	if err != nil {
		t.Fatalf("GetRuntimeRefs after remove: %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("refs after remove=%v, want []", refs)
	}
}

func TestRuntimeRefs_LoadSnapshotReturnsCopy(t *testing.T) {
	t.Parallel()

	cfg := testConfig(t)
	runtimeKey := "8899aabbccddeeff"
	vmID := "vm-01HF00TESTSNAP0000000000"

	if err := AddRuntimeRef(cfg, runtimeKey, vmID); err != nil {
		t.Fatalf("AddRuntimeRef: %v", err)
	}

	snapshot, err := LoadRuntimeRefsSnapshot(cfg)
	if err != nil {
		t.Fatalf("LoadRuntimeRefsSnapshot: %v", err)
	}
	entry := snapshot.Runtimes[runtimeKey]
	entry.Refs[0] = "mutated"
	snapshot.Runtimes[runtimeKey] = entry

	refs, err := GetRuntimeRefs(cfg, runtimeKey)
	if err != nil {
		t.Fatalf("GetRuntimeRefs: %v", err)
	}
	if len(refs) != 1 || refs[0] != vmID {
		t.Fatalf("refs=%v, want [%s]", refs, vmID)
	}
}

func TestRuntimeRefs_InvalidRuntimeKey(t *testing.T) {
	t.Parallel()

	cfg := testConfig(t)
	if err := AddRuntimeRef(cfg, "../bad-key", "vm-1"); err == nil {
		t.Fatal("expected invalid runtime key error, got nil")
	}
}
