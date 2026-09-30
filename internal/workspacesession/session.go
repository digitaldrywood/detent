// Package workspacesession owns the domain vocabulary of workspace sessions
// and the relay that carries their surfaces (decisions sections 18.1 and
// 18.2): identifiers, the state machine and its reasons, capabilities,
// isolation levels and the relay frame shapes. It has no database or HTTP
// code; the hub owns storage, dispatch, authority and streaming, and the
// runner owns the worktree and the channels it serves.
//
// It is deliberately not internal/workspace, which is the runner's git
// worktree package: a workspace session is the durable object a surface
// attaches to, and the worktree is only one of the things it holds.
package workspacesession

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

const (
	sessionPrefix    = "ws_"
	identifierHexLen = 32
)

// ErrInvalidID reports an identifier that does not match its expected shape.
var ErrInvalidID = errors.New("workspacesession: invalid identifier")

// NewID returns a fresh workspace identifier ("ws_" + 32 hex).
func NewID() string { return sessionPrefix + randomHex() }

// ValidateID reports whether value is a well-formed workspace id.
func ValidateID(value string) error {
	rest, ok := strings.CutPrefix(value, sessionPrefix)
	if !ok || len(rest) != identifierHexLen {
		return fmt.Errorf("%w: workspace %q", ErrInvalidID, value)
	}
	if _, err := hex.DecodeString(rest); err != nil {
		return fmt.Errorf("%w: workspace %q", ErrInvalidID, value)
	}
	return nil
}

func randomHex() string {
	var buf [identifierHexLen / 2]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand never fails on supported platforms; a failure here is
		// unrecoverable for identifier generation.
		panic(fmt.Sprintf("workspacesession: random identifier: %v", err))
	}
	return hex.EncodeToString(buf[:])
}

// States of a workspace session. The legal moves between them are the whole
// contract of section 18.1: every other move is a bug, and Transition is what
// the hub asks before it writes one.
const (
	// StateRequested is a workspace waiting for an eligible runner to claim
	// its work item, and also where a hub restart puts an open one.
	StateRequested = "requested"
	// StateStarting is a claimed workspace whose runner has bound and is
	// preparing the worktree.
	StateStarting = "starting"
	// StateReady is a workspace whose surfaces can be used.
	StateReady = "ready"
	// StateIdle is a ready workspace with no person-originated activity.
	StateIdle = "idle"
	// StateUnreachable is a workspace whose runner missed two heartbeats. It
	// is not terminal: a returning runner takes it back to ready.
	StateUnreachable = "unreachable"
	// StateClosing is a workspace the hub has asked the runner to release.
	StateClosing = "closing"
	// StateClosed is a workspace that released cleanly.
	StateClosed = "closed"
	// StateFailed is a workspace that will never be resumed. The person opens
	// a new one.
	StateFailed = "failed"
)

// Reasons carried by a transition. A reason names why the workspace moved,
// never what a person typed.
const (
	// ReasonNoRunner is a request no eligible runner claimed in time.
	ReasonNoRunner = "no_runner"
	// ReasonCheckoutFailed is a runner that could not produce the worktree.
	ReasonCheckoutFailed = "checkout_failed"
	// ReasonWorktreeMissing is a worktree that disappeared under a bound
	// runner.
	ReasonWorktreeMissing = "worktree_missing"
	// ReasonRunnerRestarted is the unbind a restarting runner sends.
	ReasonRunnerRestarted = "runner_restarted"
	// ReasonHubRestarted marks the re-request every open workspace gets when
	// the hub comes back.
	ReasonHubRestarted = "hub_restarted"
	// ReasonLeaseLost is a workspace lease that expired.
	ReasonLeaseLost = "lease_lost"
	// ReasonCapacity is a claim that could not reserve a runner slot.
	ReasonCapacity = "capacity"
	// ReasonClosedByActor is a DELETE.
	ReasonClosedByActor = "closed_by_actor"
	// ReasonExpired is the idle timeout or the hard lifetime cap.
	ReasonExpired = "expired"
)

// Reasons lists every reason the state machine may record.
func Reasons() []string {
	return []string{
		ReasonNoRunner, ReasonCheckoutFailed, ReasonWorktreeMissing, ReasonRunnerRestarted,
		ReasonHubRestarted, ReasonLeaseLost, ReasonCapacity, ReasonClosedByActor, ReasonExpired,
	}
}

// States lists every state a workspace may hold.
func States() []string {
	return []string{
		StateRequested, StateStarting, StateReady, StateIdle,
		StateUnreachable, StateClosing, StateClosed, StateFailed,
	}
}

// ValidState reports whether value names a state.
func ValidState(value string) bool { return slices.Contains(States(), value) }

// ValidReason reports whether value names a transition reason.
func ValidReason(value string) bool { return slices.Contains(Reasons(), value) }

// Terminal reports whether a workspace in this state will never move again. A
// failed workspace is never resumed and a closed one is history.
func Terminal(state string) bool { return state == StateClosed || state == StateFailed }

// Open reports whether a workspace occupies a runner slot and counts against
// the open-workspace limits.
func Open(state string) bool { return !Terminal(state) }

// Bound reports whether a runner currently holds the workspace, which is when
// heartbeats are expected and a relay may carry frames.
func Bound(state string) bool {
	return state == StateStarting || state == StateReady || state == StateIdle
}

// transitions is the whole legal move set of section 18.1, written out rather
// than derived, so that reading this map is reading the contract.
var transitions = map[string][]string{
	StateRequested: {
		StateStarting,  // an eligible runner claimed and bound
		StateReady,     // the original runner re-bound inside the restart grace
		StateRequested, // a second hub restart before anyone bound
		StateClosing,   // DELETE, or expires_at passed
		StateFailed,    // no_runner, or capacity
	},
	StateStarting: {
		StateReady,
		StateRequested, // hub restart
		StateClosing,   // DELETE, or expires_at passed
		StateFailed,
	},
	StateReady: {
		StateIdle,
		StateUnreachable,
		StateRequested, // hub restart
		StateClosing,
		StateFailed,
	},
	StateIdle: {
		StateReady,
		StateUnreachable,
		StateRequested, // hub restart
		StateClosing,
		StateFailed,
	},
	StateUnreachable: {
		StateReady,     // the runner's heartbeat came back
		StateRequested, // hub restart
		StateClosed,    // idle_timeout_seconds elapsed while unreachable
		StateFailed,    // the lease expired
	},
	StateClosing: {StateClosed, StateFailed},
	StateClosed:  {},
	StateFailed:  {},
}

// Transition reports whether from → to is one of the legal moves. Terminal
// states move nowhere; the only self-transition is the hub-restart
// re-request, which must be idempotent because recovery may run twice.
func Transition(from, to string) bool {
	if !ValidState(from) || !ValidState(to) {
		return false
	}
	return slices.Contains(transitions[from], to)
}

// Capability names. requires on a request is a subset of these, and the runner
// reports the same keys in its heartbeat (section 18.10).
const (
	CapabilityTerminal = "terminal"
	CapabilityFiles    = "files"
	CapabilityDiff     = "diff"
	CapabilityPreview  = "preview"
	// CapabilityExec is running one project action non-interactively on the
	// worktree (section 18.12). It is separate from terminal because the two
	// are gated differently: a terminal hands a person the runner account's
	// interactive shell, and an action runs a command the project wrote down
	// in advance. A runner that offers one need not offer the other.
	CapabilityExec = "exec"
	// CapabilityGit is the git channel of section 18.13: status, commit and
	// push on the workspace's own worktree. A runner reports it only when it
	// has a git binary and the worktree is a repository, so the header's git
	// action group disables from the workspace's capabilities rather than
	// discovering the absence one failed frame at a time.
	CapabilityGit = "git"
)

// Capabilities is what a runner can serve for one workspace.
type Capabilities struct {
	Terminal bool `json:"terminal"`
	Files    bool `json:"files"`
	Diff     bool `json:"diff"`
	Preview  bool `json:"preview"`
	Exec     bool `json:"exec"`
	Git      bool `json:"git"`
}

// Has reports whether the set contains the named capability. An unknown name
// is never held, so a request that asks for one is refused rather than
// silently satisfied.
func (c Capabilities) Has(name string) bool {
	switch name {
	case CapabilityTerminal:
		return c.Terminal
	case CapabilityFiles:
		return c.Files
	case CapabilityDiff:
		return c.Diff
	case CapabilityPreview:
		return c.Preview
	case CapabilityExec:
		return c.Exec
	case CapabilityGit:
		return c.Git
	}
	return false
}

// Names lists the capabilities held, in a stable order.
func (c Capabilities) Names() []string {
	names := []string{}
	for _, name := range CapabilityNames() {
		if c.Has(name) {
			names = append(names, name)
		}
	}
	return names
}

// CapabilitiesFrom builds a set from a list of names, ignoring unknown ones.
func CapabilitiesFrom(names []string) Capabilities {
	var set Capabilities
	for _, name := range names {
		switch strings.ToLower(strings.TrimSpace(name)) {
		case CapabilityTerminal:
			set.Terminal = true
		case CapabilityFiles:
			set.Files = true
		case CapabilityDiff:
			set.Diff = true
		case CapabilityPreview:
			set.Preview = true
		case CapabilityExec:
			set.Exec = true
		case CapabilityGit:
			set.Git = true
		}
	}
	return set
}

// CapabilityNames lists every capability key in its canonical order.
func CapabilityNames() []string {
	return []string{
		CapabilityTerminal, CapabilityFiles, CapabilityDiff, CapabilityPreview,
		CapabilityExec, CapabilityGit,
	}
}

// ValidCapability reports whether value names a capability.
func ValidCapability(value string) bool { return slices.Contains(CapabilityNames(), value) }

// DefaultRequires is what a request without requires asks for: the two
// read-only surfaces (section 18.1).
func DefaultRequires() []string { return []string{CapabilityFiles, CapabilityDiff} }

// NormalizeRequires lowercases, trims, de-duplicates and orders a requires
// list, and substitutes the default for an empty one. An unknown entry is an
// error rather than a silent drop: a client that asked for something the hub
// does not know must not receive a workspace that cannot serve it.
func NormalizeRequires(values []string) ([]string, error) {
	if len(values) == 0 {
		return DefaultRequires(), nil
	}
	if len(values) > len(CapabilityNames()) {
		return nil, errors.New("requires may name each capability at most once")
	}
	seen := map[string]struct{}{}
	for _, value := range values {
		name := strings.ToLower(strings.TrimSpace(value))
		if !ValidCapability(name) {
			return nil, fmt.Errorf("unknown capability %q", value)
		}
		seen[name] = struct{}{}
	}
	required := []string{}
	for _, name := range CapabilityNames() {
		if _, ok := seen[name]; ok {
			required = append(required, name)
		}
	}
	return required, nil
}

// Satisfies reports whether the capability set can serve every requirement.
func (c Capabilities) Satisfies(requires []string) bool {
	for _, name := range requires {
		if !c.Has(name) {
			return false
		}
	}
	return true
}

// Isolation levels a runner may offer a terminal (section 18.3). They are
// carried on the workspace because only the bound runner's answer matters.
const (
	// IsolationUser is a PTY as the runner's own user. It is the runner's
	// authority handed to a person, and the contract does not pretend
	// otherwise.
	IsolationUser = "user"
	// IsolationContainer is a PTY confined to the worktree.
	IsolationContainer = "container"
)

// ValidIsolation reports whether value names an isolation level. An empty
// value is valid and means the runner has not reported one.
func ValidIsolation(value string) bool {
	return value == "" || value == IsolationUser || value == IsolationContainer
}

// Worktree origins a workspace reports. A workspace on an attempt whose
// worktree is still retained keeps it; after the retention window the runner
// checks out head_sha into a fresh worktree instead (section 18.1).
const (
	WorktreeRetained = "retained"
	WorktreeFresh    = "fresh"
)

// ValidWorktree reports whether value names a worktree origin.
func ValidWorktree(value string) bool {
	return value == "" || value == WorktreeRetained || value == WorktreeFresh
}

// Owner is the workspace owner tuple: its own generation, never the subject
// attempt's. A finished attempt's lease is released and never revived, so a
// workspace writing under it would be writing under a dead generation.
type Owner struct {
	WorkspaceID  string `json:"workspace_id"`
	RunnerID     string `json:"runner_id"`
	MachineID    string `json:"machine_id"`
	LeaseID      string `json:"lease_id"`
	FencingToken int64  `json:"fencing_token"`
}

// Zero reports whether the tuple names no generation at all.
func (o Owner) Zero() bool {
	return o.WorkspaceID == "" && o.RunnerID == "" && o.MachineID == "" && o.LeaseID == "" && o.FencingToken == 0
}

// Session is the workspace resource (section 18.1). RelaySessions is served to
// owners and admins only; every other reader sees it absent.
type Session struct {
	ID             string `json:"id"`
	OrganizationID string `json:"organization_id"`
	ProjectID      string `json:"project_id"`
	WorkItemID     string `json:"work_item_id"`
	AttemptID      string `json:"attempt_id,omitempty"`
	Ref            string `json:"ref"`
	HeadSHA        string `json:"head_sha,omitempty"`
	RunnerID       string `json:"runner_id,omitempty"`
	MachineID      string `json:"machine_id,omitempty"`
	// MachineHostname is the host the bound runner reported, and WorktreePath
	// is the absolute path it prepared (section 18.13). The header's Open
	// picker needs both: a `cursor://file/…` link only reaches a worktree on
	// the reader's own machine, so the client compares the hostname and
	// disables every item naming the machine the worktree is actually on.
	MachineHostname string        `json:"machine_hostname,omitempty"`
	WorktreePath    string        `json:"worktree_path,omitempty"`
	State           string        `json:"state"`
	Reason          string        `json:"reason,omitempty"`
	Requires        []string      `json:"requires"`
	Capabilities    *Capabilities `json:"capabilities"`
	Isolation       string        `json:"isolation,omitempty"`
	Worktree        string        `json:"worktree,omitempty"`
	// ReadOnly is set while the subject attempt still runs: the person may
	// look at the worktree the model is editing and may not type into it.
	ReadOnly           bool           `json:"read_only"`
	IdleTimeoutSeconds int            `json:"idle_timeout_seconds"`
	ExpiresAt          time.Time      `json:"expires_at"`
	OpenedAt           *time.Time     `json:"opened_at"`
	LastActivityAt     *time.Time     `json:"last_activity_at"`
	CreatedBy          string         `json:"created_by"`
	RelaySessions      []RelaySession `json:"relay_sessions,omitempty"`
	Revision           int64          `json:"revision"`
	CreatedAt          time.Time      `json:"created_at"`
	UpdatedAt          time.Time      `json:"updated_at"`
}

// RelaySession is one person connection's audit row (section 18.2).
type RelaySession struct {
	ID            string     `json:"id"`
	WorkspaceID   string     `json:"workspace_id"`
	PrincipalID   string     `json:"principal_id"`
	Subject       string     `json:"subject"`
	ConnectionID  string     `json:"connection_id"`
	Channels      []string   `json:"channels"`
	OpenedAt      time.Time  `json:"opened_at"`
	ClosedAt      *time.Time `json:"closed_at"`
	CloseReason   string     `json:"close_reason,omitempty"`
	BytesIn       int64      `json:"bytes_in"`
	BytesOut      int64      `json:"bytes_out"`
	SupportReason string     `json:"support_reason,omitempty"`
}

// EventType is the project stream event one transition emits: workspace.ready,
// workspace.failed and so on (section 18.1).
func EventType(state string) string { return "workspace." + state }
