#!/usr/bin/env bash
#
# Cocoon development environment setup script.
# Installs Cloud Hypervisor, firmware, system packages, and Go dev tools.
#
# Usage:
#   sudo bash scripts/setup-dev.sh            # Full install
#   bash scripts/setup-dev.sh --check-only    # Report what's missing (no root needed)
#   # or via Makefile:
#   make setup-dev
#
# Environment variables:
#   CH_VERSION              Cloud Hypervisor version (default: v51.0)
#   COCOON_ROOT             Root data directory (default: /var/lib/cocoon)
#   COCOON_RUN              Runtime directory (default: /run/cocoon)
#   COCOON_LOG              Log directory (default: /var/log/cocoon)
#   INSTALL_DIR             Binary install directory (default: /usr/local/bin)
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib.sh"

MIN_GO_VERSION="1.22"
CHECK_ONLY=false

# ----- Argument parsing -----
parse_args() {
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --check-only) CHECK_ONLY=true; shift ;;
            -h|--help)
                echo "Usage: $0 [--check-only] [--help]"
                echo ""
                echo "Install Cocoon development environment (Linux only)."
                echo ""
                echo "Options:"
                echo "  --check-only  Report what is missing without installing anything"
                echo "  --help        Show this help message"
                echo ""
                echo "Environment variables:"
                echo "  CH_VERSION              Cloud Hypervisor version (default: ${CH_VERSION})"
                echo "  COCOON_ROOT             Data directory (default: ${COCOON_ROOT})"
                exit 0
                ;;
            *) error "Unknown argument: $1"; exit 1 ;;
        esac
    done
}

# ----- Go version check -----
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

# ----- Check-only mode -----
run_check_only() {
    echo ""
    echo -e "${BOLD}============================================${NC}"
    echo -e "${BOLD}  Cocoon Development Environment Check${NC}"
    echo -e "${BOLD}============================================${NC}"

    # CH runtime checks (KVM, binaries, tools, firmware, dirs).
    check_ch_runtime

    echo ""
    echo -e "${BOLD}--- Go toolchain ---${NC}"
    check_go_version

    echo ""
    echo -e "${BOLD}--- Development tools ---${NC}"
    check_tool "golangci-lint" "golangci-lint" "install via Makefile or go install"
    check_tool "gofumpt" "gofumpt" "go install mvdan.cc/gofumpt@latest"
    check_tool "goimports" "goimports" "go install golang.org/x/tools/cmd/goimports@latest"
    check_tool "mockery" "mockery" "go install github.com/vektra/mockery/v2@latest"

    print_check_summary
    local rc=$?
    if [[ $rc -ne 0 ]]; then
        info "Run 'sudo bash $0' to install missing dependencies."
    fi
    exit $rc
}

# ----- Install Go dev tools -----
install_dev_tools() {
    info "Installing Go development tools..."

    if ! command -v go &>/dev/null; then
        warn "Go not found, skipping Go tool installation."
        warn "Install Go >= ${MIN_GO_VERSION} from https://go.dev/dl/"
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

# ----- Main -----
main() {
    parse_args "$@"
    check_platform
    detect_arch

    if [[ "$CHECK_ONLY" == "true" ]]; then
        run_check_only
        return
    fi

    require_root
    detect_pkg_manager

    echo ""
    echo -e "${BOLD}================================================${NC}"
    echo -e "${BOLD}  Cocoon Development Environment Setup${NC}"
    echo -e "${BOLD}================================================${NC}"
    echo ""
    info "CH version: ${CH_VERSION}, arch: ${ARCH}"
    echo ""

    create_cocoon_directories
    echo ""
    install_cloud_hypervisor
    echo ""
    install_firmware
    echo ""
    install_system_packages
    echo ""
    install_dev_tools

    echo ""
    echo -e "${BOLD}Setup complete.${NC}"
    info "Ensure KVM access: sudo usermod -aG kvm \$USER"
    info "Log out and back in for group changes to take effect."
    echo ""
}

main "$@"
