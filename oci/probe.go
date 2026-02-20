package oci

import (
	"context"
	"fmt"
	"log"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/CMGS/cocoon/config"
	"github.com/CMGS/cocoon/types"
)

// ProbeRegistryVMImageType fetches the manifest for ref from a remote registry
// and returns whether it is a Cocoon VM image or a standard container image.
// Errors are classified via ClassifyRegistryError.
func ProbeRegistryVMImageType(ctx context.Context, cfg *config.CocoonConfig, ref string) (types.VMImageType, error) {
	ref = EnsureLatestTag(ref)

	tag, err := name.NewTag(ref)
	if err != nil {
		return types.VMImageTypeQCOW2, types.NewPermanentError(fmt.Errorf("invalid registry reference %q: %w", ref, err))
	}

	keychain := authn.NewMultiKeychain(CocoonKeychain(), authn.DefaultKeychain)

	desc, err := remote.Get(tag,
		remote.WithAuthFromKeychain(keychain),
		remote.WithContext(ctx),
		remote.WithPlatform(hostPlatform()),
	)
	if err != nil {
		return types.VMImageTypeQCOW2, ClassifyRegistryError("probe", err)
	}

	if validateErr := validateCocoonVMManifest(desc.Manifest); validateErr != nil {
		log.Printf("probe %q: not a Cocoon VM manifest: %v", ref, validateErr)
		return types.VMImageTypeQCOW2, nil
	}
	return types.VMImageTypeOCIVM, nil
}
