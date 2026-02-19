package oci

import "time"

// OCI media types for Cocoon VM images.
// See docs/04.1-oci-vm-images.md Section 2.1.
const (
	MediaTypeVMConfig    = "application/vnd.cocoon.vm.config.v1+json"
	MediaTypeKernelLayer = "application/vnd.cocoon.vm.kernel.v1.tar"
	MediaTypeRootfsLayer = "application/vnd.cocoon.vm.rootfs.v1.tar"
	ArtifactTypeVMImage  = "application/vnd.cocoon.vm.image.v1"
)

// VMImageConfig is the config blob stored in the OCI manifest.
// See docs/04.1-oci-vm-images.md Section 2.2.
type VMImageConfig struct {
	Arch            string            `json:"arch"`
	DefaultCPUs     int               `json:"default_cpus"`
	DefaultMemoryMB int               `json:"default_memory_mb"`
	KernelCmdline   string            `json:"kernel_cmdline"`
	KernelPath      string            `json:"kernel_path"`
	InitrdPath      string            `json:"initrd_path"`
	VirtiofsTag     string            `json:"virtiofs_tag"`
	RootfsPartUUID  string            `json:"rootfs_partuuid,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
}

// KernelInfo holds detected kernel and initrd paths from a cloud image.
type KernelInfo struct {
	// Version is the kernel version string (e.g., "5.15.0-100-generic").
	Version string
	// KernelPath is the full path inside the image (e.g., "/boot/vmlinuz-5.15.0-100-generic").
	KernelPath string
	// InitrdPath is the matched initrd path (e.g., "/boot/initrd.img-5.15.0-100-generic").
	InitrdPath string
}

// BuildResult is returned by a successful Build operation.
type BuildResult struct {
	// Tag is the OCI reference tag assigned to this build.
	Tag string `json:"tag"`
	// KernelVersion is the detected kernel version.
	KernelVersion string `json:"kernel_version"`
	// ManifestDigest is the sha256 digest of the OCI manifest.
	ManifestDigest string `json:"manifest_digest"`
	// LayoutPath is the local filesystem path to the OCI layout directory.
	LayoutPath string `json:"layout_path"`
}

// PushResult is returned by a successful Push operation.
type PushResult struct {
	// Ref is the full registry reference that was pushed.
	Ref string `json:"ref"`
	// ManifestDigest is the sha256 digest of the pushed manifest.
	ManifestDigest string `json:"manifest_digest"`
}

// PullResult is returned by a successful Pull operation.
type PullResult struct {
	// Ref is the full registry reference that was pulled.
	Ref string `json:"ref"`
	// ManifestDigest is the sha256 digest of the pulled manifest.
	ManifestDigest string `json:"manifest_digest"`
	// LayoutPath is the local filesystem path to the OCI layout directory.
	LayoutPath string `json:"layout_path"`
}

// TagEntry records one local OCI build tag mapping.
type TagEntry struct {
	// Tag is the OCI reference (e.g., "myregistry.io/ubuntu-vm:22.04").
	Tag string `json:"tag"`
	// LayoutPath is the absolute path to the OCI layout directory.
	LayoutPath string `json:"layout_path"`
	// ManifestDigest is the sha256 digest of the manifest.
	ManifestDigest string `json:"manifest_digest"`
	// CreatedAt is when the build was created.
	CreatedAt time.Time `json:"created_at"`
}

// TagIndex is the top-level structure of the oci-build-tags.json file.
type TagIndex struct {
	Tags map[string]TagEntry `json:"tags"`
}

// ociManifest is a minimal OCI image manifest.
type ociManifest struct {
	SchemaVersion int             `json:"schemaVersion"`
	MediaType     string          `json:"mediaType"`
	ArtifactType  string          `json:"artifactType,omitempty"`
	Config        ociDescriptor   `json:"config"`
	Layers        []ociDescriptor `json:"layers"`
}

// ociDescriptor is an OCI content descriptor.
type ociDescriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

// ociIndex is a minimal OCI image index.
type ociIndex struct {
	SchemaVersion int             `json:"schemaVersion"`
	MediaType     string          `json:"mediaType"`
	Manifests     []ociDescriptor `json:"manifests"`
}
