package engine

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/CMGS/cocoon/lock/flock"
	"github.com/CMGS/cocoon/types"
	"github.com/CMGS/cocoon/utils"
	"github.com/CMGS/cocoon/vm"
)

// Reconcile scans all VMs and detects inconsistencies between metadata and
// actual system state. When fix is true, it attempts to repair them. When
// force is true, it will also kill zombie processes and force-move stuck VMs
// to ERROR state.
func (m *manager) Reconcile(ctx context.Context, fix bool, force bool) ([]vm.Inconsistency, error) {
	entries, err := os.ReadDir(m.cfg.VMDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("scan VM directory: %w", err)
	}

	var inconsistencies []vm.Inconsistency
	knownPIDs := make(map[int]string) // pid -> vmID
	vmConfigs := make(map[string]*types.VMConfig)

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		vmID := entry.Name()

		configPath := m.cfg.VMConfigPath(vmID)
		if _, statErr := os.Stat(configPath); os.IsNotExist(statErr) {
			overlayPath := m.cfg.VMOverlayPath(vmID)
			if _, overlayErr := os.Stat(overlayPath); overlayErr == nil {
				inconsistencies = append(inconsistencies, vm.Inconsistency{
					VMID:     vmID,
					Type:     vm.InconsistencyOrphanedOverlay,
					Severity: vm.SeverityCritical,
					Details:  fmt.Sprintf("overlay exists at %s but config.json is missing", overlayPath),
				})
			} else {
				inconsistencies = append(inconsistencies, vm.Inconsistency{
					VMID:     vmID,
					Type:     vm.InconsistencyMetadataCorrupt,
					Severity: vm.SeverityCritical,
					Details:  "config.json missing; VM directory is orphaned",
				})
			}
			continue
		}

		vmCfg, cfgErr := m.LoadConfig(vmID)
		if cfgErr != nil {
			inconsistencies = append(inconsistencies, vm.Inconsistency{
				VMID:     vmID,
				Type:     vm.InconsistencyMetadataCorrupt,
				Severity: vm.SeverityCritical,
				Details:  fmt.Sprintf("failed to load config.json: %v", cfgErr),
			})
			continue
		}
		vmConfigs[vmID] = vmCfg

		// Crash point 2: config exists but references.json does not contain vmID.
		if vmCfg.BaseKey != "" {
			refs, refsErr := m.refCounter.GetReferences(vmCfg.BaseKey)
			if refsErr != nil {
				inconsistencies = append(inconsistencies, vm.Inconsistency{
					VMID:     vmID,
					Type:     vm.InconsistencyMetadataCorrupt,
					Severity: vm.SeverityWarning,
					Details:  fmt.Sprintf("failed to load references for base_key %s: %v", vmCfg.BaseKey, refsErr),
				})
			} else if !slices.Contains(refs, vmID) {
				inconsistencies = append(inconsistencies, vm.Inconsistency{
					VMID:       vmID,
					Type:       vm.InconsistencyMissingReference,
					Severity:   vm.SeverityWarning,
					Details:    fmt.Sprintf("config exists but references.json missing vmID under base_key %s", vmCfg.BaseKey),
					BaseKey:    vmCfg.BaseKey,
					DigestFull: vmCfg.BaseDigestFull,
					ImageRef:   vmCfg.ImageRef,
				})
			}
		}

		// Load metadata.json.
		meta, metaErr := m.LoadMetadata(vmID)
		if metaErr != nil {
			inconsistencies = append(inconsistencies, vm.Inconsistency{
				VMID:     vmID,
				Type:     vm.InconsistencyMetadataCorrupt,
				Severity: vm.SeverityCritical,
				Details:  fmt.Sprintf("failed to load metadata.json: %v", metaErr),
			})
			continue
		}

		// VM marked DELETED but directory still exists.
		if types.VMState(meta.State) == types.VMStateDeleted {
			inconsistencies = append(inconsistencies, vm.Inconsistency{
				VMID:     vmID,
				Type:     vm.InconsistencyDeletedVMDir,
				Severity: vm.SeverityInfo,
				Details:  "metadata state is DELETED but VM directory/resources still exist",
			})
			continue
		}

		// Track known PIDs for orphan detection.
		if meta.ProcessPID > 0 {
			knownPIDs[meta.ProcessPID] = vmID
		}

		// Determine actual state by probing the system.
		actualState := m.determineActualState(meta, vmCfg)
		metaState := types.VMState(meta.State)

		if actualState != metaState {
			inconsistencies = append(inconsistencies, vm.Inconsistency{
				VMID:          vmID,
				Type:          vm.InconsistencyStateMismatch,
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
		if vmCfg.OverlayPath != "" {
			if _, overlayErr := os.Stat(vmCfg.OverlayPath); os.IsNotExist(overlayErr) {
				if metaState != types.VMStateDeleted {
					inconsistencies = append(inconsistencies, vm.Inconsistency{
						VMID:     vmID,
						Type:     vm.InconsistencyMissingOverlay,
						Severity: vm.SeverityCritical,
						Details:  fmt.Sprintf("overlay missing at %s", vmCfg.OverlayPath),
					})
				}
			}
		}
	}

	refIssues, refErr := m.detectDanglingReferenceIssues(vmConfigs)
	if refErr != nil {
		inconsistencies = append(inconsistencies, vm.Inconsistency{
			Type:     vm.InconsistencyMetadataCorrupt,
			Severity: vm.SeverityWarning,
			Details:  fmt.Sprintf("failed to scan references.json: %v", refErr),
		})
	} else {
		inconsistencies = append(inconsistencies, refIssues...)
	}

	nameIssues, nameErr := m.detectNameIndexIssues(vmConfigs)
	if nameErr != nil {
		inconsistencies = append(inconsistencies, vm.Inconsistency{
			Type:     vm.InconsistencyMetadataCorrupt,
			Severity: vm.SeverityWarning,
			Details:  fmt.Sprintf("failed to check name index: %v", nameErr),
		})
	} else {
		inconsistencies = append(inconsistencies, nameIssues...)
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

	// Detect orphaned cloud-hypervisor and swtpm processes not tracked by any VM.
	orphans := detectOrphanedProcesses(knownPIDs)
	inconsistencies = append(inconsistencies, orphans...)

	return inconsistencies, nil
}

func (m *manager) detectDanglingReferenceIssues(vmConfigs map[string]*types.VMConfig) ([]vm.Inconsistency, error) {
	refs, err := m.loadReferencesSnapshot()
	if err != nil {
		return nil, err
	}

	issues := make([]vm.Inconsistency, 0)
	for baseKey, entry := range refs {
		if entry == nil {
			continue
		}
		for _, vmID := range entry.Refs {
			vmCfg, ok := vmConfigs[vmID]
			if !ok {
				issues = append(issues, vm.Inconsistency{
					VMID:     vmID,
					Type:     vm.InconsistencyDanglingReference,
					Severity: vm.SeverityWarning,
					Details:  fmt.Sprintf("references.json contains vmID %s under base_key %s but VM config is missing", vmID, baseKey),
					BaseKey:  baseKey,
				})
				continue
			}
			if vmCfg.BaseKey != "" && vmCfg.BaseKey != baseKey {
				issues = append(issues, vm.Inconsistency{
					VMID:     vmID,
					Type:     vm.InconsistencyDanglingReference,
					Severity: vm.SeverityWarning,
					Details:  fmt.Sprintf("references.json maps vmID %s to %s but config.json base_key is %s", vmID, baseKey, vmCfg.BaseKey),
					BaseKey:  baseKey,
				})
			}
		}
	}

	return issues, nil
}

func (m *manager) detectNameIndexIssues(vmConfigs map[string]*types.VMConfig) ([]vm.Inconsistency, error) {
	desired := make(vm.NameIndex)
	issues := make([]vm.Inconsistency, 0)

	for vmID, cfg := range vmConfigs {
		name := strings.TrimSpace(cfg.Name)
		if name == "" {
			continue
		}
		if existing, ok := desired[name]; ok && existing != vmID {
			issues = append(issues, vm.Inconsistency{
				VMID:     vmID,
				Type:     vm.InconsistencyDuplicateVMName,
				Severity: vm.SeverityWarning,
				Details:  fmt.Sprintf("duplicate VM name %q in config.json (vm_id=%s and vm_id=%s)", name, existing, vmID),
			})
			continue
		}
		desired[name] = vmID
	}

	current, err := LoadNameIndex(m.cfg)
	if err != nil {
		return nil, err
	}
	if !nameIndexEqual(current, desired) {
		issues = append(issues, vm.Inconsistency{
			Type:     vm.InconsistencyNameIndexStale,
			Severity: vm.SeverityWarning,
			Details:  fmt.Sprintf("name-index.json is out of sync with VM configs (index=%d, expected=%d)", len(current), len(desired)),
		})
	}

	return issues, nil
}

func nameIndexEqual(a, b vm.NameIndex) bool {
	if len(a) != len(b) {
		return false
	}
	for name, vmID := range a {
		if b[name] != vmID {
			return false
		}
	}
	return true
}

func (m *manager) loadReferencesSnapshot() (types.ReferencesFile, error) {
	fl := flock.New(m.cfg.ReferencesLock())
	if err := fl.Lock(); err != nil {
		return nil, fmt.Errorf("acquire references.lock: %w", err)
	}
	defer fl.Unlock() //nolint:errcheck

	refs := make(types.ReferencesFile)
	err := utils.ReadJSON(m.cfg.ReferencesFile(), &refs)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("load references.json: %w", err)
	}
	return refs, nil
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

	socketConnectable := false
	if socketPath != "" {
		if _, err := os.Stat(socketPath); err == nil {
			socketConnectable = canConnectToSocket(socketPath)
		}
	}

	switch metaState {
	case types.VMStateRunning:
		if processValid && socketConnectable {
			return types.VMStateRunning
		}
		// Process dead or PID reused -> crashed.
		return types.VMStateError

	case types.VMStateStarting:
		if processValid {
			if isStuckInState(meta.UpdatedAt, 5*time.Minute) {
				return types.VMStateError
			}
			return types.VMStateStarting // still booting
		}
		// Process died during start.
		return types.VMStateError

	case types.VMStateStopping:
		if processValid {
			if isStuckInState(meta.UpdatedAt, 2*time.Minute) {
				return types.VMStateError
			}
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

	case types.VMStateDeleted:
		// Directory should not exist for DELETED VMs.
		return types.VMStateError

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
func (m *manager) detectZombieResources(vmID string, meta *types.VMMetadataFile, vmCfg *types.VMConfig) []vm.Inconsistency {
	var zombies []vm.Inconsistency

	// Check PID file.
	pidFilePath := m.cfg.VMPIDPath(vmID)
	if pidData, err := os.ReadFile(pidFilePath); err == nil { //nolint:gosec // G304: PID file path is derived from internal config
		pidFromFile, _ := strconv.Atoi(strings.TrimSpace(string(pidData)))
		if pidFromFile > 0 && pidFromFile != meta.ProcessPID {
			zombies = append(zombies, vm.Inconsistency{
				VMID:     vmID,
				Type:     vm.InconsistencyStalePIDFile,
				Severity: vm.SeverityWarning,
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
			if !utils.ValidateProcess(meta.ProcessPID, "cloud-hypervisor") {
				zombies = append(zombies, vm.Inconsistency{
					VMID:     vmID,
					Type:     vm.InconsistencyZombieSocket,
					Severity: vm.SeverityWarning,
					Details:  fmt.Sprintf("socket exists at %s but process %d not running", socketPath, meta.ProcessPID),
				})
			}
		}
	}

	return zombies
}

// applyFix attempts to repair an inconsistency.
func (m *manager) applyFix(inc *vm.Inconsistency, force bool) error {
	switch inc.Type {
	case vm.InconsistencyStateMismatch:
		return m.fixStateMismatch(inc, force)

	case vm.InconsistencyZombieSocket:
		// Remove the stale socket file.
		vmCfg, err := m.LoadConfig(inc.VMID)
		if err != nil {
			return err
		}
		return os.Remove(vmCfg.SocketPath)

	case vm.InconsistencyStalePIDFile:
		pidFilePath := m.cfg.VMPIDPath(inc.VMID)
		return os.Remove(pidFilePath)

	case vm.InconsistencyZombieProcess:
		if !force {
			return fmt.Errorf("--force required to kill zombie processes")
		}
		if inc.VMID == "" {
			return nil
		}
		meta, err := m.LoadMetadata(inc.VMID)
		if err != nil {
			return err
		}
		if meta.ProcessPID > 0 {
			// Only kill if it is actually cloud-hypervisor (guard against PID reuse).
			if utils.ValidateProcess(meta.ProcessPID, "cloud-hypervisor") {
				_ = syscall.Kill(meta.ProcessPID, syscall.SIGKILL)
			}
			meta.ProcessPID = 0
			return m.SaveMetadata(meta)
		}
		return nil

	case vm.InconsistencyOrphanedOverlay:
		return m.fixOrphanedOverlay(inc.VMID)

	case vm.InconsistencyMissingReference:
		baseKey := inc.BaseKey
		digestFull := inc.DigestFull
		imageRef := inc.ImageRef

		if baseKey == "" || imageRef == "" {
			cfg, err := m.LoadConfig(inc.VMID)
			if err != nil {
				return err
			}
			if baseKey == "" {
				baseKey = cfg.BaseKey
			}
			if digestFull == "" {
				digestFull = cfg.BaseDigestFull
			}
			if imageRef == "" {
				imageRef = cfg.ImageRef
			}
		}

		if baseKey == "" {
			return fmt.Errorf("missing base_key for reference repair")
		}

		return m.refCounter.AddReference(baseKey, inc.VMID, digestFull, imageRef)

	case vm.InconsistencyDanglingReference:
		if inc.BaseKey == "" {
			return fmt.Errorf("missing base_key for dangling reference cleanup")
		}
		return m.refCounter.RemoveReference(inc.BaseKey, inc.VMID)

	case vm.InconsistencyNameIndexStale:
		_, err := RebuildNameIndex(m.cfg)
		return err

	case vm.InconsistencyDuplicateVMName:
		return fmt.Errorf("duplicate VM name requires manual intervention")

	case vm.InconsistencyDeletedVMDir:
		return m.cleanupDeletedVMArtifacts(inc.VMID)

	case vm.InconsistencyMetadataCorrupt:
		// Cannot auto-fix corrupt metadata without more context.
		return fmt.Errorf("manual intervention required for corrupt metadata")

	case vm.InconsistencyMissingOverlay:
		// Cannot recreate an overlay.
		return fmt.Errorf("overlay disk missing; VM data is lost")

	default:
		return fmt.Errorf("unknown inconsistency type: %s", inc.Type)
	}
}

func (m *manager) cleanupDeletedVMArtifacts(vmID string) error {
	_ = os.RemoveAll(m.cfg.VMPersistDir(vmID))
	_ = os.RemoveAll(m.cfg.VMRuntimeDir(vmID))
	_ = os.Remove(m.cfg.VMSerialLogPath(vmID))
	_ = os.Remove(m.cfg.VMCHLogPath(vmID))
	_ = os.Remove(m.cfg.VMSwtpmLogPath(vmID))
	return nil
}

func (m *manager) fixOrphanedOverlay(vmID string) error {
	overlayPath := m.cfg.VMOverlayPath(vmID)
	if _, err := os.Stat(overlayPath); os.IsNotExist(err) {
		return nil
	}

	if err := os.MkdirAll(m.cfg.TrashDir(), 0o755); err != nil { //nolint:gosec // G301: trash dir permissions align with other cocoon state dirs
		return fmt.Errorf("ensure trash dir: %w", err)
	}

	trashName := fmt.Sprintf("%d_%s-orphan-overlay.qcow2", time.Now().UnixNano(), vmID)
	trashPath := filepath.Join(m.cfg.TrashDir(), trashName)
	if err := os.Rename(overlayPath, trashPath); err != nil {
		if rmErr := os.Remove(overlayPath); rmErr != nil {
			return fmt.Errorf("cleanup orphaned overlay: rename=%v, remove=%w", err, rmErr)
		}
	}

	_ = os.RemoveAll(m.cfg.VMPersistDir(vmID))
	_ = os.RemoveAll(m.cfg.VMRuntimeDir(vmID))
	_ = os.Remove(m.cfg.VMSerialLogPath(vmID))
	_ = os.Remove(m.cfg.VMCHLogPath(vmID))
	_ = os.Remove(m.cfg.VMSwtpmLogPath(vmID))
	return nil
}

// fixStateMismatch updates metadata.json to reflect the actual system state.
func (m *manager) fixStateMismatch(inc *vm.Inconsistency, force bool) error {
	meta, err := m.LoadMetadata(inc.VMID)
	if err != nil {
		return err
	}

	actualState := types.VMState(inc.ActualState)

	switch actualState {
	case types.VMStateError:
		meta.PreviousState = meta.State
		meta.State = string(types.VMStateError)
		errorReason := fmt.Sprintf("reconciliation: was %s but actual state is ERROR (%s)", inc.ExpectedState, inc.Details)
		meta.LastError = errorReason
		meta.LastErrorType = string(classifyError(errorReason))
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

// isStuckInState returns true if the VM has been in its current state longer
// than the given timeout, based on the UpdatedAt timestamp.
func isStuckInState(updatedAt string, timeout time.Duration) bool {
	if updatedAt == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, updatedAt)
	if err != nil {
		log.Printf("WARNING: reconcile: invalid UpdatedAt timestamp %q for stuck-state check: %v", updatedAt, err)
		return false
	}
	return time.Since(t) > timeout
}

// detectOrphanedProcesses scans /proc for cloud-hypervisor and swtpm processes
// that are not tracked by any VM's metadata.
func detectOrphanedProcesses(knownPIDs map[int]string) []vm.Inconsistency {
	var orphans []vm.Inconsistency
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil // Not on Linux or /proc unavailable
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		if _, known := knownPIDs[pid]; known {
			continue
		}
		for _, procName := range []string{"cloud-hypervisor", "swtpm"} {
			if utils.ValidateProcess(pid, procName) {
				orphans = append(orphans, vm.Inconsistency{
					Type:     vm.InconsistencyZombieProcess,
					Severity: vm.SeverityWarning,
					Details:  fmt.Sprintf("orphaned %s process PID=%d not tracked by any VM", procName, pid),
				})
			}
		}
	}
	return orphans
}

// reconcileSeverity returns the severity based on the state mismatch.
func reconcileSeverity(expected, actual types.VMState) vm.InconsistencySeverity {
	// A running VM that is actually dead is critical.
	if expected == types.VMStateRunning && actual == types.VMStateError {
		return vm.SeverityCritical
	}
	// Transient states that resolved are info.
	if expected == types.VMStateStopping && actual == types.VMStateStopped {
		return vm.SeverityInfo
	}
	return vm.SeverityWarning
}
