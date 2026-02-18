//go:build linux

package oci

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

type deltaEntryKind uint8

const (
	deltaEntryWhiteout deltaEntryKind = iota
	deltaEntryDir
	deltaEntryFile
	deltaEntrySymlink
)

type fileEntry struct {
	AbsPath string
	RelPath string

	Kind deltaEntryKind
	Mode int64
	UID  int
	GID  int
	Size int64
	Link string
}

type deltaTarEntry struct {
	Name string
	Kind deltaEntryKind
	File *fileEntry
}

// generateDeltaLayerTar compares baseRootfsDir with modifiedRootfsDir and
// emits an OCI-compatible delta tar containing changed/added entries and
// whiteouts for removals.
//
// Returns digest (hex, no prefix), tar size in bytes, and number of entries.
func generateDeltaLayerTar(baseRootfsDir, modifiedRootfsDir, outTarPath string) (string, int64, int, error) {
	baseEntries, err := snapshotRootfs(baseRootfsDir)
	if err != nil {
		return "", 0, 0, fmt.Errorf("snapshot base rootfs: %w", err)
	}
	modifiedEntries, err := snapshotRootfs(modifiedRootfsDir)
	if err != nil {
		return "", 0, 0, fmt.Errorf("snapshot modified rootfs: %w", err)
	}

	deletions := planDeletions(baseEntries, modifiedEntries)
	updates, err := planUpdates(baseEntries, modifiedEntries)
	if err != nil {
		return "", 0, 0, err
	}

	all := deletions
	all = append(all, updates...)
	if len(all) == 0 {
		return "", 0, 0, nil
	}

	sort.Slice(all, func(i, j int) bool {
		return all[i].Name < all[j].Name
	})

	df, err := os.Create(outTarPath) //nolint:gosec // outTarPath is a caller-created temp path
	if err != nil {
		return "", 0, 0, fmt.Errorf("create delta tar: %w", err)
	}
	defer df.Close() //nolint:errcheck

	h := sha256.New()
	mw := io.MultiWriter(df, h)
	tw := tar.NewWriter(mw)

	for _, entry := range all {
		hdr, headerErr := buildDeltaHeader(entry)
		if headerErr != nil {
			_ = tw.Close()
			return "", 0, 0, headerErr
		}
		writeHeaderErr := tw.WriteHeader(hdr)
		if writeHeaderErr != nil {
			_ = tw.Close()
			return "", 0, 0, fmt.Errorf("write delta header %q: %w", hdr.Name, writeHeaderErr)
		}
		if entry.Kind == deltaEntryFile && entry.File != nil && entry.File.Size > 0 {
			f, openErr := os.Open(entry.File.AbsPath) //nolint:gosec // path comes from walked temporary rootfs directory
			if openErr != nil {
				_ = tw.Close()
				return "", 0, 0, fmt.Errorf("open delta file %q: %w", entry.File.AbsPath, openErr)
			}
			if _, copyErr := io.Copy(tw, f); copyErr != nil {
				_ = f.Close()
				_ = tw.Close()
				return "", 0, 0, fmt.Errorf("write delta body %q: %w", entry.Name, copyErr)
			}
			_ = f.Close()
		}
	}

	closeErr := tw.Close()
	if closeErr != nil {
		return "", 0, 0, fmt.Errorf("close delta tar writer: %w", closeErr)
	}
	syncErr := df.Sync()
	if syncErr != nil {
		return "", 0, 0, fmt.Errorf("sync delta tar: %w", syncErr)
	}
	stat, err := df.Stat()
	if err != nil {
		return "", 0, 0, fmt.Errorf("stat delta tar: %w", err)
	}

	return hex.EncodeToString(h.Sum(nil)), stat.Size(), len(all), nil
}

func buildDeltaHeader(entry deltaTarEntry) (*tar.Header, error) {
	switch entry.Kind {
	case deltaEntryWhiteout:
		hdr := &tar.Header{
			Name:     entry.Name,
			Mode:     0,
			Size:     0,
			Typeflag: tar.TypeReg,
		}
		normalizeHeader(hdr)
		return hdr, nil
	case deltaEntryDir:
		if entry.File == nil {
			return nil, fmt.Errorf("delta directory entry %q missing file metadata", entry.Name)
		}
		name := entry.Name
		if !strings.HasSuffix(name, "/") {
			name += "/"
		}
		hdr := &tar.Header{
			Name:     name,
			Mode:     entry.File.Mode,
			Size:     0,
			Typeflag: tar.TypeDir,
			Uid:      entry.File.UID,
			Gid:      entry.File.GID,
		}
		normalizeHeader(hdr)
		return hdr, nil
	case deltaEntryFile:
		if entry.File == nil {
			return nil, fmt.Errorf("delta file entry %q missing file metadata", entry.Name)
		}
		hdr := &tar.Header{
			Name:     entry.Name,
			Mode:     entry.File.Mode,
			Size:     entry.File.Size,
			Typeflag: tar.TypeReg,
			Uid:      entry.File.UID,
			Gid:      entry.File.GID,
		}
		normalizeHeader(hdr)
		return hdr, nil
	case deltaEntrySymlink:
		if entry.File == nil {
			return nil, fmt.Errorf("delta symlink entry %q missing file metadata", entry.Name)
		}
		hdr := &tar.Header{
			Name:     entry.Name,
			Mode:     entry.File.Mode,
			Size:     0,
			Typeflag: tar.TypeSymlink,
			Linkname: entry.File.Link,
			Uid:      entry.File.UID,
			Gid:      entry.File.GID,
		}
		normalizeHeader(hdr)
		return hdr, nil
	default:
		return nil, fmt.Errorf("unsupported delta entry kind for %q", entry.Name)
	}
}

func planUpdates(baseEntries, modifiedEntries map[string]fileEntry) ([]deltaTarEntry, error) {
	paths := make([]string, 0, len(modifiedEntries))
	for path := range modifiedEntries {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	out := make([]deltaTarEntry, 0)
	for _, path := range paths {
		modified := modifiedEntries[path]
		base, exists := baseEntries[path]
		if !exists {
			out = append(out, deltaTarEntry{
				Name: path,
				Kind: modified.Kind,
				File: &modified,
			})
			continue
		}
		changed, err := fileEntryChanged(base, modified)
		if err != nil {
			return nil, fmt.Errorf("compare %q: %w", path, err)
		}
		if changed {
			out = append(out, deltaTarEntry{
				Name: path,
				Kind: modified.Kind,
				File: &modified,
			})
		}
	}
	return out, nil
}

func planDeletions(baseEntries, modifiedEntries map[string]fileEntry) []deltaTarEntry {
	type deletionCandidate struct {
		path  string
		isDir bool
	}
	candidates := make([]deletionCandidate, 0)

	for path, base := range baseEntries {
		modified, exists := modifiedEntries[path]
		if !exists {
			candidates = append(candidates, deletionCandidate{
				path:  path,
				isDir: base.Kind == deltaEntryDir,
			})
			continue
		}
		if base.Kind != modified.Kind {
			candidates = append(candidates, deletionCandidate{
				path:  path,
				isDir: base.Kind == deltaEntryDir,
			})
		}
	}

	sort.Slice(candidates, func(i, j int) bool {
		di := strings.Count(candidates[i].path, "/")
		dj := strings.Count(candidates[j].path, "/")
		if di != dj {
			return di < dj
		}
		return candidates[i].path < candidates[j].path
	})

	removedDirs := make(map[string]struct{})
	out := make([]deltaTarEntry, 0, len(candidates))
	seenWhiteouts := make(map[string]struct{})
	for _, candidate := range candidates {
		if hasRemovedDirAncestor(candidate.path, removedDirs) {
			continue
		}
		parent := filepath.Dir(candidate.path)
		if parent == "." {
			parent = ""
		}
		baseName := filepath.Base(candidate.path)
		if baseName == "." || baseName == string(filepath.Separator) {
			continue
		}
		whiteoutName := ".wh." + baseName
		if parent != "" {
			whiteoutName = filepath.ToSlash(filepath.Join(parent, whiteoutName))
		}
		if _, exists := seenWhiteouts[whiteoutName]; !exists {
			seenWhiteouts[whiteoutName] = struct{}{}
			out = append(out, deltaTarEntry{
				Name: whiteoutName,
				Kind: deltaEntryWhiteout,
			})
		}
		if candidate.isDir {
			removedDirs[candidate.path] = struct{}{}
		}
	}

	return out
}

func hasRemovedDirAncestor(path string, removedDirs map[string]struct{}) bool {
	parent := filepath.Dir(path)
	for parent != "." && parent != "/" {
		parent = filepath.ToSlash(parent)
		if _, ok := removedDirs[parent]; ok {
			return true
		}
		next := filepath.Dir(parent)
		if next == parent {
			break
		}
		parent = next
	}
	return false
}

func fileEntryChanged(base, modified fileEntry) (bool, error) {
	if base.Kind != modified.Kind {
		return true, nil
	}
	if base.Mode != modified.Mode || base.UID != modified.UID || base.GID != modified.GID {
		return true, nil
	}
	switch base.Kind {
	case deltaEntryDir:
		return false, nil
	case deltaEntrySymlink:
		return base.Link != modified.Link, nil
	case deltaEntryFile:
		if base.Size != modified.Size {
			return true, nil
		}
		equal, err := filesEqualByHash(base.AbsPath, modified.AbsPath)
		if err != nil {
			return false, err
		}
		return !equal, nil
	default:
		return true, nil
	}
}

func filesEqualByHash(pathA, pathB string) (bool, error) {
	hashA, err := fileSHA256(pathA)
	if err != nil {
		return false, err
	}
	hashB, err := fileSHA256(pathB)
	if err != nil {
		return false, err
	}
	return hashA == hashB, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // path is from the rootfs snapshot walk
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close() //nolint:errcheck

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func snapshotRootfs(rootDir string) (map[string]fileEntry, error) {
	entries := make(map[string]fileEntry)
	err := filepath.WalkDir(rootDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == rootDir {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(rootDir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." || rel == "" {
			return nil
		}

		mode := info.Mode()
		entry := fileEntry{
			AbsPath: path,
			RelPath: rel,
			Mode:    modeToUnixPerm(mode),
			Size:    info.Size(),
		}
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			entry.UID = int(stat.Uid)
			entry.GID = int(stat.Gid)
		}

		switch {
		case mode.IsDir():
			entry.Kind = deltaEntryDir
			entry.Size = 0
		case mode&os.ModeSymlink != 0:
			target, err := os.Readlink(path) //nolint:gosec // path from trusted walk root
			if err != nil {
				return fmt.Errorf("readlink %s: %w", path, err)
			}
			entry.Kind = deltaEntrySymlink
			entry.Link = target
			entry.Size = 0
		case mode.IsRegular():
			entry.Kind = deltaEntryFile
		case shouldSkipSnapshotMode(mode):
			log.Printf("warning: skipping special file in rootfs snapshot: %s (%s)", rel, mode.String())
			return nil
		default:
			log.Printf("warning: skipping unsupported file type in rootfs snapshot: %s (%s)", rel, mode.String())
			return nil
		}
		entries[rel] = entry
		return nil
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}

func modeToUnixPerm(mode fs.FileMode) int64 {
	unixMode := int64(mode.Perm())
	if mode&os.ModeSetuid != 0 {
		unixMode |= 0o4000
	}
	if mode&os.ModeSetgid != 0 {
		unixMode |= 0o2000
	}
	if mode&os.ModeSticky != 0 {
		unixMode |= 0o1000
	}
	return unixMode
}

func shouldSkipSnapshotMode(mode fs.FileMode) bool {
	if mode&os.ModeDevice != 0 {
		return true
	}
	if mode&os.ModeNamedPipe != 0 {
		return true
	}
	if mode&os.ModeSocket != 0 {
		return true
	}
	if mode&os.ModeIrregular != 0 {
		return true
	}
	return false
}
