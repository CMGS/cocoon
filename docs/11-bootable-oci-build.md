# Building Bootable OCI Images

**Version**: 1.0
**Status**: Planned
**Phase**: Phase 2
**Last Updated**: 2026-02-14

## Overview

This document will provide comprehensive guidance on building custom bootable OCI images that satisfy Cocoon's Boot Contract requirements.

## Coming Soon

**Planned Content**:

1. **Minimum Requirements**
   - Package lists per distribution (Ubuntu, Fedora, Debian)
   - Kernel, initrd, systemd, cloud-init, GRUB configuration

2. **Build Process**
   - Dockerfile multi-stage build examples
   - Build scripts for automated image creation
   - GRUB configuration and installation

3. **Example Implementations**
   - `examples/ubuntu-bootable/` - Complete Ubuntu example
   - `examples/fedora-bootable/` - Complete Fedora example
   - Reproducible builds with versioned dependencies

4. **Verification**
   - How to test bootability locally
   - Using `cocoon image verify` for validation
   - Common pitfalls and troubleshooting

5. **Publishing**
   - Pushing to registries (Docker Hub, GHCR)
   - Versioning and tagging strategy
   - Reference images with digest

## Current Workaround

**For Phase 1**, we recommend using **Cloud Hypervisor native cloud images** instead of building custom bootable OCI images:

### Recommended Cloud Images

**Ubuntu Cloud Images**:
```bash
# Download Ubuntu 22.04 Cloud Image
wget https://cloud-images.ubuntu.com/releases/22.04/release/ubuntu-22.04-server-cloudimg-amd64.img

# Convert to qcow2 if needed
qemu-img convert -O qcow2 ubuntu-22.04-server-cloudimg-amd64.img ubuntu-22.04-cloudimg.qcow2

# Use with Cocoon — IMAGE is a positional parameter, --disk sets overlay size
cocoon create ubuntu-22.04-cloudimg.qcow2 --name myvm --cpus 2 --memory 2G --disk 20G
```

**Fedora Cloud Images**:
```bash
# Download Fedora 39 Cloud Image
wget https://download.fedoraproject.org/pub/fedora/linux/releases/39/Cloud/x86_64/images/Fedora-Cloud-Base-39-1.5.x86_64.qcow2

# Use with Cocoon — IMAGE is positional, not a flag
cocoon create Fedora-Cloud-Base-39-1.5.x86_64.qcow2 --name fedora-vm
```

**Why Cloud Images Work**:
- ✅ Pre-installed kernel, initrd, systemd
- ✅ GRUB bootloader configured
- ✅ cloud-init pre-configured
- ✅ GPT + ESP partition layout
- ✅ Optimized for Cloud Hypervisor/KVM

## Why Defer to Phase 2?

Building bootable OCI images is **complex** and requires:
- Multi-stage Dockerfile with package installation
- GRUB installation and configuration inside container
- Partition table and filesystem setup
- ESP partition creation
- Thorough testing across distributions

This complexity is **orthogonal to Cocoon's core VM management functionality**. By using native cloud images in Phase 1, we can:
- Validate Boot Contract requirements
- Test PVH/UEFI boot modes
- Develop storage and lifecycle management
- Ensure Cloud Hypervisor integration works

Once Phase 1 is stable, we can add bootable OCI build tooling as a convenience feature.

## References

- [Boot Contract Specification](./01-boot-contract.md) - Required components
- [OCI Conversion Guide](./04-oci-conversion.md) - Validation rules and verified images
- [CLI Design](./09-cli-design.md) - CLI command reference
