#!/usr/bin/env bash
#
# Cloud Hypervisor initialization script for Cocoon.
# Installs Cloud Hypervisor, ch-remote, firmware, and system packages on Linux.
#
# ** Linux only ** - Cloud Hypervisor requires Linux with KVM support.
#
# Usage:
#   sudo bash scripts/init-cloud-hypervisor.sh              # Full install
#   bash scripts/init-cloud-hypervisor.sh --check-only       # Check installation status
#   # or via Makefile:
#   make setup-ch
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

CHECK_ONLY=false

# ----- Argument parsing -----
parse_args() {
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --check-only) CHECK_ONLY=true; shift ;;
            -h|--help)
                echo "Usage: $0 [--check-only] [--help]"
                echo ""
                echo "Install Cloud Hypervisor runtime for Cocoon (Linux only)."
                echo ""
                echo "Options:"
                echo "  --check-only  Report installation status without installing anything"
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

# ----- Check-only mode -----
run_check_only() {
    echo ""
    echo -e "${BOLD}Cloud Hypervisor Installation Status${NC}"
    echo -e "${BOLD}=====================================${NC}"

    check_ch_runtime
    print_check_summary
    local rc=$?
    if [[ $rc -ne 0 ]]; then
        info "Run 'sudo bash $0' to install missing components."
    fi
    exit $rc
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
    echo -e "${BOLD}Cloud Hypervisor Setup for Cocoon${NC}"
    echo -e "${BOLD}=================================${NC}"
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
    echo -e "${BOLD}Setup complete.${NC}"
    info "Verify: cloud-hypervisor --version && ch-remote --version"
    info "Ensure KVM access: sudo usermod -aG kvm \$USER"
    echo ""
}

main "$@"
