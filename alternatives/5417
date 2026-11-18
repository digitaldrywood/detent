package workspacerunner_test

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/workspacerunner"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

// The terminal channel is tested end to end through the relay for the reason
// the files and git channels are: what the session has to get right is the
// order of its gates and the shape of a stream that outlives the frame that
// opened it. An open refused on a read-only workspace, an input refused after a
// lost lease and a shell that ends with an exit frame followed by a closed one
// are all invisible from a method call.

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
