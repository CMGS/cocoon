# OCI VM镜像的OverlayFS和COW实现分析

**日期**: 2026-02-19  
**状态**: 分析文档  
**相关文档**: [04.1-oci-vm-images.md](./04.1-oci-vm-images.md), [05-storage-management.md](./05-storage-management.md)

---

## 问题

**检查项目在运行 OCI image based VM 的时候，overlayfs 有用么？怎么做 COW**

---

## 执行摘要

### 简短回答

1. **OverlayFS的用途**: OverlayFS是Cocoon Phase 2 OCI VM镜像设计的核心存储机制，但**当前尚未实现**。设计用于：
   - 将OCI镜像层（kernel、rootfs、customization）组合成统一的文件系统
   - 通过virtiofs将组合后的文件系统提供给VM guest
   - 实现跨VM的层共享和per-VM的写入隔离

2. **COW实现方式**:
   - **Phase 1 (当前实现)**: 使用qcow2 overlay文件，通过`qemu-img create -b`创建backing file实现COW
   - **Phase 2 (计划中)**: 使用Linux OverlayFS内核特性，通过upperdir/lowerdir实现COW

---

## 详细分析

### Phase 1: 当前的qcow2 COW实现

#### 存储结构
```
/var/lib/cocoon/
+-- cache/
|   +-- images/
|       +-- {baseKey}.qcow2           # 共享的base镜像（只读）
+-- vms/
    +-- {vmID}/
        +-- overlay.qcow2              # per-VM的COW overlay
        +-- config.json                # VM配置
```

#### COW机制
- **Base镜像**: 存储在`cache/images/`，权限0444（只读）
- **Overlay文件**: 每个VM有独立的overlay.qcow2文件
- **创建命令**: `qemu-img create -f qcow2 -F qcow2 -b <backing> <overlay>`
- **写入行为**: 
  - VM首次写入某个块 → 从base读取 → 复制到overlay → 修改
  - 后续写入 → 直接修改overlay中的块
  - Base镜像保持不变，可被多个VM共享

#### 实现代码
- 文件: `storage/local/cow.go`
- 接口: `storage.COWManager`
- 关键方法:
  - `CreateBaseImage()` - 复制源镜像到cache
  - `CreateOverlay()` - 创建qcow2 overlay
  - `RemoveOverlay()` - 删除overlay（可选移到trash）

---

### Phase 2: 计划的OverlayFS COW实现

#### 设计目标

根据`docs/04.1-oci-vm-images.md`，Phase 2将使用OverlayFS而不是qcow2，原因是：

1. **直接内核启动**: 跳过UEFI固件，kernel和initrd直接传给Cloud Hypervisor
2. **层级共享**: OCI镜像层作为目录存储，可被多个VM共享
3. **即时组合**: OverlayFS在挂载时组合层，无需重建qcow2
4. **virtiofs传输**: merged目录通过virtiofs提供给guest作为rootfs

#### 存储结构（未实现）

```
/var/lib/cocoon/
+-- cache/
|   +-- oci/
|       +-- {manifest-digest}/          # 按manifest digest索引
|           +-- manifest.json
|           +-- config.json
|           +-- vmlinuz                 # 提取的kernel
|           +-- initrd.img              # 提取的initrd
|           +-- rootfs/                 # base rootfs层（目录树，只读）
|           +-- custom-1/               # 第一个自定义层（目录树，只读）
|           +-- custom-2/               # 第二个自定义层（目录树，只读）
+-- vms/
    +-- {vmID}/
        +-- upper/                      # OverlayFS upperdir (COW写入层)
        +-- work/                       # OverlayFS workdir (内核使用)
        +-- merged/                     # OverlayFS mount point
        +-- virtiofsd.sock              # virtiofsd socket
        +-- config.json
```

#### OverlayFS挂载命令（未实现）

对于有2个customization层的VM：

```bash
mount -t overlay overlay \
  -o lowerdir=/var/lib/cocoon/cache/oci/{digest}/custom-2:\
              /var/lib/cocoon/cache/oci/{digest}/custom-1:\
              /var/lib/cocoon/cache/oci/{digest}/rootfs,\
     upperdir=/var/lib/cocoon/vms/{vmID}/upper,\
     workdir=/var/lib/cocoon/vms/{vmID}/work \
  /var/lib/cocoon/vms/{vmID}/merged
```

#### COW机制设计

**层次结构**:
```
┌─────────────────────────────────────┐
│ Guest VM                            │
│ (sees merged filesystem via         │
│  virtiofs with tag "cocoonfs")      │
└─────────────────┬───────────────────┘
                  │ virtiofs
                  ↓
┌─────────────────────────────────────┐
│ Host: /var/lib/cocoon/vms/{vmID}/   │
│       merged/  (OverlayFS mount)    │
└─────────────────┬───────────────────┘
                  │ OverlayFS kernel
                  ↓
┌─────────────────────────────────────┐
│ Layers (top to bottom)              │
├─────────────────────────────────────┤
│ upper/          ← Guest writes here │  Per-VM (读写)
├─────────────────────────────────────┤
│ custom-2/       ← User customization│  Shared (只读)
│ custom-1/       ← User customization│  Shared (只读)
│ rootfs/         ← Base OS           │  Shared (只读)
└─────────────────────────────────────┘
```

**写入行为**:
1. Guest在rootfs中写入文件（如创建`/tmp/foo`）
2. virtiofsd接收FUSE write请求
3. OverlayFS检测到对只读层的写入
4. OverlayFS执行copy-up：
   - 如果文件已存在于lower层 → 复制整个文件到upper/
   - 创建新文件 → 直接在upper/创建
5. 后续对该文件的修改 → 直接修改upper/中的版本
6. Lower层（rootfs/, custom-*/）保持不变

**删除行为**:
- 删除lower层中的文件 → 在upper/创建whiteout文件（字符设备0/0）
- OverlayFS在merged视图中隐藏该文件
- Lower层原始文件仍然存在（被mask）

**优势**:
- ✅ 层共享：多个VM共享相同的base和custom层（目录）
- ✅ 即时启动：无需qcow2复制或转换
- ✅ 空间效率：只有per-VM写入存储在upper/
- ✅ 与OCI标准对齐：layer = directory tree

---

## 当前实现状态

### 已实现 (Phase 1)

| 组件 | 文件 | 状态 |
|------|------|------|
| qcow2 COW管理 | `storage/local/cow.go` | ✅ 完整实现 |
| Reference counting | `storage/local/refcount.go` | ✅ 完整实现 |
| Garbage collection | `storage/local/gc.go` | ✅ 完整实现 |
| qcow2 base镜像缓存 | `cache/images/` | ✅ 运行中 |

### 未实现 (Phase 2)

| 组件 | 预计位置 | 状态 |
|------|----------|------|
| OCI layer提取 | `image/pipeline/oci_extract.go` | ❌ 未实现 |
| OverlayFS挂载管理 | `storage/local/overlay.go` | ❌ 未实现 |
| OCI reference counting | `storage/local/oci_refcount.go` | ❌ 未实现 |
| virtiofsd生命周期 | `vm/engine/virtiofsd.go` | ❌ 未实现 |
| 直接内核启动 | `vm/engine/manager.go` | ⚠️ 部分schema已添加 |
| OCI cache目录 | `cache/oci/` | ❌ 未创建 |

---

## Phase 2实现依赖关系

```
┌────────────────────────────────────────┐
│ 1. OCI Image Pull & Layer Extraction   │
│    - Pull OCI manifest                 │
│    - Extract layers to cache/oci/      │
│    - Validate media types              │
└────────────────┬───────────────────────┘
                 │ 需要先实现
                 ↓
┌────────────────────────────────────────┐
│ 2. OverlayFS Mount Management          │
│    - Create upper/work/merged dirs     │
│    - Mount OverlayFS                   │
│    - Handle unmount on VM stop         │
└────────────────┬───────────────────────┘
                 │ 需要先实现
                 ↓
┌────────────────────────────────────────┐
│ 3. virtiofsd Daemon Management         │
│    - Spawn virtiofsd process           │
│    - Point to merged/ directory        │
│    - Manage socket lifecycle           │
└────────────────┬───────────────────────┘
                 │ 需要先实现
                 ↓
┌────────────────────────────────────────┐
│ 4. Direct Kernel Boot Integration      │
│    - Wire kernel/initrd to CH API      │
│    - Configure fs[] for virtiofs       │
│    - Pass kernel cmdline               │
└────────────────┬───────────────────────┘
                 │ 需要先实现
                 ↓
┌────────────────────────────────────────┐
│ 5. OCI Reference Counting & GC         │
│    - Track manifest → VM references    │
│    - GC unreferenced cache entries     │
│    - Clean up upper/work/merged        │
└────────────────────────────────────────┘
```

---

## 对比：Phase 1 vs Phase 2 COW

| 方面 | Phase 1 (qcow2) | Phase 2 (OverlayFS) |
|------|-----------------|---------------------|
| **Base存储** | qcow2文件 | 目录树 |
| **COW机制** | qemu-img overlay | OverlayFS upperdir |
| **共享单位** | 文件（qcow2 backing） | 目录树（lowerdir） |
| **写入位置** | overlay.qcow2内部块 | vms/{vmID}/upper/ |
| **Boot方法** | UEFI firmware | 直接内核启动 |
| **Rootfs传递** | virtio-blk (块设备) | virtiofs (文件系统) |
| **Layer概念** | 单个base qcow2 | 多个layer目录 |
| **空间效率** | 好（块级COW） | 更好（文件级COW） |
| **启动速度** | 较慢（UEFI初始化） | 快（直接启动） |
| **实现复杂度** | 简单（成熟工具） | 中等（需要挂载管理） |
| **当前状态** | ✅ 已实现 | ❌ 未实现 |

---

## 安全考虑

### Phase 2 OverlayFS安全模型

根据`docs/04.1-oci-vm-images.md`第6.6节：

**隔离层**:
1. **OverlayFS隔离**: Lower层只读，upper层per-VM
   - Guest无法破坏共享的cache层
   - 每个VM的写入互相隔离

2. **virtiofsd沙箱**: 
   - `--sandbox=chroot` 限制访问到merged/目录
   - Guest即使逃逸也只能访问本VM的OverlayFS树
   - 无法访问host的其他文件系统

3. **挂载选项**:
   - `nosuid` - 防止setuid程序
   - `nodev` - 防止设备节点
   - 建议在专用mount namespace中挂载

**与user-volume virtiofsd的区别**:
- User volume: 有allowlist，默认只读，用户指定路径
- Rootfs virtiofsd: 无allowlist，必须读写，Cocoon管理路径

---

## 性能考虑

### OverlayFS层数限制

根据`docs/04.1-oci-vm-images.md`第14节：

- **Kernel限制**: OverlayFS默认最多支持500层（可配置）
- **Cocoon建议**: 限制在20层以内，超过则在build时squash
- **验证**: Pull时检查层数，超过限制拒绝

### 性能优化

**Phase 1 (qcow2)**:
- ✅ 成熟的块级COW
- ✅ 良好的随机访问性能
- ❌ UEFI启动开销

**Phase 2 (OverlayFS)**:
- ✅ 极快的启动（跳过UEFI）
- ✅ 零拷贝层组合
- ✅ 文件级粒度共享
- ⚠️ Copy-up性能取决于文件大小
- ⚠️ 多层查找可能影响性能（因此限制层数）

---

## 迁移路径

### 向后兼容性

Phase 2实现后，两种模式将共存：

| 镜像类型 | 存储机制 | Boot方法 | 支持状态 |
|----------|----------|----------|----------|
| 传统cloud镜像 | qcow2 COW | UEFI | ✅ Phase 1（继续支持） |
| OCI容器镜像 | qcow2 COW | UEFI | ✅ Phase 1（继续支持） |
| OCI VM镜像 | OverlayFS | 直接内核 | 📋 Phase 2（计划） |

### VM配置区分

`types.VMConfig`已包含`ImageType`字段：

```go
type VMImageType string

const (
    VMImageTypeQCOW2 VMImageType = "qcow2"    // Phase 1
    VMImageTypeOCIVM VMImageType = "oci-vm"   // Phase 2
)
```

VM创建时记录镜像类型，启动时选择对应的路径。

---

## 结论

### 关键发现

1. **OverlayFS是Phase 2的核心**，但当前项目**完全基于Phase 1的qcow2机制**运行
2. **COW在两个阶段以不同方式实现**：
   - Phase 1: qcow2块级COW（已实现）
   - Phase 2: OverlayFS文件系统级COW（未实现）
3. **文档完整**，设计清晰，但代码实现尚未开始
4. **架构兼容**，两种模式可以共存

### 实现建议

如果要实现Phase 2 OverlayFS支持，建议顺序：

1. **PR 1**: OCI layer提取到目录
   - 实现OCI manifest解析
   - 实现layer提取到`cache/oci/{digest}/`
   - 添加media type验证

2. **PR 2**: OverlayFS挂载管理
   - 实现mount/unmount逻辑
   - 创建upper/work/merged目录
   - 处理层排序和合并

3. **PR 3**: virtiofsd生命周期
   - Spawn virtiofsd进程
   - 管理socket
   - 处理进程清理

4. **PR 4**: 直接内核启动
   - 完成CH REST API集成
   - Wire kernel/initrd路径
   - 传递kernel cmdline

5. **PR 5**: OCI引用计数和GC
   - 实现oci-references.json
   - 扩展gc命令
   - 清理OverlayFS挂载

### 测试策略

- Unit tests: 各个组件独立测试
- Integration tests: 端到端OCI VM创建/启动/删除
- Performance tests: OverlayFS vs qcow2性能对比
- Compatibility tests: Phase 1和Phase 2共存

---

## 参考文档

- [docs/04.1-oci-vm-images.md](./04.1-oci-vm-images.md) - OCI VM镜像格式完整设计
- [docs/04-oci-conversion.md](./04-oci-conversion.md) - Phase 1 OCI转换（已实现）
- [docs/05-storage-management.md](./05-storage-management.md) - 存储和引用计数
- [docs/11-bootable-oci-build.md](./11-bootable-oci-build.md) - 构建可启动镜像
- [docs/17-volume-passthrough.md](./17-volume-passthrough.md) - virtiofsd用于user volumes

---

**总结**: OverlayFS是Phase 2设计的重要组件，将为OCI VM镜像提供高效的层共享和COW机制，但目前项目仍使用Phase 1的qcow2方案。实现Phase 2需要跨多个模块的协调开发。
