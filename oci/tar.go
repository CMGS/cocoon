package oci

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

// normalizeHeader zeroes timestamps and strips ACL xattrs for deterministic output.
// Preserves UID/GID/mode/xattrs/symlinks per docs/04.1-oci-vm-images.md Section 3.5.
func normalizeHeader(hdr *tar.Header) {
	epoch := time.Unix(0, 0)
	hdr.ModTime = epoch
	hdr.AccessTime = epoch
	hdr.ChangeTime = epoch
	hdr.Format = tar.FormatPAX

	// Strip ACL xattrs that vary between systems.
	if hdr.PAXRecords != nil {
		for key := range hdr.PAXRecords {
			if strings.HasPrefix(key, "SCHILY.acl.") {
				delete(hdr.PAXRecords, key)
			}
		}
		// Also remove timestamp PAX records that might override our zeroed values.
		delete(hdr.PAXRecords, "atime")
		delete(hdr.PAXRecords, "ctime")
		delete(hdr.PAXRecords, "mtime")
	}
}

// tarEntry holds a tar header and its body content in memory for sorting.
type tarEntry struct {
	header *tar.Header
	body   []byte
}

// rewriteDeterministicTar reads a raw tar from srcTar, sorts entries by path
// (byte-order UTF-8), normalizes headers, and writes a deterministic tar to
// dstTar. Entries whose paths match excludePaths are skipped.
//
// Returns the sha256 digest (hex-encoded, no prefix) and size of the output tar.
//
// NOTE: v1 reads the entire tar into memory for sorting. This is acceptable
// for build machines where cloud images are typically 1-3 GB.
func rewriteDeterministicTar(srcTar, dstTar string, excludePaths []string) (string, int64, error) {
	sf, err := os.Open(srcTar) //nolint:gosec // G304: srcTar is a temp file created by the build process
	if err != nil {
		return "", 0, fmt.Errorf("open source tar: %w", err)
	}
	defer sf.Close() //nolint:errcheck

	// Build exclude set for O(1) lookups.
	excludeSet := make(map[string]struct{}, len(excludePaths))
	for _, p := range excludePaths {
		// Normalize: remove leading/trailing slashes for matching.
		excludeSet[strings.TrimPrefix(p, "/")] = struct{}{}
	}

	// Read all entries into memory.
	tr := tar.NewReader(sf)
	var entries []tarEntry
	for {
		hdr, rerr := tr.Next()
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return "", 0, fmt.Errorf("read tar entry: %w", rerr)
		}

		// Check exclude list.
		cleanName := strings.TrimPrefix(hdr.Name, "./")
		cleanName = strings.TrimPrefix(cleanName, "/")
		if _, excluded := excludeSet[cleanName]; excluded {
			continue
		}

		var body []byte
		if hdr.Size > 0 {
			body = make([]byte, hdr.Size)
			if _, rerr := io.ReadFull(tr, body); rerr != nil {
				return "", 0, fmt.Errorf("read tar entry body %q: %w", hdr.Name, rerr)
			}
		}

		normalizeHeader(hdr)
		entries = append(entries, tarEntry{header: hdr, body: body})
	}

	// Sort by path (byte-order UTF-8).
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].header.Name < entries[j].header.Name
	})

	// Write sorted entries to destination tar, computing digest as we go.
	df, err := os.Create(dstTar) //nolint:gosec // G304: dstTar is a temp file
	if err != nil {
		return "", 0, fmt.Errorf("create destination tar: %w", err)
	}
	defer df.Close() //nolint:errcheck

	h := sha256.New()
	mw := io.MultiWriter(df, h)
	tw := tar.NewWriter(mw)

	for _, entry := range entries {
		if werr := tw.WriteHeader(entry.header); werr != nil {
			return "", 0, fmt.Errorf("write tar header %q: %w", entry.header.Name, werr)
		}
		if len(entry.body) > 0 {
			if _, werr := tw.Write(entry.body); werr != nil {
				return "", 0, fmt.Errorf("write tar body %q: %w", entry.header.Name, werr)
			}
		}
	}

	if err := tw.Close(); err != nil {
		return "", 0, fmt.Errorf("close tar writer: %w", err)
	}
	if err := df.Sync(); err != nil {
		return "", 0, fmt.Errorf("sync destination tar: %w", err)
	}

	stat, err := df.Stat()
	if err != nil {
		return "", 0, fmt.Errorf("stat destination tar: %w", err)
	}

	digest := hex.EncodeToString(h.Sum(nil))
	return digest, stat.Size(), nil
}

// buildKernelLayerTar creates a two-entry tar containing /vmlinuz and /initrd.img.
// The entries are already in sorted order (initrd.img < vmlinuz).
//
// Returns the sha256 digest (hex-encoded, no prefix) and size of the tar.
func buildKernelLayerTar(kernelPath, initrdPath, outPath string) (string, int64, error) {
	type fileEntry struct {
		tarName  string
		srcPath  string
	}

	// Sorted order: initrd.img before vmlinuz.
	files := []fileEntry{
		{"initrd.img", initrdPath},
		{"vmlinuz", kernelPath},
	}

	of, err := os.Create(outPath) //nolint:gosec // G304: outPath is a temp file
	if err != nil {
		return "", 0, fmt.Errorf("create kernel layer tar: %w", err)
	}
	defer of.Close() //nolint:errcheck

	h := sha256.New()
	mw := io.MultiWriter(of, h)
	tw := tar.NewWriter(mw)

	for _, f := range files {
		stat, serr := os.Stat(f.srcPath)
		if serr != nil {
			return "", 0, fmt.Errorf("stat %s: %w", f.srcPath, serr)
		}

		hdr := &tar.Header{
			Name:    f.tarName,
			Size:    stat.Size(),
			Mode:    0o644,
			ModTime: time.Unix(0, 0),
			Format:  tar.FormatPAX,
		}
		if werr := tw.WriteHeader(hdr); werr != nil {
			return "", 0, fmt.Errorf("write header for %s: %w", f.tarName, werr)
		}

		sf, oerr := os.Open(f.srcPath) //nolint:gosec // G304: srcPath is from temp extraction dir
		if oerr != nil {
			return "", 0, fmt.Errorf("open %s: %w", f.srcPath, oerr)
		}
		if _, cerr := io.Copy(tw, sf); cerr != nil {
			sf.Close() //nolint:errcheck
			return "", 0, fmt.Errorf("copy %s: %w", f.srcPath, cerr)
		}
		sf.Close() //nolint:errcheck
	}

	if err := tw.Close(); err != nil {
		return "", 0, fmt.Errorf("close kernel tar writer: %w", err)
	}
	if err := of.Sync(); err != nil {
		return "", 0, fmt.Errorf("sync kernel layer tar: %w", err)
	}

	stat, err := of.Stat()
	if err != nil {
		return "", 0, fmt.Errorf("stat kernel layer tar: %w", err)
	}

	digest := hex.EncodeToString(h.Sum(nil))
	return digest, stat.Size(), nil
}
