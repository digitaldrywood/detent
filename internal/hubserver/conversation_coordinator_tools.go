package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// Bounds for coordinator tool arguments and results.
const (
	coordinatorToolArgumentBytes  = 16 << 10
	coordinatorToolResultBytes    = 64 << 10
	coordinatorAttentionDefault   = 20
	coordinatorAttentionMax       = 50
	coordinatorAttentionScan      = 500
	coordinatorIssueBodyRunes     = 4000
	coordinatorCommentRunes       = 1000
	coordinatorCommentCount       = 5
	coordinatorProposalTitleBytes = 500
	coordinatorProposalBodyRunes  = 4000
)

const (
	coordinatorToolListAttention = "list_attention"
	coordinatorToolExplainIssue  = "explain_issue"
	coordinatorToolProposeIssue  = "propose_issue"
)

var errCoordinatorToolArguments = errors.New("invalid tool arguments")

// coordinatorToolset executes the read-only coordination tools for one turn.
// Every query is scoped to the conversation's organization; project access
// follows the conversation owner's grants.
type coordinatorToolset struct {
	coordinator *conversationTurnCoordinator
	state       *coordinatorTurnState
}

func newCoordinatorToolset(coordinator *conversationTurnCoordinator, state *coordinatorTurnState) *coordinatorToolset {
	return &coordinatorToolset{coordinator: coordinator, state: state}
}

func coordinatorTool(name, description, schema string) runner.AgentTool {
	return runner.AgentTool{Name: name, Description: description, InputSchema: json.RawMessage(schema)}
}

func (t *coordinatorToolset) tools() []runner.AgentTool {
	return []runner.AgentTool{
		coordinatorTool(coordinatorToolListAttention,
			"List issues that need attention, grouped as blocked, waiting_for_input, running and review. Scope is this conversation's project or every project the user can read.",
			`{"type":"object","properties":{"scope":{"type":"string","enum":["project","all_projects"],"description":"project (default) or all_projects"},"limit":{"type":"integer","minimum":1,"maximum":50,"description":"Maximum issues per group, default 20"}},"additionalProperties":false}`),
		coordinatorTool(coordinatorToolExplainIssue,
			"Explain one issue: title, body, workflow state, latest attempt, recent comments and its linked conversation.",
			`{"type":"object","required":["work_item_id"],"properties":{"work_item_id":{"type":"string","description":"Work item identifier (wi_...)"}},"additionalProperties":false}`),
		coordinatorTool(coordinatorToolProposeIssue,
			"Propose a new issue for the user to confirm. This never creates the issue; it prepares a card the user can accept in the app.",
			`{"type":"object","required":["title","objective"],"properties":{"title":{"type":"string","maxLength":500},"objective":{"type":"string","maxLength":4000,"description":"What the issue should achieve, in Markdown"},"project_id":{"type":"string","description":"Target project; defaults to this conversation's project"}},"additionalProperties":false}`),
	}
}

// handle runs one tool call. Errors are returned to the model as
// {"error": ...} and logged; they never end the turn.
func (t *coordinatorToolset) handle(ctx context.Context, call runner.AgentToolCall) (runner.AgentToolResult, error) {
	result, err := t.execute(ctx, call)
	if err != nil {
		t.coordinator.logger.Warn("coordinator tool failed", "conversation_id", t.state.conversationID, "tool", call.Name, "error", err)
		return coordinatorToolError(err), nil
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return coordinatorToolError(fmt.Errorf("encode %s result: %w", call.Name, err)), nil
	}
	if len(encoded) > coordinatorToolResultBytes {
		return coordinatorToolError(fmt.Errorf("%s result exceeds %d bytes; narrow the request", call.Name, coordinatorToolResultBytes)), nil
	}
	return runner.AgentToolResult{Content: string(encoded), Success: true}, nil
}

func coordinatorToolError(err error) runner.AgentToolResult {
	encoded, encodeErr := json.Marshal(map[string]string{"error": boundRunes(err.Error(), coordinatorErrorRunes)})
	if encodeErr != nil {
		encoded = []byte(`{"error":"tool failed"}`)
	}
	return runner.AgentToolResult{Content: string(encoded), Success: false}
}

func (t *coordinatorToolset) execute(ctx context.Context, call runner.AgentToolCall) (any, error) {
	record, err := t.coordinator.readConversation(ctx, t.coordinator.service.store.db, t.state.conversationID)
	if err != nil {
		return nil, err
	}
	switch call.Name {
	case coordinatorToolListAttention:
		var args struct {
			Scope string `json:"scope"`
			Limit int    `json:"limit"`
		}
		if err := decodeCoordinatorArguments(call.Arguments, &args); err != nil {
			return nil, err
		}
		return t.listAttention(ctx, record, args.Scope, args.Limit)
	case coordinatorToolExplainIssue:
		var args struct {
			WorkItemID string `json:"work_item_id"`
		}
		if err := decodeCoordinatorArguments(call.Arguments, &args); err != nil {
			return nil, err
		}
		return t.explainIssue(ctx, record, args.WorkItemID)
	case coordinatorToolProposeIssue:
		var args struct {
			Title     string `json:"title"`
			Objective string `json:"objective"`
			ProjectID string `json:"project_id"`
		}
		if err := decodeCoordinatorArguments(call.Arguments, &args); err != nil {
			return nil, err
		}
		return t.proposeIssue(ctx, record, args.Title, args.Objective, args.ProjectID)
	default:
		return nil, fmt.Errorf("unknown tool %q", call.Name)
	}
}

func decodeCoordinatorArguments(raw json.RawMessage, target any) error {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		raw = json.RawMessage(`{}`)
	}
	if len(raw) > coordinatorToolArgumentBytes {
		return fmt.Errorf("%w: payload exceeds %d bytes", errCoordinatorToolArguments, coordinatorToolArgumentBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("%w: %w", errCoordinatorToolArguments, err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: trailing content", errCoordinatorToolArguments)
	}
	return nil
}

// readableProjects returns the projects in the conversation's organization
// the owner can read: hosted grants for the owner subject, token grants for
// the owner principal, and always the conversation's own project.
func (t *coordinatorToolset) readableProjects(ctx context.Context, record conversationRecord) ([]tracker.ProjectID, error) {
	projects := []tracker.ProjectID{record.ProjectID}
	db := t.coordinator.service.store.db
	// A revoked token grants nothing, the same rule the conversation API
	// applies on every request.
	rows, err := db.QueryContext(ctx, `SELECT g.project_id FROM hosted_project_grants g JOIN hosted_members m ON m.user_id = g.user_id
WHERE ? != '' AND m.user_id = ? AND m.active = 1 AND g.organization_id = ?
UNION SELECT g.project_id FROM token_grants g JOIN api_tokens t ON t.id = g.token_id
WHERE g.token_id = ? AND g.organization_id = ? AND t.revoked_at IS NULL`, record.OwnerSubject, record.OwnerSubject, record.OrganizationID, record.OwnerPrincipalID, record.OrganizationID)
	if err != nil {
		return nil, fmt.Errorf("list readable projects: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var project tracker.ProjectID
		if err := rows.Scan(&project); err != nil {
			return nil, fmt.Errorf("list readable projects: %w", err)
		}
		if !slices.Contains(projects, project) {
			projects = append(projects, project)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list readable projects: %w", err)
	}
	return projects, nil
}

// projectList encodes project identifiers for an IN (SELECT value FROM
// json_each(?)) clause so the SQL text stays static.
func projectList(projects []tracker.ProjectID) (string, error) {
	encoded, err := json.Marshal(projects)
	if err != nil {
		return "", fmt.Errorf("encode project list: %w", err)
	}
	return string(encoded), nil
}

type coordinatorAttentionItem struct {
	WorkItemID string            `json:"work_item_id"`
	Number     int64             `json:"number"`
	Title      string            `json:"title"`
	State      string            `json:"state"`
	ProjectID  tracker.ProjectID `json:"project_id"`
	Reason     string            `json:"reason"`
	UpdatedAt  string            `json:"updated_at"`
	URL        string            `json:"url"`
}

type coordinatorAttention struct {
	Scope           string                     `json:"scope"`
	Projects        []tracker.ProjectID        `json:"projects"`
	Blocked         []coordinatorAttentionItem `json:"blocked"`
	WaitingForInput []coordinatorAttentionItem `json:"waiting_for_input"`
	Running         []coordinatorAttentionItem `json:"running"`
	Review          []coordinatorAttentionItem `json:"review"`
	Unavailable     []coordinatorAttentionItem `json:"unavailable"`
	Truncated       bool                       `json:"truncated"`
}

// listAttention groups open issues by what they wait for. Classification
// order: a pending question, a live lease, no workflow state (unavailable),
// a non-terminal dependency or blocked state, a review state.
func (t *coordinatorToolset) listAttention(ctx context.Context, record conversationRecord, scope string, limit int) (any, error) {
	switch scope {
	case "", "project":
		scope = "project"
	case "all_projects":
	default:
		return nil, fmt.Errorf("%w: scope must be project or all_projects", errCoordinatorToolArguments)
	}
	if limit <= 0 {
		limit = coordinatorAttentionDefault
	}
	if limit > coordinatorAttentionMax {
		limit = coordinatorAttentionMax
	}
	projects := []tracker.ProjectID{record.ProjectID}
	if scope == "all_projects" {
		var err error
		if projects, err = t.readableProjects(ctx, record); err != nil {
			return nil, err
		}
	}
	projectJSON, err := projectList(projects)
	if err != nil {
		return nil, err
	}
	db := t.coordinator.service.store.db
	now, err := t.coordinator.service.server.database.currentTime()
	if err != nil {
		return nil, err
	}
	running, err := runningAttempts(ctx, db, record.OrganizationID, projectJSON, now)
	if err != nil {
		return nil, err
	}
	waiting, err := waitingQuestions(ctx, db, record.OrganizationID, projectJSON)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT i.native_id, i.project_id, COALESCE(i.number, 0), i.title, COALESCE(ws.detent_state, ''), i.workflow_state_id IS NULL, i.native_updated_at,
 COALESCE((SELECT group_concat(b.native_id, ' ') FROM issue_dependencies d JOIN issues b ON b.id = d.blocker_issue_id LEFT JOIN workflow_states bs ON bs.id = b.workflow_state_id
   WHERE d.dependent_issue_id = i.id AND COALESCE(bs.terminal, 0) = 0), '')
FROM issues i LEFT JOIN workflow_states ws ON ws.id = i.workflow_state_id
WHERE i.organization_id = ? AND i.project_id IN (SELECT value FROM json_each(?)) AND COALESCE(ws.terminal, 0) = 0
 AND `+notCoordinatorItemClause+`
ORDER BY i.native_updated_at DESC, i.native_id LIMIT ?`, record.OrganizationID, projectJSON, coordinatorAttentionScan+1)
	if err != nil {
		return nil, fmt.Errorf("list issues: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := coordinatorAttention{Scope: scope, Projects: projects, Blocked: []coordinatorAttentionItem{}, WaitingForInput: []coordinatorAttentionItem{}, Running: []coordinatorAttentionItem{}, Review: []coordinatorAttentionItem{}, Unavailable: []coordinatorAttentionItem{}}
	scanned := 0
	for rows.Next() {
		var item coordinatorAttentionItem
		var unmapped bool
		var blockers string
		if err := rows.Scan(&item.WorkItemID, &item.ProjectID, &item.Number, &item.Title, &item.State, &unmapped, &item.UpdatedAt, &blockers); err != nil {
			return nil, fmt.Errorf("list issues: %w", err)
		}
		scanned++
		if scanned > coordinatorAttentionScan {
			result.Truncated = true
			break
		}
		item.URL = "/chat/issues/" + item.WorkItemID
		item.Title = boundRunes(item.Title, coordinatorToolSummaryRunes)
		state := strings.ToLower(item.State)
		var group *[]coordinatorAttentionItem
		switch {
		case waiting[item.WorkItemID]:
			item.Reason = "a runner question is waiting for an answer"
			group = &result.WaitingForInput
		case running[item.WorkItemID] != "":
			item.Reason = "attempt " + running[item.WorkItemID] + " holds a live lease"
			group = &result.Running
		case unmapped:
			item.Reason = "no workflow state mapping"
			group = &result.Unavailable
		case blockers != "":
			item.Reason = "blocked by " + strings.Join(strings.Fields(blockers), ", ")
			group = &result.Blocked
		case strings.Contains(state, "block"):
			item.Reason = "workflow state " + item.State
			group = &result.Blocked
		case strings.Contains(state, "review"):
			item.Reason = "workflow state " + item.State
			group = &result.Review
		default:
			continue
		}
		if len(*group) >= limit {
			result.Truncated = true
			continue
		}
		*group = append(*group, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list issues: %w", err)
	}
	return result, nil
}

// runningAttempts maps work items to the attempt whose lease is unreleased
// and unexpired.
func runningAttempts(ctx context.Context, db *sql.DB, organization tracker.OrganizationID, projectJSON string, now time.Time) (map[string]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT a.work_item_id, a.id, l.expires_at FROM native_attempts a JOIN leases l ON l.lease_id = a.lease_id
WHERE a.organization_id = ? AND a.project_id IN (SELECT value FROM json_each(?)) AND a.status = 'running' AND l.released_at IS NULL`, organization, projectJSON)
	if err != nil {
		return nil, fmt.Errorf("list running attempts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	running := map[string]string{}
	for rows.Next() {
		var workItem, attempt, expires string
		if err := rows.Scan(&workItem, &attempt, &expires); err != nil {
			return nil, fmt.Errorf("list running attempts: %w", err)
		}
		expiry, err := parseTimeValue(expires)
		if err != nil {
			return nil, fmt.Errorf("decode lease expiry: %w", err)
		}
		if now.Before(expiry) {
			running[workItem] = attempt
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list running attempts: %w", err)
	}
	return running, nil
}

// waitingQuestions reports the work items whose linked conversation has a
// question that still awaits an answer.
func waitingQuestions(ctx context.Context, db *sql.DB, organization tracker.OrganizationID, projectJSON string) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT c.work_item_id FROM conversation_questions q JOIN conversations c ON c.id = q.conversation_id
WHERE c.organization_id = ? AND c.project_id IN (SELECT value FROM json_each(?)) AND c.work_item_id IS NOT NULL AND q.status IN ('pending', 'sending', 'sent')`, organization, projectJSON)
	if err != nil {
		return nil, fmt.Errorf("list pending questions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	waiting := map[string]bool{}
	for rows.Next() {
		var workItem string
		if err := rows.Scan(&workItem); err != nil {
			return nil, fmt.Errorf("list pending questions: %w", err)
		}
		waiting[workItem] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list pending questions: %w", err)
	}
	return waiting, nil
}

type coordinatorAttempt struct {
	AttemptID string `json:"attempt_id"`
	Status    string `json:"status"`
	Outcome   string `json:"outcome"`
	StartedAt string `json:"started_at"`
	UpdatedAt string `json:"updated_at"`
}

type coordinatorComment struct {
	Author    string `json:"author"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
}

type coordinatorIssue struct {
	WorkItemID     string               `json:"work_item_id"`
	Number         int64                `json:"number"`
	ProjectID      tracker.ProjectID    `json:"project_id"`
	Title          string               `json:"title"`
	Body           string               `json:"body"`
	State          string               `json:"state"`
	Terminal       bool                 `json:"terminal"`
	UpdatedAt      string               `json:"updated_at"`
	LatestAttempt  *coordinatorAttempt  `json:"latest_attempt"`
	Comments       []coordinatorComment `json:"comments"`
	ConversationID *string              `json:"conversation_id"`
	URL            string               `json:"url"`
}

// explainIssue describes one issue in a project the owner can read.
func (t *coordinatorToolset) explainIssue(ctx context.Context, record conversationRecord, workItemID string) (any, error) {
	workItemID = strings.TrimSpace(workItemID)
	if workItemID == "" {
		return nil, fmt.Errorf("%w: work_item_id is required", errCoordinatorToolArguments)
	}
	projects, err := t.readableProjects(ctx, record)
	if err != nil {
		return nil, err
	}
	projectJSON, err := projectList(projects)
	if err != nil {
		return nil, err
	}
	db := t.coordinator.service.store.db
	issue := coordinatorIssue{Comments: []coordinatorComment{}}
	// A coordinator item carries someone's private chat and is not project
	// work; it is invisible here for the same reason it is invisible in the
	// issue list (decisions sections 9.2 and 10.1).
	err = db.QueryRowContext(ctx, `SELECT i.native_id, i.project_id, COALESCE(i.number, 0), i.title, i.body, COALESCE(ws.detent_state, ''), COALESCE(ws.terminal, 0), i.native_updated_at
FROM issues i LEFT JOIN workflow_states ws ON ws.id = i.workflow_state_id
WHERE i.organization_id = ? AND i.project_id IN (SELECT value FROM json_each(?)) AND i.native_id = ?
 AND `+notCoordinatorItemClause, record.OrganizationID, projectJSON, workItemID).Scan(
		&issue.WorkItemID, &issue.ProjectID, &issue.Number, &issue.Title, &issue.Body, &issue.State, &issue.Terminal, &issue.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("work item %s is not readable in this conversation", workItemID)
	}
	if err != nil {
		return nil, fmt.Errorf("read issue: %w", err)
	}
	issue.Body = boundRunes(issue.Body, coordinatorIssueBodyRunes)
	issue.URL = "/chat/issues/" + issue.WorkItemID
	now, err := t.coordinator.service.server.database.currentTime()
	if err != nil {
		return nil, err
	}
	var attempt coordinatorAttempt
	var data, expires string
	var released sql.NullString
	err = db.QueryRowContext(ctx, `SELECT a.id, a.status, a.data_json, a.started_at, a.updated_at, l.expires_at, l.released_at
FROM native_attempts a JOIN leases l ON l.lease_id = a.lease_id
WHERE a.organization_id = ? AND a.project_id = ? AND a.work_item_id = ? ORDER BY a.fencing_token DESC LIMIT 1`, record.OrganizationID, issue.ProjectID, issue.WorkItemID).Scan(
		&attempt.AttemptID, &attempt.Status, &data, &attempt.StartedAt, &attempt.UpdatedAt, &expires, &released)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return nil, fmt.Errorf("read latest attempt: %w", err)
	default:
		var run tracker.NativeRunData
		if err := json.Unmarshal([]byte(data), &run); err != nil {
			return nil, fmt.Errorf("decode attempt: %w", err)
		}
		attempt.Outcome = run.Outcome
		expiry, err := parseTimeValue(expires)
		if err != nil {
			return nil, fmt.Errorf("decode lease expiry: %w", err)
		}
		if attempt.Status == "running" && (released.Valid || !now.Before(expiry)) {
			attempt.Status = "interrupted"
		}
		issue.LatestAttempt = &attempt
	}
	rows, err := db.QueryContext(ctx, "SELECT actor_json, body, created_at FROM native_comments WHERE organization_id = ? AND project_id = ? AND work_item_id = ? ORDER BY sequence DESC LIMIT ?", record.OrganizationID, issue.ProjectID, issue.WorkItemID, coordinatorCommentCount)
	if err != nil {
		return nil, fmt.Errorf("list comments: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var actorJSON string
		var comment coordinatorComment
		if err := rows.Scan(&actorJSON, &comment.Body, &comment.CreatedAt); err != nil {
			return nil, fmt.Errorf("list comments: %w", err)
		}
		var actor tracker.Actor
		if err := json.Unmarshal([]byte(actorJSON), &actor); err == nil {
			comment.Author = strings.TrimSpace(actor.Kind + " " + actor.PrincipalID)
		}
		comment.Body = boundRunes(comment.Body, coordinatorCommentRunes)
		issue.Comments = append(issue.Comments, comment)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list comments: %w", err)
	}
	slices.Reverse(issue.Comments)
	var linked string
	err = db.QueryRowContext(ctx, "SELECT id FROM conversations WHERE organization_id = ? AND project_id = ? AND work_item_id = ?", record.OrganizationID, issue.ProjectID, issue.WorkItemID).Scan(&linked)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return nil, fmt.Errorf("read linked conversation: %w", err)
	default:
		issue.ConversationID = &linked
	}
	return issue, nil
}

type coordinatorProposal struct {
	ProjectID tracker.ProjectID `json:"project_id"`
	Title     string            `json:"title"`
	Objective string            `json:"objective"`
}

// proposeIssue validates a proposal and records it as an assistant status
// message so the client can render a confirmation card. It creates nothing.
func (t *coordinatorToolset) proposeIssue(ctx context.Context, record conversationRecord, title, objective, projectID string) (any, error) {
	title = strings.TrimSpace(title)
	objective = strings.TrimSpace(objective)
	if title == "" || len(title) > coordinatorProposalTitleBytes {
		return nil, fmt.Errorf("%w: title must contain 1 to %d bytes", errCoordinatorToolArguments, coordinatorProposalTitleBytes)
	}
	if objective == "" {
		return nil, fmt.Errorf("%w: objective is required", errCoordinatorToolArguments)
	}
	objective = boundRunes(objective, coordinatorProposalBodyRunes)
	target := record.ProjectID
	if strings.TrimSpace(projectID) != "" {
		target = tracker.ProjectID(strings.TrimSpace(projectID))
	}
	projects, err := t.readableProjects(ctx, record)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(projects, target) {
		return nil, fmt.Errorf("project %s is not readable in this conversation", target)
	}
	proposal := coordinatorProposal{ProjectID: target, Title: title, Objective: objective}
	data, err := json.Marshal(map[string]any{"proposal": proposal})
	if err != nil {
		return nil, fmt.Errorf("encode proposal: %w", err)
	}
	t.state.mu.Lock()
	defer t.state.mu.Unlock()
	err = t.coordinator.write(ctx, record.ID, func(ctx context.Context, tx *sql.Tx, fresh *conversationRecord, now time.Time) error {
		message := conversationMessageRecord{
			Role:     conversation.RoleAssistant,
			Kind:     conversation.MessageStatus,
			Text:     "Proposed issue: " + title,
			Data:     data,
			Delivery: conversation.DeliveryCompleted,
			ThreadID: t.state.threadID,
			Actor:    conversation.Actor{Kind: conversation.ActorCoordinator},
		}
		return t.coordinator.service.appendMessage(ctx, tx, fresh, &message, now)
	})
	if err != nil {
		return nil, fmt.Errorf("record proposal: %w", err)
	}
	return map[string]any{"proposal": proposal}, nil
}
