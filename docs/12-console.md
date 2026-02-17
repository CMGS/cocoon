# VM Console

**Version**: 1.0
**Status**: Implemented
**Phase**: Phase 2
**Last Updated**: 2026-02-14

## Executive Summary

This document specifies the design for interactive VM console access in Cocoon. The `cocoon console` command provides a bidirectional TTY session to a running VM via a Cloud Hypervisor PTY device, enabling real-time keyboard input and screen output. The design uses a dual-port strategy that preserves existing `cocoon logs` functionality (serial log file) while adding interactive access through a virtio-console PTY.

## Table of Contents

1. [Overview](#1-overview)
2. [Design](#2-design)
3. [Configuration Changes](#3-configuration-changes)
4. [Implementation](#4-implementation)
5. [CLI](#5-cli)
6. [Guest Requirements](#6-guest-requirements)
7. [Error Handling](#7-error-handling)
8. [Security](#8-security)
9. [Testing](#9-testing)
10. [Cross-References](#10-cross-references)

---

## 1. Overview

### 1.1 Problem Statement

Cocoon Phase 1 provides `cocoon logs` for read-only serial output, which is sufficient for monitoring boot progress and capturing guest logs. However, several use cases require bidirectional interaction with the guest:

1. **Debugging**: When a VM boots but a service fails, an operator needs a shell inside the guest to inspect processes, logs, and configuration. Without network access (Phase 1 default), serial console is the only path in.
2. **No-network VMs**: Cocoon's primary use case is AI Agent sandboxes with no network connectivity. SSH is unavailable. The serial console is the sole interactive channel.
3. **Initial setup and rescue**: First-boot configuration (setting passwords, enabling services, fixing fstab) requires interactive input. If a VM enters single-user mode or a rescue shell, there is no way to interact with it without console access.
4. **Boot failures**: When a VM hangs during boot (waiting for user input, GRUB menu, fsck prompt), the operator needs to type responses. Read-only `cocoon logs` cannot help.

### 1.2 Approach: PTY Mode via CH REST API

The console uses Cloud Hypervisor's PTY mode for the virtio-console device. CH allocates a pseudo-terminal and reports its path via `GET /api/v1/vm.info`. Cocoon opens the PTY device and relays I/O between the user's terminal and the PTY.

```
+----------+       +-------------------+       +------------------+
|  User    |       |  cocoon console   |       | Cloud Hypervisor |
| Terminal | <---> |  (raw mode relay)  | <---> |    PTY device    |
|  stdin/  |       |  open(/dev/pts/X) |       |  --console pty   |
|  stdout  |       +-------------------+       +------------------+
+----------+                                          |
                                                      v
                                              +----------------+
                                              |   Guest VM     |
                                              |   /dev/hvc0    |
                                              |   getty/agetty  |
                                              +----------------+
```

### 1.3 Dual-Port Strategy

To preserve `cocoon logs` compatibility while enabling interactive console, the VM configuration uses both serial ports available in Cloud Hypervisor:

| Device | Mode | Purpose | Guest Device |
|--------|------|---------|-------------|
| Serial (`--serial`) | `File` | Persistent log capture (`cocoon logs`) | `/dev/ttyS0` |
| Console (`--console`) | `Pty` | Interactive console (`cocoon console`) | `/dev/hvc0` |

This means:
- `cocoon logs` continues reading `/var/log/cocoon/{vmID}-serial.log` as before
- `cocoon console` connects to the virtio-console PTY device
- No breaking changes to existing VMs created before console support

### 1.3 Minimum Cloud Hypervisor Version

**Minimum Cloud Hypervisor Version**: ≥38.0.0 (same baseline as Phase 1, validated by `cocoon doctor` — see [docs/08-dependencies.md](./08-dependencies.md)). The `vm.info` API (used to discover the PTY path) has been available since early CH releases. If integration testing during Phase 2 development reveals that stable PTY allocation requires a higher version, the doctor check will be updated accordingly.

---

## 2. Design

### 2.1 Console Attach Flow

```
consoleAction(c *cli.Context)
    |
    v
[1] Resolve VM_REF -> vmID
    |
    v
[2] Load metadata, verify VM is RUNNING
    (PAUSED will be allowed when pause/resume is implemented — see docs/13-pause-resume.md)
    |
    v
[3] GET /api/v1/vm.info -> extract console PTY path
    |
    v
[4] Validate PTY path (/dev/pts/<digits>), open PTY (os.OpenFile, O_RDWR)
    |
    v
[5] Put user terminal into raw mode (golang.org/x/term)
    |                               defer restore
    v
[6] Print connection banner
    |
    v
[7] Start bidirectional I/O relay with escape handling
    |    goroutine 1: PTY -> stdout
    |    goroutine 2: stdin -> PTY (with escape detection)
    v
[8] Block until:
    - Escape sequence (~.) detected
    - PTY read returns EOF (VM shut down)
    - Signal received (SIGINT, SIGTERM)
    - Context canceled
    |
    v
[9] Restore terminal, close PTY fd, print disconnect message
```

### 2.2 Escape Sequence State Machine

The escape sequence follows SSH conventions: the escape character (default `~`) is only recognized at the start of a new line (immediately after `\r` or `\n`). This prevents accidental disconnects when typing `~` in normal text.

```
                  +-----------+
        CR/LF    |           |  any other byte
    +----------->| LINE_START |---+-----------> NORMAL
    |            |           |   |
    |            +-----------+   |
    |                 |          |
    |           escape char      |
    |                 |          |
    |                 v          |
    |            +-----------+   |
    |            |  ESCAPED  |---+--- other (non-CR/LF) ---> NORMAL
    |            +-----------+             (forward both)
    |              |    |    |
    |          '.' |    |'?' | CR/LF
    |              v    v    +------> LINE_START
    |        DISCONNECT HELP          (forward both)
    |
    +---- all non-CR/LF paths eventually return to NORMAL
```

### 2.3 Clean Detach Without Killing the VM

A critical property is that detaching (via escape sequence, Ctrl-C in raw mode, or SIGTERM) never affects the running VM. The PTY is allocated and owned by the Cloud Hypervisor process, not by Cocoon. Closing the file descriptor simply disconnects the reader/writer. CH continues writing guest output to the primary side of the PTY.

```
Before detach:
  cocoon console <--> PTY slave <--> PTY master <--> CH <--> Guest

After detach:
  (disconnected)     PTY slave       PTY master <--> CH <--> Guest
                                     (output buffered in kernel)
```

---

## 3. Configuration Changes

### 3.1 CHConsoleConfig Update

The `CHConsoleConfig` type in `hypervisor/types.go` gains a `File` field for PTY path discovery:

```go
// CHConsoleConfig controls the virtio console.
//
// Supported modes:
//   - "Off"  : no console
//   - "Tty"  : connect to a TTY
//   - "File" : write output to a file
//   - "Pty"  : allocate a PTY (path reported in vm.info)
type CHConsoleConfig struct {
    Mode string `json:"mode"`
    File string `json:"file,omitempty"` // Populated by CH when mode is "Pty" or "File"
}
```

### 3.2 VM Engine Configuration

The `buildCHVMConfig` function in `vm/engine/manager.go` changes from:

```go
Console: hypervisor.CHConsoleConfig{
    Mode: "Off",
},
```

to:

```go
Console: hypervisor.CHConsoleConfig{
    Mode: "Pty",
},
```

Serial remains `File` mode, unchanged.

### 3.3 PTY Path Discovery

After the VM boots, the allocated PTY path is available from the CH REST API:

```
GET /api/v1/vm.info
```

Response (relevant excerpt):

```json
{
  "config": {
    "console": {
      "mode": "Pty",
      "file": "/dev/pts/7"
    }
  },
  "state": "Running"
}
```

When console mode is `Pty`, CH allocates a PTY pair at process start and populates the `file` field with the secondary (slave) PTY path. Cocoon reads this path by calling `GetVMInfo()` on the existing hypervisor client.

---

## 4. Implementation

### 4.1 Escape Sequence Handler

```go
type escapeState int

const (
    stateNormal escapeState = iota
    stateLineStart
    stateEscaped
)

func relayStdinToPTY(ctx context.Context, stdin io.Reader, pty io.Writer, escapeChar byte) error {
    state := stateLineStart // Start of session acts as start of line.
    buf := make([]byte, 1)

    for {
        select {
        case <-ctx.Done():
            return nil
        default:
        }

        n, err := stdin.Read(buf)
        if n == 0 || err != nil {
            return err
        }
        b := buf[0]

        switch state {
        case stateNormal:
            if b == '\r' || b == '\n' {
                state = stateLineStart
            }
            if _, werr := pty.Write(buf[:1]); werr != nil {
                return werr
            }

        case stateLineStart:
            if b == escapeChar {
                state = stateEscaped
                continue // Do not forward escape char yet.
            }
            if b == '\r' || b == '\n' {
                state = stateLineStart // Stay in line start.
            } else {
                state = stateNormal
            }
            if _, werr := pty.Write(buf[:1]); werr != nil {
                return werr
            }

        case stateEscaped:
            switch b {
            case '.':
                // Disconnect.
                return nil
            case '?':
                // Print help to user (not to guest).
                helpMsg := "\r\nSupported escape sequences:\r\n" +
                    "  " + string(escapeChar) + ".  Disconnect\r\n" +
                    "  " + string(escapeChar) + "?  This help\r\n" +
                    "  " + string(escapeChar) + string(escapeChar) + "  Send escape character\r\n"
                _, _ = os.Stdout.Write([]byte(helpMsg))
                state = stateLineStart // Allow immediate follow-up escape.
                continue
            case escapeChar:
                // Send literal escape char.
                state = stateNormal
                if _, werr := pty.Write([]byte{escapeChar}); werr != nil {
                    return werr
                }
            default:
                // Not a recognized sequence; forward both the escape char
                // and the current byte. Track CR/LF so the next escape
                // sequence at the new line start is still recognized.
                if b == '\r' || b == '\n' {
                    state = stateLineStart
                } else {
                    state = stateNormal
                }
                if _, werr := pty.Write([]byte{escapeChar, b}); werr != nil {
                    return werr
                }
            }
        }
    }
}
```

### 4.2 Bidirectional I/O Relay

```go
func relayConsole(ctx context.Context, pty *os.File, escapeChar byte) error {
    ctx, cancel := context.WithCancel(ctx)
    defer cancel()

    errCh := make(chan error, 2)

    // PTY -> stdout (guest output to user).
    go func() {
        _, err := io.Copy(os.Stdout, pty)
        errCh <- err
        cancel()
    }()

    // stdin -> PTY (user input to guest), with escape detection.
    go func() {
        err := relayStdinToPTY(ctx, os.Stdin, pty, escapeChar)
        errCh <- err
        cancel()
    }()

    // Wait for either goroutine to finish.
    select {
    case <-ctx.Done():
        return nil
    case err := <-errCh:
        return err
    }
}
```

### 4.3 Terminal Raw Mode Handling

```go
import "golang.org/x/term"

func consoleAction(c *cli.Context) error {
    // ... resolve VM, get PTY path ...

    // Save and restore terminal state.
    fd := int(os.Stdin.Fd())
    oldState, err := term.MakeRaw(fd)
    if err != nil {
        return fmt.Errorf("set terminal raw mode: %w", err)
    }
    defer func() {
        _ = term.Restore(fd, oldState)
        fmt.Fprintf(os.Stderr, "\r\nDisconnected.\r\n")
    }()

    // ... open PTY, start relay ...
}
```

Key considerations:

- `term.MakeRaw()` disables echo, canonical mode, signal generation (Ctrl-C), and output processing.
- The deferred `term.Restore()` ensures the terminal is always restored, even on panic.
- If the process is killed with SIGKILL (untrappable), the terminal remains in raw mode. The user can recover with `reset` or `stty sane`. This is standard behavior for all console tools (ssh, screen, minicom).

### 4.4 Signal Handling and Terminal Resize

```go
func consoleAction(c *cli.Context) error {
    // ... setup ...

    // Handle SIGWINCH for terminal resize propagation.
    sigCh := make(chan os.Signal, 1)
    signal.Notify(sigCh, syscall.SIGWINCH)
    defer signal.Stop(sigCh)

    go func() {
        for range sigCh {
            propagateTerminalSize(os.Stdin, pty)
        }
    }()

    // Re-register SIGINT/SIGTERM to prevent double-signal from bypassing
    // terminal restore (signal.NotifyContext re-arms the default handler
    // after the first signal). Drain absorbed signals in a goroutine.
    sigCh2 := make(chan os.Signal, 1)
    signal.Notify(sigCh2, syscall.SIGINT, syscall.SIGTERM)
    defer signal.Stop(sigCh2)
    go func() {
        for range sigCh2 {
            // Absorbed — context cancellation handles graceful shutdown.
        }
    }()

    // In raw mode, Ctrl-C generates a raw byte (0x03), not SIGINT,
    // so it is forwarded to the guest as-is.

    // ... start relay ...
}

func propagateTerminalSize(local *os.File, remote *os.File) {
    width, height, err := term.GetSize(int(local.Fd()))
    if err != nil {
        return
    }
    _ = setWinSize(remote, width, height)
}
```

Terminal resize propagation ensures that full-screen applications inside the guest (e.g., `top`, `vim`, `less`) receive correct terminal dimensions. The `TIOCSWINSZ` ioctl is set on the PTY file descriptor:

```go
import "unsafe"
import "syscall"

type winSize struct {
    Rows uint16
    Cols uint16
    X    uint16 // unused
    Y    uint16 // unused
}

func setWinSize(f *os.File, cols, rows int) error {
    ws := winSize{Rows: uint16(rows), Cols: uint16(cols)}
    _, _, errno := syscall.Syscall(
        syscall.SYS_IOCTL,
        f.Fd(),
        syscall.TIOCSWINSZ,
        uintptr(unsafe.Pointer(&ws)),
    )
    if errno != 0 {
        return errno
    }
    return nil
}
```

### 4.5 Hypervisor Client Addition

Add a convenience method to retrieve the console PTY path:

```go
// In hypervisor/hypervisor.go (addition to Client interface)

type Client interface {
    // ... existing methods ...

    // GetConsolePTYPath retrieves the virtio-console PTY path for a running VM.
    // Returns an empty string if the console is not in Pty mode.
    GetConsolePTYPath(ctx context.Context, socketPath string) (string, error)
}
```

Implementation in `hypervisor/cloudhypervisor/client.go`:

```go
func (c *client) GetConsolePTYPath(ctx context.Context, socketPath string) (string, error) {
    info, err := c.GetVMInfo(ctx, socketPath)
    if err != nil {
        return "", fmt.Errorf("get VM info: %w", err)
    }
    if info.Config.Console.Mode != "Pty" {
        return "", nil
    }
    return info.Config.Console.File, nil
}
```

### 4.6 VMInspect Extension

Add the console PTY path to the inspect output when available:

```go
// In types/inspect.go (addition)

type InspectHypervisorInfo struct {
    CHSocket   string `json:"ch_socket"`
    CHPID      int    `json:"ch_pid"`
    SerialLog  string `json:"serial_log"`
    ConsolePTY string `json:"console_pty,omitempty"` // PTY path when console mode is Pty
}
```

This field is populated at inspect time by querying the CH REST API, not stored in config.json or metadata.json.

### 4.7 Project Structure Additions

```
cocoon/
├── cmd/cocoon/
│   ├── console.go             # cocoon console command
│   ├── console_linux.go       # Linux-specific SIGWINCH + ioctl
│   ├── console_darwin.go      # Stub: SIGWINCH no-op (Linux check is in console.go via runtime.GOOS)
│   └── console_test.go        # Escape state machine tests, write error tests, context cancellation
```

---

## 5. CLI

### 5.1 Command Registration

```go
func consoleCommand() *cli.Command {
    return &cli.Command{
        Name:      "console",
        Usage:     "Attach an interactive console to a running VM",
        ArgsUsage: "VM_REF",
        Flags: []cli.Flag{
            &cli.StringFlag{
                Name:  "escape-char",
                Value: "~",
                Usage: "escape character for disconnect (must be a printable ASCII character, 0x20-0x7E)",
            },
        },
        Action: consoleAction,
    }
}
```

Registration in `cmd/cocoon/main.go`:

```go
app.Commands = []*cli.Command{
    // ... existing commands ...
    consoleCommand(),
}
```

### 5.2 Complete Console Action

```go
func consoleAction(c *cli.Context) error {
    if runtime.GOOS != "linux" {
        return fmt.Errorf("cocoon console requires Linux (Cloud Hypervisor is Linux-only)")
    }

    if c.NArg() < 1 {
        return fmt.Errorf("VM_REF argument required\n\nUsage: cocoon console VM_REF [flags]")
    }

    app, err := initApp(c)
    if err != nil {
        return err
    }

    ref := c.Args().Get(0)
    vmID, err := app.vmMgr.ResolveVMRef(ref)
    if err != nil {
        return fmt.Errorf("resolve VM ref %q: %w", ref, err)
    }

    // Verify VM is running.
    // TODO: also allow VMStatePaused when pause/resume is implemented (docs/13-pause-resume.md).
    meta, err := app.vmMgr.LoadMetadata(vmID)
    if err != nil {
        return err
    }
    state := types.VMState(meta.State)
    if state != types.VMStateRunning {
        return fmt.Errorf("VM %s is not running (state: %s)", ref, meta.State)
    }

    // Get PTY path from CH API.
    cfg, err := app.vmMgr.LoadConfig(vmID)
    if err != nil {
        return err
    }
    vmInfo, err := app.hyper.GetVMInfo(c.Context, cfg.SocketPath)
    if err != nil {
        return fmt.Errorf("get VM info for %s: %w", vmID, err)
    }

    ptyPath := vmInfo.Config.Console.File
    if ptyPath == "" {
        mode := vmInfo.Config.Console.Mode
        if mode != "Pty" {
            return fmt.Errorf("no console PTY available for %s (console mode: %s); "+
                "this VM was created before console support was added, "+
                "recreate it to enable interactive console; "+
                "if the issue persists, run 'cocoon doctor' to verify Cloud Hypervisor version (v38.0+ required)",
                ref, mode)
        }
        return fmt.Errorf("no console PTY allocated for %s; "+
            "Cloud Hypervisor reported Pty mode but no PTY path, "+
            "run 'cocoon doctor' to verify Cloud Hypervisor version (v38.0+ required)", ref)
    }
    ptyPath = filepath.Clean(ptyPath)
    if matched, _ := regexp.MatchString(`^/dev/pts/\d+$`, ptyPath); !matched {
        return fmt.Errorf("unexpected PTY path %q (expected /dev/pts/<number>)", ptyPath)
    }

    // Open PTY device. Note: there is an inherent TOCTOU window between the
    // state check above and this open — the VM may stop in between.
    pty, err := os.OpenFile(ptyPath, os.O_RDWR, 0)
    if err != nil {
        return fmt.Errorf("open console PTY %s (is the VM still running?): %w", ptyPath, err)
    }
    defer pty.Close()

    // Parse escape character before entering raw mode so validation errors
    // render correctly and don't trigger the "Disconnected" defer message.
    escapeCharStr := c.String("escape-char")
    if len(escapeCharStr) != 1 {
        return fmt.Errorf("--escape-char must be a single ASCII character, got %q", escapeCharStr)
    }
    escapeChar := escapeCharStr[0]
    if escapeChar < 0x20 || escapeChar > 0x7E {
        return fmt.Errorf("--escape-char must be a printable ASCII character (0x20-0x7E), got %q", escapeCharStr)
    }

    // Require interactive terminal.
    fd := int(os.Stdin.Fd())
    if !term.IsTerminal(fd) {
        return fmt.Errorf("stdin is not a terminal; cocoon console requires an interactive TTY")
    }

    // Set terminal to raw mode.
    oldState, err := term.MakeRaw(fd)
    if err != nil {
        return fmt.Errorf("set terminal raw mode: %w", err)
    }
    defer func() {
        _ = term.Restore(fd, oldState)
        fmt.Fprintf(os.Stderr, "\r\nDisconnected from %s.\r\n", ref)
    }()

    // Re-register SIGINT/SIGTERM to prevent double-signal from bypassing
    // terminal restore. Go's signal.NotifyContext re-arms the default
    // handler after the first signal, so a second Ctrl-C would force-kill
    // the process before the deferred term.Restore runs, leaving the
    // terminal in raw mode. We absorb these signals here and let the
    // relay's context cancellation handle graceful shutdown.
    sigCh := make(chan os.Signal, 1)
    signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
    defer signal.Stop(sigCh)
    go func() {
        for range sigCh {
            // Absorbed — context cancellation handles graceful shutdown.
        }
    }()

    // Propagate initial terminal size and handle SIGWINCH.
    cleanupSIGWINCH := handleSIGWINCH(os.Stdin, pty)
    defer cleanupSIGWINCH()

    // Print connection banner.
    fmt.Fprintf(os.Stderr, "Connected to %s (escape sequence: %s.)\r\n", ref, escapeCharStr)

    // Start bidirectional relay.
    return relayConsole(c.Context, pty, escapeChar)
}
```

### 5.3 Usage Examples

```bash
# Attach interactive console to a running VM
$ cocoon console myvm
Connected to myvm (escape sequence: ~.)

Ubuntu 22.04 LTS myvm hvc0

myvm login: _

# Disconnect with escape sequence (type Enter, then ~.)
~.
Disconnected from myvm.

# Use a different escape character
$ cocoon console myvm --escape-char "^"

# Show help for escape sequences (type Enter, then ~?)
~?
Supported escape sequences:
  ~.  Disconnect
  ~?  This help
  ~~  Send escape character
```

---

## 6. Guest Requirements

### 6.1 Kernel Support

The guest kernel must have virtio-console support compiled in or available as a module:

```
CONFIG_VIRTIO_CONSOLE=y   (or =m)
```

All standard Linux distribution kernels (Ubuntu, Fedora, Debian, Arch) include this by default. This is not a concern for cloud images.

### 6.2 Console Device

When CH is configured with `console.mode = "Pty"`, the guest sees `/dev/hvc0` (virtio-console device). The device appears automatically during boot.

### 6.3 Login Prompt (getty)

For interactive login, the guest must run `getty` or `agetty` on `/dev/hvc0`. Configuration depends on the init system:

**systemd** (most modern distributions):

```bash
# Usually auto-enabled. If not:
systemctl enable serial-getty@hvc0.service
```

The `serial-getty@.service` template is part of systemd and handles `hvc*` and `ttyS*` devices. Systemd's `systemd-getty-generator` auto-spawns getty instances for detected serial consoles.

**sysvinit / OpenRC**:

```
# /etc/inittab
h0:2345:respawn:/sbin/agetty -L 115200 hvc0 vt100
```

### 6.4 Kernel Command Line

For boot messages to appear on the virtio-console, the guest kernel command line should include:

```
console=ttyS0 console=hvc0
```

When multiple `console=` arguments are present, the Linux kernel sends output to all listed consoles. The **last** one listed becomes the primary console (where `/dev/console` points).

### 6.5 Cocoon Console Strategy

Cocoon uses two boot strategies: UEFI firmware boot via `CLOUDHV.fd` for cloud images, and direct kernel boot (`payload.kernel`) for OCI VM images. For UEFI boot, the firmware loads the kernel from the guest disk image, and the **bootloader inside the guest** (e.g., GRUB) controls the kernel command line. For direct kernel boot, the kernel command line is set by Cocoon via `payload.cmdline`. In the UEFI case, Cocoon cannot inject kernel parameters such as `console=hvc0` from the host side.

| Boot Strategy | Kernel cmdline control | Console mechanism |
|---------------|----------------------|-------------------|
| UEFI (firmware boot via `CLOUDHV.fd`) | Guest bootloader (GRUB) | `systemd-getty-generator` auto-detects `/dev/hvc0` |
| Direct kernel boot (OCI VM images) | Cocoon (`payload.cmdline`) | `systemd-getty-generator` auto-detects `/dev/hvc0` |

**Default behavior (no user action required)**:

- Standard cloud images (Ubuntu, Fedora, Debian) ship with `systemd-getty-generator`, which auto-spawns `serial-getty@hvcN.service` when it detects `/dev/hvc0`. In most cases, `cocoon console` works immediately after boot without guest configuration.
- Boot messages on `/dev/hvc0` depend on the guest's kernel command line containing `console=hvc0`. Most cloud images already include this, or the serial console on `/dev/ttyS0` captures boot output via `cocoon logs`.

**When console shows no login prompt**:

If `cocoon console` connects successfully but shows no login prompt, the guest is not running `getty` on `/dev/hvc0`. Remediation:

1. Connect via `cocoon logs` (serial on `/dev/ttyS0`) to inspect the guest.
2. Enable getty: `systemctl enable --now serial-getty@hvc0.service`
3. For custom images: ensure `console=hvc0` is in the guest's GRUB config (`/etc/default/grub` → `GRUB_CMDLINE_LINUX`) and getty is configured (see §6.3 and §6.4).

### 6.5.1 Kernel Command Line Injection Strategy

Phase 1's image conversion pipeline (`image/pipeline/convert_linux.go`) already injects `console=ttyS0,115200n8` into the guest GRUB configuration to ensure boot output is captured via `cocoon logs`.

**Phase 2 v1.0 approach**: Phase 2 v1.0 does **not** inject `console=hvc0` into the kernel command line. The `convert_linux.go` pipeline only injects `console=ttyS0,115200n8`. Instead, the v1.0 approach relies on `systemd-getty-generator` to automatically spawn a login prompt on `/dev/hvc0` when the device is detected. Standard cloud images (Ubuntu, Fedora, Debian) ship with `systemd-getty-generator` enabled, so `cocoon console` provides an interactive login prompt immediately after boot without any kernel command line changes. However, boot messages will **not** appear on the virtio-console (only on the serial port via `cocoon logs`) until `console=hvc0` injection is implemented.

**Future improvement**: Extend `convert_linux.go` to also inject `console=hvc0` alongside `console=ttyS0,115200n8` during image conversion, for images that do not have `systemd-getty-generator` or where boot-time console output on `/dev/hvc0` is desired. The resulting GRUB kernel command line would include:

```
console=ttyS0,115200n8 console=hvc0
```

When multiple `console=` arguments are present, the Linux kernel sends output to all listed consoles (see §6.4). The **last** entry becomes the primary console (`/dev/console`), so `hvc0` is listed last to make it the primary interactive console. This ensures boot messages are visible on both `cocoon logs` and `cocoon console` from the earliest point in the boot sequence.

### 6.6 Compatibility and Requirements

This section summarizes the hard requirements for console support and the compatibility status of common guest images.

**Hard Requirements for Console Support:**

1. **Kernel**: `CONFIG_VIRTIO_CONSOLE=y` (auto-satisfied by standard cloud images)
2. **Init System**: Must run `getty` on `/dev/hvc0` (auto-satisfied by `systemd-getty-generator` in Ubuntu/Fedora/Debian cloud images)
3. **Kernel Command Line**: Should include `console=hvc0` for boot output visibility

**Compatibility Matrix:**

| Image Source | Console Support | Notes |
|---|---|---|
| Ubuntu Cloud Image | Ready | systemd-getty-generator auto-starts getty on hvc0 |
| Fedora Cloud Image | Ready | systemd-getty-generator auto-starts getty on hvc0 |
| Debian Cloud Image | Ready | systemd-getty-generator auto-starts getty on hvc0 |
| Custom OCI Image | Requires Configuration | Must ensure getty runs on /dev/hvc0 |
| Direct qcow2 (URL/local) | Depends on image configuration | Virtio-console works if guest kernel has CONFIG_VIRTIO_CONSOLE. Falls back to serial if not. |

For custom images that do not include `systemd-getty-generator`, see §6.3 for init-system-specific getty configuration and §6.4 for kernel command line requirements.

---

## 7. Error Handling

### 7.1 Error Cases

| Condition | Error Message | Exit Code |
|-----------|--------------|-----------|
| VM not found | `VM not found: <ref>` | 1 |
| VM not running | `VM <ref> is not running (state: <state>)` | 1 |
| No console PTY available | `no console PTY available for <ref> (console mode: Off)` | 1 |
| PTY path does not exist | `open console PTY <path> (is the VM still running?): no such file or directory` | 1 |
| stdin is not a terminal | `stdin is not a terminal; cocoon console requires an interactive TTY` | 1 |
| Raw mode failure | `set terminal raw mode: <err>` | 1 |
| CH API unreachable | `get VM info for <vmID>: <err>` | 1 |
| CH version does not support Pty mode | `run 'cocoon doctor' to verify Cloud Hypervisor version (v38.0+ required)` | 1 |

### 7.2 Backward Compatibility

VMs created before console support is enabled will have `console.mode = "Off"`. Running `cocoon console` against such VMs returns a clear error with remediation instructions:

```
$ cocoon console old-vm
Error: no console PTY available for old-vm (console mode: Off); this VM was created before console support was added, recreate it to enable interactive console; if the issue persists, run 'cocoon doctor' to verify Cloud Hypervisor version (v38.0+ required)
```

### 7.3 Terminal Recovery

If `cocoon console` exits abnormally (SIGKILL), the terminal remains in raw mode. The user recovers with:

```bash
reset
# or
stty sane
```

This is standard behavior for all console tools (ssh, screen, minicom).

---

## 8. Security

### 8.1 Console Access Authorization

Console access provides full interactive control of the VM, equivalent to physical console access. Access is governed by:

- Host OS permissions to run `cocoon` (root in Phase 1)
- The guest's own login mechanism (getty prompts for username/password)

No additional authentication layer is introduced in Phase 2. For multi-user environments with different permissions, this must be handled at the host OS level (sudo rules, group membership) or deferred to the Phase 2 API server with authentication.

### 8.2 Session Logging

Interactive console sessions are not logged by default. This is intentional -- passwords and sensitive data may be typed. However, serial output on `/dev/ttyS0` continues to be logged to the serial log file, providing a baseline audit trail.

### 8.3 Escape Sequence Security

The escape character (`~`) is only recognized at the start of a line (after CR/LF or at session start). This prevents accidental disconnects from malicious guest programs that print `~.` sequences. The `--escape-char` flag allows changing the escape character if `~` conflicts with the guest workload.

The `--escape-char` value must be a printable ASCII character (0x20-0x7E). Control characters (including CR 0x0D, LF 0x0A, and DEL 0x7F) are rejected because they would break the escape state machine: CR and LF trigger the `stateLineStart` transition, so using them as the escape character would make the escape sequence undetectable.

### 8.4 PTY Buffer Limits

The kernel PTY buffer is finite (typically 4096 bytes). If no reader is attached, output accumulates until the buffer fills, then CH blocks (or drops data, depending on driver behavior). This does not pose a security risk but may cause data loss for unread output between attach sessions.

---

## 9. Testing

### 9.1 Unit Tests

#### Escape Sequence State Machine

```go
func TestEscapeSequence(t *testing.T) {
    tests := []struct {
        name     string
        input    []byte
        wantPTY  []byte // bytes written to PTY
        wantExit bool   // true if disconnect detected
    }{
        {
            name:    "normal text",
            input:   []byte("hello world"),
            wantPTY: []byte("hello world"),
        },
        {
            name:     "escape at line start",
            input:    []byte("\r~."),
            wantPTY:  []byte("\r"),
            wantExit: true,
        },
        {
            name:    "tilde in middle of line",
            input:   []byte("hello~.world"),
            wantPTY: []byte("hello~.world"),
        },
        {
            name:    "double tilde sends literal",
            input:   []byte("\r~~"),
            wantPTY: []byte("\r~"),
        },
        {
            name:    "unrecognized escape forwards both chars",
            input:   []byte("\r~x"),
            wantPTY: []byte("\r~x"),
        },
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            // ... test relay with in-memory buffers ...
        })
    }
}
```

#### PTY Path Extraction

```go
func TestGetConsolePTYPath(t *testing.T) {
    tests := []struct {
        name     string
        mode     string
        file     string
        wantPath string
    }{
        {"pty mode", "Pty", "/dev/pts/7", "/dev/pts/7"},
        {"off mode", "Off", "", ""},
        {"file mode", "File", "/var/log/foo", ""},
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            // ... test with mock CH client ...
        })
    }
}
```

### 9.2 Integration Tests

```go
func TestConsoleAttachDetach(t *testing.T) {
    if testing.Short() {
        t.Skip("skipping integration test")
    }

    // 1. Create VM with console.mode = "Pty"
    // 2. Start VM, wait for boot
    // 3. Verify PTY path in vm.info response
    // 4. Open PTY, send "whoami\n", verify output contains username
    // 5. Close PTY, verify VM still running
    // 6. Cleanup
}

func TestConsoleLegacyVMError(t *testing.T) {
    // 1. Create a VM with console.mode = "Off" (simulating pre-console VM)
    // 2. Attempt cocoon console -> expect clear error message
}
```

### 9.3 Cloud Image Verification

| Image | Expected Behavior |
|-------|-------------------|
| Ubuntu Cloud Image (amd64) | getty on hvc0 auto-starts |
| Fedora Cloud Image | getty on hvc0 auto-starts |
| Custom bootable OCI image | May need kernel command line adjustment |

---

## Design Notes

### Console-Serial / Boot-Detection Relationship

The serial console output (`/dev/ttyS0`, written to `{vmID}-serial.log` via
`--serial file=...`) is the same serial log that boot detection monitors
(see [01-boot-contract.md](./01-boot-contract.md) Section 3). The relationship
between the three consumers of serial output:

- **Boot detection** tail-reads the serial log file looking for success/failure
  regex patterns (`BootSuccessPatterns`, `BootFailurePatterns`). This operates
  on the `--serial` device, which writes to a file.
- **`cocoon logs`** reads the same serial log file for display to the user.
- **`cocoon console`** connects to the `--console` PTY device (`/dev/hvc0`),
  which is a **separate** device from `--serial`. Console and serial are
  independent I/O channels in Cloud Hypervisor.

Boot detection and `cocoon logs` share the serial port (`/dev/ttyS0` in the
guest). `cocoon console` uses the virtio-console port (`/dev/hvc0` in the
guest). These are distinct streams -- attaching to the console does not
interfere with boot detection or serial log capture.

### Multi-Attach Policy

Cloud Hypervisor allocates a single PTY pair for the `--console pty` device.
Only one file descriptor can meaningfully read from the secondary (slave) side
of the PTY at a time. If two processes open the same PTY path concurrently,
they race on reads and each receives an unpredictable subset of the output
bytes. Writes from both processes are interleaved into the guest input stream.

**Current policy**: Cocoon does not enforce single-attach at the application
level. If a user runs `cocoon console myvm` twice in separate terminals, both
sessions open the same PTY and produce garbled output. This is a known
limitation shared with other PTY-based console tools (e.g., `virsh console`).

**Mitigation**: A future improvement could use an advisory lock file
(`/run/cocoon/vms/{vmID}/console.lock`) to prevent concurrent attach, returning
a clear error: `"console is already attached by PID {pid}"`. This is deferred
beyond Phase 2 v1.0.

---

## 10. Cross-References

### 10.1 Related Cocoon Documents

- [03-hypervisor-integration.md](./03-hypervisor-integration.md): CH process model, API socket management, REST API mapping
- [07-vm-lifecycle.md](./07-vm-lifecycle.md): VM state machine and allowed operations per state
- [09-cli-design.md](./09-cli-design.md): CLI command structure, `cocoon logs` command

### 10.2 Interaction with Other Phase 2 Features

- **Pause/Resume** ([13-pause-resume.md](./13-pause-resume.md)): When pause/resume is implemented, console will be attachable to a PAUSED VM. The PTY remains open but no guest output arrives while paused. Input typed while the VM is paused is buffered in the kernel PTY buffer and delivered to the guest when the VM is resumed. If more than ~4 KB is typed while paused, excess input may be dropped (kernel PTY buffer limit). On resume, output resumes. (Currently only RUNNING VMs are supported — see step [2] in Section 2.1.)
- **Checkpoint/Restore** ([15-warm-start.md](./15-warm-start.md)): On checkpoint restore, Cloud Hypervisor allocates a new PTY with a different path. `cocoon console` discovers the PTY path dynamically via `GET /api/v1/vm.info`, so it works correctly on restored VMs without additional configuration.
- **Device Passthrough** ([14-device-passthrough.md](./14-device-passthrough.md)): Console is independent of device passthrough. Both can coexist on the same VM.

### 10.3 External References

- Cloud Hypervisor console documentation: https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/serial_console.md
- Cloud Hypervisor API schema: https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/vmm/src/api/openapi/cloud-hypervisor.yaml
- `golang.org/x/term` package: https://pkg.go.dev/golang.org/x/term
- Linux PTY documentation: `man 7 pty`
- SSH escape sequences: `man ssh` (ESCAPE CHARACTERS section)

---

**End of VM Console Design Document v1.0**
