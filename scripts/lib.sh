#!/usr/bin/env bash
#
# Shared library for Cocoon setup/teardown/update scripts.
# Source this file; do not execute directly.
#
# Usage in other scripts:
#   SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
#   source "${SCRIPT_DIR}/lib.sh"

# Prevent direct execution.
if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
    echo "ERROR: lib.sh should be sourced, not executed directly." >&2
    exit 1
fi

# ----- Version defaults (env-overridable) -----
CH_VERSION="${CH_VERSION:-v50.0}"
HYPERVISOR_FW_VERSION="${HYPERVISOR_FW_VERSION:-0.5.0}"

# ----- Path defaults (env-overridable) -----
COCOON_ROOT="${COCOON_ROOT:-/var/lib/cocoon}"
COCOON_RUN="${COCOON_RUN:-/run/cocoon}"
COCOON_LOG="${COCOON_LOG:-/var/log/cocoon}"
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"

# ----- Colors -----
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
BOLD='\033[1m'
NC='\033[0m'

# ----- Logging -----
info()  { echo -e "${BLUE}[INFO]${NC}  $*"; }
pass()  { echo -e "${GREEN}[PASS]${NC}  $*"; }
fail()  { echo -e "${RED}[FAIL]${NC}  $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC}  $*"; }
ok()    { echo -e "${GREEN}[OK]${NC}    $*"; }
error() { echo -e "${RED}[ERROR]${NC} $*"; }

# ----- Check counters (for --check-only mode) -----
CHECKS_PASSED=0
CHECKS_FAILED=0

check_result() {
    local name="$1" found="$2" detail="${3:-}"
    if [[ "$found" == "true" ]]; then
        pass "$name $detail"
        CHECKS_PASSED=$((CHECKS_PASSED + 1))
    else
        fail "$name $detail"
        CHECKS_FAILED=$((CHECKS_FAILED + 1))
    fi
}

check_tool() {
    local name="$1" cmd="${2:-$1}" note="${3:-}"
    if command -v "$cmd" &>/dev/null; then
        local ver
        ver="$("$cmd" --version 2>/dev/null | head -1 || echo "installed")"
        check_result "$name" "true" "(${ver})"
    else
        check_result "$name" "false" "(not found${note:+ - $note})"
    fi
}

check_file_exists() {
    local name="$1" path="$2"
    if [[ -f "$path" ]]; then
        check_result "$name" "true" "($path)"
    else
        check_result "$name" "false" "($path missing)"
    fi
}

check_dir_exists() {
    local name="$1" path="$2"
    if [[ -d "$path" ]]; then
        check_result "$name" "true" "($path)"
    else
        check_result "$name" "false" "($path missing)"
    fi
}

check_cap_net_admin() {
    local binary_path="$1" label="${2:-cap_net_admin}"
    if ! command -v getcap &>/dev/null; then
        check_result "$label" "false" "(getcap not available)"
        return
    fi
    local caps
    caps="$(getcap "$binary_path" 2>/dev/null || echo "")"
    if echo "$caps" | grep -q "cap_net_admin"; then
        check_result "$label" "true" "(on $binary_path)"
    else
        check_result "$label" "false" "(not set on $binary_path)"
    fi
}

# ----- Version comparison -----
# Returns 0 if $1 >= $2 (semantic version comparison).
version_gte() {
    local v1="$1" v2="$2"
    local highest
    highest="$(printf '%s\n%s' "$v1" "$v2" | sort -V | tail -n1)"
    [[ "$highest" == "$v1" ]]
}

# ----- Platform check -----
check_platform() {
    if [[ "$(uname -s)" != "Linux" ]]; then
        error "Cocoon requires Linux with KVM support."
        error "Detected platform: $(uname -s)"
        exit 1
    fi
}

# ----- Root check -----
require_root() {
    if [[ "$EUID" -ne 0 ]]; then
        error "This operation requires root. Usage: sudo bash $0"
        exit 1
    fi
}

# ----- Detect architecture -----
# Sets: ARCH, CH_BINARY, CH_REMOTE_BINARY, FW_BINARY
detect_arch() {
    ARCH="$(uname -m)"
    case "$ARCH" in
        x86_64)
            CH_BINARY="cloud-hypervisor-static"
            CH_REMOTE_BINARY="ch-remote-static"
            FW_BINARY="hypervisor-fw"
            ;;
        aarch64)
            CH_BINARY="cloud-hypervisor-static-aarch64"
            CH_REMOTE_BINARY="ch-remote-static-aarch64"
            FW_BINARY="hypervisor-fw-aarch64"
            ;;
        *)
            error "Unsupported architecture: $ARCH"
            error "Cloud Hypervisor supports x86_64 and aarch64 only."
            exit 1
            ;;
    esac
}

# ----- Detect package manager -----
# Sets: PKG_MANAGER
detect_pkg_manager() {
    PKG_MANAGER=""
    if command -v apt-get &>/dev/null; then
        PKG_MANAGER="apt"
    elif command -v dnf &>/dev/null; then
        PKG_MANAGER="dnf"
    else
        warn "No supported package manager found (apt or dnf)."
        warn "System packages must be installed manually."
    fi
}

# ----- Download helper -----
download_binary() {
    local url="$1" dest="$2" label="${3:-binary}"
    local tmp_dir
    tmp_dir="$(mktemp -d)"
    local tmp_file="${tmp_dir}/$(basename "$dest")"

    if ! curl -fsSL -o "$tmp_file" "$url"; then
        error "Failed to download $label from $url"
        rm -rf "$tmp_dir"
        return 1
    fi

    chmod +x "$tmp_file"
    mv "$tmp_file" "$dest"
    rm -rf "$tmp_dir"
    return 0
}

# ----- Directory structure (matches config.go:EnsureDirs) -----
COCOON_DIRS=(
    "db"
    "cache/images"
    "cache/manifests"
    "cache/locks"
    "vms"
    "temp"
    "trash"
    "firmware"
    "buildah"
)

create_cocoon_directories() {
    info "Creating Cocoon directory structure..."

    # Persistent storage.
    for subdir in "${COCOON_DIRS[@]}"; do
        mkdir -p "${COCOON_ROOT}/${subdir}"
    done

    # Runtime directory (typically tmpfs on /run).
    mkdir -p "${COCOON_RUN}/vms"

    # Log directory.
    mkdir -p "${COCOON_LOG}"

    # Set ownership to invoking user (not root).
    local owner="${SUDO_USER:-root}"
    local group
    group="$(id -gn "$owner" 2>/dev/null || echo root)"

    chown -R "${owner}:${group}" "${COCOON_ROOT}"
    chown -R "${owner}:${group}" "${COCOON_RUN}"
    chown -R "${owner}:${group}" "${COCOON_LOG}"

    chmod -R 755 "${COCOON_ROOT}"
    chmod -R 755 "${COCOON_RUN}"
    chmod -R 755 "${COCOON_LOG}"

    ok "Directory structure ready (owner=${owner}:${group})"
}

# ----- Install CH binaries -----
install_cloud_hypervisor() {
    local ch_url="https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/${CH_VERSION}"

    # cloud-hypervisor
    if [[ -f "${INSTALL_DIR}/cloud-hypervisor" ]] && "${INSTALL_DIR}/cloud-hypervisor" --version &>/dev/null; then
        ok "cloud-hypervisor already installed: $("${INSTALL_DIR}/cloud-hypervisor" --version 2>/dev/null | head -1)"
    else
        info "Installing cloud-hypervisor ${CH_VERSION} (${ARCH})..."
        if download_binary "${ch_url}/${CH_BINARY}" "${INSTALL_DIR}/cloud-hypervisor" "cloud-hypervisor"; then
            ok "cloud-hypervisor installed: $("${INSTALL_DIR}/cloud-hypervisor" --version 2>/dev/null | head -1)"
        fi
    fi

    # ch-remote
    if [[ -f "${INSTALL_DIR}/ch-remote" ]] && "${INSTALL_DIR}/ch-remote" --version &>/dev/null; then
        ok "ch-remote already installed: $("${INSTALL_DIR}/ch-remote" --version 2>/dev/null | head -1)"
    else
        info "Installing ch-remote ${CH_VERSION} (${ARCH})..."
        if download_binary "${ch_url}/${CH_REMOTE_BINARY}" "${INSTALL_DIR}/ch-remote" "ch-remote"; then
            ok "ch-remote installed: $("${INSTALL_DIR}/ch-remote" --version 2>/dev/null | head -1)"
        fi
    fi

    # Set capabilities.
    if command -v setcap &>/dev/null; then
        setcap cap_net_admin+ep "${INSTALL_DIR}/cloud-hypervisor" 2>/dev/null && \
            ok "Set cap_net_admin+ep on cloud-hypervisor"
    else
        warn "setcap not found; cloud-hypervisor may need root for networking (install libcap2-bin)"
    fi
}

# ----- Install firmware -----
install_firmware() {
    local fw_dir="${COCOON_ROOT}/firmware"
    mkdir -p "$fw_dir"

    # PVH firmware (rust-hypervisor-firmware).
    if [[ -f "${fw_dir}/hypervisor-fw" ]]; then
        ok "PVH firmware already present at ${fw_dir}/hypervisor-fw"
    else
        local fw_url="https://github.com/cloud-hypervisor/rust-hypervisor-firmware/releases/download/${HYPERVISOR_FW_VERSION}/${FW_BINARY}"
        info "Downloading PVH firmware (rust-hypervisor-firmware ${HYPERVISOR_FW_VERSION})..."
        if download_binary "$fw_url" "${fw_dir}/hypervisor-fw" "PVH firmware"; then
            chmod 755 "${fw_dir}/hypervisor-fw"
            ok "PVH firmware installed at ${fw_dir}/hypervisor-fw"
        fi
    fi

    # UEFI firmware (CLOUDHV.fd).
    if [[ -f "${fw_dir}/CLOUDHV.fd" ]]; then
        ok "UEFI firmware already present at ${fw_dir}/CLOUDHV.fd"
    else
        info "Looking for UEFI firmware (CLOUDHV.fd)..."
        local found_uefi=false

        # Try distro package paths first.
        local uefi_search_paths=(
            "/usr/share/edk2/x64/CLOUDHV.fd"
            "/usr/share/OVMF/CLOUDHV.fd"
            "/usr/share/qemu/CLOUDHV.fd"
        )
        for uefi_path in "${uefi_search_paths[@]}"; do
            if [[ -f "$uefi_path" ]]; then
                cp "$uefi_path" "${fw_dir}/CLOUDHV.fd"
                chmod 644 "${fw_dir}/CLOUDHV.fd"
                ok "UEFI firmware copied from $uefi_path"
                found_uefi=true
                break
            fi
        done

        if [[ "$found_uefi" == "false" ]]; then
            warn "UEFI firmware (CLOUDHV.fd) not found."
            warn "  Install via distro package (e.g. apt install ovmf) or build from edk2."
            warn "  PVH boot (default) does not require UEFI firmware."
        fi
    fi
}

# ----- Install system packages -----
install_system_packages() {
    if [[ -z "${PKG_MANAGER:-}" ]]; then
        detect_pkg_manager
    fi

    if [[ -z "$PKG_MANAGER" ]]; then
        warn "No package manager detected; skipping system package installation."
        return
    fi

    info "Installing system packages..."

    case "$PKG_MANAGER" in
        apt)
            apt-get update -qq
            local packages=(qemu-utils buildah skopeo libguestfs-tools swtpm swtpm-tools)
            for pkg in "${packages[@]}"; do
                if dpkg -l "$pkg" 2>/dev/null | grep -q "^ii"; then
                    ok "$pkg already installed"
                else
                    info "Installing $pkg..."
                    if apt-get install -y -qq "$pkg" 2>/dev/null; then
                        ok "$pkg installed"
                    else
                        warn "Failed to install $pkg (may not be available in current repos)"
                    fi
                fi
            done
            ;;
        dnf)
            local packages=(qemu-img buildah skopeo libguestfs-tools swtpm swtpm-tools)
            for pkg in "${packages[@]}"; do
                if rpm -q "$pkg" &>/dev/null; then
                    ok "$pkg already installed"
                else
                    info "Installing $pkg..."
                    if dnf install -y -q "$pkg" 2>/dev/null; then
                        ok "$pkg installed"
                    else
                        warn "Failed to install $pkg (may not be available in current repos)"
                    fi
                fi
            done
            ;;
    esac

    # Disable AppArmor restriction on swtpm so it can create sockets
    # under /run/cocoon/. Without this, swtpm fails with "Permission denied"
    # when Cocoon starts a TPM emulator for a VM.
    configure_swtpm_apparmor
}

# ----- Configure swtpm AppArmor -----
configure_swtpm_apparmor() {
    local profile="/etc/apparmor.d/usr.bin.swtpm"
    if [[ ! -f "$profile" ]]; then
        return # No AppArmor profile for swtpm; nothing to do.
    fi
    if ! command -v apparmor_parser &>/dev/null; then
        return # AppArmor tools not installed.
    fi

    # Check if already disabled.
    if [[ -L /etc/apparmor.d/disable/usr.bin.swtpm ]]; then
        ok "swtpm AppArmor profile already disabled"
        return
    fi

    info "Disabling AppArmor restriction on swtpm..."
    mkdir -p /etc/apparmor.d/disable
    ln -sf "$profile" /etc/apparmor.d/disable/usr.bin.swtpm
    apparmor_parser -R "$profile" 2>/dev/null || true
    ok "swtpm AppArmor profile disabled (allows socket creation under /run/cocoon/)"
}

# ----- Check all CH runtime components -----
check_ch_runtime() {
    echo ""
    echo -e "${BOLD}--- KVM ---${NC}"
    if [[ -c /dev/kvm ]]; then
        check_result "KVM device" "true" "(/dev/kvm)"
        if [[ -r /dev/kvm && -w /dev/kvm ]]; then
            check_result "KVM access" "true" "(read/write)"
        else
            check_result "KVM access" "false" "(no read/write - add user to kvm group)"
        fi
    else
        check_result "KVM device" "false" "(/dev/kvm not found)"
    fi

    echo ""
    echo -e "${BOLD}--- Cloud Hypervisor binaries ---${NC}"
    check_tool "cloud-hypervisor" "cloud-hypervisor" "VMM backend"
    if command -v cloud-hypervisor &>/dev/null; then
        check_cap_net_admin "$(command -v cloud-hypervisor)" "cap_net_admin (cloud-hypervisor)"
    fi
    check_tool "ch-remote" "ch-remote" "VM remote management"

    echo ""
    echo -e "${BOLD}--- System tools ---${NC}"
    check_tool "qemu-img" "qemu-img" "required for overlay creation"
    check_tool "buildah" "buildah" "OCI image pull"
    check_tool "skopeo" "skopeo" "OCI image inspection"
    check_tool "guestfish" "guestfish" "OCI conversion and boot verification"
    check_tool "swtpm" "swtpm" "TPM 2.0 emulator for VM TPM support"
    # Check swtpm AppArmor is not blocking socket creation.
    if [[ -f /etc/apparmor.d/usr.bin.swtpm ]]; then
        if [[ -L /etc/apparmor.d/disable/usr.bin.swtpm ]]; then
            check_result "swtpm AppArmor" "true" "(profile disabled)"
        else
            check_result "swtpm AppArmor" "false" "(profile active - swtpm will fail with Permission denied)"
        fi
    fi

    echo ""
    echo -e "${BOLD}--- Firmware ---${NC}"
    check_file_exists "PVH firmware" "${COCOON_ROOT}/firmware/hypervisor-fw"
    check_file_exists "UEFI firmware (optional)" "${COCOON_ROOT}/firmware/CLOUDHV.fd"

    echo ""
    echo -e "${BOLD}--- Directories ---${NC}"
    for subdir in "${COCOON_DIRS[@]}"; do
        check_dir_exists "${COCOON_ROOT}/${subdir}" "${COCOON_ROOT}/${subdir}"
    done
    check_dir_exists "${COCOON_RUN}/vms" "${COCOON_RUN}/vms"
    check_dir_exists "${COCOON_LOG}" "${COCOON_LOG}"
}

# ----- Print check summary -----
print_check_summary() {
    echo ""
    echo "============================================"
    echo -e "  Results: ${GREEN}${CHECKS_PASSED} passed${NC}, ${RED}${CHECKS_FAILED} failed${NC}"
    echo "============================================"
    echo ""
    return "$([[ "$CHECKS_FAILED" -eq 0 ]] && echo 0 || echo 1)"
}
