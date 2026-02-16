// Package image provides the interface and implementation for image management
// in Cocoon. It handles pulling OCI images or cloud images, converting them to
// qcow2 base images, verifying bootability, and managing the image cache.
//
// The package uses content-addressed caching: each image is identified by a
// base_key ({checksum_16}_{arch}) derived from the image manifest or file
// content. Conversion is protected by per-image file locks (Level 3) to prevent
// duplicate work across concurrent callers.
//
// See docs/04-oci-conversion.md for the full pipeline specification and
// docs/01-boot-contract.md for bootability requirements.
package image

import "context"

// Manager is the interface for image operations.
// Implementations coordinate pulling, converting, caching, and verifying
// images for use as VM base disks.
type Manager interface {
	// Pull downloads an OCI image or cloud image.
	// The ref can be:
	//   - An OCI registry reference (e.g., "docker.io/library/ubuntu:22.04")
	//   - An HTTP/HTTPS URL to a cloud image (e.g., "https://cloud-images.ubuntu.com/...")
	//   - A local file path to a qcow2 or raw image
	//
	// Returns the ImageIdentity describing the content-addressed identity of the
	// pulled image.
	Pull(ctx context.Context, ref string) (*ImageIdentity, error)

	// Convert transforms a pulled image into a qcow2 base image.
	// Uses a per-image conversion lock (Level 3) at
	// /var/lib/cocoon/cache/locks/{base_key}.lock to prevent duplicate work
	// when multiple callers request the same image concurrently.
	//
	// Returns the filesystem path to the converted base image in the cache.
	Convert(ctx context.Context, identity *ImageIdentity) (baseImagePath string, err error)

	// Prepare is the combined pull+convert+cache pipeline.
	// It first checks whether a cached base image already exists for the given
	// ref. If so, it skips pull and convert entirely. Otherwise it runs
	// Pull + Convert and caches the result.
	//
	// Returns the image identity, cached base image path, and any error.
	Prepare(ctx context.Context, ref string) (*ImageIdentity, string, error)

	// VerifyBootability checks if an image meets the Cocoon boot contract.
	// The image must contain:
	//   - A Linux kernel (/boot/vmlinuz*)
	//   - An initrd (/boot/initrd* or /boot/initramfs*)
	//   - A UEFI bootloader (EFI/BOOT/BOOT*.EFI or similar)
	//   - systemd as the init system (/sbin/init -> systemd)
	//
	// Phase 1 supports UEFI boot only (via CLOUDHV.fd firmware).
	// Direct kernel boot will be added in Phase 2 for OCI VM images.
	VerifyBootability(ctx context.Context, imagePath string) (*BootCheckResult, error)

	// ListCached returns all cached base images in the image cache directory.
	ListCached(ctx context.Context) ([]*CachedImage, error)

	// RemoveCached removes a cached base image identified by its base_key
	// ({checksum_16}_{arch}). Callers should ensure no VMs reference the image
	// before removal.
	RemoveCached(ctx context.Context, baseKey string) error
}
