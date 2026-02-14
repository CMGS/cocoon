package refcache

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/CMGS/cocoon/config"
	"github.com/CMGS/cocoon/lock/flock"
	"github.com/CMGS/cocoon/utils"
)

// Entry describes one IMAGE_REF -> base_key mapping in manifest cache.
type Entry struct {
	BaseKey    string `json:"base_key"`
	DigestFull string `json:"digest_full,omitempty"`
	LastSeenAt string `json:"last_seen_at"`
}

type indexFile map[string]Entry

func indexPath(cfg *config.CocoonConfig) string {
	return filepath.Join(cfg.ManifestCacheDir(), "index.json")
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

// Upsert records the source IMAGE_REF mapping to base_key.
// It also stores derived aliases (basename, basename-without-extension, and
// simplified alias) to support ergonomic cache lookups.
func Upsert(cfg *config.CocoonConfig, ref, baseKey, digestFull string) error {
	if strings.TrimSpace(ref) == "" || strings.TrimSpace(baseKey) == "" {
		return fmt.Errorf("ref and baseKey are required")
	}
	return withLock(cfg, func(idx indexFile) error {
		now := time.Now().UTC().Format(time.RFC3339)
		for _, candidate := range candidates(ref) {
			entry := Entry{
				BaseKey:    baseKey,
				DigestFull: digestFull,
				LastSeenAt: now,
			}
			if existing, ok := idx[candidate]; ok && entry.DigestFull == "" {
				entry.DigestFull = existing.DigestFull
			}
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
	if strings.TrimSpace(ref) == "" {
		return "", false, nil
	}
	err := withLock(cfg, func(idx indexFile) error {
		for _, candidate := range candidates(ref) {
			entry, ok := idx[candidate]
			if !ok {
				continue
			}
			baseKey = entry.BaseKey
			found = true
			return nil
		}
		return nil
	})
	if err != nil {
		return "", false, err
	}
	return baseKey, found, nil
}

// RefsForBaseKey returns all IMAGE_REF aliases that currently map to base_key.
// It also returns one non-empty digest_full value when present.
func RefsForBaseKey(cfg *config.CocoonConfig, baseKey string) ([]string, string, error) {
	refs := make([]string, 0)
	digestFull := ""
	err := withLock(cfg, func(idx indexFile) error {
		for ref, entry := range idx {
			if entry.BaseKey != baseKey {
				continue
			}
			refs = append(refs, ref)
			if digestFull == "" && entry.DigestFull != "" {
				digestFull = entry.DigestFull
			}
		}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	sort.Strings(refs)
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
			if entry.BaseKey == baseKey {
				delete(idx, ref)
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
		if i := strings.LastIndexByte(withoutExt, ':'); i > 0 {
			add(withoutExt[:i]) // oci tag-less alias
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
	return strings.TrimSuffix(v, filepath.Ext(v))
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
