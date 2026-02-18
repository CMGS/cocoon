package image

import "time"

// ImageType classifies the source of an image reference.
type ImageType int

const (
	// ImageTypeOCI is an OCI registry reference (e.g., "docker.io/library/ubuntu:22.04").
	ImageTypeOCI ImageType = iota
	// ImageTypeURL is an HTTP/HTTPS URL to a cloud image file.
	ImageTypeURL
	// ImageTypeLocalFile is a local filesystem path to a qcow2 or raw image.
	ImageTypeLocalFile
)

// String returns the human-readable name of the image type.
func (t ImageType) String() string {
	switch t {
	case ImageTypeOCI:
		return "oci"
	case ImageTypeURL:
		return "url"
	case ImageTypeLocalFile:
		return "local"
	default:
		return "unknown"
	}
}

// ImageIdentity holds the content-addressed identity of a base image.
// The identity is derived from the image manifest (for OCI images) or file
// content (for cloud images/local files). It serves as the cache key and
// references.json key for deduplication.
//
// See docs/04-oci-conversion.md Section 6.2 for the checksum contract.
type ImageIdentity struct {
	// Checksum is the first 16 hex characters (64 bits) of the full SHA-256
	// digest. Used as part of the cache filename and base_key.
	Checksum string `json:"checksum"`

	// Arch is the architecture string (e.g., "amd64", "arm64").
	// Combined with Checksum to form the base_key.
	Arch string `json:"arch"`

	// FullDigest is the complete 64-character hex SHA-256 digest.
	// Stored in references.json for collision verification.
	FullDigest string `json:"full_digest"`

	// ManifestDigest is the sha256 of the raw OCI manifest JSON.
	// Used to pin buildah pull to the exact manifest identified by skopeo,
	// preventing TOCTOU races when a tag is updated between identify and pull.
	// Only set for OCI images.
	ManifestDigest string `json:"manifest_digest,omitempty"`

	// SourceRef is the original image reference as provided by the user
	// (e.g., "docker.io/library/ubuntu:22.04" or a file path).
	SourceRef string `json:"source_ref"`

	// ImageType records how the image was sourced.
	ImageType ImageType `json:"image_type"`

	// TempPath is the transient filesystem path where the pulled image is
	// stored before conversion. It is not persisted to JSON.
	TempPath string `json:"-"`

	// ContainerID is the buildah container ID for OCI images.
	// Transient; needed for cleanup after conversion.
	ContainerID string `json:"-"`
}

// BaseKey returns the content-addressed key: {checksum_16}_{arch}.
// This is used as the key in references.json, lock filenames, and cache
// filenames.
func (id *ImageIdentity) BaseKey() string {
	return id.Checksum + "_" + id.Arch
}

// CacheFilename returns the content-addressed filename: {checksum_16}_{arch}.qcow2.
func (id *ImageIdentity) CacheFilename() string {
	return id.Checksum + "_" + id.Arch + ".qcow2"
}

// BootCheckResult reports the findings from a bootability verification.
type BootCheckResult struct {
	// Bootable indicates whether the image meets the minimum boot contract
	// requirements (kernel + initrd + UEFI bootloader + systemd).
	Bootable bool `json:"bootable"`

	// BootModes lists the boot modes the image supports.
	// Phase 1: "uefi" (CLOUDHV.fd firmware).
	// Phase 2 will add: "direct" (kernel boot for OCI VM images).
	BootModes []string `json:"boot_modes"`

	// Errors lists critical issues that prevent booting.
	// If any errors are present, Bootable will be false.
	Errors []string `json:"errors,omitempty"`

	// Warnings lists non-critical issues detected during verification.
	// Warnings do not prevent booting but may affect functionality.
	Warnings []string `json:"warnings,omitempty"`

	// KernelFound indicates whether a Linux kernel was detected.
	KernelFound bool `json:"kernel_found"`

	// InitrdFound indicates whether an initrd/initramfs was detected.
	InitrdFound bool `json:"initrd_found"`

	// SystemdFound indicates whether systemd was detected as the init system.
	SystemdFound bool `json:"systemd_found"`

	// BootloaderFound indicates whether a UEFI bootloader (GRUB) was detected.
	BootloaderFound bool `json:"bootloader_found"`
}

// CachedImage describes a base image stored in the image cache.
type CachedImage struct {
	// BaseKey is the content-addressed key ({checksum_16}_{arch}).
	BaseKey string `json:"base_key"`

	// Path is the absolute filesystem path to the cached qcow2 file.
	Path string `json:"path"`

	// Size is the file size in bytes.
	Size int64 `json:"size"`

	// CreatedAt is the time the cached image was created.
	CreatedAt time.Time `json:"created_at"`

	// RefCount is the number of VMs currently referencing this base image.
	// A zero refcount means the image is eligible for garbage collection.
	RefCount int `json:"ref_count"`
}
