package cli

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/charliek/craze/internal/rundir"
)

// craze bridge is the far side of an SSH transport for session control
// (plan 027 §3.10, SD-05): a device execs it once per accepted connection,
// and it dials this machine's control socket and pumps bytes between it and
// its own stdio, so the device's local connection reaches the session with no
// remote socket path of its own to manage. It speaks no protocol itself —
// hello is the client's — and never parses a line.
//
// Every failure honours one contract, flag parsing and the root's
// PersistentPreRunE (a stray removed config-file variable in an SSH environment) included: one
// "craze bridge: " line on stderr, exit 1 — never 2, ssh's own 255 never —
// and nothing on stdout. bridge.go's own errors are already in that shape
// (bridgeErrorf); Execute (root.go) maps whatever else reaches it once the
// ran command is bridge's (diagnose), so a usage error from the shared
// PersistentPreRunE reads as a bridge error too, without bridge defining a
// PersistentPreRunE of its own (root.go: no subcommand may).

// bridgePrefix is craze bridge's one stderr line, on every failure.
const bridgePrefix = "craze bridge: "

// bridgeErrorf is a craze bridge failure: exit 1, one bridgePrefix line. It
// builds the message itself, rather than through exitf, so a "%" that a
// wrapped error's own text happens to carry (a path, another program's
// message) is never fed back through Sprintf as a verb.
func bridgeErrorf(format string, args ...any) error {
	return &exitError{code: 1, msg: bridgePrefix + fmt.Sprintf(format, args...)}
}

func newBridgeCmd() *cobra.Command {
	var session string
	cmd := &cobra.Command{
		Use:   "bridge",
		Short: "Pump bytes between stdin/stdout and a running craze session's control socket",
		Long: "craze bridge is a pure byte pump for an SSH client (plan 027 §3.10): it " +
			"resolves the one running craze session (or --session's), dials its control " +
			"socket, and relays stdin/stdout to it verbatim. It speaks no protocol itself.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runBridge(cmd, session)
		},
	}
	cmd.Flags().StringVar(&session, "session", "",
		"a craze session id, provider session id, or host id (default: the one running session)")
	return cmd
}

// runBridge is craze bridge's whole job: resolve the target session, dial its
// socket, check its peer, and pump. Every error it returns is already
// bridgeErrorf's shape.
func runBridge(cmd *cobra.Command, session string) error {
	if session != "" && !rundir.ValidToken(session) {
		// Refused before any registry read builds a path (§3.10).
		return bridgeErrorf("--session %q is not a valid session id", session)
	}
	entries, err := rundir.Hosts(rundir.ProcessEnv())
	if err != nil {
		return bridgeErrorf("%v", err)
	}
	entry, err := resolveTarget(entries, session)
	if err != nil {
		return err
	}
	return dialAndPump(entryID(entry), entry.Socket, cmd.InOrStdin(), cmd.OutOrStdout(),
		rundir.DialCheck(os.Geteuid()))
}

// entryID is what names entry in a message: its craze session id, or its
// host id while that is not yet known (before the engine is ready).
func entryID(e rundir.Entry) string {
	if e.CrazeSessionID != "" {
		return e.CrazeSessionID
	}
	return e.HostID
}

// resolveTarget picks the one live entry to bridge to (§3.10):
//
//   - With no --session, exactly one live host in total is the target; zero
//     or several is an error listing them on the one line.
//   - With --session, the live entries whose crazeSessionId, providerSessionId
//     or hostId equals it exactly; none is the contract line "no session
//     <id>"; more than one (ids do not collide in practice, but nothing here
//     assumes it) is an error naming them.
func resolveTarget(entries []rundir.Entry, session string) (rundir.Entry, error) {
	if session == "" {
		switch len(entries) {
		case 0:
			return rundir.Entry{}, bridgeErrorf("no session running")
		case 1:
			return entries[0], nil
		default:
			return rundir.Entry{}, bridgeErrorf("%d sessions running; pass --session <id>: %s",
				len(entries), formatEntries(entries))
		}
	}
	var matches []rundir.Entry
	for _, e := range entries {
		if e.CrazeSessionID == session || e.ProviderSessionID == session || e.HostID == session {
			matches = append(matches, e)
		}
	}
	switch len(matches) {
	case 0:
		return rundir.Entry{}, bridgeErrorf("no session %s", session)
	case 1:
		return matches[0], nil
	default:
		return rundir.Entry{}, bridgeErrorf("%d sessions match --session %s: %s",
			len(matches), session, formatEntries(matches))
	}
}

// formatEntries is several entries named on the one line every bridge error
// keeps to: "<id> (<provider>, <workspace>), <id> (<provider>, <workspace>)".
func formatEntries(entries []rundir.Entry) string {
	parts := make([]string, len(entries))
	for i, e := range entries {
		parts[i] = fmt.Sprintf("%s (%s, %s)", entryID(e), e.Provider, e.Workspace)
	}
	return strings.Join(parts, ", ")
}

// dialAndPump dials socket, checks its peer before a byte is written
// (peerCheck; rundir.DialCheck(os.Geteuid()) in production, so a test can
// inject one that refuses), and pumps stdin/stdout against it. id names the
// session in an unreachable error: the socket gone is R10, a real risk once
// logind or systemd-tmpfiles has cleared the runtime directory.
func dialAndPump(id, socket string, stdin io.Reader, stdout io.Writer, peerCheck func(*net.UnixConn) error) error {
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		return bridgeErrorf("session %s is unreachable: %v", id, err)
	}
	if err := peerCheck(conn); err != nil {
		_ = conn.Close()
		return bridgeErrorf("session %s is unreachable: %v", id, err)
	}
	if err := pump(stdin, stdout, conn); err != nil {
		return bridgeErrorf("%v", err)
	}
	return nil
}

// pumpChunk is one read's worth of wire bytes: large enough that a snapshot
// stream is not chopped into a syscall per frame (roost's own CHUNK,
// bridge.rs:38).
const pumpChunk = 64 << 10

// pump is craze bridge's whole job once dialed (§3.10, roost's bridge.rs:91-
// 152): stdin -> conn and conn -> stdout, verbatim, with no framing and
// nothing ever buffered across a read.
//
// conn -> stdout owns the exit: a clean read there (EOF) ends the pump
// successfully at once, whatever stdin is doing; a read error there, or a
// stdout write error (EPIPE: the SSH channel is gone), ends it with that
// error. stdin -> conn's clean end (EOF) is only a half-close (CloseWrite):
// the session may still have plenty to send, so the pump keeps reading conn.
// An error there — a read off stdin, or a write to conn — ends the pump at
// once with that error, without waiting for conn to end on its own, mirroring
// roost's select (bridge.rs:93-108).
func pump(stdin io.Reader, stdout io.Writer, conn *net.UnixConn) error {
	ignoreSIGPIPE()
	downCh := make(chan error, 1)
	upCh := make(chan error, 1)
	go func() { downCh <- copyToStdout(conn, stdout) }()
	go func() { upCh <- copyFromStdin(stdin, conn) }()

	select {
	case err := <-downCh:
		return err
	case err := <-upCh:
		if err != nil {
			return err
		}
		// stdin's clean EOF: the half-close is already done. Keep waiting on
		// the socket, which owns the exit.
		return <-downCh
	}
}

// copyToStdout is the pump's conn -> stdout direction: each read written to
// stdout at once, never accumulated across reads.
func copyToStdout(conn *net.UnixConn, stdout io.Writer) error {
	buf := make([]byte, pumpChunk)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			if _, werr := stdout.Write(buf[:n]); werr != nil {
				return fmt.Errorf("write stdout: %w", werr)
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("read the session socket: %w", err)
		}
	}
}

// copyFromStdin is the pump's stdin -> conn direction: each read written to
// conn at once. stdin's EOF half-closes conn's write side and returns —
// never a fatal end of the pump, only of this direction.
func copyFromStdin(stdin io.Reader, conn *net.UnixConn) error {
	buf := make([]byte, pumpChunk)
	for {
		n, err := stdin.Read(buf)
		if n > 0 {
			if _, werr := conn.Write(buf[:n]); werr != nil {
				return fmt.Errorf("write to the session socket: %w", werr)
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				if cerr := conn.CloseWrite(); cerr != nil {
					return fmt.Errorf("half-close the session socket: %w", cerr)
				}
				return nil
			}
			return fmt.Errorf("read stdin: %w", err)
		}
	}
}

// ignoreSIGPIPEOnce guards the one process-wide Notify (Do runs its function
// at most once, ever, regardless of how many times pump runs).
var ignoreSIGPIPEOnce sync.Once

// ignoreSIGPIPE makes a write to stdout return syscall.EPIPE instead of
// killing the process with the signal (§3.10): by default Go raises SIGPIPE's
// own action — process death — for a write to a broken pipe on fd 1 or 2
// (an SSH channel gone out from under the bridge), unless something is
// notified for it; the channel is never read, since the signal needs
// somewhere to go and nothing more.
func ignoreSIGPIPE() {
	ignoreSIGPIPEOnce.Do(func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGPIPE)
	})
}
