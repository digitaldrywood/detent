package hubclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

// Workspace session worker endpoints (decisions section 18.1). They mirror the
// conversation worker set: bind claims the workspace and returns the tuple and
// the checkout instructions, heartbeat renews the lease and carries the
// runner's report, and unbind releases it.
//
// Every call carries the workspace owner tuple, which is this workspace's own
// generation and not the subject attempt's. A call the hub refuses with
// stale_execution means the runner has lost the workspace and must stop serving
// it: there is no state in which two runners hold one worktree.

// ErrStaleWorkspace reports that the hub no longer recognises this runner as
// the workspace's owner. It is terminal for the session; the person opens a new
// workspace rather than the runner retrying.
var ErrStaleWorkspace = errors.New("workspace execution is stale")

// ErrNoWorkspace reports that the workspace is gone.
var ErrNoWorkspace = errors.New("workspace was not found")

// WorkspaceIdentity is the owner tuple every workspace worker request carries.
type WorkspaceIdentity struct {
	LeaseID      tracker.LeaseID      `json:"lease_id"`
	FencingToken tracker.FencingToken `json:"fencing_token,string"`
}

// WorkspaceBindRequest claims a workspace for the runner holding its lease.
type WorkspaceBindRequest struct {
	WorkspaceIdentity
	Capabilities workspacesession.Capabilities `json:"capabilities"`
	Isolation    string                        `json:"isolation,omitempty"`
}

// WorkspaceCheckout is what the runner is told to produce.
type WorkspaceCheckout struct {
	WorkItemID         string    `json:"work_item_id"`
	AttemptID          string    `json:"attempt_id,omitempty"`
	Ref                string    `json:"ref,omitempty"`
	HeadSHA            string    `json:"head_sha,omitempty"`
	Worktree           string    `json:"worktree"`
	ReadOnly           bool      `json:"read_only"`
	Requires           []string  `json:"requires"`
	IdleTimeoutSeconds int       `json:"idle_timeout_seconds"`
	HeartbeatSeconds   int       `json:"heartbeat_seconds"`
	ExpiresAt          time.Time `json:"expires_at"`
	Deny               []string  `json:"deny,omitempty"`
}

// WorkspaceBindResponse answers a successful bind.
type WorkspaceBindResponse struct {
	Owner    workspacesession.Owner   `json:"owner"`
	Checkout WorkspaceCheckout        `json:"checkout"`
	Session  workspacesession.Session `json:"workspace"`
	// Actions is the project's run-on-worktree-creation set, in authoring
	// order (decisions section 18.12). The runner starts them itself once the
	// worktree exists, in the order they arrive: an author who wants install
	// before build writes install first.
	Actions []workspacesession.Action `json:"actions,omitempty"`
}

// WorkspaceHeartbeatRequest renews the lease and carries the runner's report.
type WorkspaceHeartbeatRequest struct {
	WorkspaceIdentity
	State        string                        `json:"state,omitempty"`
	Reason       string                        `json:"reason,omitempty"`
	HeadSHA      string                        `json:"head_sha,omitempty"`
	Capabilities workspacesession.Capabilities `json:"capabilities"`
	Isolation    string                        `json:"isolation,omitempty"`
	Worktree     string                        `json:"worktree,omitempty"`
	// WorktreePath and MachineHostname are what the Open picker needs
	// (section 18.13): the absolute path this runner prepared and the host it
	// prepared it on. They are reported on every beat rather than on the bind
	// alone, so a runner that re-prepared a fresh worktree corrects the
	// resource instead of leaving a stale path behind.
	WorktreePath    string `json:"worktree_path,omitempty"`
	MachineHostname string `json:"machine_hostname,omitempty"`
}

// WorkspaceHeartbeatResponse answers a heartbeat with the workspace as stored,
// which is how a runner learns it has been asked to close.
type WorkspaceHeartbeatResponse struct {
	Session workspacesession.Session `json:"workspace"`
}

// WorkspaceUnbindRequest releases the workspace.
type WorkspaceUnbindRequest struct {
	WorkspaceIdentity
	Reason string `json:"reason"`
}

// workspacePath builds the worker path for one workspace, refusing an id that
// is not the hub's own shape rather than letting it reach a URL.
func workspacePath(workspaceID, suffix string) (string, error) {
	if err := workspacesession.ValidateID(workspaceID); err != nil {
		return "", err
	}
	return "/workspaces/" + workspaceID + suffix, nil
}

// BindWorkspace claims a workspace for this runner.
func (c *NativeClient) BindWorkspace(ctx context.Context, workspaceID string, request WorkspaceBindRequest) (WorkspaceBindResponse, error) {
	var response WorkspaceBindResponse
	path, err := workspacePath(workspaceID, "/worker/bind")
	if err != nil {
		return response, err
	}
	err = c.client.request(ctx, http.MethodPost, c.base()+path, request, &response)
	return response, workspaceError(err)
}

// HeartbeatWorkspace renews the lease and reports the runner's state. The
// answer is the workspace as stored, so a runner that has been asked to close
// learns it from the same call that keeps it alive.
func (c *NativeClient) HeartbeatWorkspace(ctx context.Context, workspaceID string, request WorkspaceHeartbeatRequest) (workspacesession.Session, error) {
	var response WorkspaceHeartbeatResponse
	path, err := workspacePath(workspaceID, "/worker/heartbeat")
	if err != nil {
		return response.Session, err
	}
	err = c.client.request(ctx, http.MethodPost, c.base()+path, request, &response)
	return response.Session, workspaceError(err)
}

// WorkspaceActionRunReport records one project action run with the hub
// (decisions section 18.12).
//
// It is how a run the runner started itself becomes visible: a
// run-on-worktree-creation action has no person watching and no exec stream to
// carry its frames, so the row is written by a report rather than by the relay.
// An empty RunID creates the run and the answer carries its id; every later
// report of the same run names it.
//
// The tuple is a named field rather than embedded, unlike the other worker
// bodies: this one already carries a status, a reason and timestamps of its
// own, and a bare lease_id beside them would read as the run's rather than the
// workspace's.
type WorkspaceActionRunReport struct {
	WorkspaceIdentity WorkspaceIdentity `json:"workspace_identity"`
	ActionID          string            `json:"action_id"`
	RunID             string            `json:"run_id,omitempty"`
	Status            string            `json:"status"`
	ExitCode          *int              `json:"exit_code,omitempty"`
	Reason            string            `json:"reason,omitempty"`
	StartedAt         *time.Time        `json:"started_at,omitempty"`
	FinishedAt        *time.Time        `json:"finished_at,omitempty"`
	Output            string            `json:"output,omitempty"`
	Truncated         bool              `json:"truncated,omitempty"`
}

// ReportWorkspaceActionRun records a project action run and answers with the
// run as stored.
//
// The refusal mapping is the other worker calls': a report the hub fences off
// answers stale_execution and comes back as ErrStaleWorkspace, so a runner can
// tell "you no longer hold this workspace" -- stop reporting -- from a
// transport failure worth retrying.
func (c *NativeClient) ReportWorkspaceActionRun(ctx context.Context, workspaceID string, request WorkspaceActionRunReport) (workspacesession.Run, error) {
	var run workspacesession.Run
	path, err := workspacePath(workspaceID, "/worker/action-runs")
	if err != nil {
		return run, err
	}
	err = c.client.request(ctx, http.MethodPost, c.base()+path, request, &run)
	return run, workspaceError(err)
}

// UnbindWorkspace releases the workspace.
func (c *NativeClient) UnbindWorkspace(ctx context.Context, workspaceID string, request WorkspaceUnbindRequest) error {
	path, err := workspacePath(workspaceID, "/worker/unbind")
	if err != nil {
		return err
	}
	return workspaceError(c.client.request(ctx, http.MethodPost, c.base()+path, request, nil))
}

// DialWorkspaceRelay opens the runner's relay socket for a workspace. The tuple
// rides in headers because a GET has no body, the same shape the conversation
// controls poll uses.
//
// A second runner connection for the same workspace replaces the first, so a
// runner that reconnects without having lost the workspace supersedes itself:
// the session dials once and redials only after its socket ended.
func (c *NativeClient) DialWorkspaceRelay(ctx context.Context, workspaceID string, identity WorkspaceIdentity) (*websocket.Conn, error) {
	path, err := workspacePath(workspaceID, "/worker/relay")
	if err != nil {
		return nil, err
	}
	target, err := c.relayURL(c.base() + path)
	if err != nil {
		return nil, err
	}
	token, err := c.client.bearerToken(ctx)
	if err != nil {
		return nil, err
	}
	header := http.Header{
		"Authorization":          {token},
		"X-Detent-Lease":         {string(identity.LeaseID)},
		"X-Detent-Fencing-Token": {strconv.FormatInt(int64(identity.FencingToken), 10)},
	}
	socket, response, err := websocket.Dial(ctx, target, &websocket.DialOptions{HTTPHeader: header, HTTPClient: c.client.httpClient})
	defer closeDialResponse(response)
	if err != nil {
		if response != nil && response.StatusCode == http.StatusConflict {
			return nil, fmt.Errorf("%w: %w", ErrStaleWorkspace, err)
		}
		return nil, fmt.Errorf("dial workspace relay: %w", err)
	}
	socket.SetReadLimit(workspacesession.MaxFrameBytes + (16 << 10))
	return socket, nil
}

// relayURL turns a hub path into its WebSocket URL.
//
// The scheme is only ever http or https: New refuses anything else, so there
// is no third case to handle here and inventing one would be a branch no test
// could reach.
func (c *NativeClient) relayURL(path string) (string, error) {
	target, err := url.Parse(c.client.baseURL.String())
	if err != nil {
		return "", fmt.Errorf("parse hub url: %w", err)
	}
	target.Scheme = "ws"
	if c.client.baseURL.Scheme == "https" {
		target.Scheme = "wss"
	}
	target.Path = strings.TrimSuffix(target.Path, "/") + path
	return target.String(), nil
}

// workspaceError maps the hub's refusals onto the sentinels a session acts on.
// stale_execution is the one that matters: it means this runner is no longer
// the workspace's owner, and continuing to serve frames under it would be
// serving them under a dead generation.
func workspaceError(err error) error {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return err
	}
	switch {
	case apiErr.Status == http.StatusNotFound:
		return fmt.Errorf("%w: %w", ErrNoWorkspace, err)
	case apiErr.Code == "stale_execution", apiErr.Code == "stale_fencing_token", apiErr.Code == "lease_not_found":
		return fmt.Errorf("%w: %w", ErrStaleWorkspace, err)
	default:
		return err
	}
}

// WorkspaceForWorkItem resolves the workspace a claimed work item dispatches.
//
// A claim hands a runner a lease on a work item; the workspace worker
// endpoints are addressed by workspace id. This is the join, fenced by the same
// tuple the bind is, so a runner cannot read the workspace of work it did not
// claim.
func (c *NativeClient) WorkspaceForWorkItem(ctx context.Context, item tracker.NativeWorkItemID, identity WorkspaceIdentity) (workspacesession.Session, error) {
	var session workspacesession.Session
	name := strings.TrimSpace(string(item))
	if name == "" || strings.ContainsAny(name, "/?#%\\") {
		return session, errors.New("native work item ID is invalid")
	}
	headers := http.Header{
		"X-Detent-Lease":         {string(identity.LeaseID)},
		"X-Detent-Fencing-Token": {strconv.FormatInt(int64(identity.FencingToken), 10)},
	}
	err := c.client.requestWithHeaders(ctx, http.MethodGet, c.base()+"/work-items/"+name+"/workspace", headers, nil, &session)
	return session, workspaceError(err)
}

// closeDialResponse releases the upgrade response websocket.Dial hands back.
// On a refusal it is an ordinary body carrying the hub's error; on a successful
// upgrade it is the hijacked connection's own reader. Both have to be closed,
// and neither is read here: the socket is the thing that matters and the
// refusal's status has already been taken off the response.
func closeDialResponse(response *http.Response) {
	if response == nil || response.Body == nil {
		return
	}
	response.Body.Close()
}
