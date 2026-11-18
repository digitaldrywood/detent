//go:build unix

package workspacerunner_test

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/workspacerunner"
	"github.com/digitaldrywood/detent/internal/workspacesession"
	"github.com/digitaldrywood/detent/internal/workspaceterminal"
)

func requireSessionPTY(t *testing.T) {
	t.Helper()
	if !workspaceterminal.Supported {
		t.Fatal("Unix terminal fixture requires pseudo-terminal support")
	}
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Fatalf("Unix terminal fixture requires /bin/sh: %v", err)
	}
}

func TestSessionServesTheTerminalChannel(t *testing.T) {
	t.Parallel()
	requireSessionPTY(t)

	f := startSession(t, withTerminal)
	openTerminal(t, f, "conn:1")

	f.send(t, terminalFrame(t, workspacesession.TypeTerminalInput, "conn:1",
		workspacesession.TerminalInput{Data: "echo detent-relay-hi\n"}))
	awaitTerminalOutput(t, f, "detent-relay-hi")
}

func TestSessionTerminalReportsTheCapability(t *testing.T) {
	t.Parallel()
	requireSessionPTY(t)

	f := startSession(t, withTerminal)
	// The bind carries the build's answer and the first heartbeat narrows it to
	// this worktree (the project actions contract's note on the same seam for git). Both have
	// to say terminal, or the hub would either never offer this runner a
	// terminal workspace or would offer one whose resource claims a surface the
	// worktree cannot serve.
	binds := f.hub.bindRequests()
	if len(binds) == 0 || !binds[0].Capabilities.Terminal {
		t.Fatalf("bind capabilities = %+v, want the terminal reported", binds)
	}
	beats := f.hub.heartbeatRequests()
	if len(beats) == 0 || !beats[0].Capabilities.Terminal {
		t.Fatalf("first heartbeat capabilities = %+v, want the terminal reported", beats)
	}
}

func TestSessionTerminalResize(t *testing.T) {
	t.Parallel()
	requireSessionPTY(t)

	f := startSession(t, withTerminal)
	openTerminal(t, f, "conn:1")

	f.send(t, terminalFrame(t, workspacesession.TypeTerminalResize, "conn:1",
		workspacesession.TerminalResize{Cols: 132, Rows: 43}))
	// A resize is answered with nothing, so the proof it happened is the shell
	// reading the window back out of the kernel.
	f.send(t, terminalFrame(t, workspacesession.TypeTerminalInput, "conn:1",
		workspacesession.TerminalInput{Data: "stty size\n"}))
	awaitTerminalOutput(t, f, "43 132")
}

func TestSessionTerminalExitIsFollowedByClosed(t *testing.T) {
	t.Parallel()
	requireSessionPTY(t)

	f := startSession(t, withTerminal)
	openTerminal(t, f, "conn:1")

	f.send(t, terminalFrame(t, workspacesession.TypeTerminalInput, "conn:1",
		workspacesession.TerminalInput{Data: "exit 5\n"}))

	exit, closed := awaitTerminalEnding(t, f)
	if exit.Code != 5 {
		t.Fatalf("exit = %+v, want code 5", exit)
	}
	// The hub releases a stream's slot on close or closed and never on exit, so
	// a runner that stopped at the exit frame would leak one slot per terminal.
	if !closed {
		t.Fatal("the terminal's stream was never given back with a closed frame")
	}
}

func TestSessionTerminalCloseEndsTheShell(t *testing.T) {
	t.Parallel()
	requireSessionPTY(t)

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	shell := filepath.Join(t.TempDir(), "shell")
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }
	script := "#!/bin/sh\nGOCOVERDIR=" + quote(t.TempDir()) + " exec " + quote(executable) + " -test.run='^TestSessionTerminalHangupHelper$' -- detent-terminal-hangup-helper\n"
	if err := os.WriteFile(shell, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	f := startSession(t, func(config *workspacerunner.Config) {
		withTerminal(config)
		config.Shell = shell
	})
	openTerminal(t, f, "conn:1")
	awaitTerminalOutput(t, f, "terminal-ready")

	// A close is what a person's own close carries and what the hub's sweep
	// sends once a parked stream has gone unresumed for the window. Neither is
	// distinguishable from the runner, and neither should be.
	f.send(t, workspacesession.Frame{
		Channel: workspacesession.ChannelTerminal, Stream: "conn:1", Type: workspacesession.TypeClose,
	})
	exit, closed := awaitTerminalEnding(t, f)
	if !closed {
		t.Fatal("a closed terminal never gave its stream back")
	}
	if exit.Code != 129 || exit.Signal != "" {
		t.Fatalf("exit = %+v, want the shell's hangup handler to exit 129", exit)
	}
}

func TestSessionTerminalHangupHelper(t *testing.T) {
	if len(os.Args) == 0 || os.Args[len(os.Args)-1] != "detent-terminal-hangup-helper" {
		return
	}
	hangups := make(chan os.Signal, 1)
	signal.Notify(hangups, syscall.SIGHUP)
	if _, err := fmt.Fprintln(os.Stdout, "terminal-ready"); err != nil {
		t.Fatal(err)
	}
	<-hangups
	os.Exit(129)
}

func TestSessionTerminalRefusals(t *testing.T) {
	t.Parallel()
	requireSessionPTY(t)

	tests := []struct {
		name     string
		readOnly bool
		frame    func(t *testing.T) workspacesession.Frame
		wantCode string
	}{
		{
			name: "a read-only workspace refuses an open",
			// The workspace session contract: a workspace on an attempt that is still running is
			// read-only and refuses a terminal, so a person cannot type into a
			// worktree the model is editing.
			readOnly: true,
			frame: func(t *testing.T) workspacesession.Frame {
				return terminalFrame(t, workspacesession.TypeTerminalOpen, "conn:1",
					workspacesession.TerminalOpen{Cols: 80, Rows: 24})
			},
			wantCode: workspacesession.CodeReadOnly,
		},
		{
			name: "a read-only workspace refuses input too",
			// A terminal is a write in its entirety; there is no read-only half
			// of typing into a shell.
			readOnly: true,
			frame: func(t *testing.T) workspacesession.Frame {
				return terminalFrame(t, workspacesession.TypeTerminalInput, "conn:1",
					workspacesession.TerminalInput{Data: "x"})
			},
			wantCode: workspacesession.CodeReadOnly,
		},
		{
			name: "a type the channel does not define is unknown",
			frame: func(t *testing.T) workspacesession.Frame {
				return terminalFrame(t, "detach", "conn:1", map[string]any{})
			},
			wantCode: workspacesession.CodeUnknownFrame,
		},
		{
			name: "input on a stream with no terminal is not found",
			frame: func(t *testing.T) workspacesession.Frame {
				return terminalFrame(t, workspacesession.TypeTerminalInput, "conn:9",
					workspacesession.TerminalInput{Data: "x"})
			},
			wantCode: workspacesession.CodeNotFound,
		},
		{
			name: "a resize on a stream with no terminal is not found",
			frame: func(t *testing.T) workspacesession.Frame {
				return terminalFrame(t, workspacesession.TypeTerminalResize, "conn:9",
					workspacesession.TerminalResize{Cols: 80, Rows: 24})
			},
			wantCode: workspacesession.CodeNotFound,
		},
		{
			name: "an illegal window is refused before a shell exists",
			frame: func(t *testing.T) workspacesession.Frame {
				return terminalFrame(t, workspacesession.TypeTerminalOpen, "conn:1",
					workspacesession.TerminalOpen{Cols: 99999, Rows: 24})
			},
			wantCode: workspacesession.CodeInvalidFrame,
		},
		{
			name: "input marked base64 that does not decode is refused",
			frame: func(t *testing.T) workspacesession.Frame {
				return terminalFrame(t, workspacesession.TypeTerminalInput, "conn:1",
					workspacesession.TerminalInput{Data: "not base64!!", Encoding: workspacesession.EncodingBase64})
			},
			wantCode: workspacesession.CodeInvalidFrame,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f := startSessionWith(t, worktreeWith(t), func(checkout *hubclient.WorkspaceCheckout) {
				checkout.ReadOnly = tt.readOnly
			}, withTerminal)
			f.send(t, tt.frame(t))
			answer := f.receive(t)
			if code := errorCode(t, answer); code != tt.wantCode {
				t.Fatalf("answered %q (%s), want %q", code, string(answer.Payload), tt.wantCode)
			}
		})
	}
}

// TestSessionRefusesATerminalAfterLeaseLoss is the relay protocol's rule applied to
// the longest-lived thing on this surface. The lease is validated immediately
// before every frame is acted on, so an open under a lost lease never becomes a
// shell and a keystroke under one never reaches the worktree.
func TestSessionRefusesATerminalAfterLeaseLoss(t *testing.T) {
	t.Parallel()
	requireSessionPTY(t)

	expired := time.Now()
	f := startSession(t, func(config *workspacerunner.Config) {
		withTerminal(config)
		config.Now = func() time.Time { return expired }
	})
	expired = expired.Add(workspacesession.LeaseTTL + time.Minute)

	f.send(t, terminalFrame(t, workspacesession.TypeTerminalOpen, "conn:1",
		workspacesession.TerminalOpen{Cols: 80, Rows: 24}))
	answer := f.receive(t)
	if code := errorCode(t, answer); code != workspacesession.CodeStaleExecution {
		t.Fatalf("an open after lease loss answered %q (%s), want stale_execution", code, string(answer.Payload))
	}
	f.send(t, terminalFrame(t, workspacesession.TypeTerminalInput, "conn:1",
		workspacesession.TerminalInput{Data: "echo never\n"}))
	answer = f.receive(t)
	if code := errorCode(t, answer); code != workspacesession.CodeStaleExecution {
		t.Fatalf("input after lease loss answered %q (%s), want stale_execution", code, string(answer.Payload))
	}
}

func TestSessionTerminalIsAbsentOnAReadOnlyWorkspace(t *testing.T) {
	t.Parallel()
	requireSessionPTY(t)

	f := startSessionWith(t, worktreeWith(t), func(checkout *hubclient.WorkspaceCheckout) {
		checkout.ReadOnly = true
	}, withTerminal)
	// A read-only workspace never opens a terminal service at all, so the
	// capability it reports is the honest one: the card is disabled with a
	// reason rather than enabled against a surface that would refuse every
	// open.
	beats := f.hub.heartbeatRequests()
	if len(beats) == 0 {
		t.Fatal("the session never heartbeat")
	}
	if beats[0].Capabilities.Terminal {
		t.Fatalf("heartbeat capabilities = %+v, want no terminal on a read-only workspace", beats[0].Capabilities)
	}
}

// TestSessionTerminalOutputSurvivesARelayRedial drops the runner's relay socket
// while the shell is producing output and expects that output, and output
// produced after the redial, to arrive on the new socket.
func TestSessionTerminalOutputSurvivesARelayRedial(t *testing.T) {
	t.Parallel()
	requireSessionPTY(t)

	f := startSession(t, withTerminal)
	openTerminal(t, f, "conn:1")
	f.send(t, terminalFrame(t, workspacesession.TypeTerminalInput, "conn:1", workspacesession.TerminalInput{
		Data: "sleep 1; for i in 1 2 3; do echo detent-held-$i; done; sleep 4; echo detent-after-$((40+2))\n",
	}))
	if err := f.socket.Close(websocket.StatusGoingAway, "hub restarted"); err != nil {
		t.Fatalf("close relay socket: %v", err)
	}
	f.socket = f.hub.waitForSocket()
	seen := awaitTerminalOutput(t, f, "detent-after-42")
	if !strings.Contains(seen, "detent-held-3") {
		t.Fatalf("output produced while the socket was down was lost; saw %q", seen)
	}
}
