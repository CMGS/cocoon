package image

import "time"

// BuildResult is returned by a successful Builder.Build operation.
type BuildResult struct {
	Tag            string `json:"tag"`
	KernelVersion  string `json:"kernel_version"`
	ManifestDigest string `json:"manifest_digest"`
	LayoutPath     string `json:"layout_path"`
}

// PushResult is returned by a successful Builder.Push operation.
type PushResult struct {
	Ref            string `json:"ref"`
	ManifestDigest string `json:"manifest_digest"`
}

// BuildEntry records one locally built OCI VM image tag.
type BuildEntry struct {
	Tag            string    `json:"tag"`
	LayoutPath     string    `json:"layout_path"`
	ManifestDigest string    `json:"manifest_digest"`
	CreatedAt      time.Time `json:"created_at"`
}

// InspectResult holds metadata from an OCI VM image layout inspection.
type InspectResult struct {
	ManifestDigest string            `json:"manifest_digest"`
	Layers         []InspectLayer    `json:"layers"`
	Config         map[string]any    `json:"config"`
}

// InspectLayer describes a single layer in the OCI manifest.
type InspectLayer struct {
	MediaType string `json:"media_type"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}
