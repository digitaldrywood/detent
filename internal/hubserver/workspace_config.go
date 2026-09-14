package hubserver

import (
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/workspacesession"
)

// WorkspaceConfig is the `workspaces:` section of the hosted configuration
// (decisions section 18.1). Every field is a bound: how long a request waits
// for a runner, how long a worktree stays available after its run, how long an
// unused workspace lives, how many may be open at once, and how much of the
// hub process the relay may hold.
type WorkspaceConfig struct {
	// Enabled mounts the workspace endpoints and the relay. With it off the
	// surfaces stay disabled with their reason (section 16) and no workspace
	// work item is ever created.
	Enabled bool
	// RequestTimeout is workspaces.request_timeout: how long a workspace
	// stays requested with no eligible runner before it fails with
	// no_runner.
	RequestTimeout time.Duration
	// RetainAfterRun is workspaces.retain_after_run: how long an attempt's
	// worktree stays available after the run. Past it a workspace on that
	// attempt checks out head_sha fresh.
	RetainAfterRun time.Duration
	// IdleTimeout is workspaces.idle_timeout, the idle_timeout_seconds a
	// workspace reports. Only person-originated frames reset it, so a busy
	// shell left alone still expires.
	IdleTimeout time.Duration
	// MaxLifetime is workspaces.max_lifetime, the hard cap no workspace
	// outlives whatever it is doing.
	MaxLifetime time.Duration
	// PlanMaxOpen is plan.workspaces.max_open: open workspaces per
	// organization. Beyond it a request fails with workspace_limit.
	PlanMaxOpen int
	// PersonMaxOpen is the per-person cap. Section 18.1 fixes it at 3; it is
	// a field rather than a constant so a test can reach the refusal without
	// opening three workspaces.
	PersonMaxOpen int
	// RelayMemoryBytes is workspaces.relay.memory, the whole hub process's
	// relay budget. Beyond it new streams fail with relay_busy.
	RelayMemoryBytes int64
	// FilesDeny is workspaces.files.deny: the project's extra denylist globs,
	// applied on top of the fixed list both the live surface and the stored
	// diff already share.
	FilesDeny []string
	// Terminal is the terminal surface's own settings (section 18.3). All three
	// are live: Enabled gates the creation request and every frame, Isolation
	// decides who may open one and what a recording's audience is, and Record
	// decides whether the stream is stored.
	Terminal WorkspaceTerminalConfig
}

// WorkspaceTerminalConfig is `workspaces.terminal` (decisions section 18.3).
type WorkspaceTerminalConfig struct {
	// Enabled is workspaces.terminal.enabled, default off; an owner turns it
	// on.
	Enabled bool
	// Isolation is workspaces.terminal.isolation. container is the default
	// the setting offers and the only level recommended; user is allowed only
	// when an organization has set it explicitly, and the setting page says
	// in words what that level exposes.
	Isolation string
	// Record is workspaces.terminal.record, default on.
	//
	// It is a pointer because "absent" and "false" are different answers and a
	// bool cannot hold both: section 18.3 makes recording on by default, so a
	// configuration that says nothing must record, and only one that says
	// `record: false` must not. Read it through RecordEnabled rather than
	// directly, so nothing has to remember which nil means.
	Record *bool
}

// Workspace configuration defaults, all from section 18.1 and 18.2.
const (
	defaultWorkspaceRequestTimeout = 5 * time.Minute
	defaultWorkspaceRetainAfterRun = 30 * time.Minute
	defaultWorkspaceIdleTimeout    = 30 * time.Minute
	defaultWorkspaceMaxLifetime    = 4 * time.Hour
	defaultWorkspacePlanMaxOpen    = 20
	// defaultWorkspacePersonMaxOpen is section 18.1's "per person, 3 at a
	// time".
	defaultWorkspacePersonMaxOpen = 3
)

// ErrWorkspaceConfig reports a workspace section the hub refuses to run with.
var ErrWorkspaceConfig = errors.New("hub workspace configuration is invalid")

func (c WorkspaceConfig) normalized() WorkspaceConfig {
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = defaultWorkspaceRequestTimeout
	}
	if c.RetainAfterRun < 0 {
		c.RetainAfterRun = defaultWorkspaceRetainAfterRun
	}
	if c.IdleTimeout <= 0 {
		c.IdleTimeout = defaultWorkspaceIdleTimeout
	}
	if c.MaxLifetime <= 0 {
		c.MaxLifetime = defaultWorkspaceMaxLifetime
	}
	if c.MaxLifetime < c.IdleTimeout {
		// A hard cap under the idle timeout would make every workspace expire
		// on the cap instead, which reads as a bug rather than a policy.
		c.MaxLifetime = c.IdleTimeout
	}
	if c.PlanMaxOpen <= 0 {
		c.PlanMaxOpen = defaultWorkspacePlanMaxOpen
	}
	if c.PersonMaxOpen <= 0 {
		c.PersonMaxOpen = defaultWorkspacePersonMaxOpen
	}
	if c.RelayMemoryBytes <= 0 {
		c.RelayMemoryBytes = workspacesession.DefaultRelayMemoryBytes
	}
	if c.Terminal.Isolation == "" {
		c.Terminal.Isolation = workspacesession.IsolationContainer
	}
	if c.Terminal.Record == nil {
		recording := true
		c.Terminal.Record = &recording
	}
	return c
}

// validate refuses a section the hub cannot honour. It is separate from
// normalized because a default is a choice the hub may make for an operator
// and an invalid value is not.
func (c WorkspaceConfig) validate() error {
	if !workspacesession.ValidIsolation(c.Terminal.Isolation) || c.Terminal.Isolation == "" {
		return errors.Join(ErrWorkspaceConfig, errors.New("workspaces.terminal.isolation must be container or user"))
	}
	if _, err := workspacesession.NewDenylist(c.FilesDeny); err != nil {
		return errors.Join(ErrWorkspaceConfig, err)
	}
	return nil
}

// RecordEnabled reports whether terminal sessions are recorded. A
// configuration that never said records, which is section 18.3's default.
func (c WorkspaceTerminalConfig) RecordEnabled() bool {
	return c.Record == nil || *c.Record
}

// idleTimeoutSeconds is what a workspace resource reports.
func (c WorkspaceConfig) idleTimeoutSeconds() int {
	return int(c.IdleTimeout / time.Second)
}
