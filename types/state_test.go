package types

import (
	"errors"
	"fmt"
	"testing"
)

// ---------------------------------------------------------------------------
// ValidateTransition tests (table-driven)
// ---------------------------------------------------------------------------

func TestValidateTransition_ValidTransitions(t *testing.T) {
	tests := []struct {
		from VMState
		to   VMState
	}{
		{VMStateCreating, VMStateCreated},
		{VMStateCreating, VMStateError},
		{VMStateCreated, VMStateStarting},
		{VMStateCreated, VMStateDeleted},
		{VMStateStarting, VMStateRunning},
		{VMStateStarting, VMStateError},
		{VMStateRunning, VMStateStopping},
		{VMStateRunning, VMStateError},
		{VMStateStopping, VMStateStopped},
		{VMStateStopping, VMStateError},
		{VMStateStopped, VMStateStarting},
		{VMStateStopped, VMStateDeleted},
		{VMStateError, VMStateDeleted},
	}

	for _, tt := range tests {
		name := fmt.Sprintf("%s->%s", tt.from, tt.to)
		t.Run(name, func(t *testing.T) {
			if err := ValidateTransition(tt.from, tt.to); err != nil {
				t.Errorf("expected valid transition %s -> %s, got error: %v", tt.from, tt.to, err)
			}
		})
	}
}

func TestValidateTransition_DeletedHasNoValidTargets(t *testing.T) {
	allStates := []VMState{
		VMStateCreating, VMStateCreated, VMStateStarting,
		VMStateRunning, VMStateStopping, VMStateStopped,
		VMStateError, VMStateDeleted,
	}
	for _, to := range allStates {
		name := fmt.Sprintf("DELETED->%s", to)
		t.Run(name, func(t *testing.T) {
			if err := ValidateTransition(VMStateDeleted, to); err == nil {
				t.Errorf("expected invalid transition DELETED -> %s, got nil error", to)
			}
		})
	}
}

func TestValidateTransition_InvalidTransitions(t *testing.T) {
	tests := []struct {
		from VMState
		to   VMState
	}{
		{VMStateCreated, VMStateRunning},
		{VMStateRunning, VMStateCreated},
		{VMStateStopped, VMStateRunning},
		{VMStateDeleted, VMStateCreated},
		{VMStateStarting, VMStateStopped},
	}

	for _, tt := range tests {
		name := fmt.Sprintf("%s->%s", tt.from, tt.to)
		t.Run(name, func(t *testing.T) {
			if err := ValidateTransition(tt.from, tt.to); err == nil {
				t.Errorf("expected invalid transition %s -> %s, but got nil error", tt.from, tt.to)
			}
		})
	}
}

func TestValidateTransition_UnknownState(t *testing.T) {
	err := ValidateTransition(VMState("UNKNOWN"), VMStateCreated)
	if err == nil {
		t.Error("expected error for unknown state, got nil")
	}
}

// ---------------------------------------------------------------------------
// IsTerminal tests
// ---------------------------------------------------------------------------

func TestIsTerminal(t *testing.T) {
	tests := []struct {
		state    VMState
		terminal bool
	}{
		{VMStateCreating, false},
		{VMStateCreated, false},
		{VMStateStarting, false},
		{VMStateRunning, false},
		{VMStateStopping, false},
		{VMStateStopped, false},
		{VMStateError, false},
		{VMStateDeleted, true},
	}

	for _, tt := range tests {
		t.Run(string(tt.state), func(t *testing.T) {
			if got := tt.state.IsTerminal(); got != tt.terminal {
				t.Errorf("IsTerminal(%s) = %v, want %v", tt.state, got, tt.terminal)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// IsRunnable tests
// ---------------------------------------------------------------------------

func TestIsRunnable(t *testing.T) {
	tests := []struct {
		state    VMState
		runnable bool
	}{
		{VMStateCreating, false},
		{VMStateCreated, false},
		{VMStateStarting, true},
		{VMStateRunning, true},
		{VMStateStopping, true},
		{VMStateStopped, false},
		{VMStateError, false},
		{VMStateDeleted, false},
	}

	for _, tt := range tests {
		t.Run(string(tt.state), func(t *testing.T) {
			if got := tt.state.IsRunnable(); got != tt.runnable {
				t.Errorf("IsRunnable(%s) = %v, want %v", tt.state, got, tt.runnable)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// ClassifiedError / IsTransient tests
// ---------------------------------------------------------------------------

func TestIsTransient_TransientError(t *testing.T) {
	err := NewTransientError(errors.New("net timeout"))
	if !IsTransient(err) {
		t.Error("expected IsTransient to return true for NewTransientError")
	}
}

func TestIsTransient_PermanentError(t *testing.T) {
	err := NewPermanentError(errors.New("not found"))
	if IsTransient(err) {
		t.Error("expected IsTransient to return false for NewPermanentError")
	}
}

func TestIsTransient_WrappedTransientError(t *testing.T) {
	inner := NewTransientError(errors.New("timeout"))
	wrapped := fmt.Errorf("wrapped: %w", inner)
	if !IsTransient(wrapped) {
		t.Error("expected IsTransient to return true for wrapped transient error")
	}
}

func TestIsTransient_Nil(t *testing.T) {
	if IsTransient(nil) {
		t.Error("expected IsTransient to return false for nil")
	}
}

func TestIsTransient_PlainError(t *testing.T) {
	err := errors.New("something")
	if IsTransient(err) {
		t.Error("expected IsTransient to return false for plain error")
	}
}

// ---------------------------------------------------------------------------
// ClassifiedError.Error() format test
// ---------------------------------------------------------------------------

func TestClassifiedError_ErrorFormat(t *testing.T) {
	err := NewTransientError(errors.New("connection reset"))
	got := err.Error()
	expected := "[transient] connection reset"
	if got != expected {
		t.Errorf("ClassifiedError.Error() = %q, want %q", got, expected)
	}
}

func TestClassifiedError_Unwrap(t *testing.T) {
	inner := errors.New("original")
	ce := NewPermanentError(inner)
	if !errors.Is(ce, inner) {
		t.Error("expected Unwrap to expose the inner error via errors.Is")
	}
}
