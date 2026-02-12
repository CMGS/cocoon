package vm

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/CMGS/cocoon/types"
	"github.com/CMGS/cocoon/utils"
)

// Reconcile scans all VMs and detects inconsistencies between metadata and
// actual system state. When fix is true, it attempts to repair them. When
// force is true, it will also kill zombie processes and force-move stuck VMs
// to ERROR state.
//
// The name index is always rebuilt during reconciliation since it is a
// derived cache (source of truth is config.json).
func (m *manager) Reconcile(ctx context.Context, fix bool, force bool) ([]Inconsistency, error) {
	entries, err := os.ReadDir(m.cfg.VMDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("scan VM directory: %w", err)
	}

	var inconsistencies []Inconsistency

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		vmID := entry.Name()

		// Check config.json existence (Priority 0 source of truth).
		configPath := m.cfg.VMConfigPath(vmID)
		if _, statErr := os.Stat(configPath); os.IsNotExist(statErr) {
			inconsistencies = append(inconsistencies, Inconsistency{
				VMID:     vmID,
				Type:     InconsistencyMetadataCorrupt,
				Severity: SeverityCritical,
				Details:  "config.json missing; VM directory is orphaned",
			})
			continue
		}

		// Load metadata.json.
		meta, metaErr := m.LoadMetadata(vmID)
		if metaErr != nil {
			inconsistencies = append(inconsistencies, Inconsistency{
				VMID:     vmID,
				Type:     InconsistencyMetadataCorrupt,
				Severity: SeverityCritical,
				Details:  fmt.Sprintf("failed to load metadata.json: %v", metaErr),
			})
			continue
		}

		// Load config for path information.
		vmCfg, _ := m.LoadConfig(vmID)

		// Determine actual state by probing the system.
		actualState := m.determineActualState(meta, vmCfg)
		metaState := types.VMState(meta.State)

		if actualState != metaState {
			inconsistencies = append(inconsistencies, Inconsistency{
				VMID:          vmID,
				Type:          InconsistencyStateMismatch,
				Severity:      reconcileSeverity(metaState, actualState),
				Details:       fmt.Sprintf("metadata=%s, actual=%s", meta.State, actualState),
				ExpectedState: meta.State,
				ActualState:   string(actualState),
			})
		}

		// Detect zombie resources.
		zombies := m.detectZombieResources(vmID, meta, vmCfg)
		inconsistencies = append(inconsistencies, zombies...)

		// Check overlay existence.
		if vmCfg != nil && vmCfg.OverlayPath != "" {
			if _, overlayErr := os.Stat(vmCfg.OverlayPath); os.IsNotExist(overlayErr) {
				if metaState != types.VMStateDeleted {
					inconsistencies = append(inconsistencies, Inconsistency{
						VMID:     vmID,
						Type:     InconsistencyMissingOverlay,
						Severity: SeverityCritical,
						Details:  fmt.Sprintf("overlay missing at %s", vmCfg.OverlayPath),
					})
				}
			}
		}
	}

	// Apply fixes if requested.
	if fix {
		for i := range inconsistencies {
			if err := m.applyFix(&inconsistencies[i], force); err != nil {
				// Record the error but keep going.
				inconsistencies[i].Details += fmt.Sprintf(" (fix failed: %v)", err)
			}
		}
	}

	// Rebuild the name index (it is a derived cache).
	if _, err := RebuildNameIndex(m.cfg); err != nil {
		inconsistencies = append(inconsistencies, Inconsistency{
			VMID:     "",
			Type:     InconsistencyMetadataCorrupt,
			Severity: SeverityWarning,
			Details:  fmt.Sprintf("failed to rebuild name index: %v", err),
		})
	}

	return inconsistencies, nil
}

// determineActualState probes the system to find out what a VM is really doing.
// Uses the priority order from the RFC: process status > socket > metadata > overlay > PID file.
func (m *manager) determineActualState(meta *types.VMMetadataFile, vmCfg *types.VMConfig) types.VMState {
	metaState := types.VMState(meta.State)
	pid := meta.ProcessPID

	socketPath := ""
	if vmCfg != nil {
		socketPath = vmCfg.SocketPath
	}

	processRunning := utils.IsProcessAlive(pid)
	processValid := false
	if processRunning {
		processValid = utils.ValidateProcess(pid, "cloud-hypervisor")
	}

	socketExists := false
	socketConnectable := false
	if socketPath != "" {
		if _, err := os.Stat(socketPath); err == nil {
			socketExists = true
			socketConnectable = canConnectToSocket(socketPath)
		}
	}

	switch metaState {
	case types.VMStateRunning:
		if processValid && (socketConnectable || socketExists) {
			return types.VMStateRunning
		}
		// Process dead or PID reused -> crashed.
		return types.VMStateError

	case types.VMStateStarting:
		if processValid {
			return types.VMStateStarting // still booting
		}
		// Process died during start.
		return types.VMStateError

	case types.VMStateStopping:
		if processValid {
			return types.VMStateStopping // still shutting down
		}
		// Process exited -> actually stopped.
		return types.VMStateStopped

	case types.VMStateStopped:
		if processRunning {
			// Process shouldn't be running if metadata says STOPPED.
			return types.VMStateError
		}
		return types.VMStateStopped

	case types.VMStateCreated:
		if processRunning {
			return types.VMStateError // unexpected process
		}
		return types.VMStateCreated

	case types.VMStateError:
		return types.VMStateError

	case types.VMStateCreating:
		// If stuck in CREATING the creation did not complete.
		return types.VMStateError

	default:
		return metaState
	}
}

// detectZombieResources finds stale PID files and zombie sockets.
func (m *manager) detectZombieResources(vmID string, meta *types.VMMetadataFile, vmCfg *types.VMConfig) []Inconsistency {
	var zombies []Inconsistency

	// Check PID file.
	pidFilePath := m.cfg.VMPIDPath(vmID)
	if pidData, err := os.ReadFile(pidFilePath); err == nil { //nolint:gosec // G304: PID file path is derived from internal config
		pidFromFile, _ := strconv.Atoi(strings.TrimSpace(string(pidData)))
		if pidFromFile > 0 && pidFromFile != meta.ProcessPID {
			zombies = append(zombies, Inconsistency{
				VMID:     vmID,
				Type:     InconsistencyStalePIDFile,
				Severity: SeverityWarning,
				Details:  fmt.Sprintf("PID file has %d, metadata has %d", pidFromFile, meta.ProcessPID),
			})
		}
	}

	// Check for zombie socket.
	socketPath := ""
	if vmCfg != nil {
		socketPath = vmCfg.SocketPath
	}
	if socketPath != "" {
		if _, err := os.Stat(socketPath); err == nil {
			if !utils.IsProcessAlive(meta.ProcessPID) {
				zombies = append(zombies, Inconsistency{
					VMID:     vmID,
					Type:     InconsistencyZombieSocket,
					Severity: SeverityWarning,
					Details:  fmt.Sprintf("socket exists at %s but process %d not running", socketPath, meta.ProcessPID),
				})
			}
		}
	}

	return zombies
}

// applyFix attempts to repair an inconsistency.
func (m *manager) applyFix(inc *Inconsistency, force bool) error {
	switch inc.Type {
	case InconsistencyStateMismatch:
		return m.fixStateMismatch(inc, force)

	case InconsistencyZombieSocket:
		// Remove the stale socket file.
		vmCfg, err := m.LoadConfig(inc.VMID)
		if err != nil {
			return err
		}
		return os.Remove(vmCfg.SocketPath)

	case InconsistencyStalePIDFile:
		pidFilePath := m.cfg.VMPIDPath(inc.VMID)
		return os.Remove(pidFilePath)

	case InconsistencyZombieProcess:
		if !force {
			return fmt.Errorf("--force required to kill zombie processes")
		}
		meta, err := m.LoadMetadata(inc.VMID)
		if err != nil {
			return err
		}
		if meta.ProcessPID > 0 {
			_ = syscall.Kill(meta.ProcessPID, syscall.SIGKILL)
			meta.ProcessPID = 0
			return m.SaveMetadata(meta)
		}
		return nil

	case InconsistencyMetadataCorrupt:
		// Cannot auto-fix corrupt metadata without more context.
		return fmt.Errorf("manual intervention required for corrupt metadata")

	case InconsistencyMissingOverlay:
		// Cannot recreate an overlay.
		return fmt.Errorf("overlay disk missing; VM data is lost")

	default:
		return fmt.Errorf("unknown inconsistency type: %s", inc.Type)
	}
}

// fixStateMismatch updates metadata.json to reflect the actual system state.
func (m *manager) fixStateMismatch(inc *Inconsistency, force bool) error {
	meta, err := m.LoadMetadata(inc.VMID)
	if err != nil {
		return err
	}

	actualState := types.VMState(inc.ActualState)

	switch actualState {
	case types.VMStateError:
		meta.PreviousState = meta.State
		meta.State = string(types.VMStateError)
		meta.LastError = fmt.Sprintf("reconciliation: was %s but actual state is ERROR (%s)", inc.ExpectedState, inc.Details)
		meta.ErrorCount++

		// Kill zombie process if present and force is set.
		if force && meta.ProcessPID > 0 && utils.IsProcessAlive(meta.ProcessPID) {
			// Only kill if it is actually cloud-hypervisor.
			if utils.ValidateProcess(meta.ProcessPID, "cloud-hypervisor") {
				_ = syscall.Kill(meta.ProcessPID, syscall.SIGKILL)
			}
		}
		meta.ProcessPID = 0
		return m.SaveMetadata(meta)

	case types.VMStateStopped:
		meta.PreviousState = meta.State
		meta.State = string(types.VMStateStopped)
		meta.ProcessPID = 0
		meta.StoppedAt = time.Now().UTC().Format(time.RFC3339)
		return m.SaveMetadata(meta)

	default:
		if !force {
			return fmt.Errorf("--force required to reconcile state %s -> %s", inc.ExpectedState, inc.ActualState)
		}
		meta.PreviousState = meta.State
		meta.State = inc.ActualState
		return m.SaveMetadata(meta)
	}
}

// ---------------------------------------------------------------------------
// Process / socket probing helpers
// ---------------------------------------------------------------------------

// canConnectToSocket attempts a Unix domain socket connection with a 1s timeout.
func canConnectToSocket(socketPath string) bool {
	conn, err := net.DialTimeout("unix", socketPath, 1*time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// reconcileSeverity returns the severity based on the state mismatch.
func reconcileSeverity(expected, actual types.VMState) InconsistencySeverity {
	// A running VM that is actually dead is critical.
	if expected == types.VMStateRunning && actual == types.VMStateError {
		return SeverityCritical
	}
	// Transient states that resolved are info.
	if expected == types.VMStateStopping && actual == types.VMStateStopped {
		return SeverityInfo
	}
	return SeverityWarning
}
