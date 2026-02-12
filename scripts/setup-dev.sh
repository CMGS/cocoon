#!/usr/bin/env bash
#
# Cocoon development environment setup script.
# Installs Cloud Hypervisor, firmware, and required tools on Linux.
#
# Usage:
#   sudo bash scripts/setup-dev.sh            # Full install
#   bash scripts/setup-dev.sh --check-only    # Report what's missing (no root needed)
#   # or via Makefile:
#   make setup-dev
#
set -euo pipefail

# ----- Configuration -----
CH_VERSION="v41.0"
HYPERVISOR_FW_VERSION="0.4.2"
COCOON_ROOT="/var/lib/cocoon"
COCOON_LOG="/var/log/cocoon"
COCOON_RUN="/run/cocoon"
MIN_GO_VERSION="1.22"
CHECK_ONLY=false

# ----- Colors -----
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
BOLD='\033[1m'
NC='\033[0m' # No Color

info()  { echo -e "${BLUE}[INFO]${NC}  $*"; }
pass()  { echo -e "${GREEN}[PASS]${NC}  $*"; }
fail()  { echo -e "${RED}[FAIL]${NC}  $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC}  $*"; }
ok()    { echo -e "${GREEN}[OK]${NC}    $*"; }
error() { echo -e "${RED}[ERROR]${NC} $*"; }

# ----- Argument parsing -----
parse_args() {
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --check-only)
                CHECK_ONLY=true
                shift
                ;;
            -h|--help)
                echo "Usage: $0 [--check-only] [--help]"
                echo ""
                echo "Options:"
                echo "  --check-only  Report what is missing without installing anything"
                echo "  --help        Show this help message"
                exit 0
                ;;
            *)
                error "Unknown argument: $1"
                exit 1
                ;;
        esac
    done
}

# ----- Version comparison -----
# Returns 0 if $1 >= $2 (semantic version comparison)
version_gte() {
    local v1="$1" v2="$2"
    # Use sort -V for version comparison
    local highest
    highest="$(printf '%s\n%s' "$v1" "$v2" | sort -V | tail -n1)"
    [[ "$highest" == "$v1" ]]
}

# ----- Check-only mode functions -----
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

check_go_version() {
    if ! command -v go &>/dev/null; then
        check_result "Go" "false" "(not installed, need >= ${MIN_GO_VERSION})"
        return
    fi
    local go_version
    go_version="$(go version | sed -E 's/.*go([0-9]+\.[0-9]+(\.[0-9]+)?).*/\1/')"
    if version_gte "$go_version" "$MIN_GO_VERSION"; then
        check_result "Go" "true" "(${go_version} >= ${MIN_GO_VERSION})"
    else
        check_result "Go" "false" "(${go_version} < ${MIN_GO_VERSION}, upgrade required)"
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

run_check_only() {
    echo ""
    echo -e "${BOLD}============================================${NC}"
    echo -e "${BOLD}  Cocoon Development Environment Check${NC}"
    echo -e "${BOLD}============================================${NC}"
    echo ""

    echo -e "${BOLD}--- Go toolchain ---${NC}"
    check_go_version

    echo ""
    echo -e "${BOLD}--- Development tools ---${NC}"
    check_tool "golangci-lint" "golangci-lint" "install via: go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest"
    check_tool "gofumpt" "gofumpt" "install via: go install mvdan.cc/gofumpt@latest"
    check_tool "goimports" "goimports" "install via: go install golang.org/x/tools/cmd/goimports@latest"
    check_tool "mockery" "mockery" "install via: go install github.com/vektra/mockery/v2@latest"

    echo ""
    echo -e "${BOLD}--- Runtime dependencies ---${NC}"
    check_tool "qemu-img" "qemu-img" "required for overlay creation"
    check_tool "cloud-hypervisor" "cloud-hypervisor" "VMM backend"
    # Check cap_net_admin capability on cloud-hypervisor
    if command -v cloud-hypervisor &>/dev/null && command -v getcap &>/dev/null; then
        local caps
        caps="$(getcap "$(command -v cloud-hypervisor)" 2>/dev/null || echo "")"
        if echo "$caps" | grep -q "cap_net_admin"; then
            check_result "cap_net_admin" "true" "(on cloud-hypervisor)"
        else
            check_result "cap_net_admin" "false" "(not set - run: sudo setcap cap_net_admin+ep $(command -v cloud-hypervisor))"
        fi
    fi
    check_tool "buildah" "buildah" "OCI image operations"

    echo ""
    echo -e "${BOLD}--- Firmware ---${NC}"
    check_file_exists "PVH firmware" "${COCOON_ROOT}/firmware/hypervisor-fw"
    check_file_exists "UEFI firmware" "${COCOON_ROOT}/firmware/CLOUDHV.fd"

    echo ""
    echo -e "${BOLD}--- Directories ---${NC}"
    check_result "Data directory" "$([[ -d "${COCOON_ROOT}" ]] && echo true || echo false)" "(${COCOON_ROOT})"
    check_result "Runtime directory" "$([[ -d "${COCOON_RUN}" ]] && echo true || echo false)" "(${COCOON_RUN})"
    check_result "Log directory" "$([[ -d "${COCOON_LOG}" ]] && echo true || echo false)" "(${COCOON_LOG})"

    echo ""
    echo "============================================"
    echo -e "  Results: ${GREEN}${CHECKS_PASSED} passed${NC}, ${RED}${CHECKS_FAILED} failed${NC}"
    echo "============================================"
    echo ""

    if [[ "$CHECKS_FAILED" -gt 0 ]]; then
        info "Run 'sudo bash scripts/setup-dev.sh' to install missing dependencies."
        exit 1
    else
        pass "All checks passed."
        exit 0
    fi
}

# ----- Pre-flight checks -----
preflight_checks() {
    # Must be Linux
    if [[ "$(uname -s)" != "Linux" ]]; then
        error "This script only supports Linux. Detected: $(uname -s)"
        error "Cloud Hypervisor requires Linux with KVM support."
        exit 1
    fi

    # Must be root or sudo
    if [[ "$EUID" -ne 0 ]]; then
        error "This script must be run as root (or with sudo)."
        error "Usage: sudo bash $0"
        exit 1
    fi
}

# ----- Detect architecture -----
detect_arch() {
    ARCH="$(uname -m)"
    case "$ARCH" in
        x86_64)
            CH_BINARY="cloud-hypervisor-static"
            FW_BINARY="hypervisor-fw"
            ;;
        aarch64)
            CH_BINARY="cloud-hypervisor-static-aarch64"
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
detect_pkg_manager() {
    PKG_MANAGER=""
    if command -v apt-get &>/dev/null; then
        PKG_MANAGER="apt"
    elif command -v dnf &>/dev/null; then
        PKG_MANAGER="dnf"
    else
        error "Unsupported distribution. Requires apt (Ubuntu/Debian) or dnf (Fedora/RHEL)."
        exit 1
    fi
    info "Detected: arch=$ARCH, package_manager=$PKG_MANAGER"
}

# ----- Check Go version -----
check_go_version_install() {
    if ! command -v go &>/dev/null; then
        warn "Go is not installed. Please install Go >= ${MIN_GO_VERSION}"
        warn "Visit: https://go.dev/dl/"
        return
    fi
    local go_version
    go_version="$(go version | sed -E 's/.*go([0-9]+\.[0-9]+(\.[0-9]+)?).*/\1/')"
    if version_gte "$go_version" "$MIN_GO_VERSION"; then
        ok "Go version ${go_version} (>= ${MIN_GO_VERSION})"
    else
        warn "Go version ${go_version} is below minimum ${MIN_GO_VERSION}. Please upgrade."
        warn "Visit: https://go.dev/dl/"
    fi
}

# ----- Install development tools -----
install_dev_tools() {
    info "Installing Go development tools..."

    if ! command -v go &>/dev/null; then
        warn "Go not found, skipping Go tool installation."
        return
    fi

    # Determine the user for go install (not root)
    local run_as="${SUDO_USER:-}"
    local go_install_cmd="go install"
    if [[ -n "$run_as" && "$run_as" != "root" ]]; then
        go_install_cmd="sudo -u $run_as go install"
    fi

    local tools=(
        "github.com/golangci/golangci-lint/cmd/golangci-lint@latest"
        "mvdan.cc/gofumpt@latest"
        "golang.org/x/tools/cmd/goimports@latest"
        "github.com/vektra/mockery/v2@latest"
    )

    for tool in "${tools[@]}"; do
        local tool_name
        tool_name="$(basename "${tool%%@*}")"
        if command -v "$tool_name" &>/dev/null; then
            ok "$tool_name already installed"
        else
            info "Installing $tool_name..."
            if $go_install_cmd "$tool" 2>/dev/null; then
                ok "$tool_name installed"
            else
                warn "Failed to install $tool_name. Install manually: go install $tool"
            fi
        fi
    done
}

# ----- 1. Install Cloud Hypervisor -----
install_cloud_hypervisor() {
    if command -v cloud-hypervisor &>/dev/null; then
        local current_version
        current_version="$(cloud-hypervisor --version 2>/dev/null | head -1 || echo unknown)"
        ok "Cloud Hypervisor already installed: $current_version"
        return
    fi

    info "Installing Cloud Hypervisor $CH_VERSION..."
    local tmp_dir
    tmp_dir="$(mktemp -d)"

    if ! curl -fsSL -o "$tmp_dir/$CH_BINARY" \
        "https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/${CH_VERSION}/${CH_BINARY}"; then
        error "Failed to download Cloud Hypervisor"
        rm -rf "$tmp_dir"
        return 1
    fi

    chmod +x "$tmp_dir/$CH_BINARY"
    mv "$tmp_dir/$CH_BINARY" /usr/local/bin/cloud-hypervisor

    rm -rf "$tmp_dir"

    # Grant CAP_NET_ADMIN so CH can create TAP network devices without running as root.
    if command -v setcap &>/dev/null; then
        setcap cap_net_admin+ep /usr/local/bin/cloud-hypervisor
        ok "Set cap_net_admin+ep on cloud-hypervisor"
    else
        warn "setcap not found; cloud-hypervisor may need root for networking (install libcap2-bin)"
    fi

    local installed_version
    installed_version="$(cloud-hypervisor --version 2>/dev/null | head -1 || echo unknown)"
    ok "Cloud Hypervisor installed: $installed_version"
}

# ----- 2. Install system packages -----
install_packages() {
    info "Installing system packages..."

    case "$PKG_MANAGER" in
        apt)
            apt-get update -qq
            # qemu-utils provides qemu-img
            # buildah for OCI image operations
            local packages=(qemu-utils buildah)
            for pkg in "${packages[@]}"; do
                if dpkg -l "$pkg" &>/dev/null; then
                    ok "$pkg already installed"
                else
                    info "Installing $pkg..."
                    apt-get install -y -qq "$pkg"
                    ok "$pkg installed"
                fi
            done
            ;;
        dnf)
            # qemu-img is the direct package name on Fedora
            # buildah for OCI image operations
            local packages=(qemu-img buildah)
            for pkg in "${packages[@]}"; do
                if rpm -q "$pkg" &>/dev/null; then
                    ok "$pkg already installed"
                else
                    info "Installing $pkg..."
                    dnf install -y -q "$pkg"
                    ok "$pkg installed"
                fi
            done
            ;;
    esac
}

# ----- 3. Install firmware -----
install_firmware() {
    local fw_dir="${COCOON_ROOT}/firmware"
    mkdir -p "$fw_dir"

    # PVH firmware (rust-hypervisor-firmware)
    if [[ -f "$fw_dir/hypervisor-fw" ]]; then
        ok "PVH firmware already installed at $fw_dir/hypervisor-fw"
    else
        info "Downloading PVH firmware (rust-hypervisor-firmware $HYPERVISOR_FW_VERSION)..."
        if curl -fsSL -o "$fw_dir/hypervisor-fw" \
            "https://github.com/cloud-hypervisor/rust-hypervisor-firmware/releases/download/${HYPERVISOR_FW_VERSION}/${FW_BINARY}"; then
            chmod 755 "$fw_dir/hypervisor-fw"
            ok "PVH firmware installed at $fw_dir/hypervisor-fw"
        else
            error "Failed to download PVH firmware"
        fi
    fi

    # UEFI firmware (CLOUDHV.fd from Cloud Hypervisor releases)
    if [[ -f "$fw_dir/CLOUDHV.fd" ]]; then
        ok "UEFI firmware already installed at $fw_dir/CLOUDHV.fd"
    else
        info "Downloading UEFI firmware (CLOUDHV.fd from CH $CH_VERSION)..."
        if curl -fsSL -o "$fw_dir/CLOUDHV.fd" \
            "https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/${CH_VERSION}/CLOUDHV.fd"; then
            chmod 644 "$fw_dir/CLOUDHV.fd"
            ok "UEFI firmware installed at $fw_dir/CLOUDHV.fd"
        else
            error "Failed to download UEFI firmware"
        fi
    fi
}

# ----- 4. Create directory structure -----
create_directories() {
    info "Creating Cocoon directory structure..."

    # Persistent storage
    local dirs=(
        "${COCOON_ROOT}/cache/images"
        "${COCOON_ROOT}/cache/manifests"
        "${COCOON_ROOT}/cache/locks"
        "${COCOON_ROOT}/cache/buildah"
        "${COCOON_ROOT}/vms"
        "${COCOON_ROOT}/temp"
        "${COCOON_ROOT}/trash"
        "${COCOON_ROOT}/firmware"
    )

    for dir in "${dirs[@]}"; do
        mkdir -p "$dir"
    done
    ok "Created ${COCOON_ROOT}/ hierarchy"

    # Runtime directory (typically tmpfs on /run)
    mkdir -p "${COCOON_RUN}/vms"
    ok "Created ${COCOON_RUN}/vms"

    # Log directory
    mkdir -p "${COCOON_LOG}"
    ok "Created ${COCOON_LOG}"

    # Set permissions: allow the invoking user (SUDO_USER) to own everything
    local owner="${SUDO_USER:-root}"
    local group
    group="$(id -gn "$owner" 2>/dev/null || echo root)"

    chown -R "${owner}:${group}" "${COCOON_ROOT}"
    chown -R "${owner}:${group}" "${COCOON_RUN}"
    chown -R "${owner}:${group}" "${COCOON_LOG}"

    chmod -R 755 "${COCOON_ROOT}"
    chmod -R 755 "${COCOON_RUN}"
    chmod -R 755 "${COCOON_LOG}"

    ok "Permissions set (owner=${owner}:${group})"
}

# ----- Print summary -----
print_summary() {
    echo ""
    echo "================================================"
    echo "  Setup Summary"
    echo "================================================"
    echo ""

    # Go
    if command -v go &>/dev/null; then
        ok "go                $(go version 2>/dev/null | head -1)"
    else
        error "go                NOT FOUND"
    fi

    # Cloud Hypervisor
    if command -v cloud-hypervisor &>/dev/null; then
        ok "cloud-hypervisor  $(cloud-hypervisor --version 2>/dev/null | head -1)"
    else
        error "cloud-hypervisor  NOT FOUND"
    fi

    # qemu-img
    if command -v qemu-img &>/dev/null; then
        ok "qemu-img          $(qemu-img --version 2>/dev/null | head -1)"
    else
        error "qemu-img          NOT FOUND"
    fi

    # buildah
    if command -v buildah &>/dev/null; then
        ok "buildah           $(buildah --version 2>/dev/null | head -1)"
    else
        error "buildah           NOT FOUND"
    fi

    # Dev tools
    echo ""
    for tool in golangci-lint gofumpt goimports mockery; do
        if command -v "$tool" &>/dev/null; then
            ok "$tool"
        else
            warn "$tool            NOT FOUND"
        fi
    done

    # Firmware
    echo ""
    if [[ -f "${COCOON_ROOT}/firmware/hypervisor-fw" ]]; then
        ok "PVH firmware      ${COCOON_ROOT}/firmware/hypervisor-fw"
    else
        error "PVH firmware      MISSING"
    fi

    if [[ -f "${COCOON_ROOT}/firmware/CLOUDHV.fd" ]]; then
        ok "UEFI firmware     ${COCOON_ROOT}/firmware/CLOUDHV.fd"
    else
        warn "UEFI firmware     MISSING (optional, fallback only)"
    fi

    # Directories
    echo ""
    ok "Data directory    ${COCOON_ROOT}/"
    ok "Runtime directory ${COCOON_RUN}/"
    ok "Log directory     ${COCOON_LOG}/"

    echo ""
    info "Setup complete. You may need to:"
    info "  1. Add your user to the 'kvm' group: sudo usermod -aG kvm \$USER"
    info "  2. Log out and back in for group changes to take effect."
    info "  3. Verify KVM access: ls -l /dev/kvm"
    echo ""
}

# ----- Main -----
main() {
    parse_args "$@"

    # Handle --check-only mode (no root required)
    if [[ "$CHECK_ONLY" == "true" ]]; then
        run_check_only
        return
    fi

    # Full install requires pre-flight checks
    preflight_checks
    detect_arch
    detect_pkg_manager
    echo ""

    echo "================================================"
    echo "  Cocoon Development Environment Setup"
    echo "================================================"
    echo ""

    check_go_version_install
    echo ""
    install_dev_tools
    echo ""
    install_cloud_hypervisor
    echo ""
    install_packages
    echo ""
    install_firmware
    echo ""
    create_directories

    print_summary
}

main "$@"
