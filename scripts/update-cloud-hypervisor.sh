#!/usr/bin/env bash
#
# Update Cloud Hypervisor runtime components for Cocoon.
#
# Usage:
#   sudo bash scripts/update-cloud-hypervisor.sh                            # Update to default version
#   sudo CH_VERSION=v50.0 bash scripts/update-cloud-hypervisor.sh           # Specific CH version
#   bash scripts/update-cloud-hypervisor.sh --check-only                     # Show current vs target
#   # or via Makefile:
#   make update-ch
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
                echo "Update Cloud Hypervisor runtime for Cocoon."
                echo ""
                echo "Options:"
                echo "  --check-only  Show current vs target versions without updating"
                echo "  --help        Show this help message"
                echo ""
                echo "Environment variables:"
                echo "  CH_VERSION              Target CH version (default: ${CH_VERSION})"
                echo "  EDK2_CH_VERSION         UEFI firmware version (default: ${EDK2_CH_VERSION})"
                exit 0
                ;;
            *) error "Unknown argument: $1"; exit 1 ;;
        esac
    done
}

# ----- Get current installed version -----
get_current_ch_version() {
    if command -v cloud-hypervisor &>/dev/null; then
        cloud-hypervisor --version 2>/dev/null | head -1 || echo "unknown"
    else
        echo "not installed"
    fi
}

get_current_ch_remote_version() {
    if command -v ch-remote &>/dev/null; then
        ch-remote --version 2>/dev/null | head -1 || echo "unknown"
    else
        echo "not installed"
    fi
}

# ----- Check-only mode -----
run_check_only() {
    echo ""
    echo -e "${BOLD}Cloud Hypervisor Update Preview${NC}"
    echo -e "${BOLD}================================${NC}"
    echo ""

    echo -e "${BOLD}--- Current versions ---${NC}"
    info "cloud-hypervisor: $(get_current_ch_version)"
    info "ch-remote:        $(get_current_ch_remote_version)"

    echo ""
    echo -e "${BOLD}--- Target versions ---${NC}"
    info "CH version:       ${CH_VERSION}"
    echo ""
    echo -e "${BOLD}--- Firmware status ---${NC}"
    local fw_dir="${COCOON_ROOT}/firmware"
    if [[ -f "${fw_dir}/CLOUDHV.fd" ]]; then
        info "UEFI firmware: present (${fw_dir}/CLOUDHV.fd)"
    else
        info "UEFI firmware: not installed"
    fi

    echo ""
    exit 0
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

    echo ""
    echo -e "${BOLD}Cloud Hypervisor Update${NC}"
    echo -e "${BOLD}=======================${NC}"
    echo ""

    # Show current versions.
    echo -e "${BOLD}--- Current versions ---${NC}"
    local old_ch old_chr
    old_ch="$(get_current_ch_version)"
    old_chr="$(get_current_ch_remote_version)"
    info "cloud-hypervisor: ${old_ch}"
    info "ch-remote:        ${old_chr}"
    echo ""

    # Remove old binaries.
    echo -e "${BOLD}--- Removing old binaries ---${NC}"
    for bin in cloud-hypervisor ch-remote; do
        if [[ -f "${INSTALL_DIR}/${bin}" ]]; then
            rm -f "${INSTALL_DIR}/${bin}"
            ok "Removed ${INSTALL_DIR}/${bin}"
        else
            info "Not present: ${INSTALL_DIR}/${bin}"
        fi
    done
    echo ""

    # Remove old firmware (will be re-downloaded).
    echo -e "${BOLD}--- Removing old firmware ---${NC}"
    local fw_dir="${COCOON_ROOT}/firmware"
    for fw in CLOUDHV.fd; do
        if [[ -f "${fw_dir}/${fw}" ]]; then
            rm -f "${fw_dir}/${fw}"
            ok "Removed ${fw_dir}/${fw}"
        else
            info "Not present: ${fw_dir}/${fw}"
        fi
    done
    echo ""

    # Install new versions.
    echo -e "${BOLD}--- Installing ${CH_VERSION} ---${NC}"
    install_cloud_hypervisor
    echo ""
    install_firmware

    # Show updated versions.
    echo ""
    echo -e "${BOLD}--- Updated versions ---${NC}"
    info "cloud-hypervisor: $(get_current_ch_version)"
    info "ch-remote:        $(get_current_ch_remote_version)"

    echo ""
    echo -e "${BOLD}Update complete.${NC}"
    echo ""
}

main "$@"
