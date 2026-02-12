#!/usr/bin/env bash
#
# Uninstall Cocoon development environment.
# Removes Cloud Hypervisor runtime + Go dev tools.
#
# Usage:
#   sudo bash scripts/teardown-dev.sh              # Remove binaries + dev tools
#   sudo bash scripts/teardown-dev.sh --purge      # Also remove data directories
#   bash scripts/teardown-dev.sh --check-only       # Show what would be removed
#   # or via Makefile:
#   make teardown-dev
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

# Go dev tools installed by setup-dev.sh.
GO_DEV_TOOLS=(gofumpt goimports mockery)

# ----- Argument parsing -----
parse_args() {
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --check-only) CHECK_ONLY=true; shift ;;
            --purge)      PURGE=true; shift ;;
            -h|--help)
                echo "Usage: $0 [--purge] [--check-only] [--help]"
                echo ""
                echo "Uninstall Cocoon development environment."
                echo ""
                echo "Options:"
                echo "  --check-only  Show what would be removed without removing anything"
                echo "  --purge       Also remove data directories"
                echo "  --help        Show this help message"
                exit 0
                ;;
            *) error "Unknown argument: $1"; exit 1 ;;
        esac
    done
}

# ----- Resolve GOPATH/bin -----
resolve_gobin() {
    local user="${SUDO_USER:-$USER}"
    local gobin=""

    # Try GOBIN first.
    if [[ -n "${GOBIN:-}" ]]; then
        gobin="$GOBIN"
    elif command -v go &>/dev/null; then
        gobin="$(go env GOBIN 2>/dev/null || true)"
        if [[ -z "$gobin" ]]; then
            gobin="$(go env GOPATH 2>/dev/null || true)/bin"
        fi
    fi

    # Fallback to common locations.
    if [[ -z "$gobin" || ! -d "$gobin" ]]; then
        local home_dir
        home_dir="$(eval echo "~${user}")"
        gobin="${home_dir}/go/bin"
    fi

    echo "$gobin"
}

# ----- Check-only mode -----
run_check_only() {
    echo ""
    echo -e "${BOLD}Development Teardown Preview${NC}"
    echo -e "${BOLD}============================${NC}"
    echo ""

    # Delegate CH teardown preview.
    echo -e "${BOLD}--- CH Runtime (via teardown-cloud-hypervisor.sh) ---${NC}"
    local ch_args="--check-only"
    [[ "$PURGE" == "true" ]] && ch_args="--check-only --purge"
    bash "${SCRIPT_DIR}/teardown-cloud-hypervisor.sh" $ch_args 2>/dev/null || true

    echo ""
    echo -e "${BOLD}--- Go dev tools ---${NC}"
    local gobin
    gobin="$(resolve_gobin)"
    for tool in "${GO_DEV_TOOLS[@]}"; do
        local tool_path="${gobin}/${tool}"
        if [[ -f "$tool_path" ]]; then
            info "Would remove: $tool_path"
        elif command -v "$tool" &>/dev/null; then
            info "Would remove: $(command -v "$tool")"
        else
            info "Not present:  $tool"
        fi
    done

    echo ""
    exit 0
}

# ----- Remove Go dev tools -----
remove_dev_tools() {
    echo -e "${BOLD}--- Removing Go dev tools ---${NC}"
    local gobin
    gobin="$(resolve_gobin)"

    for tool in "${GO_DEV_TOOLS[@]}"; do
        local tool_path="${gobin}/${tool}"
        if [[ -f "$tool_path" ]]; then
            rm -f "$tool_path"
            ok "Removed $tool ($tool_path)"
        elif command -v "$tool" &>/dev/null; then
            local actual_path
            actual_path="$(command -v "$tool")"
            rm -f "$actual_path"
            ok "Removed $tool ($actual_path)"
        else
            info "Already absent: $tool"
        fi
    done
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
    echo -e "${BOLD}Development Environment Teardown${NC}"
    echo -e "${BOLD}=================================${NC}"
    echo ""

    # Run CH teardown (pass through --purge if set).
    local ch_args=()
    [[ "$PURGE" == "true" ]] && ch_args+=("--purge")
    bash "${SCRIPT_DIR}/teardown-cloud-hypervisor.sh" "${ch_args[@]+"${ch_args[@]}"}"

    echo ""
    remove_dev_tools

    echo ""
    echo -e "${BOLD}Teardown complete.${NC}"
    echo ""
}

main "$@"
