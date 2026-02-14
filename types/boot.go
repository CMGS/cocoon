package types

import (
	"fmt"
	"strings"
)

// BootStrategy determines how the VM firmware is selected at boot time.
// Stored in config.json (immutable after creation).
type BootStrategy string

const (
	// BootStrategyPVHThenUEFI tries PVH first, falls back to UEFI on failure.
	BootStrategyPVHThenUEFI BootStrategy = "pvh_then_uefi"
	// BootStrategyUEFIOnly boots with UEFI only.
	BootStrategyUEFIOnly BootStrategy = "uefi_only"
	// BootStrategyPVHOnly boots with PVH only (fails on PVH error).
	BootStrategyPVHOnly BootStrategy = "pvh_only"
)

// DefaultBootStrategy is the default boot strategy for new VMs.
const DefaultBootStrategy = BootStrategyPVHThenUEFI

// ParseBootStrategy validates and normalizes a user-provided boot strategy.
// Empty input resolves to DefaultBootStrategy.
func ParseBootStrategy(raw string) (BootStrategy, error) {
	normalized := strings.ToLower(strings.TrimSpace(raw))
	if normalized == "" {
		return DefaultBootStrategy, nil
	}

	switch BootStrategy(normalized) {
	case BootStrategyPVHThenUEFI, BootStrategyUEFIOnly, BootStrategyPVHOnly:
		return BootStrategy(normalized), nil
	default:
		return "", fmt.Errorf("invalid boot strategy %q (must be one of: %s, %s, %s)",
			raw, BootStrategyPVHThenUEFI, BootStrategyUEFIOnly, BootStrategyPVHOnly)
	}
}

// BootMode records the actual firmware mode used during a boot attempt.
// Stored in metadata.json (mutable, updated each boot).
type BootMode string

const (
	BootModePVH  BootMode = "pvh"
	BootModeUEFI BootMode = "uefi"
)
