package workspacesession

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// The exec channel (decisions section 18.12). One project action, run once,
// non-interactively, on the workspace's worktree.
//
// It is deliberately not the terminal channel with a command pre-typed into it.
// A terminal is a PTY: it is interactive, it is recorded as an asciicast, its
// lifetime is a person's attention span, and section 18.3 gates it behind the
// grant's runners flag and an owner-only isolation choice because it hands the
// runner account's authority to a person. An action is the opposite of all
// four: the command is the project's, written down in advance and reviewable,
// it produces one output artifact with the issue's audience, it ends on its own
// and it may run with no person watching at all (run-on-worktree-creation). A
// channel of its own is what lets the gate, the recording and the lifetime
// differ without either surface having to ask which of the two it is.

// Frame types the exec channel defines. Anything else on this channel is
// answered with unknown_frame and dropped.
const (
	// TypeExecRun is person → runner: run this action's command once.
	TypeExecRun = "run"
	// TypeExecOutput is runner → person: one span of combined stdout and
	// stderr. Interleaving is the shell's, not the relay's; a reader sees what
	// a terminal would have shown.
	TypeExecOutput = "output"
	// TypeExecExited is runner → person: the process ended with this code. It
	// is the last frame of a run, and the hub reads the run's terminal status
	// from it.
	TypeExecExited = "exited"
)

// ExecRequestTypes lists what a person may send on the exec channel.
func ExecRequestTypes() []string { return []string{TypeExecRun} }

// ValidExecRequest reports whether type names an exec request.
func ValidExecRequest(value string) bool { return slices.Contains(ExecRequestTypes(), value) }

// Exec channel limits.
const (
	// MaxExecOutputBytes is the whole output a run may stream before the runner
	// truncates it. Section 18.12: the runner stops forwarding past the cap,
	// appends ExecTruncationMarker once, and lets the process run to completion
	// so the exit code still means what it says.
	MaxExecOutputBytes = 1 << 20
	// MaxExecCommandBytes bounds a command. A project action is a command line,
	// not a script file, and a bound well past any real one keeps a stored
	// action from becoming a memory cost on every listing.
	MaxExecCommandBytes = 4096
	// MaxExecOutputFrameBytes is the largest output span one frame carries. It
	// sits under MaxFrameBytes with room for the JSON envelope, so an output
	// frame never needs chunking.
	MaxExecOutputFrameBytes = 32 << 10
)

// ExecTruncationMarker is appended once, as its own output span, when a run
// passes MaxExecOutputBytes. It is a marker rather than a silent stop because a
// reader who cannot tell a finished log from a cut one will read the cut one as
// finished.
const ExecTruncationMarker = "\n[output truncated at 1 MiB]\n"

// ExecRun is the body of a run frame.
//
// RunID is what joins the stream to the run row the hub already created for
// this action (POST {nativeBase}/actions/:id/runs). Section 18.12 keeps the two
// steps separate — the row exists before the first frame, so a run that never
// reaches a runner is still visible as queued — and the id is what lets the hub
// record status from frames it is only relaying. ActionID and Command are
// carried too: the runner validates the command against the action it was
// handed rather than trusting one composed by a client.
type ExecRun struct {
	RunID    string `json:"run_id"`
	ActionID string `json:"action_id"`
	Command  string `json:"command"`
}

// ExecOutput is the body of an output frame. Data is a span of combined stdout
// and stderr; Encoding is "base64" when the span is not valid UTF-8 text, which
// a command that writes binary to its stdout will produce.
type ExecOutput struct {
	Data     string `json:"data"`
	Encoding string `json:"encoding,omitempty"`
	// Truncated marks the span that carries ExecTruncationMarker, so a client
	// can style it as the relay's word rather than the command's.
	Truncated bool `json:"truncated,omitempty"`
}

// ExecExited is the body of an exited frame. Code is the process exit status;
// Signal names the signal that killed it, when one did.
type ExecExited struct {
	Code   int    `json:"code"`
	Signal string `json:"signal,omitempty"`
}

// Run statuses (section 18.12). They are the whole lifecycle of one run:
// queued the moment the row is written, running on the first frame the runner
// sends, and then exactly one of succeeded or failed.
const (
	// RunQueued is a run recorded but not yet started by a runner.
	RunQueued = "queued"
	// RunRunning is a run whose runner has accepted the run frame.
	RunRunning = "running"
	// RunSucceeded is a run that exited zero.
	RunSucceeded = "succeeded"
	// RunFailed is a run that exited non-zero, was killed, or whose stream or
	// lease ended before it exited. A run whose outcome cannot be established
	// is failed rather than left running: section 5's rule for a control whose
	// outcome is unknown, applied to a command.
	RunFailed = "failed"
)

// RunStatuses lists every status a run may hold, in lifecycle order.
func RunStatuses() []string { return []string{RunQueued, RunRunning, RunSucceeded, RunFailed} }

// ValidRunStatus reports whether value names a run status.
func ValidRunStatus(value string) bool { return slices.Contains(RunStatuses(), value) }

// TerminalRunStatus reports whether a run in this status will never move again.
func TerminalRunStatus(value string) bool { return value == RunSucceeded || value == RunFailed }

// RunStatusForExit maps an exit code onto the run's terminal status.
func RunStatusForExit(code int) string {
	if code == 0 {
		return RunSucceeded
	}
	return RunFailed
}

// RunEventType is the project stream event one run transition emits:
// action_run.running, action_run.succeeded and so on. It rides the project
// event stream the way workspace.<state> does (section 18.1), so a client
// observes a run it did not start — a run-on-worktree-creation run, or one
// another tab began — by the same subscription and never by polling.
func RunEventType(status string) string { return "action_run." + status }

// Action is one stored project action (section 18.12). It is the resource the
// actions API serves and the record the runner is handed at bind time for the
// run-on-worktree-creation set.
type Action struct {
	ID             string `json:"id"`
	OrganizationID string `json:"organization_id,omitempty"`
	ProjectID      string `json:"project_id,omitempty"`
	Name           string `json:"name"`
	Command        string `json:"command"`
	Keybinding     string `json:"keybinding,omitempty"`
	Icon           string `json:"icon"`
	PreviewURL     string `json:"preview_url,omitempty"`
	// OpenPreview and RunOnWorktreeCreation are the dialog's two switches.
	// OpenPreview is stored and echoed only: the Browser surface is deferred
	// (section 18.7), so nothing acts on it yet and the client shows the
	// switch disabled with that reason rather than hiding the field.
	OpenPreview           bool      `json:"open_preview"`
	RunOnWorktreeCreation bool      `json:"run_on_worktree_creation"`
	CreatedBy             string    `json:"created_by"`
	Revision              int64     `json:"revision"`
	CreatedAt             time.Time `json:"created_at"`
	UpdatedAt             time.Time `json:"updated_at"`
}

// Run is one run of one action (section 18.12).
//
// ExitCode is a pointer because "exited 0" and "never exited" are different
// facts and a zero int cannot hold both: a run whose lease was lost has no exit
// code at all, and Reason is what says so.
type Run struct {
	ID          string     `json:"id"`
	ActionID    string     `json:"action_id"`
	WorkspaceID string     `json:"workspace_id"`
	Command     string     `json:"command"`
	Status      string     `json:"status"`
	ExitCode    *int       `json:"exit_code"`
	Reason      string     `json:"reason,omitempty"`
	StartedAt   *time.Time `json:"started_at"`
	FinishedAt  *time.Time `json:"finished_at"`
	// OutputArtifact names where the run's output may be read. It is empty
	// until there is output to read.
	OutputArtifact string    `json:"output_artifact,omitempty"`
	OutputBytes    int64     `json:"output_bytes"`
	Truncated      bool      `json:"truncated"`
	CreatedBy      string    `json:"created_by,omitempty"`
	Revision       int64     `json:"revision"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// RunReasonLeaseLost, RunReasonStreamClosed and RunReasonKilled are the reasons
// a run failed without exiting. A run that exited carries no reason: its exit
// code is the whole story.
const (
	RunReasonLeaseLost    = "lease_lost"
	RunReasonStreamClosed = "stream_closed"
	RunReasonKilled       = "killed"
	RunReasonWorkspaceEnd = "workspace_closed"
)

// ErrInvalidAction reports an action field the contract refuses.
var ErrInvalidAction = errors.New("workspacesession: invalid project action")

func ActionIcons() []string {
	return []string{"play", "test", "lint", "configure", "build", "debug"}
}

func NormalizeActionIcon(value string) (string, error) {
	trimmed := strings.ToLower(strings.TrimSpace(value))
	if trimmed == "" {
		return "play", nil
	}
	if !slices.Contains(ActionIcons(), trimmed) {
		return "", fmt.Errorf("%w: unknown icon %q", ErrInvalidAction, value)
	}
	return trimmed, nil
}

// Action field bounds. They exist so a stored action cannot be the reason a
// listing, a menu or a keybinding table is unbounded.
const (
	// MaxActionNameBytes bounds the display name the header button carries.
	MaxActionNameBytes = 80
	// MaxActionKeybindingBytes bounds a chord string ("mod+shift+t").
	MaxActionKeybindingBytes = 64
	// MaxActionPreviewURLBytes bounds the optional preview URL.
	MaxActionPreviewURLBytes = 2048
	// MaxActionsPerProject bounds how many actions one project may store. The
	// header menu lists every one of them, and a keybinding is registered per
	// action, so an unbounded set is an unbounded menu and an unbounded table.
	MaxActionsPerProject = 50
)

// NormalizeActionName trims an action name and applies its bounds. The name is
// what a person reads on the header button, so it may hold any printable text;
// only emptiness and length are refused.
func NormalizeActionName(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", fmt.Errorf("%w: a name is required", ErrInvalidAction)
	}
	if len(trimmed) > MaxActionNameBytes {
		return "", fmt.Errorf("%w: name exceeds %d bytes", ErrInvalidAction, MaxActionNameBytes)
	}
	if strings.ContainsRune(trimmed, 0) {
		return "", fmt.Errorf("%w: name contains a NUL byte", ErrInvalidAction)
	}
	return trimmed, nil
}

// NormalizeActionCommand trims a command and applies its bounds.
//
// It is deliberately not a validation of what the command does. The command
// runs through the project's configured shell, so any shell syntax is legal by
// construction, and a hub that tried to decide which commands are safe would
// be pretending to a boundary it does not have — the same admission section
// 18.3 makes about a terminal. What bounds the damage is who may write an
// action (write on the project) and where it runs (the worktree, under the
// workspace's isolation level), not a pattern match here.
func NormalizeActionCommand(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", fmt.Errorf("%w: a command is required", ErrInvalidAction)
	}
	if len(trimmed) > MaxExecCommandBytes {
		return "", fmt.Errorf("%w: command exceeds %d bytes", ErrInvalidAction, MaxExecCommandBytes)
	}
	if strings.ContainsRune(trimmed, 0) {
		return "", fmt.Errorf("%w: command contains a NUL byte", ErrInvalidAction)
	}
	return trimmed, nil
}

// NormalizeActionKeybinding trims a chord string and applies its bounds. An
// empty chord is legal and means the action has no shortcut; the client owns
// the chord's grammar, because the client is what resolves a keystroke against
// it (app/adapters/keybindings.ts).
func NormalizeActionKeybinding(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", nil
	}
	if len(trimmed) > MaxActionKeybindingBytes {
		return "", fmt.Errorf("%w: keybinding exceeds %d bytes", ErrInvalidAction, MaxActionKeybindingBytes)
	}
	if strings.ContainsAny(trimmed, "\x00 \t\r\n") {
		return "", fmt.Errorf("%w: keybinding must be a single chord", ErrInvalidAction)
	}
	return strings.ToLower(trimmed), nil
}

// NormalizeActionPreviewURL trims the optional preview URL and applies its
// bounds. It is stored and echoed, never fetched: section 18.7 defers the
// Browser surface, so nothing on either side opens this URL yet.
func NormalizeActionPreviewURL(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", nil
	}
	if len(trimmed) > MaxActionPreviewURLBytes {
		return "", fmt.Errorf("%w: preview_url exceeds %d bytes", ErrInvalidAction, MaxActionPreviewURLBytes)
	}
	if strings.ContainsAny(trimmed, "\x00 \t\r\n") {
		return "", fmt.Errorf("%w: preview_url must not contain whitespace", ErrInvalidAction)
	}
	if !strings.HasPrefix(trimmed, "http://") && !strings.HasPrefix(trimmed, "https://") {
		return "", fmt.Errorf("%w: preview_url must be http:// or https://", ErrInvalidAction)
	}
	return trimmed, nil
}
