package oci

import (
	"context"
	"encoding/json"
	"path/filepath"

	"github.com/CMGS/cocoon/config"
	"github.com/CMGS/cocoon/image"
)

// builder implements image.Builder using the oci package functions.
type builder struct {
	cfg   *config.CocoonConfig
	store *Store
}

// NewBuilder returns an image.Builder backed by OCI VM image operations.
func NewBuilder(cfg *config.CocoonConfig) image.Builder {
	return &builder{
		cfg:   cfg,
		store: NewStore(cfg),
	}
}

func (b *builder) Build(ctx context.Context, imagePath, tag string, cocoonfile string) (*image.BuildResult, error) {
	var cf *Cocoonfile
	if cocoonfile != "" {
		parsed, err := ParseCocoonfile(cocoonfile)
		if err != nil {
			return nil, err
		}
		// Set BaseDir so COPY source paths resolve relative to the
		// Cocoonfile's directory, not the caller's working directory.
		parsed.BaseDir = filepath.Dir(cocoonfile)
		cf = parsed
	}

	result, err := Build(ctx, b.cfg, imagePath, tag, cf)
	if err != nil {
		return nil, err
	}

	return &image.BuildResult{
		Tag:            result.Tag,
		KernelVersion:  result.KernelVersion,
		ManifestDigest: result.ManifestDigest,
		LayoutPath:     result.LayoutPath,
	}, nil
}

func (b *builder) Push(ctx context.Context, ref string) (*image.PushResult, error) {
	result, err := Push(ctx, b.cfg, ref)
	if err != nil {
		return nil, err
	}

	return &image.PushResult{
		Ref:            result.Ref,
		ManifestDigest: result.ManifestDigest,
	}, nil
}

func (b *builder) Login(ctx context.Context, registry, username, password string) error {
	return Login(ctx, registry, username, password)
}

func (b *builder) Inspect(_ context.Context, ref string) (*image.InspectResult, error) {
	layoutPath, err := b.store.ResolveTag(ref)
	if err != nil {
		return nil, err
	}

	info, err := InspectLayout(layoutPath)
	if err != nil {
		return nil, err
	}

	// Convert VMImageConfig to map[string]any for generic output.
	configData, err := json.Marshal(info.Config)
	if err != nil {
		return nil, err
	}
	var configMap map[string]any
	if err := json.Unmarshal(configData, &configMap); err != nil {
		return nil, err
	}

	layers := make([]image.InspectLayer, 0, len(info.Layers))
	for _, l := range info.Layers {
		layers = append(layers, image.InspectLayer{
			MediaType: l.MediaType,
			Digest:    l.Digest,
			Size:      l.Size,
		})
	}

	return &image.InspectResult{
		ManifestDigest: info.ManifestDigest,
		Layers:         layers,
		Config:         configMap,
	}, nil
}

func (b *builder) ListBuilds(_ context.Context) ([]image.BuildEntry, error) {
	tags, err := b.store.ListTags()
	if err != nil {
		return nil, err
	}

	entries := make([]image.BuildEntry, 0, len(tags))
	for _, t := range tags {
		entries = append(entries, image.BuildEntry{
			Tag:            t.Tag,
			LayoutPath:     t.LayoutPath,
			ManifestDigest: t.ManifestDigest,
			CreatedAt:      t.CreatedAt,
		})
	}
	return entries, nil
}
