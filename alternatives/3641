package workspacesession

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Relay frames (decisions section 18.2). One JSON object per WebSocket
// message, in both directions, on both the person and the runner side. The
// hub is the only writer of Actor: a client that supplies one has it
// overwritten, so a runner can trust the stamp.

// Channels a frame may name.
const (
	ChannelTerminal = "terminal"
	ChannelFiles    = "files"
	ChannelDiff     = "diff"
	ChannelPreview  = "preview"
	// ChannelExec runs one project action non-interactively (section 18.12).
	ChannelExec = "exec"
	// ChannelGit carries the header's git action group: the worktree's status,
	// a commit and a push (section 18.13). It is the only channel that writes
	// to the repository, which is why its frames need write on the project and
	// why a read-only workspace refuses two of its three types.
	ChannelGit = "git"
)

// ChannelNames lists every channel in its canonical order.
func ChannelNames() []string {
	return []string{ChannelTerminal, ChannelFiles, ChannelDiff, ChannelPreview, ChannelExec, ChannelGit}
}

// ValidChannel reports whether value names a channel.
func ValidChannel(value string) bool { return slices.Contains(ChannelNames(), value) }

// ChannelCapability maps a channel onto the capability a workspace must hold
// to serve it. They share a vocabulary on purpose: a surface enables from the
// capabilities the runner reported, and nothing else.
func ChannelCapability(channel string) string { return channel }

// Control frame types every channel shares.
const (
	// TypeAck acknowledges delivery through a sequence. Each side sends one
	// at least every AckEvery frames or AckInterval.
	TypeAck = "ack"
	// TypeResume asks to continue a stream on a new connection.
	TypeResume = "resume"
	// TypeResumed answers a successful resume with the sequence replay
	// continues from.
	TypeResumed = "resumed"
	// TypeClose ends one stream in either direction.
	TypeClose = "close"
	// TypeClosed reports that a stream has ended.
	TypeClosed = "closed"
	// TypeError carries one of the codes below.
	TypeError = "error"
	// TypeChunk carries one part of a payload larger than MaxFrameBytes.
	TypeChunk = "chunk"
	// TypeOpen opens a stream explicitly. Only the terminal channel needs it;
	// files, diff and preview open on their first request.
	TypeOpen = "open"
)

// Error codes a relay frame may carry. They are the relay's whole failure
// vocabulary: anything a handler cannot name with one of these is a bug.
const (
	// CodeUnknownFrame answers a type the channel does not define. The frame
	// is dropped and the stream survives.
	CodeUnknownFrame = "unknown_frame"
	// CodeStreamLimit answers an open beyond the per-connection or
	// per-workspace stream count.
	CodeStreamLimit = "stream_limit"
	// CodeOverflow closes a stream whose unacknowledged buffer passed
	// StreamBufferBytes.
	CodeOverflow = "overflow"
	// CodeRelayBusy answers a new stream when the hub process is at its relay
	// memory cap.
	CodeRelayBusy = "relay_busy"
	// CodeResumeFailed answers a resume outside the window, from the wrong
	// principal or session, or for a stream the hub no longer holds.
	CodeResumeFailed = "resume_failed"
	// CodeRevoked closes every connection of a person whose authority changed.
	CodeRevoked = "revoked"
	// CodeSuperseded closes the runner connection a second runner replaced.
	CodeSuperseded = "superseded"
	// CodeStaleExecution reports a frame that arrived after the workspace
	// lease was lost. The runner drops it; it is never executed.
	CodeStaleExecution = "stale_execution"
	// CodeUnknownDelivery reports a person-originated frame whose delivery
	// could not be established. It is never replayed, the same rule section 5
	// applies to controls.
	CodeUnknownDelivery = "unknown_delivery"
	// CodeNotFound, CodeForbidden, CodeTooLarge and CodeDenied are the files
	// and diff channel failures (section 18.4).
	CodeNotFound  = "not_found"
	CodeForbidden = "forbidden"
	CodeTooLarge  = "too_large"
	CodeDenied    = "denied"
	// CodeUnsupported answers a request the runner cannot serve, such as a
	// watch on a filesystem with no notification support.
	CodeUnsupported = "unsupported"
	// CodeInvalidFrame answers a frame whose payload does not parse or whose
	// fields are out of range.
	CodeInvalidFrame = "invalid_frame"
	// CodeReadOnly answers a write attempt on a workspace held read-only
	// while its attempt runs.
	CodeReadOnly = "read_only"
	// CodeWorkspaceClosed reports that the workspace itself ended, so no
	// stream on this connection can continue.
	CodeWorkspaceClosed = "workspace_closed"
	// CodeGitFailed reports a git command that ran and failed (section 18.13).
	// It is not a refusal: the request was allowed and git answered no. The
	// payload carries git's own stderr, because a person acting on a failed
	// push needs what git said rather than a paraphrase of it.
	CodeGitFailed = "git_failed"
	// CodeAlreadyRunning answers a run frame for a run another executor has
	// already taken (section 18.12). A queued run has two possible executors --
	// the person who opens the exec channel for it, and the hub handing it to
	// the runner that holds the workspace -- and exactly one of them may start
	// the process. The loser is told which refusal this is rather than a plain
	// forbidden, because the run is going to run: the answer is to read it
	// back through the run row, not to ask again.
	CodeAlreadyRunning = "already_running"
)

// Relay limits. Every one of them is a cap the hub enforces, so they live here
// rather than in a config: a client and a runner that disagreed about them
// would disagree about when a stream dies.
const (
	// MaxFrameBytes is the largest single frame. Bigger payloads are split
	// into chunk frames the receiver reassembles.
	MaxFrameBytes = 256 << 10
	// MaxStreamsPerConnection and MaxStreamsPerWorkspace bound stream
	// allocation. Beyond either, open fails with stream_limit.
	MaxStreamsPerConnection = 8
	MaxStreamsPerWorkspace  = 32
	// StreamBufferBytes is how much unacknowledged traffic the hub holds per
	// stream per direction before it closes the stream with overflow.
	StreamBufferBytes = 1 << 20
	// AckEvery and AckInterval are how often each side must acknowledge.
	AckEvery    = 32
	AckInterval = time.Second
	// ResumeWindow is how long a disconnected stream may be resumed, how long
	// the hub keeps its replay buffer, and how long the runner keeps a
	// disconnected PTY alive.
	ResumeWindow = 60 * time.Second
	// DefaultRelayMemoryBytes is workspaces.relay.memory's default: the whole
	// hub process's relay budget.
	DefaultRelayMemoryBytes = 256 << 20
	// AuthorityRecheckInterval is the periodic fallback for anything the
	// authority.changed event misses.
	AuthorityRecheckInterval = 30 * time.Second
	// TicketLifetime is how long a relay ticket may be redeemed.
	TicketLifetime = 30 * time.Second
	// TicketBytes is the ticket's randomness.
	TicketBytes = 32
	// HeartbeatInterval and MissedHeartbeats decide unreachability, and
	// LeaseTTL decides lease_lost (section 18.1).
	HeartbeatInterval = 30 * time.Second
	MissedHeartbeats  = 2
	LeaseTTL          = 90 * time.Second
	// RebindGrace is how long the original runner has to re-bind after a hub
	// restart before the hub revokes its lease and re-dispatches.
	RebindGrace = 30 * time.Second
)

// Actor is the hub's stamp on every person-originated frame. The runner reads
// it to audit what it was asked to do and by whom; it is never supplied by a
// client.
type Actor struct {
	PrincipalID  string `json:"principal_id"`
	Subject      string `json:"subject"`
	ConnectionID string `json:"connection_id"`
	// Name and Email are who a commit on the git channel is authored as
	// (section 18.13). The hub stamps them from the hosted principal for the
	// same reason it stamps the rest of the tuple: a client that could name
	// its own author could attribute a commit to anybody.
	Name  string `json:"name,omitempty"`
	Email string `json:"email,omitempty"`
}

// Frame is one relay message.
type Frame struct {
	Channel string `json:"channel"`
	Stream  string `json:"stream,omitempty"`
	Type    string `json:"type"`
	Seq     int64  `json:"seq,omitempty"`
	// Payload is the channel's own body, left raw so the hub forwards without
	// re-encoding what it does not interpret.
	Payload json.RawMessage `json:"payload,omitempty"`
	// Actor is stamped by the hub on the runner-bound copy only.
	Actor *Actor `json:"actor,omitempty"`
}

// ErrorPayload is the body of a TypeError frame.
type ErrorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
	// Seq names the frame the error is about, for unknown_delivery.
	Seq int64 `json:"seq,omitempty"`
	// Stderr is git's own output on a CodeGitFailed answer (section 18.13).
	// It rides on the shared error payload rather than on a shape of its own,
	// so every client keeps one error-handling path.
	Stderr string `json:"stderr,omitempty"`
}

// ErrorFrame builds a TypeError frame for one stream.
func ErrorFrame(channel, stream, code, message string) Frame {
	return Frame{Channel: channel, Stream: stream, Type: TypeError, Payload: mustEncode(ErrorPayload{Code: code, Message: message})}
}

// GitFailedFrame builds the git channel's CodeGitFailed answer, carrying what
// git wrote to stderr.
func GitFailedFrame(channel, stream, message, stderr string) Frame {
	return Frame{Channel: channel, Stream: stream, Type: TypeError,
		Payload: mustEncode(ErrorPayload{Code: CodeGitFailed, Message: message, Stderr: stderr})}
}

// AckPayload is the body of a TypeAck frame.
type AckPayload struct {
	Through int64 `json:"through"`
}

// ResumePayload is the body of a TypeResume frame.
type ResumePayload struct {
	Stream  string `json:"stream"`
	LastSeq int64  `json:"last_seq"`
}

// ChunkPayload carries one part of an oversized payload. The receiver
// reassembles parts 1..Parts in order and decodes the whole as the channel's
// own frame body.
type ChunkPayload struct {
	Part  int    `json:"part"`
	Parts int    `json:"parts"`
	Data  string `json:"data"`
	// Encoding is "base64" for binary content and empty for JSON text.
	Encoding string `json:"encoding,omitempty"`
}

// mustEncode marshals a payload the package itself built. Those payloads are
// plain structs of strings and numbers, so a failure is a programming error,
// not a runtime condition a caller could handle.
func mustEncode(value any) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic("workspacesession: encode payload: " + err.Error())
	}
	return encoded
}

// Encode wraps a channel payload for a frame.
func Encode(value any) (json.RawMessage, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("workspacesession: encode payload: %w", err)
	}
	return encoded, nil
}

// ErrInvalidFrame reports a frame the relay refuses to forward.
var ErrInvalidFrame = errors.New("workspacesession: invalid relay frame")

// StreamID is the hub's allocation shape: <connection_id>:<n>. A person
// connection owns its streams and only it sees their frames, which is why the
// connection is half the identifier rather than a lookup beside it.
func StreamID(connection string, n int) string {
	return connection + ":" + strconv.Itoa(n)
}

// ParseStreamID splits a stream id and reports the connection that owns it.
func ParseStreamID(value string) (connection string, n int, err error) {
	index := strings.LastIndex(value, ":")
	if index <= 0 || index == len(value)-1 {
		return "", 0, fmt.Errorf("%w: stream %q", ErrInvalidFrame, value)
	}
	n, convErr := strconv.Atoi(value[index+1:])
	if convErr != nil || n <= 0 {
		return "", 0, fmt.Errorf("%w: stream %q", ErrInvalidFrame, value)
	}
	return value[:index], n, nil
}

// ValidateFrame applies the shape rules the relay enforces before it looks at
// the channel's own vocabulary: a known channel, a type, a non-negative
// sequence, a parseable stream id and a payload inside the frame cap.
func ValidateFrame(frame Frame) error {
	if !ValidChannel(frame.Channel) {
		return fmt.Errorf("%w: unknown channel %q", ErrInvalidFrame, frame.Channel)
	}
	if strings.TrimSpace(frame.Type) == "" {
		return fmt.Errorf("%w: a frame type is required", ErrInvalidFrame)
	}
	if frame.Seq < 0 {
		return fmt.Errorf("%w: sequence must not be negative", ErrInvalidFrame)
	}
	if len(frame.Payload) > MaxFrameBytes {
		return fmt.Errorf("%w: payload exceeds %d bytes", ErrInvalidFrame, MaxFrameBytes)
	}
	if frame.Stream != "" {
		if _, _, err := ParseStreamID(frame.Stream); err != nil {
			return err
		}
	}
	return nil
}
