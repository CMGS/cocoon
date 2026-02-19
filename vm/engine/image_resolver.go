package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

type registryProbeManifest struct {
	MediaType    string `json:"mediaType"`
	ArtifactType string `json:"artifactType,omitempty"`
	Config       struct {
		MediaType string `json:"mediaType,omitempty"`
	} `json:"config"`
	Layers []struct {
		MediaType string `json:"mediaType,omitempty"`
	} `json:"layers,omitempty"`
}

type registryProbeIndex struct {
	MediaType string `json:"mediaType"`
}

type registryProbeRawFunc func(ctx context.Context, ref, arch string) ([]byte, error)

func defaultRunSkopeoInspectRaw(ctx context.Context, ref, arch string) ([]byte, error) {
	args := []string{"inspect", "--raw"}
	if arch != "" {
		args = append(args, "--override-arch", arch)
	}
	args = append(args, "docker://"+ref)
	cmd := exec.CommandContext(ctx, "skopeo", args...) //nolint:gosec // command is fixed and args are controlled inputs
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("skopeo %s: %s", strings.Join(args, " "), strings.TrimSpace(string(exitErr.Stderr)))
		}
		return nil, fmt.Errorf("skopeo %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

func resolveRuntimeImageRefWithProbe(
	ctx context.Context,
	cfg *config.CocoonConfig,
	ref string,
	registryProbe registryProbeRawFunc,
) (*resolvedRuntimeImage, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, fmt.Errorf("image reference is empty")
	}

	if isExplicitLocalPath(ref) {
		return resolveLocalPathRef(ref)
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
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		return &resolvedRuntimeImage{
			OriginalRef: ref,
			PrepareRef:  ref,
			Source:      runtimeImageSourceURL,
			VMImageType: types.VMImageTypeQCOW2,
		}, nil
	}
	vmType, probeErr := detectRegistryVMImageType(ctx, ref, registryProbe)
	if probeErr != nil {
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

func detectRegistryVMImageType(ctx context.Context, ref string, registryProbe registryProbeRawFunc) (types.VMImageType, error) {
	if registryProbe == nil {
		registryProbe = defaultRunSkopeoInspectRaw
	}

	rawManifest, err := registryProbe(ctx, ref, "")
	if err != nil {
		return types.VMImageTypeQCOW2, err
	}

	var idx registryProbeIndex
	if unmarshalErr := json.Unmarshal(rawManifest, &idx); unmarshalErr != nil {
		return types.VMImageTypeQCOW2, fmt.Errorf("parse registry manifest for %q: %w", ref, unmarshalErr)
	}
	if strings.Contains(idx.MediaType, "image.index") || strings.Contains(idx.MediaType, "manifest.list") {
		rawManifest, err = registryProbe(ctx, ref, hostOCIArch())
		if err != nil {
			return types.VMImageTypeQCOW2, err
		}
	}

	var manifest registryProbeManifest
	if unmarshalErr := json.Unmarshal(rawManifest, &manifest); unmarshalErr != nil {
		return types.VMImageTypeQCOW2, fmt.Errorf("parse single manifest for %q: %w", ref, unmarshalErr)
	}

	if isCocoonVMManifest(manifest) {
		return types.VMImageTypeOCIVM, nil
	}
	return types.VMImageTypeQCOW2, nil
}

func isCocoonVMManifest(manifest registryProbeManifest) bool {
	if manifest.ArtifactType == oci.ArtifactTypeVMImage {
		return true
	}
	if manifest.Config.MediaType == oci.MediaTypeVMConfig {
		return true
	}
	hasKernel := false
	hasRootfs := false
	for _, layer := range manifest.Layers {
		switch layer.MediaType {
		case oci.MediaTypeKernelLayer:
			hasKernel = true
		case oci.MediaTypeRootfsLayer:
			hasRootfs = true
		}
	}
	return hasKernel && hasRootfs
}

func hostOCIArch() string {
	switch runtime.GOARCH {
	case "arm64":
		return "arm64"
	default:
		return "amd64"
	}
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

// resolveLocalOCITagRef delegates to oci.ResolveLocalTagRef.
var resolveLocalOCITagRef = oci.ResolveLocalTagRef
