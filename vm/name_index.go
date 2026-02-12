package vm

import (
	"fmt"
	"os"

	"github.com/CMGS/cocoon/config"
	"github.com/CMGS/cocoon/lock"
	"github.com/CMGS/cocoon/utils"
)

// LoadNameIndex reads the global name-index.json file.
// Returns an empty index if the file does not exist.
func LoadNameIndex(cfg *config.CocoonConfig) (NameIndex, error) {
	path := cfg.NameIndexFile()
	index := make(NameIndex)

	if _, err := os.Stat(path); os.IsNotExist(err) {
		return index, nil
	}

	if err := utils.ReadJSON(path, &index); err != nil {
		return nil, fmt.Errorf("read name index: %w", err)
	}
	return index, nil
}

// SaveNameIndex writes the name-index.json file atomically.
// Caller MUST hold the name-index.lock before calling this.
func SaveNameIndex(cfg *config.CocoonConfig, index NameIndex) error {
	return utils.AtomicWriteJSON(cfg.NameIndexFile(), index)
}

// AddName adds a name -> vmID mapping to the name index.
// Acquires name-index.lock (Level 2) for the duration of the operation.
// Returns an error if the name already exists.
func AddName(cfg *config.CocoonConfig, name, vmID string) error {
	fl := lock.New(cfg.NameIndexLock())
	if err := fl.Lock(); err != nil {
		return fmt.Errorf("acquire name index lock: %w", err)
	}
	defer fl.Unlock() //nolint:errcheck

	index, err := LoadNameIndex(cfg)
	if err != nil {
		return err
	}

	if existing, ok := index[name]; ok {
		return fmt.Errorf("name %q already exists (vm_id=%s)", name, existing)
	}

	index[name] = vmID
	return SaveNameIndex(cfg, index)
}

// RemoveName removes a name from the name index.
// Acquires name-index.lock (Level 2) for the duration of the operation.
// No-op if the name does not exist.
func RemoveName(cfg *config.CocoonConfig, name string) error {
	fl := lock.New(cfg.NameIndexLock())
	if err := fl.Lock(); err != nil {
		return fmt.Errorf("acquire name index lock: %w", err)
	}
	defer fl.Unlock() //nolint:errcheck

	index, err := LoadNameIndex(cfg)
	if err != nil {
		return err
	}

	if _, ok := index[name]; !ok {
		return nil // no-op
	}

	delete(index, name)
	return SaveNameIndex(cfg, index)
}

// RebuildNameIndex scans all VM config.json files and rebuilds the name index.
// Used during reconciliation to ensure the derived cache is consistent.
func RebuildNameIndex(cfg *config.CocoonConfig) (NameIndex, error) {
	fl := lock.New(cfg.NameIndexLock())
	if err := fl.Lock(); err != nil {
		return nil, fmt.Errorf("acquire name index lock: %w", err)
	}
	defer fl.Unlock() //nolint:errcheck

	entries, err := os.ReadDir(cfg.VMDir())
	if err != nil {
		if os.IsNotExist(err) {
			index := make(NameIndex)
			return index, SaveNameIndex(cfg, index)
		}
		return nil, fmt.Errorf("read VM directory: %w", err)
	}

	index := make(NameIndex)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		vmID := entry.Name()
		configPath := cfg.VMConfigPath(vmID)

		var vmCfg struct {
			Name string `json:"name"`
		}
		if err := utils.ReadJSON(configPath, &vmCfg); err != nil {
			// Skip VMs with missing or corrupt config.json.
			continue
		}
		if vmCfg.Name != "" {
			index[vmCfg.Name] = vmID
		}
	}

	if err := SaveNameIndex(cfg, index); err != nil {
		return nil, err
	}
	return index, nil
}
