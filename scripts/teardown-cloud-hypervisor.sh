#!/usr/bin/env bash
#
# Uninstall Cloud Hypervisor runtime components for Cocoon.
#
# Usage:
#   sudo bash scripts/teardown-cloud-hypervisor.sh              # Remove binaries + firmware
#   sudo bash scripts/teardown-cloud-hypervisor.sh --purge      # Also remove all data dirs
#   bash scripts/teardown-cloud-hypervisor.sh --check-only       # Show what would be removed
#   # or via Makefile:
#   make teardown-ch
#
# Environment variables:
#   COCOON_ROOT   Root data directory (default: /var/lib/cocoon)
#   COCOON_RUN    Runtime directory (default: /run/cocoon)
#   COCOON_LOG    Log directory (default: /var/log/cocoon)
#   INSTALL_DIR   Binary install directory (default: /usr/local/bin)
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib.sh"

CHECK_ONLY=false
PURGE=false

# ----- Argument parsing -----
parse_args() {
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --check-only) CHECK_ONLY=true; shift ;;
            --purge)      PURGE=true; shift ;;
            -h|--help)
                echo "Usage: $0 [--purge] [--check-only] [--help]"
                echo ""
                echo "Uninstall Cloud Hypervisor runtime for Cocoon."
                echo ""
                echo "Options:"
                echo "  --check-only  Show what would be removed without removing anything"
                echo "  --purge       Also remove data directories (${COCOON_ROOT}, ${COCOON_RUN}, ${COCOON_LOG})"
                echo "  --help        Show this help message"
                exit 0
                ;;
            *) error "Unknown argument: $1"; exit 1 ;;
        esac
    done
}

# ----- Check helper -----
would_remove() {
    local label="$1" path="$2"
    if [[ -e "$path" ]]; then
        info "Would remove: $path ($label)"
    else
        info "Not present:  $path ($label)"
    fi
}

# ----- Check-only mode -----
run_check_only() {
    echo ""
    echo -e "${BOLD}Teardown Preview${NC}"
    echo -e "${BOLD}=================${NC}"
    echo ""

    echo -e "${BOLD}--- Binaries ---${NC}"
    would_remove "cloud-hypervisor" "${INSTALL_DIR}/cloud-hypervisor"
    would_remove "ch-remote" "${INSTALL_DIR}/ch-remote"

    echo ""
    echo -e "${BOLD}--- Firmware ---${NC}"
    would_remove "PVH firmware" "${COCOON_ROOT}/firmware/hypervisor-fw"
    would_remove "UEFI firmware" "${COCOON_ROOT}/firmware/CLOUDHV.fd"

    if [[ "$PURGE" == "true" ]]; then
        echo ""
        echo -e "${BOLD}--- Data directories (--purge) ---${NC}"
        would_remove "data root" "${COCOON_ROOT}"
        would_remove "runtime dir" "${COCOON_RUN}"
        would_remove "log dir" "${COCOON_LOG}"
    else
        echo ""
        info "Data directories preserved (use --purge to remove them)."
    fi

    echo ""
    info "System packages (qemu-utils, buildah, skopeo, libguestfs-tools) are NOT removed."
    info "They may be used by other software. Remove manually if needed."
    echo ""
    exit 0
}

# ----- Remove helper -----
safe_remove() {
    local label="$1" path="$2"
    if [[ -e "$path" ]]; then
        rm -rf "$path"
        ok "Removed $label ($path)"
    else
        info "Already absent: $label ($path)"
    fi
}

# ----- Main -----
main() {
    parse_args "$@"

    if [[ "$CHECK_ONLY" == "true" ]]; then
        run_check_only
        return
    fi

    require_root

    echo ""
    echo -e "${BOLD}Cloud Hypervisor Teardown${NC}"
    echo -e "${BOLD}=========================${NC}"
    echo ""

    echo -e "${BOLD}--- Removing binaries ---${NC}"
    safe_remove "cloud-hypervisor" "${INSTALL_DIR}/cloud-hypervisor"
    safe_remove "ch-remote" "${INSTALL_DIR}/ch-remote"

    echo ""
    echo -e "${BOLD}--- Removing firmware ---${NC}"
    safe_remove "PVH firmware" "${COCOON_ROOT}/firmware/hypervisor-fw"
    safe_remove "UEFI firmware" "${COCOON_ROOT}/firmware/CLOUDHV.fd"

    if [[ "$PURGE" == "true" ]]; then
        echo ""
        echo -e "${BOLD}--- Removing data directories ---${NC}"
        safe_remove "data root" "${COCOON_ROOT}"
        safe_remove "runtime dir" "${COCOON_RUN}"
        safe_remove "log dir" "${COCOON_LOG}"
    else
        echo ""
        info "Data directories preserved (use --purge to remove them)."
    fi

    echo ""
    info "System packages (qemu-utils, buildah, skopeo, libguestfs-tools) are NOT removed."
    echo ""
    echo -e "${BOLD}Teardown complete.${NC}"
    echo ""
}

main "$@"
