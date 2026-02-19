package oci

import "strings"

// NOTE on tag parsing (L1 audit item): We intentionally use string
// manipulation instead of name.NewTag() from go-containerregistry here.
// name.NewTag() enforces Docker/OCI registry naming rules (e.g., lowercase,
// valid hostname, max lengths) which would reject valid local-only tag names.
// These functions are used for quick tag/digest detection and ":latest"
// defaulting across both local and registry refs. The go-containerregistry
// library is used at the pull/push boundary (pull.go, push.go) where full
// registry validation is appropriate.

// HasExplicitTagOrDigest reports whether ref contains an explicit tag (":tag")
// or digest ("@sha256:...") suffix. Used by both CLI and runtime resolver to
// decide whether to append implicit ":latest".
func HasExplicitTagOrDigest(ref string) bool {
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

// EnsureLatestTag appends ":latest" to a ref that has no explicit tag or digest.
func EnsureLatestTag(tag string) string {
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

// ResolveLocalTagRef checks whether ref (or ref + ":latest") matches a local
// OCI build tag. Returns the resolved tag name, whether it was found, and any error.
func ResolveLocalTagRef(store *Store, ref string) (string, bool, error) {
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

	if HasExplicitTagOrDigest(ref) {
		return "", false, nil
	}
	latest := EnsureLatestTag(ref)
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
