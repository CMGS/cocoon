package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"

	cli "github.com/urfave/cli/v2"
	"golang.org/x/term"

	"github.com/CMGS/cocoon/types"
)

func consoleCommand() *cli.Command {
	return &cli.Command{
		Name:      "console",
		Usage:     "Attach an interactive console to a running VM",
		ArgsUsage: "VM_REF",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "escape-char",
				Value: "~",
				Usage: "escape character for disconnect (e.g., ~. to disconnect)",
			},
		},
		Action: consoleAction,
	}
}

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

	// Get PTY path from CH REST API.
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
			// VM was created with console mode Off (before Pty support was added),
			// or Cloud Hypervisor is too old to support Pty mode.
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
	if !strings.HasPrefix(ptyPath, "/dev/pts/") {
		return fmt.Errorf("unexpected PTY path %q (expected /dev/pts/*)", ptyPath)
	}

	// Open PTY device. Note: there is an inherent TOCTOU window between the
	// state check above and this open — the VM may stop in between.
	pty, err := os.OpenFile(ptyPath, os.O_RDWR, 0) //nolint:gosec // G304: PTY path validated above
	if err != nil {
		return fmt.Errorf("open console PTY %s (is the VM still running?): %w", ptyPath, err)
	}
	defer pty.Close() //nolint:errcheck // best-effort cleanup

	// Parse escape character before entering raw mode so validation errors
	// render correctly and don't trigger the "Disconnected" defer message.
	escapeCharStr := c.String("escape-char")
	if len(escapeCharStr) != 1 {
		return fmt.Errorf("--escape-char must be a single ASCII character, got %q", escapeCharStr)
	}
	escapeChar := escapeCharStr[0]

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

	// Propagate initial terminal size and handle SIGWINCH.
	cleanupSIGWINCH := handleSIGWINCH(os.Stdin, pty)
	defer cleanupSIGWINCH()

	// Print connection banner.
	fmt.Fprintf(os.Stderr, "Connected to %s (escape sequence: %s.)\r\n", ref, escapeCharStr)

	// Start bidirectional I/O relay.
	return relayConsole(c.Context, pty, escapeChar)
}

// escapeState tracks the escape sequence detection state machine.
type escapeState int

const (
	stateNormal    escapeState = iota
	stateLineStart             // After CR/LF or at session start — escape char is recognized here.
	stateEscaped               // Escape char received at line start.
)

// relayConsole runs bidirectional I/O between the user terminal and the PTY.
// It returns nil on clean disconnect (escape sequence or EOF).
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
	// NOTE: This goroutine may remain blocked on os.Stdin.Read() after
	// context cancellation. Go provides no portable way to interrupt a
	// blocking read on os.Stdin. Since this is a CLI command (the process
	// exits after return), the goroutine leak is benign.
	go func() {
		err := relayStdinToPTY(ctx, os.Stdin, pty, escapeChar)
		errCh <- err
		cancel()
	}()

	// Wait for either goroutine to finish. The second goroutine may outlive
	// this function: the stdin goroutine can block on os.Stdin.Read() (see
	// note above), and the PTY goroutine will unblock when pty.Close() runs
	// in consoleAction's defer. The errCh buffer of 2 prevents either
	// goroutine from deadlocking on its send.
	select {
	case <-ctx.Done():
		return nil
	case err := <-errCh:
		// EOF or clean disconnect is not an error.
		if err == nil || errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
}

// relayStdinToPTY reads from stdin and writes to the PTY, with SSH-style
// escape sequence detection. Returns nil on disconnect (~.).
func relayStdinToPTY(ctx context.Context, stdin io.Reader, pty io.Writer, escapeChar byte) error {
	state := stateLineStart // Start of session acts as start of line.
	// Single-byte reads are required for escape sequence detection.
	// This matches SSH client behavior (OpenSSH reads one byte at a time
	// in the escape-scanning path).
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
				state = stateLineStart // Allow immediate follow-up escape sequence.
				continue
			case escapeChar:
				// Send literal escape char.
				state = stateNormal
				if _, werr := pty.Write([]byte{escapeChar}); werr != nil {
					return werr
				}
			default:
				// Not a recognized sequence; forward both the escape char
				// and the current byte.
				state = stateNormal
				if _, werr := pty.Write([]byte{escapeChar, b}); werr != nil {
					return werr
				}
			}
		}
	}
}
