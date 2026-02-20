#!/usr/bin/env bash
#
# Update Cocoon development environment.
# Updates Cloud Hypervisor runtime, system packages, and Go dev tools.
#
# Usage:
#   sudo bash scripts/update-dev.sh                            # Full update
#   bash scripts/update-dev.sh --check-only                     # Show current vs target
#   # or via Makefile:
#   make update-dev
#
# Environment variables:
#   CH_VERSION              Target Cloud Hypervisor version (default: v50.0)
#   COCOON_ROOT             Root data directory (default: /var/lib/cocoon)
#   INSTALL_DIR             Binary install directory (default: /usr/local/bin)
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib.sh"

CHECK_ONLY=false

# ----- Argument parsing -----
parse_args() {
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --check-only) CHECK_ONLY=true; shift ;;
            -h|--help)
                echo "Usage: $0 [--check-only] [--help]"
                echo ""
                echo "Update Cocoon development environment."
                echo ""
                echo "Options:"
                echo "  --check-only  Show current vs target versions without updating"
                echo "  --help        Show this help message"
                exit 0
                ;;
            *) error "Unknown argument: $1"; exit 1 ;;
        esac
    done
}

# ----- Update Go dev tools -----
update_dev_tools() {
    echo -e "${BOLD}--- Updating Go dev tools ---${NC}"

    if ! command -v go &>/dev/null; then
        warn "Go not found, skipping Go tool update."
        return
    fi

    # Run go install as the invoking user (not root).
    local run_as="${SUDO_USER:-}"
    local go_install_cmd="go install"
    if [[ -n "$run_as" && "$run_as" != "root" ]]; then
        go_install_cmd="sudo -u $run_as go install"
    fi

    local tools=(
        "mvdan.cc/gofumpt@latest"
        "golang.org/x/tools/cmd/goimports@latest"
        "github.com/vektra/mockery/v2@latest"
    )

    for tool in "${tools[@]}"; do
        local tool_name
        tool_name="$(basename "${tool%%@*}")"
        info "Updating $tool_name..."
        if $go_install_cmd "$tool" 2>/dev/null; then
            ok "$tool_name updated"
        else
            warn "Failed to update $tool_name. Update manually: go install $tool"
        fi
    done
}

# ----- Update system packages -----
update_system_packages() {
    if [[ -z "${PKG_MANAGER:-}" ]]; then
        detect_pkg_manager
    fi

    if [[ -z "$PKG_MANAGER" ]]; then
        warn "No package manager detected; skipping system package update."
        return
    fi

    echo -e "${BOLD}--- Updating system packages ---${NC}"

    case "$PKG_MANAGER" in
        apt)
            apt-get update -qq
            local packages=(qemu-utils libguestfs-tools swtpm swtpm-tools virtiofsd)
            for pkg in "${packages[@]}"; do
                if dpkg -l "$pkg" 2>/dev/null | grep -q "^ii"; then
                    info "Upgrading $pkg..."
                    if apt-get install -y -qq --only-upgrade "$pkg" 2>/dev/null; then
                        ok "$pkg up to date"
                    else
                        warn "Failed to upgrade $pkg"
                    fi
                else
                    info "Installing $pkg (not previously installed)..."
                    if apt-get install -y -qq "$pkg" 2>/dev/null; then
                        ok "$pkg installed"
                    else
                        warn "Failed to install $pkg"
                    fi
                fi
            done
            ;;
        dnf)
            local packages=(qemu-img libguestfs-tools swtpm swtpm-tools virtiofsd)
            for pkg in "${packages[@]}"; do
                if rpm -q "$pkg" &>/dev/null; then
                    info "Upgrading $pkg..."
                    if dnf upgrade -y -q "$pkg" 2>/dev/null; then
                        ok "$pkg up to date"
                    else
                        warn "Failed to upgrade $pkg"
                    fi
                else
                    info "Installing $pkg (not previously installed)..."
                    if dnf install -y -q "$pkg" 2>/dev/null; then
                        ok "$pkg installed"
                    else
                        warn "Failed to install $pkg"
                    fi
                fi
            done
            ;;
    esac

    # Ensure virtiofsd command is runnable when distro packages install it
    # to a non-PATH location (for example /usr/libexec/virtiofsd).
    ensure_virtiofsd_command || true
}

# ----- Check-only mode -----
run_check_only() {
    echo ""
    echo -e "${BOLD}Development Environment Update Preview${NC}"
    echo -e "${BOLD}=======================================${NC}"
    echo ""

    # Delegate CH check.
    echo -e "${BOLD}--- CH Runtime (via update-cloud-hypervisor.sh) ---${NC}"
    bash "${SCRIPT_DIR}/update-cloud-hypervisor.sh" --check-only 2>/dev/null || true

    echo ""
    echo -e "${BOLD}--- Go dev tools ---${NC}"
    local tools=(gofumpt goimports mockery)
    for tool in "${tools[@]}"; do
        if command -v "$tool" &>/dev/null; then
            local ver
            ver="$("$tool" --version 2>/dev/null | head -1 || echo "installed")"
            info "$tool: ${ver} (will update to latest)"
        else
            info "$tool: not installed (will install)"
        fi
    done

    echo ""
    exit 0
}

# ----- Main -----
main() {
    parse_args "$@"

    if [[ "$CHECK_ONLY" == "true" ]]; then
        run_check_only
        return
    fi

    check_platform
    detect_arch
    require_root
    detect_pkg_manager

    echo ""
    echo -e "${BOLD}Development Environment Update${NC}"
    echo -e "${BOLD}==============================${NC}"
    echo ""

    # Update CH runtime.
    bash "${SCRIPT_DIR}/update-cloud-hypervisor.sh"
    echo ""

    # Update system packages.
    update_system_packages
    echo ""

    # Update Go dev tools.
    update_dev_tools

    echo ""
    echo -e "${BOLD}Update complete.${NC}"
    echo ""
}

main "$@"
