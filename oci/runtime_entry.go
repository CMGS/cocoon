package oci

import (
	"fmt"
	"os"

	"github.com/CMGS/cocoon/utils"
)

// OCIRuntimeEntryMeta describes a materialized OCI runtime entry.
// Each entry maps a manifest (runtimeKey) to the layer digests that comprise
// the kernel and rootfs, enabling per-layer dedup via shared layer cache.
type OCIRuntimeEntryMeta struct {
	ManifestDigest     string   `json:"manifest_digest"`
	KernelLayerDigest  string   `json:"kernel_layer_digest"`
	RootfsLayerDigests []string `json:"rootfs_layer_digests"` // base-to-top order
	Arch               string   `json:"arch"`
	KernelCmdline      string   `json:"kernel_cmdline,omitempty"`
	KernelPath         string   `json:"kernel_path,omitempty"` // relative path in kernel layer
	InitrdPath         string   `json:"initrd_path,omitempty"` // relative path in kernel layer
	VirtioFSTag        string   `json:"virtiofs_tag"`
}

// WriteEntryMeta atomically writes entry metadata to path.
func WriteEntryMeta(path string, meta *OCIRuntimeEntryMeta) error {
	if meta == nil {
		return fmt.Errorf("entry meta is nil")
	}
	return utils.AtomicWriteJSON(path, meta)
}

// ReadEntryMeta reads and validates entry metadata from path.
func ReadEntryMeta(path string) (*OCIRuntimeEntryMeta, error) {
	var meta OCIRuntimeEntryMeta
	if err := utils.ReadJSON(path, &meta); err != nil {
		return nil, fmt.Errorf("read entry meta %s: %w", path, err)
	}
	if meta.KernelLayerDigest == "" {
		return nil, fmt.Errorf("entry meta %s: kernel_layer_digest is empty", path)
	}
	if len(meta.RootfsLayerDigests) == 0 {
		return nil, fmt.Errorf("entry meta %s: rootfs_layer_digests is empty", path)
	}
	return &meta, nil
}

// EntryMetaExists reports whether the entry metadata file exists.
func EntryMetaExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
