package types

import (
	"errors"
	"fmt"
)

// ErrorType categorizes VM errors for structured error reporting.
type ErrorType string

const (
	// Creation errors
	ErrorOCIConversion    ErrorType = "oci_conversion_failed"
	ErrorDiskCreation     ErrorType = "disk_creation_failed"
	ErrorInsufficientDisk ErrorType = "insufficient_disk_space"
	ErrorImageNotBootable ErrorType = "image_not_bootable"

	// Boot errors
	ErrorBootTimeout       ErrorType = "boot_timeout"
	ErrorKernelPanic       ErrorType = "kernel_panic"
	ErrorBootFailure       ErrorType = "boot_failure"
	ErrorMissingBootloader ErrorType = "missing_bootloader"
	ErrorMissingKernel     ErrorType = "missing_kernel"

	// Runtime errors
	ErrorCHCrash            ErrorType = "cloud_hypervisor_crash"
	ErrorGuestCrash         ErrorType = "guest_crash"
	ErrorResourceExhaustion ErrorType = "resource_exhaustion"

	// Shutdown errors
	ErrorStopTimeout     ErrorType = "stop_timeout"
	ErrorForceKillFailed ErrorType = "force_kill_failed"

	// Reference errors
	ErrorChecksumCollision ErrorType = "checksum_collision"
)

// Sentinel errors for common failure modes.
var (
	ErrVMNotFound        = errors.New("VM not found")
	ErrVMAlreadyExists   = errors.New("VM already exists")
	ErrVMRunning         = errors.New("VM is running, use --force to delete")
	ErrInvalidTransition = errors.New("invalid state transition")
	ErrLockTimeout       = errors.New("failed to acquire lock")
	ErrChecksumCollision = errors.New("checksum collision detected")
	ErrImageNotBootable  = errors.New("image is not bootable")
	ErrCHNotFound        = errors.New("cloud-hypervisor binary not found")
	ErrFirmwareNotFound  = errors.New("firmware file not found")
)

// ErrorCategory distinguishes transient (retriable) errors from permanent ones.
type ErrorCategory string

const (
	// ErrorCategoryTransient indicates an error that may succeed on retry
	// (e.g., network timeout, HTTP 429/5xx).
	ErrorCategoryTransient ErrorCategory = "transient"
	// ErrorCategoryPermanent indicates an error that will not succeed on retry
	// (e.g., HTTP 404, invalid image format, missing file).
	ErrorCategoryPermanent ErrorCategory = "permanent"
)

// ClassifiedError wraps an error with a category (transient or permanent)
// so callers can decide whether to retry.
type ClassifiedError struct {
	Category ErrorCategory
	Err      error
}

func (e *ClassifiedError) Error() string {
	return fmt.Sprintf("[%s] %s", e.Category, e.Err.Error())
}

func (e *ClassifiedError) Unwrap() error {
	return e.Err
}

// IsTransient returns true if the error (or any error in its chain) is a
// ClassifiedError with category transient.
func IsTransient(err error) bool {
	var ce *ClassifiedError
	if errors.As(err, &ce) {
		return ce.Category == ErrorCategoryTransient
	}
	return false
}

// NewTransientError wraps err as a transient (retriable) classified error.
func NewTransientError(err error) *ClassifiedError {
	return &ClassifiedError{Category: ErrorCategoryTransient, Err: err}
}

// NewPermanentError wraps err as a permanent (non-retriable) classified error.
func NewPermanentError(err error) *ClassifiedError {
	return &ClassifiedError{Category: ErrorCategoryPermanent, Err: err}
}
