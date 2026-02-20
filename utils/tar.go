package utils

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
)

// PackDirectoryToTar writes sourceDir contents to an uncompressed tar archive.
func PackDirectoryToTar(ctx context.Context, sourceDir, tarPath string) (retErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}

	info, err := os.Stat(sourceDir)
	if err != nil {
		return fmt.Errorf("stat source directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("source path %q is not a directory", sourceDir)
	}

	out, err := os.Create(tarPath) //nolint:gosec // tarPath is an internal local path validated by callers
	if err != nil {
		return fmt.Errorf("create tar archive: %w", err)
	}
	defer func() {
		if cerr := out.Close(); retErr == nil && cerr != nil {
			retErr = fmt.Errorf("close tar archive: %w", cerr)
		}
	}()

	tw := tar.NewWriter(out)
	twClosed := false
	defer func() {
		if twClosed {
			return
		}
		if cerr := tw.Close(); retErr == nil && cerr != nil {
			retErr = fmt.Errorf("close tar writer: %w", cerr)
		}
	}()

	walkErr := filepath.WalkDir(sourceDir, func(entryPath string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entryPath == sourceDir {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("stat %q: %w", entryPath, err)
		}

		// Sockets are runtime artifacts and cannot be represented portably.
		if info.Mode()&os.ModeSocket != 0 {
			return nil
		}

		rel, err := filepath.Rel(sourceDir, entryPath)
		if err != nil {
			return fmt.Errorf("resolve tar entry path for %q: %w", entryPath, err)
		}

		linkTarget := ""
		if info.Mode()&os.ModeSymlink != 0 {
			linkTarget, err = os.Readlink(entryPath)
			if err != nil {
				return fmt.Errorf("read symlink %q: %w", entryPath, err)
			}
		}

		hdr, err := tar.FileInfoHeader(info, linkTarget)
		if err != nil {
			return fmt.Errorf("build tar header for %q: %w", entryPath, err)
		}

		hdr.Name = filepath.ToSlash(rel)
		if info.IsDir() && !strings.HasSuffix(hdr.Name, "/") {
			hdr.Name += "/"
		}

		if werr := tw.WriteHeader(hdr); werr != nil {
			return fmt.Errorf("write tar header %q: %w", hdr.Name, werr)
		}

		if !info.Mode().IsRegular() {
			return nil
		}

		f, err := os.Open(entryPath) //nolint:gosec // entryPath comes from walking a trusted local directory
		if err != nil {
			return fmt.Errorf("open %q: %w", entryPath, err)
		}
		if _, err := io.Copy(tw, f); err != nil {
			_ = f.Close()
			return fmt.Errorf("write file %q to tar: %w", entryPath, err)
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("close %q: %w", entryPath, err)
		}
		return nil
	})
	if walkErr != nil {
		return fmt.Errorf("walk source directory %q: %w", sourceDir, walkErr)
	}

	if err := tw.Close(); err != nil {
		return fmt.Errorf("close tar writer: %w", err)
	}
	twClosed = true
	if err := out.Sync(); err != nil {
		return fmt.Errorf("sync tar archive %q: %w", tarPath, err)
	}
	return nil
}

// ExtractTarToDir extracts a tar archive into targetDir with traversal protection.
func ExtractTarToDir(ctx context.Context, tarPath, targetDir string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if err := os.MkdirAll(targetDir, 0o755); err != nil { //nolint:gosec // targetDir is a caller-controlled local temp path
		return fmt.Errorf("create target directory %q: %w", targetDir, err)
	}

	f, err := os.Open(tarPath) //nolint:gosec // tarPath is an internal local path validated by callers
	if err != nil {
		return fmt.Errorf("open tar archive %q: %w", tarPath, err)
	}
	defer f.Close() //nolint:errcheck,gosec // best-effort close on read-only file

	return extractTarStream(ctx, tarPath, targetDir, tar.NewReader(f), whiteoutNone)
}

// whiteoutMode controls how OCI whiteout entries are processed during extraction.
type whiteoutMode int

const (
	whiteoutNone    whiteoutMode = iota // no whiteout processing
	whiteoutApply                       // apply/consume: delete files (flatten)
	whiteoutOverlay                     // materialize as overlayfs-native format (stack)
)

// ExtractOCILayerTarToDir extracts an OCI diff layer into targetDir while
// applying OCI whiteout semantics:
//   - .wh.<name>: remove <name> from lower layers
//   - .wh..wh..opq: mark directory opaque (remove all lower entries)
//
// Whiteout marker files are not materialized to disk. Use this for flattened
// extraction where all layers merge into a single directory.
func ExtractOCILayerTarToDir(ctx context.Context, tarPath, targetDir string) error {
	return extractOCILayer(ctx, tarPath, targetDir, whiteoutApply)
}

// ExtractOCILayerTarForOverlayDir extracts an OCI diff layer into targetDir
// while converting OCI whiteout entries to overlayfs-native format:
//   - .wh.<name>: create character device 0/0 at <name> (overlayfs whiteout)
//   - .wh..wh..opq: set trusted.overlay.opaque=y xattr on parent directory
//
// Use this for per-layer extraction where layers are stacked via overlayfs
// multi-lowerdir. Requires CAP_MKNOD (Linux only).
func ExtractOCILayerTarForOverlayDir(ctx context.Context, tarPath, targetDir string) error {
	return extractOCILayer(ctx, tarPath, targetDir, whiteoutOverlay)
}

// ExtractOCILayerTarFromReader extracts an OCI diff layer from an io.Reader
// (e.g. a gzip.Reader wrapping a compressed blob) into targetDir while applying
// OCI whiteout semantics (flatten mode). The label is used in error messages.
func ExtractOCILayerTarFromReader(ctx context.Context, label, targetDir string, r io.Reader) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if err := os.MkdirAll(targetDir, 0o755); err != nil { //nolint:gosec // targetDir is a caller-controlled local temp path
		return fmt.Errorf("create target directory %q: %w", targetDir, err)
	}

	return extractTarStream(ctx, label, targetDir, tar.NewReader(r), whiteoutApply)
}

func extractOCILayer(ctx context.Context, tarPath, targetDir string, mode whiteoutMode) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if err := os.MkdirAll(targetDir, 0o755); err != nil { //nolint:gosec // targetDir is a caller-controlled local temp path
		return fmt.Errorf("create target directory %q: %w", targetDir, err)
	}

	f, err := os.Open(tarPath) //nolint:gosec // tarPath is an internal local path validated by callers
	if err != nil {
		return fmt.Errorf("open tar archive %q: %w", tarPath, err)
	}
	defer f.Close() //nolint:errcheck,gosec // best-effort close on read-only file

	return extractTarStream(ctx, tarPath, targetDir, tar.NewReader(f), mode)
}

func extractTarStream(ctx context.Context, tarPath, targetDir string, tr *tar.Reader, mode whiteoutMode) error {
	pendingHardlinks := make([]tar.Header, 0, 8)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar entry from %q: %w", tarPath, err)
		}

		switch mode {
		case whiteoutApply:
			handled, whiteoutErr := applyOCIWhiteout(targetDir, hdr.Name)
			if whiteoutErr != nil {
				return whiteoutErr
			}
			if handled {
				continue
			}
		case whiteoutOverlay:
			handled, whiteoutErr := materializeOverlayWhiteout(targetDir, hdr.Name)
			if whiteoutErr != nil {
				return whiteoutErr
			}
			if handled {
				continue
			}
		case whiteoutNone:
			// No whiteout processing.
		}

		deferred, entryErr := extractTarEntry(tr, hdr, targetDir)
		if entryErr != nil {
			return entryErr
		}
		if deferred {
			pendingHardlinks = append(pendingHardlinks, *hdr)
		}
	}

	if err := resolveDeferredHardlinks(ctx, targetDir, pendingHardlinks); err != nil {
		return fmt.Errorf("resolve deferred hardlinks in %q: %w", tarPath, err)
	}
	return nil
}

var errHardlinkTargetMissing = errors.New("tar hardlink target missing")

func resolveDeferredHardlinks(ctx context.Context, targetDir string, pending []tar.Header) error {
	for len(pending) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}

		next := make([]tar.Header, 0, len(pending))
		progress := false
		for _, hdr := range pending {
			if err := ctx.Err(); err != nil {
				return err
			}
			deferred, err := extractHardlinkEntry(&hdr, targetDir, false)
			if err != nil {
				if errors.Is(err, errHardlinkTargetMissing) {
					next = append(next, hdr)
					continue
				}
				return err
			}
			if deferred {
				next = append(next, hdr)
				continue
			}
			progress = true
		}

		if !progress {
			stuck := next[0]
			return fmt.Errorf("hardlink target unresolved for %q -> %q", stuck.Name, stuck.Linkname)
		}
		pending = next
	}
	return nil
}

func applyOCIWhiteout(targetDir, entryName string) (bool, error) {
	clean := path.Clean(entryName)
	base := path.Base(clean)
	parent := path.Dir(clean)

	// OCI opaque whiteout: hide all lower entries for this directory.
	if base == ".wh..wh..opq" {
		parentPath, resolveErr := resolveTarEntryPath(targetDir, parent)
		if resolveErr != nil {
			return false, fmt.Errorf("invalid opaque whiteout parent %q: %w", parent, resolveErr)
		}
		mkErr := os.MkdirAll(parentPath, 0o755) //nolint:gosec // path is validated under targetDir
		if mkErr != nil {
			return false, fmt.Errorf("create opaque whiteout parent %q: %w", parentPath, mkErr)
		}
		// Opaque whiteout semantics: in a layered union fs, this means "block lower layers".
		// In our flattened extraction (materialize), this means "delete everything currently in this directory".
		rmErr := removeDirChildren(parentPath)
		if rmErr != nil {
			return false, fmt.Errorf("apply opaque whiteout for %q: %w", parentPath, rmErr)
		}
		return true, nil
	}

	// OCI whiteout: remove named entry from lower layers.
	victimBase, isWhiteout := strings.CutPrefix(base, ".wh.")
	if !isWhiteout {
		return false, nil
	}
	victimRel := victimBase
	if parent != "." {
		victimRel = path.Join(parent, victimBase)
	}
	victimPath, resolveErr := resolveTarEntryPath(targetDir, victimRel)
	if resolveErr != nil {
		return false, fmt.Errorf("invalid whiteout target %q: %w", victimRel, resolveErr)
	}
	rmErr := os.RemoveAll(victimPath)
	if rmErr != nil {
		return false, fmt.Errorf("apply whiteout remove %q: %w", victimPath, rmErr)
	}
	return true, nil
}

// materializeOverlayWhiteout converts an OCI whiteout tar entry to
// overlayfs-native format for per-layer stacking via multi-lowerdir:
//   - .wh.<name>  → character device 0/0 at <parent>/<name>
//   - .wh..wh..opq → trusted.overlay.opaque=y xattr on parent directory
//
// Returns (true, nil) when the entry was a whiteout and was materialized,
// (false, nil) when it was not a whiteout entry.
func materializeOverlayWhiteout(targetDir, entryName string) (bool, error) {
	clean := path.Clean(entryName)
	base := path.Base(clean)
	parent := path.Dir(clean)

	// OCI opaque whiteout → overlayfs opaque xattr on parent directory.
	if base == ".wh..wh..opq" {
		parentPath, resolveErr := resolveTarEntryPath(targetDir, parent)
		if resolveErr != nil {
			return false, fmt.Errorf("invalid opaque whiteout parent %q: %w", parent, resolveErr)
		}
		if mkErr := os.MkdirAll(parentPath, 0o755); mkErr != nil { //nolint:gosec // path is validated under targetDir
			return false, fmt.Errorf("create opaque whiteout parent %q: %w", parentPath, mkErr)
		}
		if err := setOverlayOpaqueXattr(parentPath); err != nil {
			return false, fmt.Errorf("set overlay opaque xattr on %q: %w", parentPath, err)
		}
		return true, nil
	}

	// OCI file whiteout → overlayfs character device 0/0.
	victimBase, isWhiteout := strings.CutPrefix(base, ".wh.")
	if !isWhiteout {
		return false, nil
	}
	victimRel := victimBase
	if parent != "." {
		victimRel = path.Join(parent, victimBase)
	}
	victimPath, resolveErr := resolveTarEntryPath(targetDir, victimRel)
	if resolveErr != nil {
		return false, fmt.Errorf("invalid whiteout target %q: %w", victimRel, resolveErr)
	}
	if mkErr := os.MkdirAll(filepath.Dir(victimPath), 0o755); mkErr != nil { //nolint:gosec // path is validated under targetDir
		return false, fmt.Errorf("create whiteout parent dir for %q: %w", victimPath, mkErr)
	}
	if rmErr := os.RemoveAll(victimPath); rmErr != nil {
		return false, fmt.Errorf("clear existing entry at whiteout path %q: %w", victimPath, rmErr)
	}
	if err := createOverlayWhiteoutDevice(victimPath); err != nil {
		return false, fmt.Errorf("create overlay whiteout device at %q: %w", victimPath, err)
	}
	return true, nil
}

func extractTarEntry(tr *tar.Reader, hdr *tar.Header, targetDir string) (bool, error) {
	targetPath, err := resolveTarEntryPath(targetDir, hdr.Name)
	if err != nil {
		return false, fmt.Errorf("invalid tar entry %q: %w", hdr.Name, err)
	}

	switch hdr.Typeflag {
	case tar.TypeDir:
		mode := tarEntryModeOrDefault(hdr, 0o755)
		if err := os.MkdirAll(targetPath, mode); err != nil { //nolint:gosec // extracted path is validated to stay under targetDir
			return false, fmt.Errorf("create directory %q: %w", targetPath, err)
		}
		if err := os.Chmod(targetPath, mode); err != nil {
			return false, fmt.Errorf("set directory mode for %q: %w", targetPath, err)
		}
		if err := applyTarOwnership(targetPath, hdr, false); err != nil {
			return false, fmt.Errorf("set directory ownership for %q: %w", targetPath, err)
		}
	case tar.TypeReg, 0:
		if hdr.Size < 0 {
			return false, fmt.Errorf("invalid negative file size for %q: %d", hdr.Name, hdr.Size)
		}
		if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil { //nolint:gosec // parent path is validated to stay under targetDir
			return false, fmt.Errorf("create parent directory for %q: %w", targetPath, err)
		}
		mode := tarEntryModeOrDefault(hdr, 0o644)
		out, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode) //nolint:gosec // extracted path is validated to stay under targetDir
		if err != nil {
			return false, fmt.Errorf("create file %q: %w", targetPath, err)
		}
		if _, err := io.CopyN(out, tr, hdr.Size); err != nil {
			_ = out.Close()
			return false, fmt.Errorf("write file %q: %w", targetPath, err)
		}
		if err := out.Close(); err != nil {
			return false, fmt.Errorf("close file %q: %w", targetPath, err)
		}
		if err := os.Chmod(targetPath, mode); err != nil {
			return false, fmt.Errorf("set file mode for %q: %w", targetPath, err)
		}
		if err := applyTarOwnership(targetPath, hdr, false); err != nil {
			return false, fmt.Errorf("set file ownership for %q: %w", targetPath, err)
		}
	case tar.TypeSymlink:
		if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil { //nolint:gosec // parent path is validated to stay under targetDir
			return false, fmt.Errorf("create parent directory for symlink %q: %w", targetPath, err)
		}
		if err := os.RemoveAll(targetPath); err != nil {
			return false, fmt.Errorf("remove existing path %q before symlink extraction: %w", targetPath, err)
		}
		if err := os.Symlink(hdr.Linkname, targetPath); err != nil {
			return false, fmt.Errorf("create symlink %q -> %q: %w", targetPath, hdr.Linkname, err)
		}
		if err := applyTarOwnership(targetPath, hdr, true); err != nil {
			return false, fmt.Errorf("set symlink ownership for %q: %w", targetPath, err)
		}
	case tar.TypeLink:
		deferred, err := extractHardlinkEntry(hdr, targetDir, true)
		if err != nil {
			return false, err
		}
		return deferred, nil
	case tar.TypeXHeader, tar.TypeXGlobalHeader:
		// Metadata entries are handled by archive/tar and have no filesystem effect here.
		return false, nil
	case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
		// Device/FIFO nodes are skipped in non-privileged extraction contexts.
		return false, nil
	default:
		return false, fmt.Errorf("unsupported tar entry type %d for %q", hdr.Typeflag, hdr.Name)
	}
	return false, nil
}

func extractHardlinkEntry(hdr *tar.Header, targetDir string, allowDefer bool) (bool, error) {
	linkTarget, err := resolveTarEntryPath(targetDir, hdr.Linkname)
	if err != nil {
		return false, fmt.Errorf("invalid hardlink target %q for %q: %w", hdr.Linkname, hdr.Name, err)
	}
	targetPath, err := resolveTarEntryPath(targetDir, hdr.Name)
	if err != nil {
		return false, fmt.Errorf("invalid tar entry %q: %w", hdr.Name, err)
	}
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil { //nolint:gosec // parent path is validated to stay under targetDir
		return false, fmt.Errorf("create parent directory for hardlink %q: %w", targetPath, err)
	}
	if err := os.RemoveAll(targetPath); err != nil {
		return false, fmt.Errorf("remove existing path %q before hardlink extraction: %w", targetPath, err)
	}
	if err := os.Link(linkTarget, targetPath); err != nil {
		if os.IsNotExist(err) && allowDefer {
			return true, nil
		}
		if os.IsNotExist(err) {
			return false, fmt.Errorf("%w: create hardlink %q -> %q: %v", errHardlinkTargetMissing, targetPath, linkTarget, err)
		}
		return false, fmt.Errorf("create hardlink %q -> %q: %w", targetPath, linkTarget, err)
	}
	if err := applyTarOwnership(targetPath, hdr, false); err != nil {
		return false, fmt.Errorf("set hardlink ownership for %q: %w", targetPath, err)
	}
	return false, nil
}

func resolveTarEntryPath(baseDir, entryName string) (string, error) {
	if strings.TrimSpace(entryName) == "" {
		return "", fmt.Errorf("empty path")
	}

	// Tar entry names are slash-separated regardless of host OS.
	clean := path.Clean(entryName)
	if clean == "" || clean == "." {
		if entryName == "." || entryName == "./" {
			return baseDir, nil
		}
		return "", fmt.Errorf("invalid path %q", entryName)
	}
	if strings.HasPrefix(clean, "/") {
		return "", fmt.Errorf("absolute paths are not allowed")
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("path escapes target directory")
	}
	if strings.Contains(clean, "/../") {
		return "", fmt.Errorf("path escapes target directory")
	}
	candidate := filepath.Join(baseDir, filepath.FromSlash(clean))
	rel, err := filepath.Rel(baseDir, candidate)
	if err != nil {
		return "", fmt.Errorf("resolve relative path: %w", err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes target directory")
	}
	return candidate, nil
}

func tarEntryModeOrDefault(hdr *tar.Header, fallback os.FileMode) os.FileMode {
	mode := hdr.FileInfo().Mode()
	// Strip file type bits (e.g. ModeDir, ModeSymlink), keep only perm and special bits.
	out := mode & os.ModePerm
	// Security hardening: strict stripping of setuid/setgid bits.
	// We do NOT preserve them to prevent privilege escalation attacks from malicious images.
	if mode&os.ModeSticky != 0 {
		out |= os.ModeSticky
	}
	if out == 0 {
		return fallback
	}
	return out
}

func applyTarOwnership(targetPath string, hdr *tar.Header, isSymlink bool) error {
	if hdr == nil {
		return nil
	}
	var err error
	if isSymlink {
		err = os.Lchown(targetPath, hdr.Uid, hdr.Gid)
	} else {
		err = os.Chown(targetPath, hdr.Uid, hdr.Gid)
	}
	if err == nil {
		return nil
	}
	// Best-effort in unprivileged/test contexts.
	if errors.Is(err, os.ErrPermission) ||
		errors.Is(err, syscall.EPERM) ||
		errors.Is(err, syscall.ENOSYS) ||
		errors.Is(err, syscall.EOPNOTSUPP) {
		return nil
	}
	return err
}

func removeDirChildren(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		child := filepath.Join(dir, entry.Name())
		if err := os.RemoveAll(child); err != nil {
			return err
		}
	}
	return nil
}
