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

	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/skills"
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

// errCoordinatorProjectUnreadable ends every tool call once the
// conversation's owner has lost read access to its project.
var errCoordinatorProjectUnreadable = errors.New("the conversation owner can no longer read this project")

// coordinatorToolset executes the coordination tools for one turn.
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
	return append([]runner.AgentTool{
		coordinatorTool("read_skill", "Read one built-in Detent skill's guidance by name or alias from the Available skills catalog.", `{"type":"object","required":["name"],"properties":{"name":{"type":"string"}},"additionalProperties":false}`),
		coordinatorTool(coordinatorToolListAttention,
			"List issues that need attention, grouped as blocked, waiting_for_input, running and review. Scope is this conversation's project or every project the user can read.",
			`{"type":"object","properties":{"scope":{"type":"string","enum":["project","all_projects"],"description":"project (default) or all_projects"},"limit":{"type":"integer","minimum":1,"maximum":50,"description":"Maximum issues per group, default 20"}},"additionalProperties":false}`),
		coordinatorTool(coordinatorToolExplainIssue,
			"Explain one issue, including Done and other terminal issues: title, body, workflow state, latest attempt, recent comments and its linked conversation. Resolve issue numbers within this conversation's project and return the native work_item_id for action proposals.",
			`{"type":"object","required":["work_item_id"],"properties":{"work_item_id":{"type":"string","description":"Issue number (#19 or 19) in this conversation's project, or native work item identifier (wi_...)"}},"additionalProperties":false}`),
		coordinatorTool("read_issue_history", "Read current issue records with pagination, including Done and terminal issues. Supply work_item_id to resolve an issue number in this conversation's project; otherwise defaults to the attached issue. Sections include full body, comments, lane and phase history with reasons, attempts with outcomes, dependencies and PR state.", `{"type":"object","required":["section"],"properties":{"work_item_id":{"type":"string","description":"Issue number (#19 or 19) or native work item identifier (wi_...) in this conversation's project; defaults to the attached issue"},"section":{"type":"string","enum":["work_item","work_comments","work_history","work_runs","work_relationships","work_references","work_attempt_receipt"]},"cursor":{"type":"string"},"offset":{"type":"integer"},"limit":{"type":"integer","minimum":1,"maximum":200},"native_attempt_id":{"type":"string"}},"additionalProperties":false}`),
		coordinatorIssueSplitTool(),
		coordinatorTool("load_split_issue_skill", "Load the split-issue skill before proposing an issue decomposition.", `{"type":"object","properties":{},"additionalProperties":false}`),
		coordinatorTool(coordinatorToolProposeIssue,
			"Propose a new issue for the user to confirm. This never creates the issue; it prepares a card the user can accept in the app.",
			`{"type":"object","required":["title","objective"],"properties":{"title":{"type":"string","maxLength":500},"objective":{"type":"string","maxLength":4000,"description":"What the issue should achieve, in Markdown"},"project_id":{"type":"string","description":"Target project; defaults to this conversation's project"}},"additionalProperties":false}`),
	}, append(append(coordinatorActionTools(), coordinatorSpriteTools()...), coordinatorRunnerTools()...)...)
}

// handle runs one tool call. Errors are returned to the model as
// {"error": ...} and logged; they never end the turn.
func (t *coordinatorToolset) handle(ctx context.Context, call runner.AgentToolCall) (runner.AgentToolResult, error) {
	result, err := t.execute(ctx, call)
	if err != nil {
		if call.Name == "get_runners" || call.Name == "set_runner_tier" || coordinatorSpriteTool(call.Name) || call.Name == "get_project_integration" || call.Name == "update_project_integration" || call.Name == "move_item" || call.Name == "edit_item" || call.Name == "add_comment" || call.Name == string(chat.ActionIssueSplit) || call.Name == string(chat.ActionArchiveItems) {
			err = coordinatorActionError(err)
		}
		if call.Name == "set_runner_tier" || coordinatorSpriteMutation(call.Name) || call.Name == "update_project_integration" || call.Name == "move_item" || call.Name == "edit_item" || call.Name == "add_comment" || call.Name == string(chat.ActionIssueSplit) || call.Name == string(chat.ActionArchiveItems) {
			if postErr := t.postActionRefusal(ctx, call.Name, err); postErr != nil {
				t.coordinator.logger.Warn("coordinator could not persist refusal", "conversation_id", t.state.conversationID, "error", postErr)
			}
		}
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
	message := err.Error()
	var native *nativeError
	if errors.As(err, &native) && native.Code == "invalid_request" {
		message = native.Message
	}
	encoded, encodeErr := json.Marshal(map[string]string{"error": boundRunes(message, coordinatorErrorRunes)})
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
	readable, err := t.readableProjects(ctx, record)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(readable, record.ProjectID) {
		return nil, errCoordinatorProjectUnreadable
	}
	switch call.Name {
	case "read_skill":
		var args struct {
			Name string `json:"name"`
		}
		if err := decodeCoordinatorArguments(call.Arguments, &args); err != nil {
			return nil, err
		}
		if strings.TrimSpace(args.Name) == "" {
			return nil, errCoordinatorToolArguments
		}
		skill, body, err := skills.ReadBuiltin(args.Name)
		if err != nil {
			return nil, err
		}
		return map[string]string{"name": skill.Name, "body": body}, nil
	case "get_runners", "set_runner_tier":
		return t.runnerTool(ctx, record, call)
	case "get_sprite_pool", "set_sprites_token", "set_sprite_pool", "scale_up_sprite_pool", "get_sprite_bootstrap_log":
		return t.spriteTool(ctx, record, call)
	case "read_issue_history":
		return t.readIssueHistory(ctx, record, call)
	case "get_project_integration", "update_project_integration", "move_item", "edit_item", "add_comment":
		return t.projectAction(ctx, record, call)
	case coordinatorToolListAttention:
		var args struct {
			Scope string `json:"scope"`
			Limit int    `json:"limit"`
		}
		if err := decodeCoordinatorArguments(call.Arguments, &args); err != nil {
			return nil, err
		}
		return t.listAttention(ctx, record, readable, args.Scope, args.Limit)
	case coordinatorToolExplainIssue:
		var args struct {
			WorkItemID string `json:"work_item_id"`
		}
		if err := decodeCoordinatorArguments(call.Arguments, &args); err != nil {
			return nil, err
		}
		return t.explainIssue(ctx, record, readable, args.WorkItemID)
	case "load_split_issue_skill":
		return t.loadSplitIssueSkill(call.Arguments)
	case string(chat.ActionArchiveItems):
		return t.proposeIssueArchive(ctx, record, call)
	case string(chat.ActionIssueSplit):
		return t.proposeIssueSplit(ctx, record, call)
	case coordinatorToolProposeIssue:
		var args struct {
			Title     string `json:"title"`
			Objective string `json:"objective"`
			ProjectID string `json:"project_id"`
		}
		if err := decodeCoordinatorArguments(call.Arguments, &args); err != nil {
			return nil, err
		}
		return t.proposeIssue(ctx, record, readable, args.Title, args.Objective, args.ProjectID)
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
// the owner can read right now, by the rules the conversation API applies to
// the owner's own requests: hosted grants of an active member for a hosted
// owner; otherwise the owner token's grants, or every project for an
// instance administrator token. A revoked or expired token reads nothing.
// The conversation's own project is not assumed: a grant withdrawn after the
// conversation started is honored on the next tool call.
func (t *coordinatorToolset) readableProjects(ctx context.Context, record conversationRecord) ([]tracker.ProjectID, error) {
	db := t.coordinator.service.store.db
	query := `SELECT g.project_id FROM hosted_project_grants g JOIN hosted_members m ON m.user_id = g.user_id
WHERE m.user_id = ? AND m.active = 1 AND g.organization_id = ?`
	args := []any{record.OwnerSubject, record.OrganizationID}
	if record.OwnerSubject == "" {
		var scope, createdAt string
		var nativeOnly bool
		var revokedAt, expiresAt sql.NullString
		err := db.QueryRowContext(ctx, "SELECT scope, native_only, revoked_at, expires_at, created_at FROM api_tokens WHERE id = ?", record.OwnerPrincipalID).
			Scan(&scope, &nativeOnly, &revokedAt, &expiresAt, &createdAt)
		if errors.Is(err, sql.ErrNoRows) {
			return []tracker.ProjectID{}, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read conversation owner: %w", err)
		}
		if revokedAt.Valid {
			return []tracker.ProjectID{}, nil
		}
		if expiresAt.Valid {
			now, err := t.coordinator.service.server.database.currentTime()
			if err != nil {
				return nil, err
			}
			if !runnerTimeValid(now, createdAt, expiresAt.String) {
				return []tracker.ProjectID{}, nil
			}
		}
		query = "SELECT project_id FROM token_grants WHERE token_id = ? AND organization_id = ?"
		args = []any{record.OwnerPrincipalID, record.OrganizationID}
		if apiScope(scope) == apiScopeAdmin && !nativeOnly {
			query = "SELECT id FROM projects WHERE organization_id = ?"
			args = []any{record.OrganizationID}
		}
	}
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list readable projects: %w", err)
	}
	defer func() { _ = rows.Close() }()
	projects := []tracker.ProjectID{}
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
func (t *coordinatorToolset) listAttention(ctx context.Context, record conversationRecord, readable []tracker.ProjectID, scope string, limit int) (any, error) {
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
		projects = readable
	}
	projectJSON, err := projectList(projects)
	if err != nil {
		return nil, err
	}
	// Blockers are named only when the owner can read them: a dependency in
	// another project must not reveal that project's issues.
	readableJSON, err := projectList(readable)
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
   WHERE d.dependent_issue_id = i.id AND COALESCE(bs.terminal, 0) = 0
    AND b.organization_id = i.organization_id AND b.project_id IN (SELECT value FROM json_each(?))), '')
FROM issues i LEFT JOIN workflow_states ws ON ws.id = i.workflow_state_id
WHERE i.organization_id = ? AND i.project_id IN (SELECT value FROM json_each(?)) AND COALESCE(ws.terminal, 0) = 0
ORDER BY i.native_updated_at DESC, i.native_id LIMIT ?`, readableJSON, record.OrganizationID, projectJSON, coordinatorAttentionScan+1)
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
		item.URL = "/work/i/" + item.WorkItemID
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

func (t *coordinatorToolset) resolveIssueReference(ctx context.Context, record conversationRecord, reference string) (string, error) {
	reference = strings.TrimSpace(reference)
	if reference == "" || strings.HasPrefix(reference, "wi_") {
		return reference, nil
	}
	s := t.coordinator.service.server
	issue, err := s.resolveOperatorNativeItem(ctx, s.database.db, nativeScope{organization: record.OrganizationID, project: record.ProjectID}, reference)
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, explain.ErrNotFound) {
		return "", fmt.Errorf("no issue %s in this project", "#"+strings.TrimPrefix(reference, "#"))
	}
	if err != nil {
		return "", err
	}
	return string(issue.WorkItemID), nil
}

// explainIssue describes one issue in a project the owner can read.
func (t *coordinatorToolset) explainIssue(ctx context.Context, record conversationRecord, readable []tracker.ProjectID, workItemID string) (any, error) {
	workItemID, err := t.resolveIssueReference(ctx, record, workItemID)
	if err != nil {
		return nil, err
	}
	if workItemID == "" {
		return nil, fmt.Errorf("%w: work_item_id is required", errCoordinatorToolArguments)
	}
	projectJSON, err := projectList(readable)
	if err != nil {
		return nil, err
	}
	db := t.coordinator.service.store.db
	issue := coordinatorIssue{Comments: []coordinatorComment{}}
	err = db.QueryRowContext(ctx, `SELECT i.native_id, i.project_id, COALESCE(i.number, 0), i.title, i.body, COALESCE(ws.detent_state, ''), COALESCE(ws.terminal, 0), i.native_updated_at
FROM issues i LEFT JOIN workflow_states ws ON ws.id = i.workflow_state_id
WHERE i.organization_id = ? AND i.project_id IN (SELECT value FROM json_each(?)) AND i.native_id = ?`, record.OrganizationID, projectJSON, workItemID).Scan(
		&issue.WorkItemID, &issue.ProjectID, &issue.Number, &issue.Title, &issue.Body, &issue.State, &issue.Terminal, &issue.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("work item %s is not readable in this conversation", workItemID)
	}
	if err != nil {
		return nil, fmt.Errorf("read issue: %w", err)
	}
	issue.Body = boundRunes(issue.Body, coordinatorIssueBodyRunes)
	issue.URL = "/work/i/" + issue.WorkItemID
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
func (t *coordinatorToolset) proposeIssue(ctx context.Context, record conversationRecord, readable []tracker.ProjectID, title, objective, projectID string) (any, error) {
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
	if !slices.Contains(readable, target) {
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
