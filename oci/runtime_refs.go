package oci

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/CMGS/cocoon/config"
	"github.com/CMGS/cocoon/lock/jsonstore"
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
	runtimeKey = strings.TrimSpace(runtimeKey)
	if err := validateRuntimeKey(runtimeKey); err != nil {
		return err
	}
	vmID = strings.TrimSpace(vmID)
	if vmID == "" {
		return fmt.Errorf("runtime ref vmID is empty")
	}

	return runtimeRefsStore(cfg).Update(func(idx *RuntimeRefsIndex) error {
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
		return nil
	})
}

// RemoveRuntimeRef unpins vmID from runtimeKey.
// If the last ref is removed the runtimeKey entry is deleted.
func RemoveRuntimeRef(cfg *config.CocoonConfig, runtimeKey, vmID string) error {
	runtimeKey = strings.TrimSpace(runtimeKey)
	if err := validateRuntimeKey(runtimeKey); err != nil {
		return err
	}
	vmID = strings.TrimSpace(vmID)
	if vmID == "" {
		return fmt.Errorf("runtime ref vmID is empty")
	}

	return runtimeRefsStore(cfg).Update(func(idx *RuntimeRefsIndex) error {
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

		return nil
	})
}

// GetRuntimeRefs returns VM IDs currently pinning runtimeKey.
func GetRuntimeRefs(cfg *config.CocoonConfig, runtimeKey string) ([]string, error) {
	runtimeKey = strings.TrimSpace(runtimeKey)
	if err := validateRuntimeKey(runtimeKey); err != nil {
		return nil, err
	}

	var refs []string
	err := runtimeRefsStore(cfg).Read(func(idx *RuntimeRefsIndex) error {
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
	err := runtimeRefsStore(cfg).Read(func(idx *RuntimeRefsIndex) error {
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

func runtimeRefsStore(cfg *config.CocoonConfig) *jsonstore.Store[RuntimeRefsIndex] {
	return jsonstore.New(
		cfg.OCIRuntimeRefsLock(),
		cfg.OCIRuntimeRefsFile(),
		func() *RuntimeRefsIndex {
			return &RuntimeRefsIndex{Runtimes: make(map[string]RuntimeRefEntry)}
		},
	).WithEnsureDir(cfg.DBDir())
}

func validateRuntimeKey(runtimeKey string) error {
	runtimeKey = strings.TrimSpace(runtimeKey)
	if runtimeKey == "" {
		return fmt.Errorf("runtime key is empty")
	}
	// Runtime keys are currently sha256-prefix hex strings, but we intentionally
	// validate only path-safety invariants here. This keeps the index format
	// forward-compatible if runtime key derivation changes in later phases.
	// Defense-in-depth against traversal/injection in path helpers.
	if strings.Contains(runtimeKey, "..") || strings.ContainsAny(runtimeKey, `/\`) {
		return fmt.Errorf("invalid runtime key %q", runtimeKey)
	}
	return nil
}
