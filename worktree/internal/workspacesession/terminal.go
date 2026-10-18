package workspacesession

import (
	"fmt"
	"slices"
	"strings"
)

// The terminal channel (decisions section 18.3). One PTY on the runner, held
// open for as long as a person is looking at it.
//
// It is the opposite of the exec channel (18.12) in every dimension that
// matters to a gate: it is interactive, it is a shell rather than a command the
// project wrote down, it is recorded as an asciicast whose audience is narrower
// than the issue's, its lifetime is a person's attention span, and it hands the
// runner account's authority to a person. That is why it is gated behind the
// grant's runners flag, workspaces.terminal.enabled and an isolation choice the
// organization makes, and why every open allocates a stream of its own: a
// terminal stream is never shared between connections, so a second tab that
// wants the same shell opens its own and gets its own PTY (18.2).
//
// What a terminal is worth as a boundary is written down in section 18.3 in so
// many words, and it is repeated here rather than softened: stripping
// environment variables does not confine a shell. At `user` isolation the
// process can read the runner's provider login files, its credential store,
// other checkouts and anything else that user can read. The scrub this package
// describes is a courtesy. The boundary is who may open one.

// Frame types the terminal channel defines. Anything else on this channel is
// answered with unknown_frame and dropped.
//
// TypeOpen is the shared control the relay already defines, because a terminal
// is the one channel that opens explicitly; it is named here as well so the
// channel's vocabulary reads as one table.
const (
	// Person -> runner.
	TypeTerminalOpen   = TypeOpen
	TypeTerminalInput  = "input"
	TypeTerminalResize = "resize"
	// Runner -> person.
	TypeTerminalOpened = "opened"
	TypeTerminalOutput = "output"
	TypeTerminalExit   = "exit"
)

// TerminalRequestTypes lists what a person may send on the terminal channel.
// close is a shared control rather than a channel frame, so it is not here: the
// relay answers it for every channel alike.
func TerminalRequestTypes() []string {
	return []string{TypeTerminalOpen, TypeTerminalInput, TypeTerminalResize}
}

// ValidTerminalRequest reports whether value names a terminal request.
func ValidTerminalRequest(value string) bool { return slices.Contains(TerminalRequestTypes(), value) }

// Terminal channel limits.
const (
	// MinTerminalCols and MinTerminalRows are the smallest window a PTY is
	// allocated with. A zero either way is a client that has not measured its
	// container yet, and a zero-width terminal renders nothing at all, so it is
	// refused rather than allocated and left looking broken.
	MinTerminalCols = 1
	MinTerminalRows = 1
	// MaxTerminalCols and MaxTerminalRows bound a window. A winsize is two
	// unsigned 16-bit fields, so anything past this could not be set anyway;
	// the bound is well past any real display and stops a resize from being a
	// way to ask the kernel for something absurd.
	MaxTerminalCols = 2000
	MaxTerminalRows = 2000
	// DefaultTerminalCols and DefaultTerminalRows are the window a PTY takes
	// when an open carries none. They are the conventional terminal size, which
	// is what a program reading COLUMNS and LINES expects to find when nobody
	// said otherwise.
	DefaultTerminalCols = 80
	DefaultTerminalRows = 24
	// MaxTerminalInputBytes bounds one input frame. It is far past a keystroke
	// and comfortably past a pasted paragraph, and it sits well under
	// MaxFrameBytes so an input frame never needs chunking.
	MaxTerminalInputBytes = 64 << 10
	// MaxTerminalOutputFrameBytes is the largest raw span one output frame
	// carries. Output is base64 (below), which costs four bytes for every
	// three, so this bound keeps an encoded frame inside MaxFrameBytes with
	// room for the envelope and no chunking on the hot path.
	MaxTerminalOutputFrameBytes = 32 << 10
	// MaxTerminalCwdBytes bounds the optional working directory an open names.
	MaxTerminalCwdBytes = 1024
)

// TerminalOpen is the body of an open frame.
//
// Cwd is relative to the worktree and optional; the shell is the runner's
// choice, which is section 18.3's own wording, so a client does not name one.
// A client that could choose the shell could choose an interpreter the project
// never configured, and the project's configured shell is the one its actions
// and its hooks already run through.
type TerminalOpen struct {
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
	Cwd  string `json:"cwd,omitempty"`
}

// TerminalOpened answers an open frame.
type TerminalOpened struct {
	PID int `json:"pid"`
	// Isolation is the level this PTY actually runs at, which a client shows
	// rather than assumes: an organization that asked for container and a
	// runner that can only offer user must not look the same.
	Isolation string `json:"isolation"`
	Cols      int    `json:"cols"`
	Rows      int    `json:"rows"`
}

// TerminalInput is the body of an input frame. Data is what the person typed.
// Encoding is "base64" when the client had bytes rather than text to send,
// which a paste of binary content produces; it is empty for ordinary typing,
// where JSON's own escaping already carries every control character a keyboard
// can produce.
type TerminalInput struct {
	Data     string `json:"data"`
	Encoding string `json:"encoding,omitempty"`
}

// TerminalResize is the body of a resize frame.
type TerminalResize struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

// TerminalOutput is the body of an output frame.
//
// It is always base64, unlike the exec channel's output, and the difference is
// deliberate. A PTY's output is not text: it is text interleaved with escape
// sequences, and a span cut at a frame boundary routinely ends in the middle of
// one. Sending it as JSON text would mean deciding what to do with a byte
// sequence that is not valid UTF-8 on every single span, at the rate a shell
// produces them, and a terminal emulator wants the bytes rather than a repaired
// approximation of them.
type TerminalOutput struct {
	Data     string `json:"data"`
	Encoding string `json:"encoding"`
}

// TerminalExit is the body of an exit frame: how the shell ended. Code is the
// process's own status, or -1 when a signal ended it instead, which is what
// Signal names.
type TerminalExit struct {
	Code   int    `json:"code"`
	Signal string `json:"signal,omitempty"`
}

// NormalizeTerminalSize applies the window bounds, defaulting a size nobody
// gave. It is shared by open and resize so the two cannot disagree about what a
// legal window is.
func NormalizeTerminalSize(cols, rows int) (int, int, error) {
	if cols == 0 && rows == 0 {
		return DefaultTerminalCols, DefaultTerminalRows, nil
	}
	if cols < MinTerminalCols || cols > MaxTerminalCols {
		return 0, 0, fmt.Errorf("%w: cols must be between %d and %d", ErrInvalidFrame, MinTerminalCols, MaxTerminalCols)
	}
	if rows < MinTerminalRows || rows > MaxTerminalRows {
		return 0, 0, fmt.Errorf("%w: rows must be between %d and %d", ErrInvalidFrame, MinTerminalRows, MaxTerminalRows)
	}
	return cols, rows, nil
}

// ValidateTerminalOpen applies the shape rules for an open frame and answers
// with the window the PTY is allocated at.
func ValidateTerminalOpen(request TerminalOpen) (TerminalOpen, error) {
	cols, rows, err := NormalizeTerminalSize(request.Cols, request.Rows)
	if err != nil {
		return TerminalOpen{}, err
	}
	cwd := strings.TrimSpace(request.Cwd)
	if len(cwd) > MaxTerminalCwdBytes {
		return TerminalOpen{}, fmt.Errorf("%w: cwd exceeds %d bytes", ErrInvalidFrame, MaxTerminalCwdBytes)
	}
	if strings.ContainsRune(cwd, 0) {
		return TerminalOpen{}, fmt.Errorf("%w: cwd contains a NUL byte", ErrInvalidFrame)
	}
	return TerminalOpen{Cols: cols, Rows: rows, Cwd: cwd}, nil
}

// ValidateTerminalResize applies the shape rules for a resize frame. A resize
// carrying neither dimension is refused rather than defaulted: an open with no
// size is a client that has not measured yet, and a resize with no size is a
// client sending nothing at all.
func ValidateTerminalResize(request TerminalResize) (TerminalResize, error) {
	if request.Cols == 0 && request.Rows == 0 {
		return TerminalResize{}, fmt.Errorf("%w: a resize must name cols and rows", ErrInvalidFrame)
	}
	cols, rows, err := NormalizeTerminalSize(request.Cols, request.Rows)
	if err != nil {
		return TerminalResize{}, err
	}
	return TerminalResize{Cols: cols, Rows: rows}, nil
}

// ValidateTerminalInput applies the bound on one input frame.
func ValidateTerminalInput(request TerminalInput) error {
	if len(request.Data) > MaxTerminalInputBytes {
		return fmt.Errorf("%w: input exceeds %d bytes", ErrInvalidFrame, MaxTerminalInputBytes)
	}
	if request.Encoding != "" && request.Encoding != EncodingBase64 {
		return fmt.Errorf("%w: unknown input encoding %q", ErrInvalidFrame, request.Encoding)
	}
	return nil
}

// EncodingBase64 is the one encoding a relay payload may name (section 18.2).
const EncodingBase64 = "base64"
