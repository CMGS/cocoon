package oci

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/CMGS/cocoon/types"
)

// classifyRegistryError categorizes a registry error (from push or pull) as
// transient or permanent. op should be "push" or "pull" for the error prefix.
func classifyRegistryError(op string, err error) error {
	errStr := err.Error()

	// Permanent errors: authentication, authorization, not found.
	permanentPatterns := []string{
		"UNAUTHORIZED", "unauthorized",
		"DENIED", "denied",
		"FORBIDDEN", "forbidden",
		"NAME_UNKNOWN", "not found",
		"MANIFEST_UNKNOWN",
		"invalid reference",
	}
	for _, p := range permanentPatterns {
		if strings.Contains(errStr, p) {
			return types.NewPermanentError(fmt.Errorf("%s %w", op, err))
		}
	}

	// Transient errors: network issues, timeouts, server errors.
	if isRegistryNetworkError(err) {
		return types.NewTransientError(fmt.Errorf("%s %w", op, err))
	}
	transientPatterns := []string{
		"timeout", "connection refused", "connection reset",
		"EOF", "INTERNAL_ERROR", "BAD_GATEWAY",
		"SERVICE_UNAVAILABLE", "TOO_MANY_REQUESTS",
		"Service Unavailable", "Too Many Requests",
		"Internal Server Error", "Bad Gateway",
	}
	errUpper := strings.ToUpper(errStr)
	for _, p := range transientPatterns {
		if strings.Contains(errUpper, strings.ToUpper(p)) {
			return types.NewTransientError(fmt.Errorf("%s %w", op, err))
		}
	}

	// Default to permanent for unknown errors.
	return types.NewPermanentError(fmt.Errorf("%s %w", op, err))
}

// isRegistryNetworkError returns true when err wraps a net.OpError or is a
// timeout error, indicating a transient network failure.
func isRegistryNetworkError(err error) bool {
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	return os.IsTimeout(err)
}
