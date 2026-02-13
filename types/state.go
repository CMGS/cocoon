package types

import (
	"fmt"
	"slices"
)

// VMState represents the current state of a VM in its lifecycle.
type VMState string

const (
	VMStateCreating VMState = "CREATING"
	VMStateCreated  VMState = "CREATED"
	VMStateStarting VMState = "STARTING"
	VMStateRunning  VMState = "RUNNING"
	VMStateStopping VMState = "STOPPING"
	VMStateStopped  VMState = "STOPPED"
	VMStateError    VMState = "ERROR"
	VMStateDeleted  VMState = "DELETED"
)

// ValidTransitions defines allowed state transitions.
var ValidTransitions = map[VMState][]VMState{
	VMStateCreating: {VMStateCreated, VMStateError},
	VMStateCreated:  {VMStateStarting, VMStateDeleted},
	VMStateStarting: {VMStateRunning, VMStateError},
	VMStateRunning:  {VMStateStopping, VMStateError},
	VMStateStopping: {VMStateStopped, VMStateError},
	VMStateStopped:  {VMStateStarting, VMStateDeleted},
	VMStateError:    {VMStateStopped, VMStateDeleted},
	VMStateDeleted:  {},
}

// ValidateTransition checks if a state transition is allowed.
func ValidateTransition(from, to VMState) error {
	allowed, exists := ValidTransitions[from]
	if !exists {
		return fmt.Errorf("unknown state: %s", from)
	}
	if slices.Contains(allowed, to) {
		return nil
	}
	return fmt.Errorf("invalid transition: %s -> %s", from, to)
}

// IsTerminal returns true if the state is terminal (no further transitions).
func (s VMState) IsTerminal() bool {
	return s == VMStateDeleted
}

// IsRunnable returns true if a VM in this state has a running CH process.
func (s VMState) IsRunnable() bool {
	return s == VMStateStarting || s == VMStateRunning || s == VMStateStopping
}
