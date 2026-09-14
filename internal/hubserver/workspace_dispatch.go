package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

// Workspace dispatch (decisions section 18.1). A workspace is its own work item
// kind, detent:workspace, distinct from the coordinator kind in section 9: it
// may check out the repository, it does not end when a turn ends, and it stays
// claimed until the workspace closes.
//
// The claim gate is therefore stricter than the coordinator's. A runner may
// claim a workspace item only if it reports every capability in requires with a
// fresh heartbeat, holds an active grant for the project, and -- when the
// workspace names an attempt whose worktree is still retained -- is the runner
// that ran that attempt, because nobody else has that worktree.

const (
	// workspaceItemLabel marks the native issue as a workspace session. The
	// runner dispatcher selects its hold-open mode from this label.
	workspaceItemLabel = "detent:workspace"
	// workspaceItemTitlePrefix names the issue after the workspace it opens.
	// The subject issue's title is deliberately not repeated: the workspace
	// item is a dispatch record, not a second copy of the work.
	workspaceItemTitlePrefix = "Workspace session "
)

// workspaceLabelContextKey marks the hub's own workspace-item creation. It is
// unexported and never derived from a request, so no API caller can reserve the
// label for itself.
type workspaceLabelContextKey struct{}

// allowWorkspaceLabel authorizes the reserved workspace label for one call.
// Only ensureWorkspaceItem uses it.
func allowWorkspaceLabel(ctx context.Context) context.Context {
	return context.WithValue(ctx, workspaceLabelContextKey{}, true)
}

// workspaceLabelAllowed reports whether this call may set the reserved label.
func workspaceLabelAllowed(ctx context.Context) bool {
	allowed, isBool := ctx.Value(workspaceLabelContextKey{}).(bool)
	return isBool && allowed
}

// workspaceItemTitle names the issue after the workspace.
func workspaceItemTitle(workspaceID string) string {
	return workspaceItemTitlePrefix + workspaceID
}

// workspaceItemBody states what the runner is being asked for. The body is the
// whole brief a claiming runner sees before it binds.
func workspaceItemBody(record workspaceRecord) string {
	body := "Workspace session " + record.ID + " on work item " + record.SubjectWorkItemID + ".\n\n" +
		"This item asks a runner to open a worktree and hold it for a person's surfaces. " +
		"No implementation, branch or pull request is expected: the runner binds through the workspace " +
		"worker endpoints, heartbeats, serves the relay and unbinds when the workspace closes.\n\n" +
		"Surfaces required: " + joinWords(record.Requires) + ".\nRef: " + record.Ref + "."
	if record.AttemptID != "" {
		body += "\nAttempt: " + record.AttemptID + "."
	}
	if record.HeadSHA != "" {
		body += "\nHead: " + record.HeadSHA + "."
	}
	if record.ReadOnly {
		body += "\n\nThe subject attempt is still running, so this workspace is read-only and refuses a terminal."
	}
	return body
}

// joinWords renders a list for the issue body.
func joinWords(values []string) string {
	switch len(values) {
	case 0:
		return "none"
	case 1:
		return values[0]
	}
	joined := ""
	for index, value := range values {
		switch {
		case index == 0:
			joined = value
		case index == len(values)-1:
			joined += " and " + value
		default:
			joined += ", " + value
		}
	}
	return joined
}

// ensureWorkspaceItem creates the native issue that dispatches a workspace and
// records the association. Both commit in the caller's transaction, so a
// refused request leaves no orphaned issue behind.
func (w *workspaceService) ensureWorkspaceItem(ctx context.Context, tx *sql.Tx, scope nativeScope, record workspaceRecord, now time.Time) (string, error) {
	project, err := readNativeProject(ctx, tx, scope)
	if err != nil {
		return "", fmt.Errorf("read project for workspace item: %w", err)
	}
	state, found := firstDispatchableState(project)
	if !found {
		return "", nativeInvalid("The project has no dispatchable workflow state for a workspace session")
	}
	created, err := createNativeIssueTx(allowWorkspaceLabel(ctx), tx, scope, tracker.CreateIssue{
		Title:  workspaceItemTitle(record.ID),
		Body:   workspaceItemBody(record),
		State:  state,
		Labels: []string{workspaceItemLabel},
	}, now)
	if err != nil {
		return "", err
	}
	issue, ok := created.(tracker.NativeIssue)
	if !ok {
		return "", fmt.Errorf("unexpected workspace issue result %T", created)
	}
	item := string(issue.WorkItemID)
	if err := insertWorkspaceItem(ctx, tx, workspaceItemRecord{
		WorkItemID: item, WorkspaceID: record.ID, OrganizationID: record.OrganizationID,
		ProjectID: record.ProjectID, CreatedAt: now,
	}); err != nil {
		return "", err
	}
	return item, nil
}

// closeWorkspaceDispatchItem ends the workspace's dispatch issue when the
// workspace reaches a terminal state: the issue moves to the project's first
// terminal workflow state and the association row is marked closed.
//
// The two halves answer two different readers. Closing the association is what
// the claim gate reads, and it alone already keeps the item from being claimed
// again. Moving the issue is what a person reads: a closed workspace that left
// its item sitting in Todo, dispatchable, is a row on the board that looks like
// outstanding work and is not. Section 18.1 asks only that closing never delete
// the attempt's artifacts, which this does not touch. A project with no
// terminal state keeps the issue where it is, exactly as closeCoordinatorItem
// does; the association closes regardless.
func (w *workspaceService) closeWorkspaceDispatchItem(ctx context.Context, tx *sql.Tx, record workspaceRecord, now time.Time) error {
	item, found, err := readWorkspaceItemByWorkspace(ctx, tx, record.ID)
	if err != nil {
		return err
	}
	if !found || item.ClosedAt != nil {
		// Either the workspace never got an item, or an earlier terminal
		// transition already closed it. Closing twice must not move an issue
		// somebody has since reopened.
		return closeWorkspaceItem(ctx, tx, record.ID, now)
	}
	scope := nativeScope{organization: record.OrganizationID, project: record.ProjectID}
	project, err := readNativeProject(ctx, tx, scope)
	if err != nil {
		return fmt.Errorf("read project for workspace item: %w", err)
	}
	terminal, found := firstTerminalState(project)
	if !found {
		w.logger.Warn("workspace.item_not_terminated", "workspace_id", record.ID, "work_item_id", item.WorkItemID,
			"reason", "the project has no terminal workflow state")
		return closeWorkspaceItem(ctx, tx, record.ID, now)
	}
	issue, _, err := readNativeIssue(ctx, tx, scope, item.WorkItemID)
	if err != nil {
		return fmt.Errorf("read workspace issue: %w", err)
	}
	if issue.State != terminal {
		from := issue.State
		issue.State = terminal
		if _, err := persistNativeIssue(ctx, tx, scope, issue, "workflow.transitioned",
			tracker.CollaborationData{FromState: from, ToState: terminal, Reason: "worker_progress"}, now); err != nil {
			return fmt.Errorf("close workspace issue: %w", err)
		}
	}
	if err := closeWorkspaceItem(ctx, tx, record.ID, now); err != nil {
		return err
	}
	w.logger.Info("workspace.item_closed", "workspace_id", record.ID, "work_item_id", item.WorkItemID, "state", terminal)
	return nil
}

// runnerWorkspaceCapabilities reads what a runner reported it can serve, and
// whether the report is fresh. A runner with no identity, a stale heartbeat or
// no report serves nothing: the workspace stays requested rather than being
// handed to a runner that cannot honour it.
func runnerWorkspaceCapabilities(ctx context.Context, query nativeQueryer, runnerID string, now time.Time) (workspacesession.Capabilities, string, bool, error) {
	if runnerID == "" {
		return workspacesession.Capabilities{}, "", false, nil
	}
	var raw, isolation, heartbeat string
	err := query.QueryRowContext(ctx, `SELECT workspace_capabilities_json, workspace_isolation, last_heartbeat_at
FROM runner_identities WHERE id = ?`, runnerID).Scan(&raw, &isolation, &heartbeat)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return workspacesession.Capabilities{}, "", false, nil
		}
		return workspacesession.Capabilities{}, "", false, fmt.Errorf("read runner workspace capabilities: %w", err)
	}
	last, err := parseTimeValue(heartbeat)
	if err != nil {
		return workspacesession.Capabilities{}, "", false, fmt.Errorf("decode runner heartbeat: %w", err)
	}
	if now.Before(last) || !now.Before(last.Add(runnerauth.HeartbeatTimeout)) {
		return workspacesession.Capabilities{}, "", false, nil
	}
	var capabilities workspacesession.Capabilities
	if err := json.Unmarshal([]byte(raw), &capabilities); err != nil {
		return workspacesession.Capabilities{}, "", false, fmt.Errorf("decode runner workspace capabilities: %w", err)
	}
	return capabilities, isolation, true, nil
}

// workspaceClaimable reports whether this runner may claim this workspace's
// item, and why not when it may not. The reason is returned rather than logged
// because it is the same sentence the workspace's failure would carry.
func workspaceClaimable(record workspaceRecord, capabilities workspacesession.Capabilities, fresh bool, runnerID string, retainedRunner string) (bool, string) {
	if !fresh {
		return false, "the runner has no fresh workspace capability report"
	}
	if !capabilities.Satisfies(record.Requires) {
		return false, "the runner does not serve every required surface"
	}
	if retainedRunner != "" && retainedRunner != runnerID {
		// The worktree is still retained on the runner that produced it, and
		// no other runner has it. Handing the item to a second runner would
		// silently give the reader a fresh checkout of a different tree.
		return false, "the attempt's retained worktree belongs to another runner"
	}
	return true, ""
}

// gateWorkspaceClaim filters the workspace items a claiming runner may take.
// It returns the set to skip, in the shape the claim loop already uses for
// coordinator items -- a skipped candidate never consumes the claim, so the
// next one is still considered -- together with the set of workspace items the
// claim can see at all, which is what tells the loop a candidate it reaches is
// a workspace claim rather than project work.
//
// workspaceLane reports that this claim is a runner's workspace lane asking
// for workspace items. Every other claim excludes them in the candidate query
// itself, so this gate only ever narrows the lane's own candidates; it is
// called with workspaceLane false as a second line of defence, and then skips
// every workspace item unconditionally.
func gateWorkspaceClaim(ctx context.Context, tx *sql.Tx, scope *nativeScope, workspaceLane bool, retain time.Duration, now time.Time) (workspaceClaimGate, error) {
	items, err := workspaceIssues(ctx, tx)
	if err != nil {
		return workspaceClaimGate{}, err
	}
	gate := workspaceClaimGate{skip: map[tracker.WorkItemID]struct{}{}, dispatch: map[tracker.WorkItemID]struct{}{}}
	for id := range items {
		gate.dispatch[id] = struct{}{}
		if !workspaceLane {
			// A workspace item is not project work. The issue lane never runs
			// one, whether its workspace is still open or long closed
			// (decisions section 18.1).
			gate.skip[id] = struct{}{}
		}
	}
	if len(items) == 0 || !workspaceLane {
		return gate, nil
	}
	runnerID := ""
	if scope != nil {
		runnerID = scope.credential.Runner.RunnerID
	}
	capabilities, _, fresh, err := runnerWorkspaceCapabilities(ctx, tx, runnerID, now)
	if err != nil {
		return workspaceClaimGate{}, err
	}
	for id, item := range items {
		if !item.open {
			// The workspace has ended and the association is closed. Its
			// issue stays as the dispatch record it was; nothing claims it
			// again.
			gate.skip[id] = struct{}{}
			continue
		}
		if item.record.State != workspacesession.StateRequested {
			// Already claimed, or on its way out. Either way this claim is
			// not the one that starts it.
			gate.skip[id] = struct{}{}
			continue
		}
		retained, err := retainedWorktreeRunnerFor(ctx, tx, item.record, retain, now)
		if err != nil {
			return workspaceClaimGate{}, err
		}
		if claimable, _ := workspaceClaimable(item.record, capabilities, fresh, runnerID, retained); !claimable {
			gate.skip[id] = struct{}{}
		}
	}
	return gate, nil
}

// workspaceClaimGate is what the claim loop needs to know about the workspace
// items in front of it: which candidates this claim may not take, and which
// candidates are workspace dispatch items at all.
//
// The two sets answer different questions and are deliberately not one map.
// skip narrows the candidates; dispatch says what kind of work a candidate is,
// which is what decides whether the provider reservation applies to it.
type workspaceClaimGate struct {
	// skip is the set of candidates this claim must pass over. A skipped
	// candidate never consumes the claim, so the next one is still
	// considered.
	skip map[tracker.WorkItemID]struct{}
	// dispatch is every workspace item, claimable by this runner or not. Only
	// the workspace lane is ever offered one, so a candidate in this set that
	// the loop actually reaches is a workspace claim.
	dispatch map[tracker.WorkItemID]struct{}
}

// skipsProviderReservation reports whether this candidate is claimed without a
// provider reservation.
//
// A workspace session serves files, a diff, a terminal or a preview over the
// relay and runs no model: section 18.1 gives its claim one slot of the
// runner's capacity and a workspace lease with its own fencing token, and says
// nothing about a provider reservation. Putting it through provider
// requirement matching asked the lane for a local model selection it has no
// reason to carry, and every claim was refused 409 provider_incompatible.
//
// Being a workspace item is the whole test, rather than the lane the claim came
// from: the lane may also carry a label filter, and a lane that dropped the
// filter must still take an ordinary issue under the ordinary provider rules.
func (g workspaceClaimGate) skipsProviderReservation(id tracker.WorkItemID) bool {
	_, workspace := g.dispatch[id]
	return workspace
}

// retainedWorktreeRunnerFor is retainedWorktreeRunner without a service, for
// the claim loop, which runs inside the database rather than the workspace
// service.
func retainedWorktreeRunnerFor(ctx context.Context, query nativeQueryer, record workspaceRecord, retain time.Duration, now time.Time) (string, error) {
	if record.AttemptID == "" {
		return "", nil
	}
	scope := nativeScope{organization: record.OrganizationID, project: record.ProjectID}
	running, err := workspaceAttemptRunning(ctx, query, scope, record.AttemptID)
	if err != nil {
		return "", err
	}
	runnerID, finishedAt, found, err := workspaceAttemptRunner(ctx, query, scope, record.AttemptID)
	if err != nil || !found || runnerID == "" {
		return "", err
	}
	if running || now.Before(finishedAt.Add(retain)) {
		return runnerID, nil
	}
	return "", nil
}

// notWorkspaceItemClause excludes workspace items, open or closed, from an
// issue query that aliases the issues table as i. A workspace item keeps its
// history and its attempts; it is simply not project work, exactly as a
// coordinator item is not.
const notWorkspaceItemClause = `NOT EXISTS (SELECT 1 FROM workspace_items wi
 WHERE wi.work_item_id = i.native_id AND wi.organization_id = i.organization_id AND wi.project_id = i.project_id)`

// updateRunnerWorkspaceReport stores what the runner says it can serve. A
// heartbeat that omits the report leaves the stored one alone rather than
// clearing it: an older runner that does not know about workspaces would
// otherwise revoke its own eligibility on every beat, and the freshness check
// on last_heartbeat_at is what decides whether a stored report still counts.
func updateRunnerWorkspaceReport(ctx context.Context, tx *sql.Tx, scope nativeScope, capabilities *workspacesession.Capabilities, isolation string) error {
	if capabilities == nil {
		return nil
	}
	encoded, err := json.Marshal(capabilities)
	if err != nil {
		return fmt.Errorf("encode runner workspace capabilities: %w", err)
	}
	result, err := tx.ExecContext(ctx, `UPDATE runner_identities SET workspace_capabilities_json = ?, workspace_isolation = ?
WHERE id = ? AND token_id = ? AND organization_id = ?`, string(encoded), isolation,
		scope.credential.Runner.RunnerID, scope.credential.ID, scope.organization)
	if err != nil {
		return fmt.Errorf("store runner workspace capabilities: %w", err)
	}
	return requireRunnerUpdate(result, nil)
}
