package refcache

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/CMGS/cocoon/config"
	"github.com/CMGS/cocoon/lock/flock"
	"github.com/CMGS/cocoon/utils"
)

// ErrAmbiguousImageRef is returned when a short/alias ref maps to multiple
// cached base images in manifest index.
var ErrAmbiguousImageRef = errors.New("ambiguous image ref")

// Entry describes one IMAGE_REF -> base_key mapping in manifest cache.
type Entry struct {
	// BaseKey is the unique mapping (legacy + current single-key form).
	BaseKey string `json:"base_key,omitempty"`
	// BaseKeys stores all candidate mappings for aliases. len>1 means ambiguous.
	// This field allows collision-safe alias handling without silent overwrite.
	BaseKeys   []string `json:"base_keys,omitempty"`
	DigestFull string   `json:"digest_full,omitempty"`
	LastSeenAt string   `json:"last_seen_at"`
}

type indexFile map[string]Entry

func indexPath(cfg *config.CocoonConfig) string {
	return filepath.Join(cfg.ManifestCacheDir(), "index.json")
}

func verifiedPath(cfg *config.CocoonConfig) string {
	return filepath.Join(cfg.ManifestCacheDir(), "verified.json")
}

func indexLockPath(cfg *config.CocoonConfig) string {
	return filepath.Join(cfg.ManifestCacheDir(), "index.lock")
}

func load(cfg *config.CocoonConfig) (indexFile, error) {
	idx := make(indexFile)
	path := indexPath(cfg)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return idx, nil
	}
	if err := utils.ReadJSON(path, &idx); err != nil {
		return nil, fmt.Errorf("read manifest index: %w", err)
	}
	return idx, nil
}

func save(cfg *config.CocoonConfig, idx indexFile) error {
	return utils.AtomicWriteJSON(indexPath(cfg), idx)
}

func withLock(cfg *config.CocoonConfig, fn func(indexFile) error) error {
	if err := os.MkdirAll(cfg.ManifestCacheDir(), 0o755); err != nil { //nolint:gosec // G301: cocoon cache dirs are shared runtime state
		return fmt.Errorf("create manifest cache dir: %w", err)
	}
	fl := flock.New(indexLockPath(cfg))
	if err := fl.Lock(); err != nil {
		return fmt.Errorf("acquire manifest index lock: %w", err)
	}
	defer fl.Unlock() //nolint:errcheck

	idx, err := load(cfg)
	if err != nil {
		return err
	}
	return fn(idx)
}

type verifiedFile map[string]string

func loadVerified(cfg *config.CocoonConfig) (verifiedFile, error) {
	vf := make(verifiedFile)
	path := verifiedPath(cfg)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return vf, nil
	}
	if err := utils.ReadJSON(path, &vf); err != nil {
		return nil, fmt.Errorf("read verified index: %w", err)
	}
	return vf, nil
}

func saveVerified(cfg *config.CocoonConfig, vf verifiedFile) error {
	return utils.AtomicWriteJSON(verifiedPath(cfg), vf)
}

// MarkVerified records that a base image has passed bootability verification.
func MarkVerified(cfg *config.CocoonConfig, baseKey string) error {
	if strings.TrimSpace(baseKey) == "" {
		return nil
	}
	return withLock(cfg, func(_ indexFile) error {
		vf, err := loadVerified(cfg)
		if err != nil {
			return err
		}
		vf[baseKey] = time.Now().UTC().Format(time.RFC3339)
		if err := saveVerified(cfg, vf); err != nil {
			return fmt.Errorf("save verified index: %w", err)
		}
		return nil
	})
}

// IsVerified reports whether the base image has a successful verify record.
func IsVerified(cfg *config.CocoonConfig, baseKey string) (bool, error) {
	if strings.TrimSpace(baseKey) == "" {
		return false, nil
	}
	var verified bool
	err := withLock(cfg, func(_ indexFile) error {
		vf, err := loadVerified(cfg)
		if err != nil {
			return err
		}
		_, verified = vf[baseKey]
		return nil
	})
	if err != nil {
		return false, err
	}
	return verified, nil
}

// DeleteVerified removes verification state for a base image.
func DeleteVerified(cfg *config.CocoonConfig, baseKey string) error {
	if strings.TrimSpace(baseKey) == "" {
		return nil
	}
	return withLock(cfg, func(_ indexFile) error {
		vf, err := loadVerified(cfg)
		if err != nil {
			return err
		}
		if _, ok := vf[baseKey]; !ok {
			return nil
		}
		delete(vf, baseKey)
		if err := saveVerified(cfg, vf); err != nil {
			return fmt.Errorf("save verified index: %w", err)
		}
		return nil
	})
}

// Upsert records the source IMAGE_REF mapping to base_key.
// It also stores derived aliases (basename, basename-without-extension, and
// simplified alias) to support ergonomic cache lookups.
func Upsert(cfg *config.CocoonConfig, ref, baseKey, digestFull string) error {
	if strings.TrimSpace(ref) == "" || strings.TrimSpace(baseKey) == "" {
		return fmt.Errorf("ref and baseKey are required")
	}
	return withLock(cfg, func(idx indexFile) error {
		now := time.Now().UTC().Format(time.RFC3339)
		exactRef := strings.TrimSpace(ref)
		for _, candidate := range candidates(ref) {
			existing, existed := idx[candidate]
			entry := existing
			keys := entry.resolvedBaseKeys()

			// Exact source ref is authoritative: always point to the latest base key.
			if candidate == exactRef {
				setResolvedBaseKeys(&entry, []string{baseKey})
			} else {
				if !containsString(keys, baseKey) {
					keys = append(keys, baseKey)
				}
				setResolvedBaseKeys(&entry, keys)
			}

			// Keep digest only for unambiguous entries.
			currentKeys := entry.resolvedBaseKeys()
			if len(currentKeys) == 1 && currentKeys[0] == baseKey {
				if digestFull != "" {
					entry.DigestFull = digestFull
				} else if existed && entry.DigestFull == "" {
					entry.DigestFull = existing.DigestFull
				}
			} else if len(currentKeys) > 1 {
				entry.DigestFull = ""
			}

			entry.LastSeenAt = now
			idx[candidate] = entry
		}
		if err := save(cfg, idx); err != nil {
			return fmt.Errorf("save manifest index: %w", err)
		}
		return nil
	})
}

// ResolveBaseKey resolves an IMAGE_REF to base_key from local manifest cache.
func ResolveBaseKey(cfg *config.CocoonConfig, ref string) (string, bool, error) {
	var (
		baseKey string
		found   bool
	)
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", false, nil
	}
	err := withLock(cfg, func(idx indexFile) error {
		// Direct exact match has highest priority.
		if entry, ok := idx[ref]; ok {
			keys := entry.resolvedBaseKeys()
			switch len(keys) {
			case 1:
				baseKey = keys[0]
				found = true
				return nil
			case 0:
				return nil
			default:
				return fmt.Errorf("%w: %q resolves to multiple base keys: %s",
					ErrAmbiguousImageRef, ref, strings.Join(keys, ", "))
			}
		}

		// Do not downgrade explicit tag/digest queries (e.g. repo:tag,
		// repo@sha256:...) to tag-less aliases.
		if hasExplicitTagOrDigestRef(ref) {
			return nil
		}

		matches := make(map[string]struct{})
		for _, candidate := range candidates(ref) {
			if candidate == ref {
				continue
			}
			entry, ok := idx[candidate]
			if !ok {
				continue
			}
			for _, k := range entry.resolvedBaseKeys() {
				matches[k] = struct{}{}
			}
		}

		keys := sortedStringSet(matches)
		switch len(keys) {
		case 0:
			return nil
		case 1:
			baseKey = keys[0]
			found = true
			return nil
		default:
			return fmt.Errorf("%w: %q matches multiple cached images: %s",
				ErrAmbiguousImageRef, ref, strings.Join(keys, ", "))
		}
	})
	if err != nil {
		return "", false, err
	}
	return baseKey, found, nil
}

func hasExplicitTagOrDigestRef(ref string) bool {
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

// RefsForBaseKey returns all IMAGE_REF aliases that currently map to base_key.
// It also returns one non-empty digest_full value when present.
func RefsForBaseKey(cfg *config.CocoonConfig, baseKey string) ([]string, string, error) {
	refSet := make(map[string]struct{})
	digestFull := ""
	err := withLock(cfg, func(idx indexFile) error {
		for ref, entry := range idx {
			if !containsString(entry.resolvedBaseKeys(), baseKey) {
				continue
			}
			refSet[ref] = struct{}{}
			if digestFull == "" && entry.DigestFull != "" {
				digestFull = entry.DigestFull
			}
		}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	refs := sortedStringSet(refSet)
	return refs, digestFull, nil
}

// DeleteByBaseKey removes all mappings that point to base_key.
func DeleteByBaseKey(cfg *config.CocoonConfig, baseKey string) error {
	if strings.TrimSpace(baseKey) == "" {
		return nil
	}
	return withLock(cfg, func(idx indexFile) error {
		changed := false
		for ref, entry := range idx {
			keys := removeString(entry.resolvedBaseKeys(), baseKey)
			switch len(keys) {
			case 0:
				delete(idx, ref)
				changed = true
			default:
				setResolvedBaseKeys(&entry, keys)
				if len(keys) > 1 {
					entry.DigestFull = ""
				}
				idx[ref] = entry
				changed = true
			}
		}
		if !changed {
			return nil
		}
		if err := save(cfg, idx); err != nil {
			return fmt.Errorf("save manifest index: %w", err)
		}
		return nil
	})
}

// PurgeBaseKey removes all entries that reference baseKey and cleans up entries
// that become empty after removal. This prevents stale refs from accumulating
// when images are deleted.
func PurgeBaseKey(cfg *config.CocoonConfig, baseKey string) error {
	if strings.TrimSpace(baseKey) == "" {
		return nil
	}
	return withLock(cfg, func(idx indexFile) error {
		changed := false
		for ref, entry := range idx {
			keys := entry.resolvedBaseKeys()
			if !containsString(keys, baseKey) {
				continue
			}
			keys = removeString(keys, baseKey)
			if len(keys) == 0 {
				delete(idx, ref)
			} else {
				setResolvedBaseKeys(&entry, keys)
				if len(keys) > 1 {
					entry.DigestFull = ""
				}
				idx[ref] = entry
			}
			changed = true
		}
		if !changed {
			return nil
		}
		return save(cfg, idx)
	})
}

func candidates(ref string) []string {
	set := make(map[string]struct{})
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v == "" || v == "." || v == "/" {
			return
		}
		set[v] = struct{}{}
	}

	add(ref)
	addVariants := func(v string) {
		add(v)
		withoutExt := trimKnownExt(v)
		add(withoutExt)
		// OCI tag-less alias: "repo:tag" → "repo". Only apply when the
		// colon appears after the last slash (looks like an OCI ref, not
		// a URL scheme like "https:").
		if i := strings.LastIndexByte(withoutExt, ':'); i > 0 {
			lastSlash := strings.LastIndexByte(withoutExt, '/')
			if i > lastSlash {
				add(withoutExt[:i])
			}
		}
		add(simplifyAlias(withoutExt))
	}

	addVariants(ref)

	if u, err := url.Parse(ref); err == nil && u.Scheme != "" && u.Host != "" {
		base := filepath.Base(u.Path)
		addVariants(base)
	}

	base := filepath.Base(ref)
	addVariants(base)

	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func trimKnownExt(v string) string {
	lower := strings.ToLower(v)
	for _, ext := range []string{".qcow2", ".img", ".raw", ".iso"} {
		if strings.HasSuffix(lower, ext) {
			return v[:len(v)-len(ext)]
		}
	}
	// Do not treat dotted OCI tags like "ubuntu:24.04" as file extensions.
	if looksLikeTaggedOCIRef(v) {
		return v
	}
	return strings.TrimSuffix(v, filepath.Ext(v))
}

func looksLikeTaggedOCIRef(v string) bool {
	// Digest-pinned refs should not be extension-trimmed.
	if strings.Contains(v, "@sha256:") {
		return true
	}
	// Heuristic: a colon after the last slash indicates a tag segment.
	lastColon := strings.LastIndexByte(v, ':')
	if lastColon < 0 {
		return false
	}
	lastSlash := strings.LastIndexByte(v, '/')
	return lastColon > lastSlash
}

func simplifyAlias(v string) string {
	s := v
	s = strings.TrimSuffix(s, "-amd64")
	s = strings.TrimSuffix(s, "-arm64")
	s = strings.TrimSuffix(s, "-x86_64")
	s = strings.TrimSuffix(s, "-aarch64")
	s = strings.ReplaceAll(s, "-server-", "-")
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	s = strings.Trim(s, "-_")
	return s
}

func (e Entry) resolvedBaseKeys() []string {
	if len(e.BaseKeys) > 0 {
		keys := append([]string(nil), e.BaseKeys...)
		sort.Strings(keys)
		return keys
	}
	if e.BaseKey != "" {
		return []string{e.BaseKey}
	}
	return nil
}

func setResolvedBaseKeys(e *Entry, keys []string) {
	unique := make(map[string]struct{})
	for _, k := range keys {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		unique[k] = struct{}{}
	}

	sorted := sortedStringSet(unique)
	switch len(sorted) {
	case 0:
		e.BaseKey = ""
		e.BaseKeys = nil
	case 1:
		e.BaseKey = sorted[0]
		e.BaseKeys = nil
	default:
		e.BaseKey = ""
		e.BaseKeys = sorted
	}
}

func containsString(items []string, target string) bool {
	return slices.Contains(items, target)
}

func removeString(items []string, target string) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		if item == target {
			continue
		}
		out = append(out, item)
	}
	return out
}

func sortedStringSet(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
