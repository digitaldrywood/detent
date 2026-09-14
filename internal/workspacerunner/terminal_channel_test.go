package workspacerunner_test

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/workspacerunner"
	"github.com/digitaldrywood/detent/internal/workspacesession"
	"github.com/digitaldrywood/detent/internal/workspaceterminal"
)

// The terminal channel is tested end to end through the relay for the reason
// the files and git channels are: what the session has to get right is the
// order of its gates and the shape of a stream that outlives the frame that
// opened it. An open refused on a read-only workspace, an input refused after a
// lost lease and a shell that ends with an exit frame followed by a closed one
// are all invisible from a method call.

// requireSessionPTY skips a terminal test on a build with no pseudo-terminal.
func requireSessionPTY(t *testing.T) {
	t.Helper()
	if !workspaceterminal.Supported {
		t.Skip("this platform has no pseudo-terminal")
	}
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("this platform has no /bin/sh to open a terminal on")
	}
}

// withTerminal is the fixture adjustment that turns the surface on, the way a
// runner whose operator allows terminals is configured.
func withTerminal(config *workspacerunner.Config) {
	config.Support = workspacerunner.Support{Terminal: true}
	config.Shell = "/bin/sh"
}

func terminalFrame(t *testing.T, kind, stream string, payload any) workspacesession.Frame {
	t.Helper()
	encoded, err := workspacesession.Encode(payload)
	if err != nil {
		t.Fatal(err)
	}
	return workspacesession.Frame{
		Channel: workspacesession.ChannelTerminal, Stream: stream, Type: kind, Payload: encoded,
	}
}

// awaitTerminalOutput reads frames until the decoded output carries want, or
// until one of the endings arrives. A shell writes its prompt, its echo of what
// was typed and its answer as however many spans it feels like, so a test that
// read one frame would be testing the scheduler.
func awaitTerminalOutput(t *testing.T, f *sessionFixture, want string) string {
	t.Helper()
	var seen strings.Builder
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		frame := f.receive(t)
		switch frame.Type {
		case workspacesession.TypeTerminalOutput:
			var span workspacesession.TerminalOutput
			if err := json.Unmarshal(frame.Payload, &span); err != nil {
				t.Fatal(err)
			}
			if span.Encoding != workspacesession.EncodingBase64 {
				t.Fatalf("output encoding = %q, want base64", span.Encoding)
			}
			decoded, err := base64.StdEncoding.DecodeString(span.Data)
			if err != nil {
				t.Fatalf("output data did not decode: %v", err)
			}
			seen.Write(decoded)
			if strings.Contains(seen.String(), want) {
				return seen.String()
			}
		case workspacesession.TypeTerminalExit, workspacesession.TypeClosed:
			t.Fatalf("the terminal ended before %q arrived; saw %q", want, seen.String())
		case workspacesession.TypeError:
			t.Fatalf("the terminal was refused: %s", string(frame.Payload))
		}
	}
	t.Fatalf("timed out waiting for %q; saw %q", want, seen.String())
	return ""
}

// openTerminal opens one PTY on a stream and returns the answer.
func openTerminal(t *testing.T, f *sessionFixture, stream string) workspacesession.TerminalOpened {
	t.Helper()
	f.send(t, terminalFrame(t, workspacesession.TypeTerminalOpen, stream,
		workspacesession.TerminalOpen{Cols: 80, Rows: 24}))
	answer := f.receive(t)
	if answer.Type != workspacesession.TypeTerminalOpened {
		t.Fatalf("open answered %+v (%s)", answer, string(answer.Payload))
	}
	if answer.Stream != stream {
		t.Fatalf("open answered stream %q, want %q", answer.Stream, stream)
	}
	var opened workspacesession.TerminalOpened
	if err := json.Unmarshal(answer.Payload, &opened); err != nil {
		t.Fatal(err)
	}
	if opened.PID <= 0 {
		t.Fatalf("opened = %+v, want a live process", opened)
	}
	if opened.Isolation != workspacesession.IsolationUser {
		t.Fatalf("opened.Isolation = %q, want user", opened.Isolation)
	}
	return opened
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
	// this worktree (section 18.12's note on the same seam for git). Both have
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

func TestSessionTerminalIsAbsentWithoutSupport(t *testing.T) {
	t.Parallel()

	f := startSession(t, nil)
	binds := f.hub.bindRequests()
	if len(binds) == 0 || binds[0].Capabilities.Terminal {
		t.Fatalf("bind capabilities = %+v, want no terminal from a runner that offers none", binds)
	}
	f.send(t, terminalFrame(t, workspacesession.TypeTerminalOpen, "conn:1",
		workspacesession.TerminalOpen{Cols: 80, Rows: 24}))
	answer := f.receive(t)
	// A runner with no terminal answers rather than ignoring the frame, so a
	// client that asked is told why instead of waiting for a shell that is
	// never coming.
	if code := errorCode(t, answer); code != workspacesession.CodeUnsupported {
		t.Fatalf("open answered %q, want %q", code, workspacesession.CodeUnsupported)
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

// awaitTerminalEnding reads until the exit frame and the closed frame that
// follows it have both arrived.
func awaitTerminalEnding(t *testing.T, f *sessionFixture) (workspacesession.TerminalExit, bool) {
	t.Helper()
	var exit workspacesession.TerminalExit
	sawExit := false
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		frame := f.receive(t)
		switch frame.Type {
		case workspacesession.TypeTerminalExit:
			if err := json.Unmarshal(frame.Payload, &exit); err != nil {
				t.Fatal(err)
			}
			sawExit = true
		case workspacesession.TypeClosed:
			if !sawExit {
				t.Fatal("the stream was closed before the exit frame")
			}
			return exit, true
		case workspacesession.TypeError:
			t.Fatalf("the terminal was refused: %s", string(frame.Payload))
		}
	}
	t.Fatal("the terminal never ended")
	return exit, false
}

func TestSessionTerminalCloseEndsTheShell(t *testing.T) {
	t.Parallel()
	requireSessionPTY(t)

	f := startSession(t, withTerminal)
	openTerminal(t, f, "conn:1")

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
	// A shell killed by a hangup has no exit code of its own; the signal is
	// what says how it ended.
	if exit.Code == 0 && exit.Signal == "" {
		t.Fatalf("exit = %+v, want a signal or a non-zero code for a hung-up shell", exit)
	}
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
			// Section 18.1: a workspace on an attempt that is still running is
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

// TestSessionRefusesATerminalAfterLeaseLoss is section 18.2's rule applied to
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
	// open (section 18.1).
	beats := f.hub.heartbeatRequests()
	if len(beats) == 0 {
		t.Fatal("the session never heartbeat")
	}
	if beats[0].Capabilities.Terminal {
		t.Fatalf("heartbeat capabilities = %+v, want no terminal on a read-only workspace", beats[0].Capabilities)
	}
}

// errorCode reads the code off an error frame.
func errorCode(t *testing.T, frame workspacesession.Frame) string {
	t.Helper()
	if frame.Type != workspacesession.TypeError {
		return ""
	}
	var payload workspacesession.ErrorPayload
	if err := json.Unmarshal(frame.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	return payload.Code
}
