package refcache

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/CMGS/cocoon/config"
)

func testConfig(t *testing.T) *config.CocoonConfig {
	t.Helper()
	root := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.RebaseRootDir(root)
	cfg.RuntimeDir = filepath.Join(root, "run")
	cfg.LogDir = filepath.Join(root, "log")
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}
	return cfg
}

func TestUpsertAndResolveBaseKey(t *testing.T) {
	cfg := testConfig(t)

	ref := "https://cloud-images.ubuntu.com/releases/22.04/release/ubuntu-22.04-server-cloudimg-amd64.img"
	baseKey := "abc123def4567890_amd64"
	digest := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	if err := Upsert(cfg, ref, baseKey, digest); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	cases := []string{
		ref,
		"ubuntu-22.04-server-cloudimg-amd64.img",
		"ubuntu-22.04-server-cloudimg-amd64",
		"ubuntu-22.04-cloudimg",
	}
	for _, c := range cases {
		got, ok, err := ResolveBaseKey(cfg, c)
		if err != nil {
			t.Fatalf("ResolveBaseKey(%q): %v", c, err)
		}
		if !ok {
			t.Fatalf("ResolveBaseKey(%q): not found", c)
		}
		if got != baseKey {
			t.Fatalf("ResolveBaseKey(%q)=%q, want %q", c, got, baseKey)
		}
	}
}

func TestRefsForBaseKeyAndDelete(t *testing.T) {
	cfg := testConfig(t)

	baseKey := "fff111aaa222bbb3_amd64"
	digest := "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	if err := Upsert(cfg, "myorg/ubuntu-bootable:22.04", baseKey, digest); err != nil {
		t.Fatalf("Upsert 1: %v", err)
	}
	if err := Upsert(cfg, "myorg/ubuntu-bootable:latest", baseKey, digest); err != nil {
		t.Fatalf("Upsert 2: %v", err)
	}

	refs, gotDigest, err := RefsForBaseKey(cfg, baseKey)
	if err != nil {
		t.Fatalf("RefsForBaseKey: %v", err)
	}
	if len(refs) == 0 {
		t.Fatalf("RefsForBaseKey returned empty refs")
	}
	if gotDigest != digest {
		t.Fatalf("RefsForBaseKey digest=%q, want %q", gotDigest, digest)
	}

	if err := DeleteByBaseKey(cfg, baseKey); err != nil {
		t.Fatalf("DeleteByBaseKey: %v", err)
	}

	if _, ok, err := ResolveBaseKey(cfg, "myorg/ubuntu-bootable:22.04"); err != nil {
		t.Fatalf("ResolveBaseKey after delete: %v", err)
	} else if ok {
		t.Fatalf("ResolveBaseKey should miss after delete")
	}
}

func TestResolveBaseKey_AmbiguousAlias(t *testing.T) {
	cfg := testConfig(t)

	baseKeyA := "aaaa1111bbbb2222_amd64"
	baseKeyB := "cccc3333dddd4444_amd64"
	digestA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	refA := "https://images.example.com/ubuntu-22.04-server-cloudimg-amd64.img"
	refB := "https://mirror.example.net/ubuntu-22.04-cloudimg-amd64.img"

	if err := Upsert(cfg, refA, baseKeyA, digestA); err != nil {
		t.Fatalf("Upsert refA: %v", err)
	}
	if err := Upsert(cfg, refB, baseKeyB, digestB); err != nil {
		t.Fatalf("Upsert refB: %v", err)
	}

	_, ok, err := ResolveBaseKey(cfg, "ubuntu-22.04-cloudimg")
	if err == nil {
		t.Fatal("ResolveBaseKey should return ambiguity error, got nil")
	}
	if ok {
		t.Fatal("ResolveBaseKey ambiguity should not return ok=true")
	}
	if !errors.Is(err, ErrAmbiguousImageRef) {
		t.Fatalf("expected ErrAmbiguousImageRef, got: %v", err)
	}
}

func TestDeleteByBaseKey_ClearsAmbiguousAlias(t *testing.T) {
	cfg := testConfig(t)

	baseKeyA := "aaaa1111bbbb2222_amd64"
	baseKeyB := "cccc3333dddd4444_amd64"
	refA := "https://images.example.com/ubuntu-22.04-server-cloudimg-amd64.img"
	refB := "https://mirror.example.net/ubuntu-22.04-cloudimg-amd64.img"

	if err := Upsert(cfg, refA, baseKeyA, ""); err != nil {
		t.Fatalf("Upsert refA: %v", err)
	}
	if err := Upsert(cfg, refB, baseKeyB, ""); err != nil {
		t.Fatalf("Upsert refB: %v", err)
	}

	if err := DeleteByBaseKey(cfg, baseKeyA); err != nil {
		t.Fatalf("DeleteByBaseKey(baseKeyA): %v", err)
	}

	got, ok, err := ResolveBaseKey(cfg, "ubuntu-22.04-cloudimg")
	if err != nil {
		t.Fatalf("ResolveBaseKey after deleting one base key: %v", err)
	}
	if !ok {
		t.Fatal("ResolveBaseKey should resolve after ambiguity is removed")
	}
	if got != baseKeyB {
		t.Fatalf("ResolveBaseKey=%q, want %q", got, baseKeyB)
	}
}

func TestVerifiedIndexLifecycle(t *testing.T) {
	cfg := testConfig(t)
	baseKey := "aaaa1111bbbb2222_amd64"

	verified, err := IsVerified(cfg, baseKey)
	if err != nil {
		t.Fatalf("IsVerified(initial): %v", err)
	}
	if verified {
		t.Fatal("IsVerified(initial)=true, want false")
	}

	if err := MarkVerified(cfg, baseKey); err != nil {
		t.Fatalf("MarkVerified: %v", err)
	}

	verified, err = IsVerified(cfg, baseKey)
	if err != nil {
		t.Fatalf("IsVerified(after mark): %v", err)
	}
	if !verified {
		t.Fatal("IsVerified(after mark)=false, want true")
	}

	if err := DeleteVerified(cfg, baseKey); err != nil {
		t.Fatalf("DeleteVerified: %v", err)
	}
	verified, err = IsVerified(cfg, baseKey)
	if err != nil {
		t.Fatalf("IsVerified(after delete): %v", err)
	}
	if verified {
		t.Fatal("IsVerified(after delete)=true, want false")
	}
}
