package engine

import (
	"fmt"
	"log"
	"maps"
	"os"

	"github.com/CMGS/cocoon/config"
	"github.com/CMGS/cocoon/lock/jsonstore"
	"github.com/CMGS/cocoon/utils"
	"github.com/CMGS/cocoon/vm"
)

func nameIndexStore(cfg *config.CocoonConfig) *jsonstore.Store[vm.NameIndex] {
	return jsonstore.New(
		cfg.NameIndexLock(),
		cfg.NameIndexFile(),
		func() *vm.NameIndex {
			m := make(vm.NameIndex)
			return &m
		},
	)
}

// LoadNameIndex reads the global name-index.json file under lock.
// Returns an empty index if the file does not exist.
func LoadNameIndex(cfg *config.CocoonConfig) (vm.NameIndex, error) {
	var result vm.NameIndex
	err := nameIndexStore(cfg).Read(func(idxPtr *vm.NameIndex) error {
		idx := *idxPtr
		result = make(vm.NameIndex, len(idx))
		maps.Copy(result, idx)
		return nil
	})
	return result, err
}

// AddName adds a name -> vmID mapping to the name index.
// Acquires name-index.lock (Level 2) for the duration of the operation.
// Returns an error if the name already exists.
func AddName(cfg *config.CocoonConfig, name, vmID string) error {
	return nameIndexStore(cfg).Update(func(idxPtr *vm.NameIndex) error {
		idx := *idxPtr
		if existing, ok := idx[name]; ok {
			return fmt.Errorf("name %q already exists (vm_id=%s)", name, existing)
		}
		idx[name] = vmID
		return nil
	})
}

// RemoveName removes a name from the name index.
// Acquires name-index.lock (Level 2) for the duration of the operation.
// No-op if the name does not exist.
func RemoveName(cfg *config.CocoonConfig, name string) error {
	return nameIndexStore(cfg).Update(func(idxPtr *vm.NameIndex) error {
		idx := *idxPtr
		delete(idx, name)
		return nil
	})
}

// RebuildNameIndex scans all VM config.json files and rebuilds the name index.
// Used during reconciliation to ensure the derived cache is consistent.
func RebuildNameIndex(cfg *config.CocoonConfig) (vm.NameIndex, error) {
	var result vm.NameIndex
	err := nameIndexStore(cfg).Update(func(idxPtr *vm.NameIndex) error {
		idx := *idxPtr
		clear(idx)

		entries, err := os.ReadDir(cfg.VMDir())
		if err != nil {
			if os.IsNotExist(err) {
				return nil // empty index saved by Update
			}
			return fmt.Errorf("read VM directory: %w", err)
		}

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
				if existing, ok := idx[vmCfg.Name]; ok {
					log.Printf("WARNING: duplicate VM name %q: vm_id=%s overwritten by vm_id=%s", vmCfg.Name, existing, vmID)
				}
				idx[vmCfg.Name] = vmID
			}
		}

		// Copy for return value.
		result = make(vm.NameIndex, len(idx))
		maps.Copy(result, idx)
		return nil
	})
	return result, err
}
