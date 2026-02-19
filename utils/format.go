package utils

import "fmt"

// HumanBytes formats a byte count into a human-readable string
// (e.g., "1.0KB", "3.5MB"). Negative values are clamped to "0B".
func HumanBytes(n int64) string {
	if n < 0 {
		return "0B"
	}
	const unit = int64(1024)
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := unit, 0
	for value := n / unit; value >= unit; value /= unit {
		div *= unit
		exp++
	}
	suffixes := []string{"KB", "MB", "GB", "TB", "PB", "EB"}
	if exp >= len(suffixes) {
		exp = len(suffixes) - 1
	}
	value := float64(n) / float64(div)
	return fmt.Sprintf("%.1f%s", value, suffixes[exp])
}
