package image

import "context"

// Builder handles OCI VM image build, push, login, and inspect operations.
// The oci package provides the concrete implementation.
type Builder interface {
	// Build creates an OCI VM image from a cloud image.
	// When cocoonfile is non-empty, it is parsed and its RUN/COPY steps are
	// applied to the image before packaging.
	Build(ctx context.Context, imagePath, tag string, cocoonfile string) (*BuildResult, error)

	// Push uploads a locally built OCI VM image to a container registry.
	Push(ctx context.Context, ref string) (*PushResult, error)

	// Login stores registry credentials for subsequent push operations.
	Login(ctx context.Context, registry, username, password string) error

	// Inspect returns metadata for a locally built OCI VM image.
	Inspect(ctx context.Context, ref string) (*InspectResult, error)

	// ListBuilds returns all locally built OCI VM image tags.
	ListBuilds(ctx context.Context) ([]BuildEntry, error)
}
