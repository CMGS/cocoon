# fix: comprehensive core hardening and modernization

## Summary
A deep audit of the codebase has revealed critical security, durability, and robustness issues in the core image pipeline (`image/pipeline`) and utility packages (`utils`). Additionally, the documentation (`docs/`) has drifted significantly from the implementation (e.g., references to `buildah` and `skopeo` when `go-containerregistry` is used). This issue tracks the necessary fixes to bring the codebase to production standards and align documentation.

## Scope
- **Packages**: `utils`, `image/pipeline`, `cmd/cocoon`, `oci`.
- **Documentation**: `docs/04-oci-conversion.md`, `docs/01-boot-contract.md`, `docs/09-cli-design.md`.
- **Focus**: Security hardening, data durability, logic correctness, resource management, and documentation accuracy.

## Out of Scope
- Major architectural rewrites of the storage layer (beyond atomic fixes).
- New feature development.

## Acceptance Criteria
- [ ] `utils/tar.go` uses `os.OpenRoot` for safe extraction.
- [ ] `classifyRef` no longer ambiguously resolves local files.
- [ ] Atomic renames trigger parent directory `fsync`.
- [ ] OCI Manifest Lists are correctly parsed to unique cache keys.
- [ ] Sparse files are handled correctly during extraction.
- [ ] Child processes are terminated cleanly with their process groups.
- [ ] GC continues execution even if individual phases fail.
- [ ] Documentation accurately reflects `go-containerregistry` usage (no `buildah`/`skopeo`).

## Checklist

### Security & Defense
- [ ] **Path Traversal**: Refactor `utils/tar.go` to use Go 1.25 `os.OpenRoot` for secure directory traversal protection.
- [ ] **Shadowing**: Fix `classifyRef` in `manager.go` to require `./` or `/` prefix for local files, preventing accidental shadowing of remote references.
- [ ] **Permissions**: Restrict all `MkdirTemp` and `MkdirAll` calls for temporary directories to `0700` (currently `0755` or `0750`).

### Data Durability & Integrity
- [ ] **Atomic Consistency**: Implement `SyncParentDir` in `utils/atomic.go` and apply it after every `os.Rename` in `manager.go` (`Convert`, `prepareOCI`) to prevent metadata loss on crash.
- [ ] **Sparse Files**: Add support for `tar.TypeGNUSparse` and `PaxHeaders` in `utils/tar.go` to correctly handle sparse image files and prevent disk exhaustion/corruption.
- [ ] **Whiteout Logic**: Fix `applyOCIWhiteout` in `utils/tar.go` to correctly implement Opaque Whiteout semantics (masking lower layers) rather than simple directory emptying.

### Logic Correctness
- [ ] **Manifest Lists**: Fix `identifyOCIRemote` in `identify.go` to recursively parse `ManifestList` (Index) media types. Currently, it fails silently on indexes, leading to empty config digests and cache collisions.

### Robustness & Resource Management
- [ ] **Process Leaks**: Use `SysProcAttr.Setpgid` for `exec.CommandContext` and implement process group killing to prevent orphaned `guestfish`/`qemu` processes.
- [ ] **GC Fault Tolerance**: Refactor `cmd/cocoon/gc.go` to use `errors.Join` (Go 1.20+), ensuring garbage collection attempts all phases even if one fails.

### Documentation & Alignment
- [ ] **Buildah/Skopeo Removal**: Update `docs/04-oci-conversion.md` and related docs to remove all references to `buildah` CLI and `skopeo`. Clarify that `go-containerregistry` library is used.
- [ ] **Guestfish Dependency**: Clarify in `docs/01-boot-contract.md` that `guestfish` is a **hard dependency** for OCI conversion, but optional for verification of pre-existing images.
- [ ] **Docker Normalization**: Explicitly state in `docs/04.1-oci-vm-images.md` that Docker reference normalization relies on `go-containerregistry`'s `name.NewTag` behavior.

### Modernization
- [ ] **Iterators**: Refactor `ListCached` and `ListVMs` to return Go 1.23+ iterators (`iter.Seq`) instead of allocating full slices, improving memory efficiency for large datasets.
