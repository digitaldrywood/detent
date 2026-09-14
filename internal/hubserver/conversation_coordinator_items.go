package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// Runner-dispatched coordinator (decisions section 9). When the hub has no
// coordinator backend an unlinked conversation's message opens a coordinator
// work item: a native issue that only a runner with a live-control backend
// may claim, and that carries the conversation's turn.

const (
	// coordinatorItemLabel marks the native issue as a coordinator turn. The
	// runner dispatcher selects its coordinator run mode from this label.
	coordinatorItemLabel = "detent:coordinator"
	// coordinatorItemTitlePrefix names the issue after its conversation.
	// The conversation title is deliberately not repeated: an issue is
	// readable by every project reader and a private chat's title is the
	// user's own words (decisions section 10.1).
	coordinatorItemTitlePrefix = "Coordinator turn for conversation "
	// coordinatorItemTitleSuffixRunes is how much of the conversation id
	// the title carries: enough to tell two open turns apart, and no user
	// text at all.
	coordinatorItemTitleSuffixRunes = 8
)

// coordinatorLabelContextKey marks the hub's own coordinator-item creation.
// It is unexported and never derived from a request, so no API caller can
// reserve the label for itself.
type coordinatorLabelContextKey struct{}

// allowCoordinatorLabel authorizes the reserved coordinator label for one
// call. Only ensureCoordinatorItem uses it.
func allowCoordinatorLabel(ctx context.Context) context.Context {
	return context.WithValue(ctx, coordinatorLabelContextKey{}, true)
}

// requireUnreservedLabels refuses the coordinator label on issue content
// that did not come from the hub. The label selects the runner's read-only
// coordinator run mode, so a tracker writer who could set it would turn any
// issue into a run that produces no deliverable.
func requireUnreservedLabels(ctx context.Context, labels []string) error {
	allowed, isBool := ctx.Value(coordinatorLabelContextKey{}).(bool)
	if isBool && allowed {
		return nil
	}
	actionAllowed := pullRequestActionLabelAllowed(ctx)
	workspaceAllowed := workspaceLabelAllowed(ctx)
	for _, label := range labels {
		trimmed := strings.TrimSpace(label)
		if strings.EqualFold(trimmed, coordinatorItemLabel) {
			return nativeInvalid("Label " + coordinatorItemLabel + " is reserved")
		}
		// The workspace label selects the runner's hold-open mode, which
		// checks out the repository and keeps the claim until the workspace
		// closes (decisions section 18.1). A tracker writer who could set it
		// would be able to park a runner on any issue indefinitely.
		if !workspaceAllowed && strings.EqualFold(trimmed, workspaceItemLabel) {
			return nativeInvalid("Label " + workspaceItemLabel + " is reserved")
		}
		// The pull request action label selects a merge-lane item the runner
		// executes without producing a deliverable (decisions section 18.6),
		// so it is reserved for the same reason the coordinator label is.
		if !actionAllowed && strings.EqualFold(trimmed, pullRequestActionLabel) {
			return nativeInvalid("Label " + pullRequestActionLabel + " is reserved")
		}
	}
	return nil
}

// liveControlBackends names the provider backends that can take a live
// coordinator turn. It is the single definition the claim gate and the
// bootstrap capability share.
var liveControlBackends = map[string]bool{"codex": true}

// coordinatorItemBody states what the runner may and may not do. The body is
// the whole brief a claiming runner sees before it binds. It names the
// conversation and nothing the user wrote: the issue is readable by every
// project reader while the chat itself stays private (decisions section 10.1).
func coordinatorItemBody(conversationID string) string {
	return "Coordinator turn for conversation " + conversationID + ".\n\n" +
		"This item carries a conversation turn to a runner with a live-control provider backend. " +
		"No implementation, worktree, branch or pull request is expected: the runner answers the " +
		"conversation through the conversation control and turn-event endpoints and unbinds when the turn ends.\n\n" +
		"Conversation: " + conversationID
}

// coordinatorItemTitle names the issue after the conversation it answers,
// by the last characters of the conversation id. The conversation title is
// never repeated: it is the user's own text (decisions section 10.1).
func coordinatorItemTitle(conversationID string) string {
	return coordinatorItemTitlePrefix + coordinatorItemTitleSuffix(conversationID)
}

// coordinatorItemTitleSuffix is the tail of the conversation id the title
// carries. A shorter id is used whole.
func coordinatorItemTitleSuffix(conversationID string) string {
	runes := []rune(strings.TrimSpace(conversationID))
	if len(runes) <= coordinatorItemTitleSuffixRunes {
		return string(runes)
	}
	return string(runes[len(runes)-coordinatorItemTitleSuffixRunes:])
}

// firstDispatchableState reports the project's first non-terminal
// dispatchable workflow state, the lane a coordinator item is created in.
func firstDispatchableState(project tracker.NativeProject) (string, bool) {
	for _, state := range project.States {
		if !state.Terminal && state.Dispatchable && !state.OperatorOnly {
			return state.Name, true
		}
	}
	return "", false
}

// firstTerminalState reports the project's first terminal workflow state,
// the lane a finished coordinator item is transitioned to.
func firstTerminalState(project tracker.NativeProject) (string, bool) {
	for _, state := range project.States {
		if state.Terminal {
			return state.Name, true
		}
	}
	return "", false
}

// ensureCoordinatorItem returns the conversation's open coordinator item and
// creates one when none is open: the native issue and the association row
// commit in the caller's transaction, so a rolled back message leaves no
// orphaned issue behind.
func (c *conversationService) ensureCoordinatorItem(ctx context.Context, tx *sql.Tx, scope nativeScope, record conversationRecord, now time.Time) (coordinatorItemRecord, error) {
	item, open, err := c.store.openCoordinatorItem(ctx, tx, record.ID)
	if err != nil {
		return item, err
	}
	if open {
		return item, nil
	}
	project, err := readNativeProject(ctx, tx, scope)
	if err != nil {
		return item, fmt.Errorf("read project for coordinator item: %w", err)
	}
	state, found := firstDispatchableState(project)
	if !found {
		return item, conversationUnsupported("The project has no dispatchable workflow state for a coordinator turn")
	}
	// A coordinator item is a native issue, so it runs the same hosted plan
	// policy POST /work-items runs through nativeMutation: an exhausted
	// feature or allowance refuses it with the same code
	// (decisions section 10.10).
	database := c.server.database
	before, err := database.hostedConsumption(ctx, tx, now)
	if err != nil {
		return item, err
	}
	if err := database.requireHostedFeature(ctx, tx, "collaboration", now); err != nil {
		return item, err
	}
	created, err := createNativeIssueTx(allowCoordinatorLabel(ctx), tx, scope, tracker.CreateIssue{
		Title:  coordinatorItemTitle(record.ID),
		Body:   coordinatorItemBody(record.ID),
		State:  state,
		Labels: []string{coordinatorItemLabel},
	}, now)
	if err != nil {
		return item, err
	}
	issue, ok := created.(tracker.NativeIssue)
	if !ok {
		return item, fmt.Errorf("unexpected coordinator issue result %T", created)
	}
	item = coordinatorItemRecord{
		WorkItemID: string(issue.WorkItemID), ConversationID: record.ID,
		OrganizationID: record.OrganizationID, ProjectID: record.ProjectID, CreatedAt: now,
	}
	if err := c.store.createCoordinatorItem(ctx, tx, item); err != nil {
		return item, err
	}
	if err := database.checkHostedGrowth(ctx, tx, before, now, false); err != nil {
		return item, err
	}
	c.logger.Info("conversation.coordinator_item_opened", "conversation_id", record.ID, "work_item_id", item.WorkItemID, "state", state)
	return item, nil
}

// closeCoordinatorItem transitions the item's issue to the project's first
// terminal state and marks the row closed. A project without a terminal
// state keeps the issue where it is; the association is closed regardless,
// so the next message opens a new item.
func (c *conversationService) closeCoordinatorItem(ctx context.Context, tx *sql.Tx, scope nativeScope, item coordinatorItemRecord, now time.Time) error {
	project, err := readNativeProject(ctx, tx, scope)
	if err != nil {
		return fmt.Errorf("read project for coordinator item: %w", err)
	}
	terminal, found := firstTerminalState(project)
	switch {
	case !found:
		c.logger.Warn("conversation.coordinator_item_not_terminated", "conversation_id", item.ConversationID, "work_item_id", item.WorkItemID,
			"reason", "the project has no terminal workflow state")
	default:
		issue, _, err := readNativeIssue(ctx, tx, scope, item.WorkItemID)
		if err != nil {
			return fmt.Errorf("read coordinator issue: %w", err)
		}
		if issue.State != terminal {
			from := issue.State
			issue.State = terminal
			if _, err := persistNativeIssue(ctx, tx, scope, issue, "workflow.transitioned",
				tracker.CollaborationData{FromState: from, ToState: terminal, Reason: "worker_progress"}, now); err != nil {
				return fmt.Errorf("close coordinator issue: %w", err)
			}
		}
	}
	if err := c.store.closeCoordinatorItem(ctx, tx, item.WorkItemID, now); err != nil {
		return err
	}
	c.logger.Info("conversation.coordinator_item_closed", "conversation_id", item.ConversationID, "work_item_id", item.WorkItemID)
	return nil
}

// coordinatorItemExecution is the execution a conversation reports while its
// coordinator item waits for a runner: no owner, no capabilities.
func coordinatorItemExecution(record conversationRecord) (conversation.Execution, bool) {
	switch record.Execution.Status {
	case conversation.ExecutionStarting, conversation.ExecutionRunning, conversation.ExecutionWaitingInput, conversation.ExecutionInterrupting:
		// A runner already holds the turn; the message reaches it through
		// the controls poll and the owner generation must not be cleared.
		return record.Execution, false
	}
	next := conversation.Execution{Status: conversation.ExecutionWaitingForRunner, Owner: conversation.Owner{ThreadID: record.Execution.Owner.ThreadID}}
	if record.Execution.Status == next.Status && record.Execution.Owner == next.Owner &&
		record.Execution.Capabilities == (conversation.Capabilities{}) && record.Execution.Error == "" {
		// Nothing changed; a second queued message is not an execution event.
		return record.Execution, false
	}
	return next, true
}

// reportFresh reports whether an observation is recent enough to act on.
func reportFresh(report providercapacity.Report, now time.Time) bool {
	return !report.ObservedAt.IsZero() && !now.Before(report.ObservedAt) && now.Before(report.ObservedAt.Add(providercapacity.MaxAge))
}

// runnerSupportsLiveControl reports whether the runner's own provider report
// is fresh and names a backend that can take a live coordinator turn. A
// legacy machine registration has no runner identity and never qualifies.
func runnerSupportsLiveControl(ctx context.Context, query nativeQueryer, runnerID string, now time.Time) (bool, error) {
	if runnerID == "" {
		return false, nil
	}
	reports, err := readProviderReports(ctx, query, runnerID)
	if errors.Is(err, sql.ErrNoRows) {
		// The credential names a runner identity that no longer exists; it
		// reports nothing and takes no coordinator turn.
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read runner provider reports: %w", err)
	}
	for _, report := range reports {
		if liveControlBackends[report.Backend] && reportFresh(report, now) {
			return true, nil
		}
	}
	return false, nil
}

// openCoordinatorIssues lists the issues that are open coordinator items, so
// the claim loop can skip them without a query per candidate. Only open items
// are listed: a closed one is ordinary history again.
func openCoordinatorIssues(ctx context.Context, tx *sql.Tx) (map[tracker.WorkItemID]struct{}, error) {
	rows, err := tx.QueryContext(ctx, `SELECT i.id FROM coordinator_items ci JOIN issues i
 ON i.native_id = ci.work_item_id AND i.organization_id = ci.organization_id AND i.project_id = ci.project_id
WHERE ci.closed_at IS NULL`)
	if err != nil {
		return nil, fmt.Errorf("list coordinator items: %w", err)
	}
	defer func() { _ = rows.Close() }()
	items := map[tracker.WorkItemID]struct{}{}
	for rows.Next() {
		var id tracker.WorkItemID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan coordinator item: %w", err)
		}
		items[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list coordinator items: %w", err)
	}
	return items, nil
}

// liveControlRunnerEnrolled reports whether an active runner with a fresh
// heartbeat, a live grant on one of the projects and a fresh live-control
// provider report is enrolled (decisions section 9.2).
func liveControlRunnerEnrolled(ctx context.Context, query nativeQueryer, organization tracker.OrganizationID, projects []string, now time.Time) (bool, error) {
	if len(projects) == 0 {
		return false, nil
	}
	encoded, err := json.Marshal(projects)
	if err != nil {
		return false, fmt.Errorf("encode project list: %w", err)
	}
	rows, err := query.QueryContext(ctx, `SELECT DISTINCT r.last_heartbeat_at, r.provider_reports_json
FROM runner_identities r
JOIN api_tokens t ON t.id = r.token_id
JOIN token_grants g ON g.token_id = r.token_id AND g.organization_id = r.organization_id
WHERE r.organization_id = ? AND r.state = 'active' AND t.revoked_at IS NULL
 AND g.project_id IN (SELECT value FROM json_each(?))`, organization, string(encoded))
	if err != nil {
		return false, fmt.Errorf("list live control runners: %w", err)
	}
	defer func() { _ = rows.Close() }()
	enrolled := false
	for rows.Next() {
		var heartbeat, raw string
		if err := rows.Scan(&heartbeat, &raw); err != nil {
			return false, fmt.Errorf("scan live control runner: %w", err)
		}
		if enrolled {
			// The rows are drained rather than abandoned: the hub pool holds
			// a single connection and the caller queries again.
			continue
		}
		last, err := parseTimeValue(heartbeat)
		if err != nil {
			return false, fmt.Errorf("decode runner heartbeat: %w", err)
		}
		if now.Before(last) || !now.Before(last.Add(runnerauth.HeartbeatTimeout)) {
			continue
		}
		var reports []providercapacity.Report
		if err := json.Unmarshal([]byte(raw), &reports); err != nil {
			return false, fmt.Errorf("decode runner provider reports: %w", err)
		}
		for _, report := range reports {
			if liveControlBackends[report.Backend] && reportFresh(report, now) {
				enrolled = true
				break
			}
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("list live control runners: %w", err)
	}
	return enrolled, nil
}

// coordinatorAttemptBound reports whether a runner currently holds the
// conversation's coordinator turn: an open coordinator item and a live
// execution owned by an attempt.
func (c *conversationService) coordinatorAttemptBound(ctx context.Context, tx *sql.Tx, record conversationRecord) (bool, error) {
	if record.WorkItemID != "" || record.Execution.Owner.AttemptID == "" {
		return false, nil
	}
	switch record.Execution.Status {
	case conversation.ExecutionStarting, conversation.ExecutionRunning, conversation.ExecutionWaitingInput:
	default:
		return false, nil
	}
	_, open, err := c.store.openCoordinatorItem(ctx, tx, record.ID)
	return open, err
}

// notCoordinatorItemClause excludes coordinator items, open or closed, from
// an issue query that aliases the issues table as i. Coordinator items keep
// their history and attempts; they are simply not project work.
const notCoordinatorItemClause = `NOT EXISTS (SELECT 1 FROM coordinator_items ci
 WHERE ci.work_item_id = i.native_id AND ci.organization_id = i.organization_id AND ci.project_id = i.project_id)`

// conversationTranscriptRunes bounds each transcript line handed to a runner
// that cannot resume the provider thread.
const conversationTranscriptRunes = 2000

// conversationTranscriptMessages is how much history that runner receives.
const conversationTranscriptMessages = 20

// readBoundConversation resolves the conversation a work item carries: its
// linked conversation first, its open coordinator item second. A coordinator
// conversation stays private, so the runner's fenced lease on the item, which
// the caller has already proved, is the authority rather than the read rule.
func (c *conversationService) readBoundConversation(ctx context.Context, tx *sql.Tx, scope nativeScope, item string) (conversationRecord, bool, error) {
	record, err := c.readLinkedConversation(ctx, tx, scope, item)
	switch {
	case err == nil:
		return record, false, nil
	case !isNativeNotFound(err):
		return record, false, err
	}
	record, err = c.store.readConversationByCoordinatorItem(ctx, tx, scope.organization, scope.project, item)
	if err != nil {
		return record, false, err
	}
	return record, true, nil
}

// isNativeNotFound reports whether err is the opaque native not-found error.
func isNativeNotFound(err error) bool {
	var failure *nativeError
	return errors.As(err, &failure) && failure != nil && failure.Code == "not_found"
}

// conversationResumeDecision is the bind's answer to "does this attempt
// continue the recorded provider thread?", separated from the transcript read
// so the rule is testable on its own.
type conversationResumeDecision struct {
	// RunnerThreadID is the thread the binding runner named on this bind,
	// empty when it named none.
	RunnerThreadID string
	// RecordedRunnerID and RecordedOrigin describe the thread the
	// conversation carries.
	RecordedRunnerID string
	RecordedOrigin   string
	// BindingRunnerID is the runner holding the lease, empty for a
	// credential with no runner identity such as a legacy worker token.
	BindingRunnerID string
	// Coordinator reports that this bind is a coordinator turn rather than a
	// worker attempt on a linked issue.
	Coordinator bool
}

// resumeThread reports whether the binding attempt may continue the recorded
// provider thread (decisions section 9.3).
//
// A provider thread carries the instructions and the permission set of the
// turn that opened it. A coordinator thread was opened read-only, with no
// checkout and no authority to change files, run tests or move issue state;
// a worker thread was opened with all of them. Resuming across that boundary
// hands the resuming turn the wrong restrictions in one direction and the
// wrong authority in the other, so the origin must match the kind of turn
// that is binding.
//
// A runner that names the thread on this bind is opening its own, so the
// recorded origin does not apply to it. Otherwise the recorded runner must be
// this runner, and both identifiers must be present so that a credential with
// no runner identity never inherits the hub-side coordinator's thread.
func (d conversationResumeDecision) resumeThread() bool {
	if strings.TrimSpace(d.RunnerThreadID) != "" {
		return true
	}
	if d.RecordedRunnerID == "" || d.RecordedRunnerID != d.BindingRunnerID {
		return false
	}
	return d.RecordedOrigin == conversation.ThreadOriginFor(d.Coordinator)
}

// resumeFor decides what a binding runner continues from: its own provider
// thread, or the recent transcript when the thread was produced elsewhere or
// by the other kind of turn (decisions section 9.3). named reports that this
// bind supplied the thread. Messages already handed over as pending controls
// are left out so the runner does not see them twice.
func (c *conversationService) resumeFor(ctx context.Context, tx *sql.Tx, record conversationRecord, scope nativeScope, thread string, named bool, coordinator bool, pending []conversationControl) (conversationResumeResource, error) {
	decision := conversationResumeDecision{
		RecordedRunnerID: record.ProviderThreadRunnerID,
		RecordedOrigin:   record.ProviderThreadOrigin,
		BindingRunnerID:  scope.credential.Runner.RunnerID,
		Coordinator:      coordinator,
	}
	if named {
		decision.RunnerThreadID = thread
	}
	if decision.resumeThread() {
		return conversationResumeResource{ThreadID: thread, ThreadOrigin: conversation.ThreadOriginFor(coordinator)}, nil
	}
	handed := make(map[string]struct{}, len(pending))
	for _, control := range pending {
		handed[control.MessageID] = struct{}{}
	}
	messages, err := c.queryMessages(ctx, tx, conversationMessageQuery+"conversation_id = ? ORDER BY seq DESC LIMIT ?",
		record.ID, conversationTranscriptMessages+len(pending))
	if err != nil {
		return conversationResumeResource{}, err
	}
	transcript := make([]conversationTranscriptEntry, 0, conversationTranscriptMessages)
	for _, message := range messages {
		if len(transcript) >= conversationTranscriptMessages {
			break
		}
		if _, pendingControl := handed[message.ID]; pendingControl {
			continue
		}
		transcript = append(transcript, conversationTranscriptEntry{
			Role: message.Role, Kind: message.Kind, Text: boundRunes(message.Text, conversationTranscriptRunes),
		})
	}
	slices.Reverse(transcript)
	// The runner starts a fresh thread from this transcript; its origin is
	// the kind of turn that is binding.
	return conversationResumeResource{ThreadOrigin: conversation.ThreadOriginFor(coordinator), Transcript: transcript}, nil
}

// conversationResumeKind reports what the bind handed the runner, for the
// execution resource's resume field (decisions section 10.4).
func conversationResumeKind(resume conversationResumeResource) string {
	switch {
	case resume.ThreadID != "":
		return conversation.ResumeThread
	case len(resume.Transcript) > 0:
		return conversation.ResumeTranscript
	default:
		return ""
	}
}

// conversationTranscriptNotice is the exact copy the history records when a
// runner continues from a transcript instead of a resumable provider thread
// (decisions section 10.4).
func conversationTranscriptNotice(messages int) string {
	return fmt.Sprintf("Provider history was not available on this runner; continuing from a transcript of the last %d messages", messages)
}

// loadWorkerConversation resolves a conversation for a lease-fenced worker
// request and reports the work item whose lease the caller must hold: the
// linked issue, or the open coordinator item of a private chat. Only the
// coordinator path skips the read rule, because the hub itself created that
// item for this conversation and the lease is the runner's authority. The
// third result reports that the item is a coordinator item, which is the
// origin a thread this request opens is recorded under (decisions section
// 9.3).
func (c *conversationService) loadWorkerConversation(ctx context.Context, tx *sql.Tx, scope nativeScope, id string) (conversationRecord, string, bool, error) {
	if err := conversation.ValidateConversationID(id); err != nil {
		return conversationRecord{}, "", false, nativeNotFound()
	}
	record, err := c.store.readConversation(ctx, tx, scope.organization, scope.project, id)
	if err != nil {
		return conversationRecord{}, "", false, translateConversationError(err)
	}
	if record.WorkItemID == "" {
		item, open, err := c.store.openCoordinatorItem(ctx, tx, record.ID)
		if err != nil {
			return conversationRecord{}, "", false, err
		}
		if open {
			return record, item.WorkItemID, true, nil
		}
	}
	if err := c.authorizeRead(scope, record); err != nil {
		return conversationRecord{}, "", false, err
	}
	return record, record.WorkItemID, false, nil
}
