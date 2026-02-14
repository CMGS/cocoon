package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"
)

// qemuImgInfo is the JSON structure returned by `qemu-img info --output=json`.
type qemuImgInfo struct {
	Format      string `json:"format"`
	VirtualSize int64  `json:"virtual-size"`
	ActualSize  int64  `json:"actual-size"`
	Filename    string `json:"filename"`
}

// detectImageFormatTimeout is the maximum time allowed for qemu-img info to complete.
const detectImageFormatTimeout = 30 * time.Second

// detectImageFormat runs `qemu-img info --output=json` on the given path and
// returns the detected format string (e.g., "qcow2", "raw").
// The parent context is respected: the effective timeout is the shorter of the
// parent deadline and detectImageFormatTimeout.
func detectImageFormat(ctx context.Context, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, detectImageFormatTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "qemu-img", "info", "--output=json", path)
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("qemu-img info %s: %s", path, string(exitErr.Stderr))
		}
		return "", fmt.Errorf("qemu-img info %s: %w", path, err)
	}

	var info qemuImgInfo
	if err := json.Unmarshal(out, &info); err != nil {
		return "", fmt.Errorf("parse qemu-img info output for %s: %w", path, err)
	}

	if info.Format == "" {
		return "", fmt.Errorf("qemu-img info returned empty format for %s", path)
	}

	return info.Format, nil
}
