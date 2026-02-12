#!/usr/bin/env bash
#
# Cloud Hypervisor initialization script for Cocoon.
# Installs Cloud Hypervisor binary and firmware files on Linux.
#
# ** Linux only ** - Cloud Hypervisor requires Linux with KVM support.
# macOS users: this script is not applicable; use QEMU or a Linux VM.
#
# Usage:
#   sudo bash scripts/init-cloud-hypervisor.sh              # Install CH + firmware
#   sudo bash scripts/init-cloud-hypervisor.sh --check-only  # Check installation status
#   # or via Makefile:
#   make setup-ch
#
set -euo pipefail

# ----- Configuration -----
CH_VERSION="v41.0"
HYPERVISOR_FW_VERSION="0.4.2"
COCOON_ROOT="${COCOON_ROOT:-/var/lib/cocoon}"
CHECK_ONLY=false

# ----- Colors -----
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
BOLD='\033[1m'
NC='\033[0m'

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
                echo "Install Cloud Hypervisor and firmware for Cocoon (Linux only)."
                echo ""
                echo "Options:"
                echo "  --check-only  Report installation status without installing anything"
                echo "  --help        Show this help message"
                echo ""
                echo "Environment variables:"
                echo "  COCOON_ROOT   Root directory (default: /var/lib/cocoon)"
                echo "  CH_VERSION    Cloud Hypervisor version (default: ${CH_VERSION})"
                exit 0
                ;;
            *)
                error "Unknown argument: $1"
                exit 1
                ;;
        esac
    done
}

# ----- Platform check -----
check_platform() {
    if [[ "$(uname -s)" != "Linux" ]]; then
        error "Cloud Hypervisor requires Linux with KVM support."
        error "Detected platform: $(uname -s)"
        error "macOS users: this script is not applicable."
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

# ----- Check-only mode -----
run_check_only() {
    echo ""
    echo -e "${BOLD}Cloud Hypervisor Installation Status${NC}"
    echo -e "${BOLD}=====================================${NC}"
    echo ""

    local failed=0

    # Check KVM
    if [[ -c /dev/kvm ]]; then
        pass "KVM device (/dev/kvm)"
        if [[ -r /dev/kvm && -w /dev/kvm ]]; then
            pass "KVM access (read/write)"
        else
            fail "KVM access (no read/write permission - add user to kvm group)"
            failed=$((failed + 1))
        fi
    else
        fail "KVM device (/dev/kvm not found - enable KVM in BIOS)"
        failed=$((failed + 1))
    fi

    # Check Cloud Hypervisor
    if command -v cloud-hypervisor &>/dev/null; then
        local ver
        ver="$(cloud-hypervisor --version 2>/dev/null | head -1 || echo unknown)"
        pass "Cloud Hypervisor ($ver)"

        # Check cap_net_admin capability
        if command -v getcap &>/dev/null; then
            local caps
            caps="$(getcap "$(command -v cloud-hypervisor)" 2>/dev/null || echo "")"
            if echo "$caps" | grep -q "cap_net_admin"; then
                pass "cap_net_admin on cloud-hypervisor"
            else
                fail "cap_net_admin not set on cloud-hypervisor (run: sudo setcap cap_net_admin+ep $(command -v cloud-hypervisor))"
                failed=$((failed + 1))
            fi
        fi
    else
        fail "Cloud Hypervisor (not installed)"
        failed=$((failed + 1))
    fi

    # Check PVH firmware
    local fw_dir="${COCOON_ROOT}/firmware"
    if [[ -f "${fw_dir}/hypervisor-fw" ]]; then
        pass "PVH firmware (${fw_dir}/hypervisor-fw)"
    else
        fail "PVH firmware (${fw_dir}/hypervisor-fw missing)"
        failed=$((failed + 1))
    fi

    # Check UEFI firmware
    if [[ -f "${fw_dir}/CLOUDHV.fd" ]]; then
        pass "UEFI firmware (${fw_dir}/CLOUDHV.fd)"
    else
        warn "UEFI firmware (${fw_dir}/CLOUDHV.fd missing - optional fallback)"
    fi

    # Check qemu-img
    if command -v qemu-img &>/dev/null; then
        pass "qemu-img ($(qemu-img --version 2>/dev/null | head -1))"
    else
        fail "qemu-img (not installed - required for overlay creation)"
        failed=$((failed + 1))
    fi

    # Check directories
    echo ""
    for dir in "${COCOON_ROOT}" "${COCOON_ROOT}/cache/images" "${COCOON_ROOT}/vms" "${COCOON_ROOT}/firmware"; do
        if [[ -d "$dir" ]]; then
            pass "Directory $dir"
        else
            fail "Directory $dir (missing)"
            failed=$((failed + 1))
        fi
    done

    echo ""
    if [[ $failed -eq 0 ]]; then
        pass "All checks passed. Cloud Hypervisor is ready."
    else
        info "Run 'sudo bash scripts/init-cloud-hypervisor.sh' to fix."
    fi
    exit $((failed > 0 ? 1 : 0))
}

# ----- Install Cloud Hypervisor binary -----
install_cloud_hypervisor() {
    if command -v cloud-hypervisor &>/dev/null; then
        local current_version
        current_version="$(cloud-hypervisor --version 2>/dev/null | head -1 || echo unknown)"
        ok "Cloud Hypervisor already installed: $current_version"
        return
    fi

    info "Installing Cloud Hypervisor ${CH_VERSION} (${ARCH})..."
    local tmp_dir
    tmp_dir="$(mktemp -d)"

    if ! curl -fsSL -o "$tmp_dir/$CH_BINARY" \
        "https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/${CH_VERSION}/${CH_BINARY}"; then
        error "Failed to download Cloud Hypervisor from GitHub releases"
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

# ----- Install firmware -----
install_firmware() {
    local fw_dir="${COCOON_ROOT}/firmware"
    mkdir -p "$fw_dir"

    # PVH firmware (rust-hypervisor-firmware)
    if [[ -f "$fw_dir/hypervisor-fw" ]]; then
        ok "PVH firmware already present at $fw_dir/hypervisor-fw"
    else
        info "Downloading PVH firmware (rust-hypervisor-firmware ${HYPERVISOR_FW_VERSION})..."
        if curl -fsSL -o "$fw_dir/hypervisor-fw" \
            "https://github.com/cloud-hypervisor/rust-hypervisor-firmware/releases/download/${HYPERVISOR_FW_VERSION}/${FW_BINARY}"; then
            chmod 755 "$fw_dir/hypervisor-fw"
            ok "PVH firmware installed at $fw_dir/hypervisor-fw"
        else
            error "Failed to download PVH firmware"
        fi
    fi

    # UEFI firmware (CLOUDHV.fd)
    if [[ -f "$fw_dir/CLOUDHV.fd" ]]; then
        ok "UEFI firmware already present at $fw_dir/CLOUDHV.fd"
    else
        info "Downloading UEFI firmware (CLOUDHV.fd from CH ${CH_VERSION})..."
        if curl -fsSL -o "$fw_dir/CLOUDHV.fd" \
            "https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/${CH_VERSION}/CLOUDHV.fd"; then
            chmod 644 "$fw_dir/CLOUDHV.fd"
            ok "UEFI firmware installed at $fw_dir/CLOUDHV.fd"
        else
            error "Failed to download UEFI firmware"
        fi
    fi
}

# ----- Create directory structure -----
create_directories() {
    info "Ensuring Cocoon directory structure..."
    local dirs=(
        "${COCOON_ROOT}/cache/images"
        "${COCOON_ROOT}/cache/locks"
        "${COCOON_ROOT}/vms"
        "${COCOON_ROOT}/temp"
        "${COCOON_ROOT}/trash"
        "${COCOON_ROOT}/firmware"
    )
    for dir in "${dirs[@]}"; do
        mkdir -p "$dir"
    done

    # Set ownership to invoking user
    local owner="${SUDO_USER:-root}"
    local group
    group="$(id -gn "$owner" 2>/dev/null || echo root)"
    chown -R "${owner}:${group}" "${COCOON_ROOT}"
    chmod -R 755 "${COCOON_ROOT}"

    ok "Directory structure ready (owner=${owner}:${group})"
}

# ----- Install qemu-img if missing -----
install_qemu_img() {
    if command -v qemu-img &>/dev/null; then
        ok "qemu-img already installed"
        return
    fi

    info "Installing qemu-img..."
    if command -v apt-get &>/dev/null; then
        apt-get update -qq && apt-get install -y -qq qemu-utils
    elif command -v dnf &>/dev/null; then
        dnf install -y -q qemu-img
    else
        warn "Cannot auto-install qemu-img. Please install it manually."
        return
    fi
    ok "qemu-img installed"
}

# ----- Main -----
main() {
    parse_args "$@"
    check_platform
    detect_arch

    if [[ "$CHECK_ONLY" == "true" ]]; then
        run_check_only
        return
    fi

    # Require root for installation
    if [[ "$EUID" -ne 0 ]]; then
        error "Installation requires root. Usage: sudo bash $0"
        exit 1
    fi

    echo ""
    echo -e "${BOLD}Cloud Hypervisor Setup for Cocoon${NC}"
    echo -e "${BOLD}=================================${NC}"
    echo ""

    create_directories
    echo ""
    install_cloud_hypervisor
    echo ""
    install_firmware
    echo ""
    install_qemu_img

    echo ""
    echo -e "${BOLD}Setup complete.${NC}"
    info "Verify: cloud-hypervisor --version"
    info "Ensure KVM access: sudo usermod -aG kvm \$USER"
    echo ""
}

main "$@"
