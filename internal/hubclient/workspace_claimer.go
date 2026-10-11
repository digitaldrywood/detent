package hubclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

// The workspace lane's claim path (decisions section 18.1).
//
// A workspace item is claimed like any other work item, with one difference
// that matters: the claim asks only for detent:workspace items. A runner's
// label filters are preferences rather than authority -- the hub decides
// eligibility from the capability report and the retained-worktree rule -- but
// asking for the right kind keeps the lane from taking ordinary work the
// dispatcher should have had.

// WorkspaceLaneConfig builds a claimer for one project.
type WorkspaceLaneConfig struct {
	// PolicyID is the approved project policy the claim pins to, the same one
	// a run claims under.
	PolicyID string
	// MachineID is this runner's machine.
	MachineID tracker.MachineID
	// SessionID produces a fresh claim session per attempt. Each claim needs
	// its own: a session already holding a lease is answered with that lease
	// rather than a new one, which is right for a retrying run and wrong for
	// a lane that wants the next workspace.
	SessionID func() (string, error)
	// LeaseTTL is how long the claim is held between renewals. The workspace
	// heartbeat renews it, so it is the same 90 seconds section 18.1 gives a
	// workspace before lease_lost.
	LeaseTTL time.Duration
}

// ErrWorkspacesNotServed reports that the hub does not advertise workspace
// sessions, so there is nothing for a workspace lane to claim.
var ErrWorkspacesNotServed = errors.New("hub does not serve workspace sessions")

// workspaceServedRecheck is how long the hub's answer about serving workspace
// sessions is trusted before it is asked again.
const workspaceServedRecheck = 10 * time.Minute

// WorkspaceClaimer claims and releases workspace items for one project.
type WorkspaceClaimer struct {
	native *NativeClient
	config WorkspaceLaneConfig
	now    func() time.Time

	mu          sync.Mutex
	servedUntil time.Time
}

// NewWorkspaceClaimer prepares the claimer.
func NewWorkspaceClaimer(native *NativeClient, config WorkspaceLaneConfig) (*WorkspaceClaimer, error) {
	if native == nil {
		return nil, errors.New("native Hub client is required")
	}
	if strings.TrimSpace(config.PolicyID) == "" {
		return nil, errors.New("an approved project policy is required to claim a workspace")
	}
	if strings.TrimSpace(string(config.MachineID)) == "" || config.SessionID == nil {
		return nil, errors.New("a machine identity and a session source are required")
	}
	if config.LeaseTTL <= 0 {
		config.LeaseTTL = workspacesession.LeaseTTL
	}
	return &WorkspaceClaimer{native: native, config: config, now: time.Now}, nil
}

// WorkspaceItemLabel is the label the hub puts on a workspace's dispatch issue.
// The runner asks for it by name so the lane never takes ordinary work.
const WorkspaceItemLabel = "detent:workspace"

// ClaimWorkspace takes the next claimable workspace item.
//
// The hub's capability document is read first, and again every
// workspaceServedRecheck while it keeps advertising workspace sessions. A hub
// that does not advertise them answers ErrWorkspacesNotServed without a claim.
func (c *WorkspaceClaimer) ClaimWorkspace(ctx context.Context) (tracker.NativeLease, error) {
	state := c.ClaimState()
	if len(state.Reasons) > 0 {
		for _, reason := range state.Reasons {
			slog.Info("workspace.claim_skipped", "project_id", c.native.project, "reason", reason)
		}
		return tracker.NativeLease{}, ErrNoClaimableWork
	}
	if err := c.requireServed(ctx); err != nil {
		return tracker.NativeLease{}, err
	}
	session, err := c.config.SessionID()
	if err != nil {
		return tracker.NativeLease{}, fmt.Errorf("workspace claim session: %w", err)
	}
	lease, err := c.native.Claim(ctx, tracker.NativeClaim{
		PolicyID: state.PolicyID, MachineID: c.config.MachineID, SessionID: session,
		TTLSeconds: int64(c.config.LeaseTTL / time.Second), ProtocolMajor: tracker.NativeProtocolMajor,
		// The workspace capability is the lane's authority to be offered
		// workspace items at all: the hub's ordinary claim excludes them from
		// its candidates, so no issue lane can reach one by asking for the
		// label (decisions section 18.1). The label filter stays as the
		// narrowing preference it always was.
		Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeWorkspaceCapability},
		LabelInclude: []string{WorkspaceItemLabel},
	})
	if err != nil {
		// ErrNoClaimableWork is the steady state of an idle runner, and the
		// lane recognises it directly: wrapping it in a second sentinel would
		// only give two names to one condition.
		return tracker.NativeLease{}, err
	}
	return lease, nil
}

func (c *WorkspaceClaimer) requireServed(ctx context.Context) error {
	c.mu.Lock()
	fresh := c.now().Before(c.servedUntil)
	c.mu.Unlock()
	if fresh {
		return nil
	}
	served, err := c.native.HubFeature(ctx, tracker.NativeWorkspaceCapability)
	if err != nil {
		return fmt.Errorf("read hub capabilities: %w", err)
	}
	if !served {
		return ErrWorkspacesNotServed
	}
	c.mu.Lock()
	c.servedUntil = c.now().Add(workspaceServedRecheck)
	c.mu.Unlock()
	return nil
}

// WorkspaceForWorkItem resolves the workspace a claimed item dispatches.
func (c *WorkspaceClaimer) WorkspaceForWorkItem(ctx context.Context, item tracker.NativeWorkItemID, identity WorkspaceIdentity) (workspacesession.Session, error) {
	return c.native.WorkspaceForWorkItem(ctx, item, identity)
}

// ReleaseWorkspaceLease gives a claim back.
//
// The hub's refusals are mapped onto the workspace sentinels the lane acts on,
// so "this lease is no longer yours" -- because the hub released it when the
// workspace ended, or because it expired -- is reported as ErrStaleWorkspace
// rather than as an opaque 409. The lane cannot do anything about a lease it no
// longer holds, and naming the condition is what lets it tell agreement from a
// release that genuinely failed.
func (c *WorkspaceClaimer) ReleaseWorkspaceLease(ctx context.Context, lease tracker.NativeLease, reason string) error {
	return workspaceError(c.native.Release(ctx, lease, reason))
}

// RunIdentifier is the identifier this runner's own runs of a work item key
// their worktree on, which is how a retained workspace finds the attempt's.
func (c *WorkspaceClaimer) RunIdentifier(ctx context.Context, workItemID string) (string, error) {
	issue, err := c.native.Issue(ctx, tracker.NativeWorkItemID(workItemID))
	if err != nil {
		return "", err
	}
	return nativeIssueIdentifier(issue), nil
}

// Native exposes the client the lane hands its sessions, so one construction
// site wires both halves.
func (c *WorkspaceClaimer) Native() *NativeClient { return c.native }

type WorkspaceClaimState struct {
	ProjectID tracker.ProjectID
	PolicyID  string
	Reasons   []string
	Changed   <-chan struct{}
}

func (c *WorkspaceClaimer) ClaimState() WorkspaceClaimState {
	state := WorkspaceClaimState{ProjectID: c.native.project, PolicyID: c.config.PolicyID}
	r := c.native.client.runner
	if r == nil {
		return state
	}
	r.routingMu.Lock()
	defer r.routingMu.Unlock()
	if r.routingChanged == nil {
		r.routingChanged = make(chan struct{})
	}
	state.Changed = r.routingChanged
	if local, ok := r.localPolicies[c.native.project]; ok {
		state.PolicyID = local
	}
	if r.routing == nil {
		return state
	}
	if r.routing.Routing.State == "draining" {
		state.Reasons = append(state.Reasons, "runner_draining")
	}
	if capacity := r.routing.ClaimState; capacity != nil {
		if approved, ok := capacity.PolicyIDs[c.native.project]; ok && approved != state.PolicyID {
			state.Reasons = append(state.Reasons, "policy_mismatch")
		}
		if len(capacity.Slots) >= capacity.HostCapacity {
			state.Reasons = append(state.Reasons, "host_capacity")
		}
		used := 0
		for _, slot := range capacity.Slots {
			if slot.RunnerID == r.routing.RunnerID {
				used++
			}
		}
		if used >= capacity.RunnerCapacity {
			state.Reasons = append(state.Reasons, "runner_capacity")
		}
	}
	return state
}
