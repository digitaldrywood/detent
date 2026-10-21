package workspacesession

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Asciicast v2, the format a terminal recording is stored in (decisions section
// 18.3).
//
// The format is one JSON object on the first line -- the header -- and then one
// JSON array per event, each `[seconds since the start, code, data]`. It is
// chosen rather than invented because it is what asciinema plays and what every
// terminal-recording tool already reads, so a recording pulled out of the hub is
// a file somebody can watch rather than a blob only this product understands.
//
// Three codes appear here. "o" is what the shell wrote, "i" is what the person
// typed, and "r" is a window resize carrying "<cols>x<rows>". Input is recorded
// and marked rather than dropped: section 18.3 says in so many words that
// recording cannot remove what the person typed, and a recording that quietly
// held only half the session would be a worse answer than one that says which
// half is which.

// Asciicast event codes.
const (
	// AsciicastOutput is a span the shell wrote.
	AsciicastOutput = "o"
	// AsciicastInput is a span the person typed.
	AsciicastInput = "i"
	// AsciicastResize is a window change, carrying "<cols>x<rows>".
	AsciicastResize = "r"
)

// AsciicastVersion is the format version the header declares.
const AsciicastVersion = 2

// MaxRecordingBytes bounds one stored recording.
//
// A terminal has no output cap on the wire, and it must not have one: a shell
// that stopped producing after a megabyte would be a terminal that stopped
// working, which is the difference between this surface and the exec channel's
// bounded log (section 18.12). So the cap lives on the stored copy instead, at
// the same 1 MiB an action's output keeps, and a recording that reaches it is
// marked truncated so a reader is told the copy is shorter than the session was
// rather than being shown a cut recording that looks complete.
const MaxRecordingBytes = 1 << 20

// AsciicastHeader is the first line of a recording.
type AsciicastHeader struct {
	Version int `json:"version"`
	Width   int `json:"width"`
	Height  int `json:"height"`
	// Timestamp is Unix seconds, which is what the format specifies.
	Timestamp int64 `json:"timestamp,omitempty"`
	// Env carries the terminal's own environment. Only TERM and SHELL are
	// conventional here, and only TERM is written: the shell is the project's
	// configured one and naming it in a recording whose audience is wider than
	// the person who ran it would disclose a fact about the runner that the
	// recording does not otherwise carry.
	Env map[string]string `json:"env,omitempty"`
}

// Recording builds an asciicast v2 document one event at a time.
//
// It is not safe for concurrent use; the relay holds one per stream under its
// own lock, which is where the frames for a stream are serialised anyway.
type Recording struct {
	builder strings.Builder
	started time.Time
	// truncated records that the cap was reached. Once it is set no further
	// event is written: a recording that resumed after a gap would play as
	// though the gap had not happened.
	truncated bool
	capped    bool
}

// NewRecording starts a recording with its header.
func NewRecording(cols, rows int, started time.Time) *Recording {
	recording := &Recording{started: started}
	header, err := json.Marshal(AsciicastHeader{
		Version: AsciicastVersion, Width: cols, Height: rows,
		Timestamp: started.Unix(), Env: map[string]string{"TERM": "xterm-256color"},
	})
	if err != nil {
		// The header is a struct of numbers and one fixed map, so a failure is
		// a programming error rather than a runtime condition. An empty
		// recording is the honest result: better a recording that is plainly
		// missing than one whose header lies about its size.
		return recording
	}
	recording.builder.Write(header)
	recording.builder.WriteByte('\n')
	return recording
}

// Append writes one event at the moment it happened.
func (r *Recording) Append(code string, data string, at time.Time) {
	if r == nil || r.capped {
		return
	}
	elapsed := at.Sub(r.started).Seconds()
	if elapsed < 0 {
		elapsed = 0
	}
	event, err := json.Marshal([]any{elapsed, code, data})
	if err != nil {
		return
	}
	if r.builder.Len()+len(event)+1 > MaxRecordingBytes {
		r.truncated = true
		r.capped = true
		return
	}
	r.builder.Write(event)
	r.builder.WriteByte('\n')
}

// AppendResize writes a window change.
func (r *Recording) AppendResize(cols, rows int, at time.Time) {
	r.Append(AsciicastResize, fmt.Sprintf("%dx%d", cols, rows), at)
}

// Cast reports the document as it stands.
func (r *Recording) Cast() string {
	if r == nil {
		return ""
	}
	return r.builder.String()
}

// Truncated reports whether the recording reached its cap.
func (r *Recording) Truncated() bool { return r != nil && r.truncated }

// Bytes reports the document's size.
func (r *Recording) Bytes() int {
	if r == nil {
		return 0
	}
	return r.builder.Len()
}
