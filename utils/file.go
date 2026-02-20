package utils

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// CopyFile copies src to dst with the given permissions. The destination file
// is fsynced before close to ensure durability. On any failure after the
// destination file is created, it is removed to prevent partial/corrupt files
// from being mistaken for valid content by subsequent operations.
func CopyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src) //nolint:gosec // caller-controlled path
	if err != nil {
		return err
	}
	defer in.Close() //nolint:errcheck

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode) //nolint:gosec // caller-controlled path
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst) // clean up partial file
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		_ = os.Remove(dst) // clean up unsynced file
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dst) // clean up on close failure
		return err
	}
	return nil
}

// FirstExistingPath joins baseDir with each relative path in rels and returns
// the first combination that exists on disk. If none exist it returns an error.
func FirstExistingPath(baseDir string, rels []string) (string, error) {
	for _, rel := range rels {
		p := filepath.Join(baseDir, rel)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("none of %v found under %s", rels, baseDir)
}
