package jsonstore

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

type testData struct {
	Items map[string]string `json:"items"`
}

func newTestData() *testData {
	return &testData{Items: make(map[string]string)}
}

func testStore(t *testing.T) *Store[testData] {
	t.Helper()
	dir := t.TempDir()
	return New(
		filepath.Join(dir, "test.lock"),
		filepath.Join(dir, "test.json"),
		newTestData,
	)
}

func TestRead_EmptyFile(t *testing.T) {
	t.Parallel()
	s := testStore(t)

	err := s.Read(func(d *testData) error {
		if d.Items == nil {
			t.Error("expected initialized map, got nil")
		}
		if len(d.Items) != 0 {
			t.Errorf("expected empty map, got %v", d.Items)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Read error: %v", err)
	}
}

func TestUpdate_PersistsData(t *testing.T) {
	t.Parallel()
	s := testStore(t)

	// Write data.
	if err := s.Update(func(d *testData) error {
		d.Items["key"] = "value"
		return nil
	}); err != nil {
		t.Fatalf("Update error: %v", err)
	}

	// Read back and verify.
	if err := s.Read(func(d *testData) error {
		if d.Items["key"] != "value" {
			t.Errorf("expected 'value', got %q", d.Items["key"])
		}
		return nil
	}); err != nil {
		t.Fatalf("Read error: %v", err)
	}
}

func TestUpdate_ErrorDoesNotSave(t *testing.T) {
	t.Parallel()
	s := testStore(t)

	// Seed initial data.
	if err := s.Update(func(d *testData) error {
		d.Items["keep"] = "yes"
		return nil
	}); err != nil {
		t.Fatalf("seed Update error: %v", err)
	}

	// Failing update should not persist.
	want := fmt.Errorf("abort")
	if err := s.Update(func(d *testData) error {
		d.Items["bad"] = "no"
		return want
	}); err != want {
		t.Fatalf("expected %v, got %v", want, err)
	}

	// Verify original data is intact.
	if err := s.Read(func(d *testData) error {
		if d.Items["keep"] != "yes" {
			t.Errorf("seed data lost: %v", d.Items)
		}
		if _, ok := d.Items["bad"]; ok {
			t.Error("failed update should not have persisted 'bad' key")
		}
		return nil
	}); err != nil {
		t.Fatalf("Read error: %v", err)
	}
}

func TestWithEnsureDir(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "sub", "nested")
	s := New(
		filepath.Join(dir, "test.lock"),
		filepath.Join(dir, "test.json"),
		newTestData,
	).WithEnsureDir(dir)

	if err := s.Update(func(d *testData) error {
		d.Items["x"] = "y"
		return nil
	}); err != nil {
		t.Fatalf("Update error: %v", err)
	}

	// Verify directory and file were created.
	if _, err := os.Stat(filepath.Join(dir, "test.json")); err != nil {
		t.Errorf("data file not created: %v", err)
	}
}

// TestBareMapType verifies Store works with bare map types (e.g., map[string]string).
func TestBareMapType(t *testing.T) {
	t.Parallel()
	type nameIndex = map[string]string

	dir := t.TempDir()
	s := New(
		filepath.Join(dir, "test.lock"),
		filepath.Join(dir, "test.json"),
		func() *nameIndex {
			m := make(nameIndex)
			return &m
		},
	)

	// Write via pointer dereference.
	if err := s.Update(func(idxPtr *nameIndex) error {
		idx := *idxPtr
		idx["foo"] = "bar"
		return nil
	}); err != nil {
		t.Fatalf("Update error: %v", err)
	}

	// Read back.
	if err := s.Read(func(idxPtr *nameIndex) error {
		idx := *idxPtr
		if idx["foo"] != "bar" {
			t.Errorf("expected 'bar', got %q", idx["foo"])
		}
		return nil
	}); err != nil {
		t.Fatalf("Read error: %v", err)
	}
}

func TestRead_PropagatesError(t *testing.T) {
	t.Parallel()
	s := testStore(t)

	want := fmt.Errorf("read failed")
	err := s.Read(func(*testData) error { return want })
	if err != want {
		t.Errorf("expected %v, got %v", want, err)
	}
}

func TestMultipleUpdates(t *testing.T) {
	t.Parallel()
	s := testStore(t)

	for i := range 3 {
		key := fmt.Sprintf("key%d", i)
		if err := s.Update(func(d *testData) error {
			d.Items[key] = "val"
			return nil
		}); err != nil {
			t.Fatalf("Update %d error: %v", i, err)
		}
	}

	if err := s.Read(func(d *testData) error {
		if len(d.Items) != 3 {
			t.Errorf("expected 3 items, got %d: %v", len(d.Items), d.Items)
		}
		return nil
	}); err != nil {
		t.Fatalf("Read error: %v", err)
	}
}
