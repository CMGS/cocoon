package types

import (
	"fmt"
	"strings"
)

// BootStrategy determines how the VM firmware is selected at boot time.
// Stored in config.json (immutable after creation).
type BootStrategy string

const (
	// BootStrategyUEFI boots with UEFI firmware (CLOUDHV.fd) via REST payload.firmware.
	// This is the default boot strategy.
	BootStrategyUEFI BootStrategy = "uefi"
	// BootStrategyDirect boots an OCI VM image using direct kernel boot via
	// REST payload.kernel + payload.initramfs + payload.cmdline.
	// Used automatically for resolver-classified OCI VM images.
	BootStrategyDirect BootStrategy = "direct"
)

// DefaultBootStrategy is the default boot strategy for new VMs.
const DefaultBootStrategy = BootStrategyUEFI

// ParseBootStrategy validates and normalizes a user-provided boot strategy.
// Empty input resolves to DefaultBootStrategy.
func ParseBootStrategy(raw string) (BootStrategy, error) {
	normalized := strings.ToLower(strings.TrimSpace(raw))
	if normalized == "" {
		return DefaultBootStrategy, nil
	}

	switch BootStrategy(normalized) {
	case BootStrategyUEFI, BootStrategyDirect:
		return BootStrategy(normalized), nil
	default:
		return "", fmt.Errorf("invalid boot strategy %q (must be one of: %s, %s)",
			raw, BootStrategyUEFI, BootStrategyDirect)
	}
}

// BootMode records the actual firmware mode used during a boot attempt.
// Stored in metadata.json (mutable, updated each boot).
type BootMode string

const (
	BootModeUEFI   BootMode = "uefi"
	BootModeDirect BootMode = "direct"
)
