package engine

import (
	"errors"
	"fmt"
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

func resolveRuntimeImageRef(cfg *config.CocoonConfig, ref string) (*resolvedRuntimeImage, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, fmt.Errorf("image reference is empty")
	}

	if isExplicitLocalPath(ref) {
		return resolveLocalPathRef(ref)
	}
	if localPath, ok, err := resolveExistingLocalPathRef(ref); err != nil {
		return nil, err
	} else if ok {
		return localPath, nil
	}

	store := oci.NewStore(cfg)
	resolvedOCITag, ociExists, err := resolveLocalOCITagRef(store, ref)
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
		return nil, fmt.Errorf(
			"ambiguous image reference %q: matches local OCI tag %q and local cache alias base_key=%s; use explicit ref",
			ref, resolvedOCITag, localBaseKey,
		)
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
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		return &resolvedRuntimeImage{
			OriginalRef: ref,
			PrepareRef:  ref,
			Source:      runtimeImageSourceURL,
			VMImageType: types.VMImageTypeQCOW2,
		}, nil
	}
	return &resolvedRuntimeImage{
		OriginalRef: ref,
		PrepareRef:  ref,
		Source:      runtimeImageSourceRegistry,
		VMImageType: types.VMImageTypeQCOW2,
	}, nil
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

func resolveExistingLocalPathRef(ref string) (*resolvedRuntimeImage, bool, error) {
	absPath, err := filepath.Abs(ref)
	if err != nil {
		return nil, false, fmt.Errorf("resolve local image path %q: %w", ref, err)
	}
	if _, statErr := os.Stat(absPath); statErr != nil {
		if os.IsNotExist(statErr) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("stat local image path %q: %w", ref, statErr)
	}
	return &resolvedRuntimeImage{
		OriginalRef: ref,
		PrepareRef:  absPath,
		Source:      runtimeImageSourceLocalPath,
		VMImageType: types.VMImageTypeQCOW2,
	}, true, nil
}

func isExplicitLocalPath(ref string) bool {
	return strings.HasPrefix(ref, "/") || strings.HasPrefix(ref, "./") || strings.HasPrefix(ref, "../")
}

func hasExplicitTagOrDigest(ref string) bool {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return false
	}
	if strings.Contains(ref, "@sha256:") {
		return true
	}
	last := ref
	if idx := strings.LastIndex(ref, "/"); idx >= 0 {
		last = ref[idx+1:]
	}
	return strings.Contains(last, ":")
}

func ensureLatestTag(tag string) string {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return ""
	}
	if strings.Contains(tag, "@sha256:") {
		return tag
	}
	last := tag
	if idx := strings.LastIndex(tag, "/"); idx >= 0 {
		last = tag[idx+1:]
	}
	if strings.Contains(last, ":") {
		return tag
	}
	return tag + ":latest"
}

func resolveLocalOCITagRef(store *oci.Store, ref string) (string, bool, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", false, nil
	}

	exists, err := store.HasTag(ref)
	if err != nil {
		return "", false, err
	}
	if exists {
		return ref, true, nil
	}

	if hasExplicitTagOrDigest(ref) {
		return "", false, nil
	}
	latest := ensureLatestTag(ref)
	if latest == ref {
		return "", false, nil
	}
	exists, err = store.HasTag(latest)
	if err != nil {
		return "", false, err
	}
	if exists {
		return latest, true, nil
	}
	return "", false, nil
}
