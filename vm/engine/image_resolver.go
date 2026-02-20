package engine

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/CMGS/cocoon/config"
	"github.com/CMGS/cocoon/image/refcache"
	"github.com/CMGS/cocoon/oci"
	"github.com/CMGS/cocoon/types"
)

type runtimeImageSource string

const (
	runtimeImageSourceLocalPath   runtimeImageSource = "local_path"
	runtimeImageSourceLocalOCITag runtimeImageSource = "local_oci_tag"
	runtimeImageSourceLocalCache  runtimeImageSource = "local_cache_alias"
	runtimeImageSourceURL         runtimeImageSource = "url"
	runtimeImageSourceRegistry    runtimeImageSource = "registry"
)

type resolvedRuntimeImage struct {
	OriginalRef string
	PrepareRef  string
	Source      runtimeImageSource
	VMImageType types.VMImageType

	LocalOCITag  string
	LocalBaseKey string
}

type registryProbeFunc func(ctx context.Context, cfg *config.CocoonConfig, ref string) (types.VMImageType, error)

func resolveRuntimeImageRefWithProbe(
	ctx context.Context,
	cfg *config.CocoonConfig,
	ref string,
	registryProbe registryProbeFunc,
) (*resolvedRuntimeImage, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, fmt.Errorf("image reference is empty")
	}

	if isExplicitLocalPath(ref) {
		return resolveLocalPathRef(ref)
	}

	store := oci.NewStore(cfg)
	resolvedOCITag, ociExists, err := oci.ResolveLocalTagRef(store, ref)
	if err != nil {
		return nil, fmt.Errorf("check local OCI tag %q: %w", ref, err)
	}

	localBaseKey, cacheExists, err := refcache.ResolveBaseKey(cfg, ref)
	if err != nil {
		if errors.Is(err, refcache.ErrAmbiguousImageRef) {
			return nil, fmt.Errorf("image reference %q is ambiguous in local cache: %w", ref, err)
		}
		return nil, fmt.Errorf("resolve local cached image %q: %w", ref, err)
	}

	if ociExists && cacheExists {
		different, diffErr := localOCITagAndCacheRefDiffer(cfg, resolvedOCITag, localBaseKey)
		if diffErr != nil {
			return nil, fmt.Errorf("compare local OCI and cached image identities for %q: %w", ref, diffErr)
		}
		if different {
			return nil, fmt.Errorf(
				"ambiguous image reference %q: local OCI tag %q and local cache alias base_key=%s point to different identities; use explicit ref",
				ref, resolvedOCITag, localBaseKey,
			)
		}
	}
	if ociExists {
		return &resolvedRuntimeImage{
			OriginalRef: ref,
			PrepareRef:  resolvedOCITag,
			Source:      runtimeImageSourceLocalOCITag,
			VMImageType: types.VMImageTypeOCIVM,
			LocalOCITag: resolvedOCITag,
		}, nil
	}
	if cacheExists {
		return &resolvedRuntimeImage{
			OriginalRef:  ref,
			PrepareRef:   ref,
			Source:       runtimeImageSourceLocalCache,
			VMImageType:  types.VMImageTypeQCOW2,
			LocalBaseKey: localBaseKey,
		}, nil
	}

	// Bare filename fallback: if ref is a simple filename (no path
	// separators) that exists as a regular file on disk (e.g. "ubuntu.qcow2"
	// in CWD), treat it as a local path before hitting the network.
	// Refs containing "/" (e.g. "user/repo") are left for registry probe
	// to avoid shadowing valid registry references with local directories.
	if !strings.Contains(ref, "/") {
		if fi, statErr := os.Stat(ref); statErr == nil && fi.Mode().IsRegular() {
			return resolveLocalPathRef(ref)
		}
	}

	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		return &resolvedRuntimeImage{
			OriginalRef: ref,
			PrepareRef:  ref,
			Source:      runtimeImageSourceURL,
			VMImageType: types.VMImageTypeQCOW2,
		}, nil
	}
	vmType, probeErr := registryProbe(ctx, cfg, ref)
	if probeErr != nil {
		log.Printf("ERROR: registry probe failed for %q — if this is a Cocoon OCI VM image, check network connectivity and registry credentials: %v", ref, probeErr)
		return nil, fmt.Errorf("probe registry image type for %q: %w", ref, probeErr)
	}
	return &resolvedRuntimeImage{
		OriginalRef: ref,
		PrepareRef:  ref,
		Source:      runtimeImageSourceRegistry,
		VMImageType: vmType,
	}, nil
}

func localOCITagAndCacheRefDiffer(cfg *config.CocoonConfig, ociRef, cacheBaseKey string) (bool, error) {
	ociBaseKey, found, err := refcache.ResolveBaseKey(cfg, ociRef)
	if err != nil {
		if errors.Is(err, refcache.ErrAmbiguousImageRef) {
			return true, nil
		}
		return false, err
	}
	if !found {
		// No canonical OCI->baseKey mapping recorded yet; cannot prove mismatch.
		// This is inconclusive — both OCI tag and cache alias exist but we
		// cannot determine whether they point to the same identity.
		log.Printf("warning: both OCI tag %q and cache alias (base_key=%s) exist but mismatch check is inconclusive; defaulting to OCI tag", ociRef, cacheBaseKey)
		return false, nil
	}
	return ociBaseKey != cacheBaseKey, nil
}

func resolveLocalPathRef(ref string) (*resolvedRuntimeImage, error) {
	absPath, err := filepath.Abs(ref)
	if err != nil {
		return nil, fmt.Errorf("resolve local image path %q: %w", ref, err)
	}
	if _, err := os.Stat(absPath); err != nil {
		return nil, fmt.Errorf("local image path %q not found: %w", ref, err)
	}
	return &resolvedRuntimeImage{
		OriginalRef: ref,
		PrepareRef:  absPath,
		Source:      runtimeImageSourceLocalPath,
		VMImageType: types.VMImageTypeQCOW2,
	}, nil
}

func isExplicitLocalPath(ref string) bool {
	return strings.HasPrefix(ref, "/") || strings.HasPrefix(ref, "./") || strings.HasPrefix(ref, "../")
}
