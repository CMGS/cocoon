package engine

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/CMGS/cocoon/lock/flock"
	"github.com/CMGS/cocoon/oci"
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
	expectedProcessNames := make(map[string]struct{})
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

		// Crash point 2 (qcow2 path): config exists but references.json does not contain vmID.
		// OCI-VM runtime pins are reconciled separately via oci-runtime-refs.
		if vmCfg.ImageType != types.VMImageTypeOCIVM && vmCfg.BaseKey != "" {
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
		if meta.VirtiofsdPID > 0 {
			knownPIDs[meta.VirtiofsdPID] = vmID
		}
		expectedProcessNames[meta.HypervisorProcessName(m.cfg.CHBinary)] = struct{}{}
		expectedProcessNames[meta.VirtiofsdProcessName(m.cfg.VirtiofsdBinary)] = struct{}{}

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

		inconsistencies = append(inconsistencies, m.detectOCIRuntimeIssues(vmID, vmCfg, meta)...)
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

	runtimeRefs, runtimeRefsErr := oci.LoadRuntimeRefsSnapshot(m.cfg)
	if runtimeRefsErr != nil {
		inconsistencies = append(inconsistencies, vm.Inconsistency{
			Type:     vm.InconsistencyMetadataCorrupt,
			Severity: vm.SeverityWarning,
			Details:  fmt.Sprintf("failed to scan oci-runtime-refs.json: %v", runtimeRefsErr),
		})
	} else {
		inconsistencies = append(inconsistencies, m.detectOCIRuntimePinIssues(vmConfigs, runtimeRefs)...)
		cacheIssues, cacheErr := m.detectOrphanedOCIRuntimeCacheIssues(vmConfigs, runtimeRefs)
		if cacheErr != nil {
			inconsistencies = append(inconsistencies, vm.Inconsistency{
				Type:     vm.InconsistencyMetadataCorrupt,
				Severity: vm.SeverityWarning,
				Details:  fmt.Sprintf("failed to scan OCI runtime cache dirs: %v", cacheErr),
			})
		} else {
			inconsistencies = append(inconsistencies, cacheIssues...)
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

	// Detect orphaned hypervisor/swtpm processes not tracked by any VM.
	// Include all process names currently expected by loaded VM metadata.
	processNames := make([]string, 0, len(expectedProcessNames)+1)
	for name := range expectedProcessNames {
		if strings.TrimSpace(name) == "" {
			continue
		}
		processNames = append(processNames, name)
	}
	// Backward-compatible fallback for empty metadata set.
	if len(processNames) == 0 {
		processNames = append(processNames, (&types.VMMetadataFile{}).HypervisorProcessName(m.cfg.CHBinary))
	}
	orphans := detectOrphanedProcesses(knownPIDs, processNames)
	inconsistencies = append(inconsistencies, orphans...)

	return inconsistencies, nil
}

func (m *manager) detectOCIRuntimePinIssues(vmConfigs map[string]*types.VMConfig, runtimeRefs oci.RuntimeRefsIndex) []vm.Inconsistency {
	issues := make([]vm.Inconsistency, 0)

	for vmID, vmCfg := range vmConfigs {
		if vmCfg.ImageType != types.VMImageTypeOCIVM || strings.TrimSpace(vmCfg.BaseKey) == "" {
			continue
		}
		entry, ok := runtimeRefs.Runtimes[vmCfg.BaseKey]
		if !ok || !slices.Contains(entry.Refs, vmID) {
			issues = append(issues, vm.Inconsistency{
				VMID:     vmID,
				Type:     vm.InconsistencyMissingOCIRuntimePin,
				Severity: vm.SeverityWarning,
				Details:  fmt.Sprintf("OCI VM config exists but oci-runtime-refs.json is missing vmID under runtime_key %s", vmCfg.BaseKey),
				BaseKey:  vmCfg.BaseKey,
				ImageRef: vmCfg.ImageRef,
			})
		}
	}

	for runtimeKey, entry := range runtimeRefs.Runtimes {
		for _, vmID := range entry.Refs {
			vmCfg, ok := vmConfigs[vmID]
			if !ok {
				issues = append(issues, vm.Inconsistency{
					VMID:     vmID,
					Type:     vm.InconsistencyDanglingOCIRuntimePin,
					Severity: vm.SeverityWarning,
					Details:  fmt.Sprintf("oci-runtime-refs.json contains vmID %s under runtime_key %s but VM config is missing", vmID, runtimeKey),
					BaseKey:  runtimeKey,
				})
				continue
			}
			if vmCfg.ImageType != types.VMImageTypeOCIVM || vmCfg.BaseKey != runtimeKey {
				issues = append(issues, vm.Inconsistency{
					VMID:     vmID,
					Type:     vm.InconsistencyDanglingOCIRuntimePin,
					Severity: vm.SeverityWarning,
					Details:  fmt.Sprintf("oci-runtime-refs runtime_key %s does not match VM config image_type/base_key (%s/%s)", runtimeKey, vmCfg.ImageType, vmCfg.BaseKey),
					BaseKey:  runtimeKey,
				})
			}
		}
	}

	return issues
}

func (m *manager) detectOrphanedOCIRuntimeCacheIssues(vmConfigs map[string]*types.VMConfig, runtimeRefs oci.RuntimeRefsIndex) ([]vm.Inconsistency, error) {
	entries, err := os.ReadDir(m.cfg.OCIRuntimeCacheDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	pinned := make(map[string]struct{}, len(runtimeRefs.Runtimes))
	for runtimeKey, entry := range runtimeRefs.Runtimes {
		for _, vmID := range entry.Refs {
			vmCfg, ok := vmConfigs[vmID]
			if !ok {
				continue
			}
			if vmCfg.ImageType == types.VMImageTypeOCIVM && vmCfg.BaseKey == runtimeKey {
				pinned[runtimeKey] = struct{}{}
				break
			}
		}
	}

	issues := make([]vm.Inconsistency, 0)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		runtimeKey := entry.Name()
		if _, ok := pinned[runtimeKey]; ok {
			continue
		}
		issues = append(issues, vm.Inconsistency{
			Type:     vm.InconsistencyOrphanedOCIRuntime,
			Severity: vm.SeverityWarning,
			Details:  fmt.Sprintf("OCI runtime cache dir %s has no active pins", m.cfg.OCIRuntimeEntryDir(runtimeKey)),
			BaseKey:  runtimeKey,
		})
	}
	return issues, nil
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

	expectedProc := meta.HypervisorProcessName(m.cfg.CHBinary)
	processRunning := utils.IsProcessAlive(pid)
	processValid := false
	if processRunning {
		processValid = utils.ValidateProcess(pid, expectedProc)
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
			if isStuckInState(meta.UpdatedAt, m.startingStateTimeout()) {
				return types.VMStateError
			}
			return types.VMStateStarting // still booting
		}
		// Process died during start.
		return types.VMStateError

	case types.VMStateStopping:
		if processValid {
			if isStuckInState(meta.UpdatedAt, m.stoppingStateTimeout()) {
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
			if !utils.ValidateProcess(meta.ProcessPID, meta.HypervisorProcessName(m.cfg.CHBinary)) {
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

func (m *manager) detectOCIRuntimeIssues(vmID string, vmCfg *types.VMConfig, meta *types.VMMetadataFile) []vm.Inconsistency {
	if vmCfg == nil || meta == nil || vmCfg.ImageType != types.VMImageTypeOCIVM {
		return nil
	}

	state := types.VMState(meta.State)
	if state == types.VMStateDeleted {
		return nil
	}

	issues := make([]vm.Inconsistency, 0)

	rootfsLowerDir := strings.TrimSpace(vmCfg.BaseImagePath)
	if rootfsLowerDir == "" {
		issues = append(issues, vm.Inconsistency{
			VMID:     vmID,
			Type:     vm.InconsistencyOCIRuntimeCache,
			Severity: vm.SeverityCritical,
			Details:  "OCI runtime rootfs cache path is empty in config.json",
		})
	} else if _, err := os.Stat(rootfsLowerDir); os.IsNotExist(err) {
		issues = append(issues, vm.Inconsistency{
			VMID:     vmID,
			Type:     vm.InconsistencyOCIRuntimeCache,
			Severity: vm.SeverityCritical,
			Details:  fmt.Sprintf("OCI runtime rootfs cache missing at %s", rootfsLowerDir),
		})
	}

	overlayMounted, overlayErr := m.overlayMgr.IsMounted(vmID)
	if overlayErr != nil {
		issues = append(issues, vm.Inconsistency{
			VMID:     vmID,
			Type:     vm.InconsistencyOCIRuntimeOverlay,
			Severity: vm.SeverityWarning,
			Details:  fmt.Sprintf("failed to probe OCI overlay mount state: %v", overlayErr),
		})
	}

	virtioExpected := meta.VirtiofsdProcessName(m.cfg.VirtiofsdBinary)
	virtioAlive := meta.VirtiofsdPID > 0 && utils.ValidateProcess(meta.VirtiofsdPID, virtioExpected)
	virtioSocket := strings.TrimSpace(meta.VirtiofsdSocket)
	if virtioSocket == "" {
		virtioSocket = m.cfg.VMOCIRootfsVirtioFSSocketPath(vmID)
	}

	switch state {
	case types.VMStateRunning:
		if !overlayMounted || !meta.OCIOverlayMounted {
			issues = append(issues, vm.Inconsistency{
				VMID:     vmID,
				Type:     vm.InconsistencyOCIRuntimeOverlay,
				Severity: vm.SeverityCritical,
				Details:  fmt.Sprintf("RUNNING OCI VM must have mounted overlay (mountinfo=%v, metadata=%v)", overlayMounted, meta.OCIOverlayMounted),
			})
		}
		if !virtioAlive {
			issues = append(issues, vm.Inconsistency{
				VMID:     vmID,
				Type:     vm.InconsistencyOCIRuntimeVirtio,
				Severity: vm.SeverityCritical,
				Details:  fmt.Sprintf("RUNNING OCI VM requires live virtiofsd process (pid=%d expected=%s)", meta.VirtiofsdPID, virtioExpected),
			})
		}
		if _, err := os.Stat(virtioSocket); err != nil {
			issues = append(issues, vm.Inconsistency{
				VMID:     vmID,
				Type:     vm.InconsistencyOCIRuntimeVirtio,
				Severity: vm.SeverityWarning,
				Details:  fmt.Sprintf("RUNNING OCI VM missing virtiofsd socket at %s", virtioSocket),
			})
		}

	case types.VMStateCreated, types.VMStateStopped, types.VMStateError:
		if overlayMounted || meta.OCIOverlayMounted {
			issues = append(issues, vm.Inconsistency{
				VMID:     vmID,
				Type:     vm.InconsistencyOCIRuntimeOverlay,
				Severity: vm.SeverityWarning,
				Details:  fmt.Sprintf("%s OCI VM should not keep mounted overlay (mountinfo=%v, metadata=%v)", state, overlayMounted, meta.OCIOverlayMounted),
			})
		}
		if virtioAlive || meta.VirtiofsdPID > 0 {
			issues = append(issues, vm.Inconsistency{
				VMID:     vmID,
				Type:     vm.InconsistencyOCIRuntimeVirtio,
				Severity: vm.SeverityWarning,
				Details:  fmt.Sprintf("%s OCI VM should not keep virtiofsd runtime (pid=%d alive=%v)", state, meta.VirtiofsdPID, virtioAlive),
			})
		}
	}

	return issues
}

// applyFix attempts to repair an inconsistency.
func (m *manager) applyFix(inc *vm.Inconsistency, force bool) error {
	handler, ok := inconsistencyFixHandlers[inc.Type]
	if !ok {
		return fmt.Errorf("unknown inconsistency type: %s", inc.Type)
	}
	return handler(m, inc, force)
}

var inconsistencyFixHandlers = map[vm.InconsistencyType]func(*manager, *vm.Inconsistency, bool) error{
	vm.InconsistencyStateMismatch:         (*manager).applyFixStateMismatch,
	vm.InconsistencyZombieSocket:          (*manager).applyFixZombieSocket,
	vm.InconsistencyStalePIDFile:          (*manager).applyFixStalePIDFile,
	vm.InconsistencyZombieProcess:         (*manager).applyFixZombieProcess,
	vm.InconsistencyMissingOverlay:        (*manager).applyFixMissingOverlay,
	vm.InconsistencyOrphanedOverlay:       (*manager).applyFixOrphanedOverlay,
	vm.InconsistencyOCIRuntimeCache:       (*manager).applyFixOCIRuntimeCache,
	vm.InconsistencyOCIRuntimeOverlay:     (*manager).applyFixOCIRuntimeMismatch,
	vm.InconsistencyOCIRuntimeVirtio:      (*manager).applyFixOCIRuntimeMismatch,
	vm.InconsistencyMissingOCIRuntimePin:  (*manager).applyFixMissingOCIRuntimePin,
	vm.InconsistencyDanglingOCIRuntimePin: (*manager).applyFixDanglingOCIRuntimePin,
	vm.InconsistencyOrphanedOCIRuntime:    (*manager).applyFixOrphanedOCIRuntime,
	vm.InconsistencyMissingReference:      (*manager).applyFixMissingReference,
	vm.InconsistencyDanglingReference:     (*manager).applyFixDanglingReference,
	vm.InconsistencyNameIndexStale:        (*manager).applyFixNameIndexStale,
	vm.InconsistencyDuplicateVMName:       (*manager).applyFixDuplicateVMName,
	vm.InconsistencyDeletedVMDir:          (*manager).applyFixDeletedVMDir,
	vm.InconsistencyMetadataCorrupt:       (*manager).applyFixMetadataCorrupt,
}

func (m *manager) applyFixStateMismatch(inc *vm.Inconsistency, force bool) error {
	return m.fixStateMismatch(inc, force)
}

func (m *manager) applyFixZombieSocket(inc *vm.Inconsistency, _ bool) error {
	vmCfg, err := m.LoadConfig(inc.VMID)
	if err != nil {
		return err
	}
	return os.Remove(vmCfg.SocketPath)
}

func (m *manager) applyFixStalePIDFile(inc *vm.Inconsistency, _ bool) error {
	return os.Remove(m.cfg.VMPIDPath(inc.VMID))
}

func (m *manager) applyFixZombieProcess(inc *vm.Inconsistency, force bool) error {
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
	if meta.ProcessPID <= 0 {
		return nil
	}
	if utils.ValidateProcess(meta.ProcessPID, meta.HypervisorProcessName(m.cfg.CHBinary)) {
		_ = syscall.Kill(meta.ProcessPID, syscall.SIGKILL)
	}
	meta.ProcessPID = 0
	return m.SaveMetadata(meta)
}

func (m *manager) applyFixMissingOverlay(_ *vm.Inconsistency, _ bool) error {
	return fmt.Errorf("overlay disk missing; VM data is lost")
}

func (m *manager) applyFixOrphanedOverlay(inc *vm.Inconsistency, _ bool) error {
	return m.fixOrphanedOverlay(inc.VMID)
}

func (m *manager) applyFixOCIRuntimeCache(_ *vm.Inconsistency, _ bool) error {
	return fmt.Errorf("OCI runtime cache missing; recreate VM from source image")
}

func (m *manager) applyFixOCIRuntimeMismatch(inc *vm.Inconsistency, force bool) error {
	meta, err := m.LoadMetadata(inc.VMID)
	if err != nil {
		return err
	}

	state := types.VMState(meta.State)
	if state == types.VMStateRunning && !force {
		return fmt.Errorf("--force required to repair OCI runtime mismatch on RUNNING VM")
	}

	if state == types.VMStateRunning {
		_ = m.hyper.ForceKill(inc.VMID)
		if meta.ProcessPID > 0 && utils.ValidateProcess(meta.ProcessPID, meta.HypervisorProcessName(m.cfg.CHBinary)) {
			_ = syscall.Kill(meta.ProcessPID, syscall.SIGKILL)
		}
	}

	if err := m.cleanupOCIRuntime(inc.VMID, meta); err != nil {
		return err
	}

	if state == types.VMStateRunning {
		return m.transitionStateWithUpdate(inc.VMID, types.VMStateError, "reconciled OCI runtime mismatch", func(md *types.VMMetadataFile) {
			md.ProcessPID = 0
		})
	}
	return nil
}

func (m *manager) applyFixMissingReference(inc *vm.Inconsistency, _ bool) error {
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
}

func (m *manager) applyFixDanglingReference(inc *vm.Inconsistency, _ bool) error {
	if inc.BaseKey == "" {
		return fmt.Errorf("missing base_key for dangling reference cleanup")
	}
	return m.refCounter.RemoveReference(inc.BaseKey, inc.VMID)
}

func (m *manager) applyFixMissingOCIRuntimePin(inc *vm.Inconsistency, _ bool) error {
	runtimeKey := strings.TrimSpace(inc.BaseKey)
	if runtimeKey == "" {
		cfg, err := m.LoadConfig(inc.VMID)
		if err != nil {
			return err
		}
		runtimeKey = strings.TrimSpace(cfg.BaseKey)
	}
	if runtimeKey == "" {
		return fmt.Errorf("missing runtime_key for OCI runtime pin repair")
	}
	return oci.AddRuntimeRef(m.cfg, runtimeKey, inc.VMID)
}

func (m *manager) applyFixDanglingOCIRuntimePin(inc *vm.Inconsistency, _ bool) error {
	runtimeKey := strings.TrimSpace(inc.BaseKey)
	if runtimeKey == "" {
		return fmt.Errorf("missing runtime_key for dangling OCI runtime pin cleanup")
	}
	if strings.TrimSpace(inc.VMID) == "" {
		return fmt.Errorf("missing vm_id for dangling OCI runtime pin cleanup")
	}
	return oci.RemoveRuntimeRef(m.cfg, runtimeKey, inc.VMID)
}

func (m *manager) applyFixOrphanedOCIRuntime(inc *vm.Inconsistency, _ bool) error {
	runtimeKey := strings.TrimSpace(inc.BaseKey)
	if runtimeKey == "" {
		return fmt.Errorf("missing runtime_key for orphan OCI runtime cleanup")
	}
	referenced, err := oci.IsRuntimeReferenced(m.cfg, runtimeKey)
	if err != nil {
		return err
	}
	if referenced {
		return nil
	}
	return os.RemoveAll(m.cfg.OCIRuntimeEntryDir(runtimeKey))
}

func (m *manager) applyFixNameIndexStale(_ *vm.Inconsistency, _ bool) error {
	_, err := RebuildNameIndex(m.cfg)
	return err
}

func (m *manager) applyFixDuplicateVMName(_ *vm.Inconsistency, _ bool) error {
	return fmt.Errorf("duplicate VM name requires manual intervention")
}

func (m *manager) applyFixDeletedVMDir(inc *vm.Inconsistency, _ bool) error {
	return m.cleanupDeletedVMArtifacts(inc.VMID)
}

func (m *manager) applyFixMetadataCorrupt(_ *vm.Inconsistency, _ bool) error {
	return fmt.Errorf("manual intervention required for corrupt metadata")
}

func (m *manager) cleanupDeletedVMArtifacts(vmID string) error {
	_ = os.RemoveAll(m.cfg.VMPersistDir(vmID))
	_ = os.RemoveAll(m.cfg.VMRuntimeDir(vmID))
	_ = os.Remove(m.cfg.VMSerialLogPath(vmID))
	_ = os.Remove(m.cfg.VMCHLogPath(vmID))
	_ = os.Remove(m.cfg.VMSwtpmLogPath(vmID))
	_ = os.Remove(m.cfg.VMVirtiofsdLogPath(vmID))
	return nil
}

func (m *manager) fixOrphanedOverlay(vmID string) error {
	overlayPath := m.cfg.VMOverlayPath(vmID)
	if _, err := os.Stat(overlayPath); os.IsNotExist(err) {
		return nil
	}

	// Permanently delete the VM directory and all associated files.
	_ = os.RemoveAll(m.cfg.VMPersistDir(vmID))
	_ = os.RemoveAll(m.cfg.VMRuntimeDir(vmID))
	_ = os.Remove(m.cfg.VMSerialLogPath(vmID))
	_ = os.Remove(m.cfg.VMCHLogPath(vmID))
	_ = os.Remove(m.cfg.VMSwtpmLogPath(vmID))
	_ = os.Remove(m.cfg.VMVirtiofsdLogPath(vmID))
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
			// Only kill if it is actually the hypervisor.
			if utils.ValidateProcess(meta.ProcessPID, meta.HypervisorProcessName(m.cfg.CHBinary)) {
				_ = syscall.Kill(meta.ProcessPID, syscall.SIGKILL)
			}
		}
		meta.ProcessPID = 0
		if err := m.SaveMetadata(meta); err != nil {
			return err
		}
		if err := m.cleanupOCIRuntimeAfterStateMismatch(inc.VMID); err != nil {
			return fmt.Errorf("cleanup OCI runtime after state mismatch: %w", err)
		}
		return nil

	case types.VMStateStopped:
		meta.PreviousState = meta.State
		meta.State = string(types.VMStateStopped)
		meta.ProcessPID = 0
		meta.StoppedAt = time.Now().UTC().Format(time.RFC3339)
		if err := m.SaveMetadata(meta); err != nil {
			return err
		}
		if err := m.cleanupOCIRuntimeAfterStateMismatch(inc.VMID); err != nil {
			return fmt.Errorf("cleanup OCI runtime after state mismatch: %w", err)
		}
		return nil

	default:
		if !force {
			return fmt.Errorf("--force required to reconcile state %s -> %s", inc.ExpectedState, inc.ActualState)
		}
		meta.PreviousState = meta.State
		meta.State = inc.ActualState
		return m.SaveMetadata(meta)
	}
}

func (m *manager) cleanupOCIRuntimeAfterStateMismatch(vmID string) error {
	vmCfg, err := m.LoadConfig(vmID)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("load config for OCI runtime cleanup: %w", err)
	}
	if vmCfg.ImageType != types.VMImageTypeOCIVM {
		return nil
	}
	return m.cleanupOCIRuntime(vmID, nil)
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

func (m *manager) startingStateTimeout() time.Duration {
	if m != nil && m.cfg != nil && m.cfg.BootTimeoutSeconds > 0 {
		return time.Duration(m.cfg.BootTimeoutSeconds) * time.Second
	}
	return 5 * time.Minute
}

func (m *manager) stoppingStateTimeout() time.Duration {
	if m != nil && m.cfg != nil && m.cfg.StopTimeoutSeconds > 0 {
		return time.Duration(m.cfg.StopTimeoutSeconds) * time.Second
	}
	return 2 * time.Minute
}

func buildOrphanScanProcessNames(expectedHypervisors []string) []string {
	procNames := make([]string, 0, len(expectedHypervisors)+1)
	seenNames := make(map[string]struct{}, len(expectedHypervisors)+1)
	for _, name := range expectedHypervisors {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, exists := seenNames[name]; exists {
			continue
		}
		seenNames[name] = struct{}{}
		procNames = append(procNames, name)
	}
	if _, exists := seenNames["swtpm"]; !exists {
		procNames = append(procNames, "swtpm")
	}
	return procNames
}

// detectOrphanedProcesses scans /proc for hypervisor and swtpm processes
// that are not tracked by any VM's metadata.
// expectedHypervisors contains one or more expected hypervisor process names.
// Only works on Linux where /proc is available; on other platforms it logs a
// warning and returns nil.
func detectOrphanedProcesses(knownPIDs map[int]string, expectedHypervisors []string) []vm.Inconsistency {
	procNames := buildOrphanScanProcessNames(expectedHypervisors)
	var orphans []vm.Inconsistency
	entries, err := os.ReadDir("/proc")
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("warning: orphan process detection skipped: /proc unavailable: %v", err)
		}
		return nil
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		if _, known := knownPIDs[pid]; known {
			continue
		}
		for _, procName := range procNames {
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
