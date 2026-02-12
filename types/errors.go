package types

import "errors"

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
