package utils

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestAtomicWriteFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	content := []byte("hello, cocoon")

	if err := AtomicWriteFile(path, content, 0o600); err != nil {
		t.Fatalf("AtomicWriteFile error: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile error: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("content = %q, want %q", got, content)
	}
}

func TestAtomicWriteFile_Permissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "perm.txt")

	if err := AtomicWriteFile(path, []byte("data"), 0o600); err != nil {
		t.Fatalf("AtomicWriteFile error: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat error: %v", err)
	}
	// On Unix the mode bits should match exactly.
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("permissions = %o, want 600", perm)
	}
}

func TestAtomicWriteFile_NonExistentDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "noexist", "sub", "file.txt")
	err := AtomicWriteFile(path, []byte("data"), 0o644)
	if err == nil {
		t.Fatal("expected error writing to non-existent directory, got nil")
	}
}

func TestAtomicWriteJSON_RoundTrip(t *testing.T) {
	type sample struct {
		Name  string `json:"name"`
		Count int    `json:"count"`
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "data.json")

	original := sample{Name: "cocoon", Count: 42}
	if err := AtomicWriteJSON(path, original); err != nil {
		t.Fatalf("AtomicWriteJSON error: %v", err)
	}

	var loaded sample
	if err := ReadJSON(path, &loaded); err != nil {
		t.Fatalf("ReadJSON error: %v", err)
	}
	if loaded != original {
		t.Errorf("got %+v, want %+v", loaded, original)
	}
}

func TestAtomicWriteJSON_ComplexStruct(t *testing.T) {
	type inner struct {
		Tags []string `json:"tags"`
	}
	type outer struct {
		ID    string `json:"id"`
		Inner inner  `json:"inner"`
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "complex.json")

	original := outer{
		ID:    "vm-001",
		Inner: inner{Tags: []string{"a", "b", "c"}},
	}
	if err := AtomicWriteJSON(path, original); err != nil {
		t.Fatalf("AtomicWriteJSON error: %v", err)
	}

	var loaded outer
	if err := ReadJSON(path, &loaded); err != nil {
		t.Fatalf("ReadJSON error: %v", err)
	}
	if loaded.ID != original.ID {
		t.Errorf("ID = %q, want %q", loaded.ID, original.ID)
	}
	if len(loaded.Inner.Tags) != 3 {
		t.Errorf("tags count = %d, want 3", len(loaded.Inner.Tags))
	}
}

func TestAtomicWriteJSON_TrailingNewline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nl.json")

	if err := AtomicWriteJSON(path, map[string]int{"x": 1}); err != nil {
		t.Fatalf("AtomicWriteJSON error: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile error: %v", err)
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		t.Error("expected file to end with a newline")
	}
}

func TestAtomicWriteJSON_Indented(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "indent.json")

	if err := AtomicWriteJSON(path, map[string]string{"key": "value"}); err != nil {
		t.Fatalf("AtomicWriteJSON error: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile error: %v", err)
	}
	if !bytes.Contains(data, []byte("  ")) {
		t.Error("expected indented JSON (contains two-space indent)")
	}
}

func TestReadJSON_ValidFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "valid.json")
	if err := os.WriteFile(path, []byte(`{"key":"val"}`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	var m map[string]string
	if err := ReadJSON(path, &m); err != nil {
		t.Fatalf("ReadJSON error: %v", err)
	}
	if m["key"] != "val" {
		t.Errorf("key = %q, want val", m["key"])
	}
}

func TestReadJSON_NonExistentFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.json")
	var m map[string]string
	err := ReadJSON(path, &m)
	if err == nil {
		t.Fatal("expected error for non-existent file, got nil")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected os.ErrNotExist, got %v", err)
	}
}

func TestReadJSON_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(path, []byte("{bad}"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	var m map[string]string
	if err := ReadJSON(path, &m); err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}
