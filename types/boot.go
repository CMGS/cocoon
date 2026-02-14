package types

import (
	"fmt"
	"strings"
)

// BootStrategy determines how the VM firmware is selected at boot time.
// Stored in config.json (immutable after creation).
type BootStrategy string

const (
	// BootStrategyUEFIOnly boots with UEFI firmware (CLOUDHV.fd) via CLI --firmware.
	BootStrategyUEFIOnly BootStrategy = "uefi_only"
	// BootStrategyPVHOnly boots with PVH firmware (hypervisor-fw) via REST payload.kernel.
	BootStrategyPVHOnly BootStrategy = "pvh_only"
)

// DefaultBootStrategy is the default boot strategy for new VMs.
const DefaultBootStrategy = BootStrategyPVHOnly

// ParseBootStrategy validates and normalizes a user-provided boot strategy.
// Empty input resolves to DefaultBootStrategy.
func ParseBootStrategy(raw string) (BootStrategy, error) {
	normalized := strings.ToLower(strings.TrimSpace(raw))
	if normalized == "" {
		return DefaultBootStrategy, nil
	}

	switch BootStrategy(normalized) {
	case BootStrategyUEFIOnly, BootStrategyPVHOnly:
		return BootStrategy(normalized), nil
	default:
		return "", fmt.Errorf("invalid boot strategy %q (must be one of: %s, %s)",
			raw, BootStrategyPVHOnly, BootStrategyUEFIOnly)
	}
}

// BootMode records the actual firmware mode used during a boot attempt.
// Stored in metadata.json (mutable, updated each boot).
type BootMode string

const (
	BootModePVH  BootMode = "pvh"
	BootModeUEFI BootMode = "uefi"
)
