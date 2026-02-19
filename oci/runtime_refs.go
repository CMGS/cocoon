package oci

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/CMGS/cocoon/config"
	"github.com/CMGS/cocoon/lock/flock"
	"github.com/CMGS/cocoon/utils"
)

// RuntimeRefEntry tracks which VMs pin an OCI runtime cache entry.
type RuntimeRefEntry struct {
	Refs      []string `json:"refs"`
	CreatedAt string   `json:"created_at,omitempty"`
}

// RuntimeRefsIndex is the top-level structure of oci-runtime-refs.json.
type RuntimeRefsIndex struct {
	Runtimes map[string]RuntimeRefEntry `json:"runtimes"`
}

// AddRuntimeRef pins vmID to a runtimeKey.
// Idempotent: adding the same vmID twice does not duplicate refs.
func AddRuntimeRef(cfg *config.CocoonConfig, runtimeKey, vmID string) error {
	if err := validateRuntimeKey(runtimeKey); err != nil {
		return err
	}
	vmID = strings.TrimSpace(vmID)
	if vmID == "" {
		return fmt.Errorf("runtime ref vmID is empty")
	}

	return withRuntimeRefsLock(cfg, func(idx *RuntimeRefsIndex) error {
		entry, ok := idx.Runtimes[runtimeKey]
		if !ok {
			entry = RuntimeRefEntry{
				Refs:      []string{},
				CreatedAt: time.Now().UTC().Format(time.RFC3339),
			}
		}
		if slices.Contains(entry.Refs, vmID) {
			return nil
		}
		entry.Refs = append(entry.Refs, vmID)
		idx.Runtimes[runtimeKey] = entry
		return saveRuntimeRefs(cfg, idx)
	})
}

// RemoveRuntimeRef unpins vmID from runtimeKey.
// If the last ref is removed the runtimeKey entry is deleted.
func RemoveRuntimeRef(cfg *config.CocoonConfig, runtimeKey, vmID string) error {
	if err := validateRuntimeKey(runtimeKey); err != nil {
		return err
	}
	vmID = strings.TrimSpace(vmID)
	if vmID == "" {
		return fmt.Errorf("runtime ref vmID is empty")
	}

	return withRuntimeRefsLock(cfg, func(idx *RuntimeRefsIndex) error {
		entry, ok := idx.Runtimes[runtimeKey]
		if !ok {
			return nil
		}

		filtered := make([]string, 0, len(entry.Refs))
		for _, ref := range entry.Refs {
			if ref != vmID {
				filtered = append(filtered, ref)
			}
		}

		if len(filtered) == 0 {
			delete(idx.Runtimes, runtimeKey)
		} else {
			entry.Refs = filtered
			idx.Runtimes[runtimeKey] = entry
		}

		return saveRuntimeRefs(cfg, idx)
	})
}

// GetRuntimeRefs returns VM IDs currently pinning runtimeKey.
func GetRuntimeRefs(cfg *config.CocoonConfig, runtimeKey string) ([]string, error) {
	if err := validateRuntimeKey(runtimeKey); err != nil {
		return nil, err
	}

	var refs []string
	err := withRuntimeRefsLock(cfg, func(idx *RuntimeRefsIndex) error {
		entry, ok := idx.Runtimes[runtimeKey]
		if !ok || len(entry.Refs) == 0 {
			refs = []string{}
			return nil
		}
		refs = make([]string, len(entry.Refs))
		copy(refs, entry.Refs)
		return nil
	})
	return refs, err
}

// IsRuntimeReferenced reports whether runtimeKey has at least one VM ref.
func IsRuntimeReferenced(cfg *config.CocoonConfig, runtimeKey string) (bool, error) {
	refs, err := GetRuntimeRefs(cfg, runtimeKey)
	if err != nil {
		return false, err
	}
	return len(refs) > 0, nil
}

// LoadRuntimeRefsSnapshot returns a detached copy of oci-runtime-refs.json.
func LoadRuntimeRefsSnapshot(cfg *config.CocoonConfig) (RuntimeRefsIndex, error) {
	var snapshot RuntimeRefsIndex
	err := withRuntimeRefsLock(cfg, func(idx *RuntimeRefsIndex) error {
		snapshot = RuntimeRefsIndex{Runtimes: make(map[string]RuntimeRefEntry, len(idx.Runtimes))}
		for key, entry := range idx.Runtimes {
			copied := RuntimeRefEntry{
				CreatedAt: entry.CreatedAt,
				Refs:      make([]string, len(entry.Refs)),
			}
			copy(copied.Refs, entry.Refs)
			snapshot.Runtimes[key] = copied
		}
		return nil
	})
	return snapshot, err
}

func withRuntimeRefsLock(cfg *config.CocoonConfig, fn func(*RuntimeRefsIndex) error) error {
	if err := os.MkdirAll(cfg.DBDir(), 0o755); err != nil { //nolint:gosec // cocoon-managed state dir
		return fmt.Errorf("create db dir: %w", err)
	}
	fl := flock.New(cfg.OCIRuntimeRefsLock())
	if err := fl.Lock(); err != nil {
		return fmt.Errorf("acquire oci runtime refs lock: %w", err)
	}
	defer fl.Unlock() //nolint:errcheck

	idx, err := loadRuntimeRefs(cfg)
	if err != nil {
		return err
	}
	return fn(idx)
}

func loadRuntimeRefs(cfg *config.CocoonConfig) (*RuntimeRefsIndex, error) {
	idx := &RuntimeRefsIndex{Runtimes: make(map[string]RuntimeRefEntry)}
	path := cfg.OCIRuntimeRefsFile()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return idx, nil
	}
	if err := utils.ReadJSON(path, idx); err != nil {
		return nil, fmt.Errorf("read OCI runtime refs index: %w", err)
	}
	if idx.Runtimes == nil {
		idx.Runtimes = make(map[string]RuntimeRefEntry)
	}
	return idx, nil
}

func saveRuntimeRefs(cfg *config.CocoonConfig, idx *RuntimeRefsIndex) error {
	return utils.AtomicWriteJSON(cfg.OCIRuntimeRefsFile(), idx)
}

func validateRuntimeKey(runtimeKey string) error {
	runtimeKey = strings.TrimSpace(runtimeKey)
	if runtimeKey == "" {
		return fmt.Errorf("runtime key is empty")
	}
	// Defense-in-depth against traversal/injection in path helpers.
	if strings.Contains(runtimeKey, "..") || strings.ContainsAny(runtimeKey, `/\`) {
		return fmt.Errorf("invalid runtime key %q", runtimeKey)
	}
	return nil
}
