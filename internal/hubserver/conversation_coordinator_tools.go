package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/skills"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// Bounds for coordinator tool arguments and results.
const (
	coordinatorToolArgumentBytes     = 16 << 10
	coordinatorWorkflowArgumentBytes = 6*(128<<10) + coordinatorToolArgumentBytes
	coordinatorToolResultBytes       = 64 << 10
	coordinatorAttentionDefault      = 20
	coordinatorAttentionMax          = 50
	coordinatorAttentionScan         = 500
	coordinatorIssueBodyRunes        = 4000
	coordinatorCommentRunes          = 1000
	coordinatorCommentCount          = 5
	coordinatorProposalTitleBytes    = 500
	coordinatorProposalBodyRunes     = 4000
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
	}, append(append(append(append(append(append(append(coordinatorActionTools(), coordinatorWorkflowTools()...), coordinatorSlashActionTools()...), coordinatorSpriteTools()...), coordinatorRunnerTools()...), coordinatorScreenTools()...), coordinatorSettingsTools()...), coordinatorCatalogTools()...)...)
}

// handle runs one tool call. Errors are returned to the model as
// {"error": ...} and logged; they never end the turn.
func (t *coordinatorToolset) handle(ctx context.Context, call runner.AgentToolCall) (runner.AgentToolResult, error) {
	if server := t.coordinator.service.server; server != nil {
		ctx = server.runDetailContext(ctx)
	}
	t.state.endTextSegment(ctx)
	result, err := t.execute(ctx, call)
	if err != nil {
		if coordinatorWorkflowTool(call.Name) || call.Name == "get_runners" || call.Name == "set_runner_tier" || coordinatorSpriteTool(call.Name) || call.Name == "get_project_integration" || call.Name == "update_project_integration" || call.Name == "move_item" || call.Name == "edit_item" || call.Name == "add_comment" || call.Name == string(chat.ActionIssueSplit) || call.Name == string(chat.ActionArchiveItems) || call.Name == coordinatorToolCallTool {
			err = coordinatorActionError(err)
		}
		if coordinatorWorkflowMutation(call.Name) || call.Name == "set_runner_tier" || coordinatorSpriteMutation(call.Name) || call.Name == "update_project_integration" || call.Name == "move_item" || call.Name == "edit_item" || call.Name == "add_comment" || call.Name == string(chat.ActionIssueSplit) || call.Name == string(chat.ActionArchiveItems) {
			if postErr := t.postActionRefusal(ctx, call.Name, err); postErr != nil {
				t.coordinator.logger.Warn("coordinator could not persist refusal", "conversation_id", t.state.conversationID, "error", postErr)
			}
		}
		failure := coordinatorToolError(err)
		t.coordinator.logger.Warn("coordinator tool failed", "conversation_id", t.state.conversationID, "tool", call.Name, "error", err, "tool_error", failure.Content)
		return failure, nil
	}
	if !coordinatorScreenRead(call.Name) {
		record, readErr := t.coordinator.readConversation(ctx, nil, t.state.conversationID)
		if readErr != nil {
			return coordinatorToolError(readErr), nil
		}
		if actorCtx, contextErr := t.actionContext(ctx, record); contextErr == nil {
			credential, authErr := currentHubOperator(actorCtx)
			if authErr != nil {
				return coordinatorToolError(authErr), nil
			}
			if err := t.restrictReadAudience(actorCtx, record, credential); err != nil {
				return coordinatorToolError(err), nil
			}
		} else if record.ReadAudience != "" {
			return coordinatorToolError(contextErr), nil
		}
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return coordinatorToolError(fmt.Errorf("encode %s result: %w", call.Name, err)), nil
	}
	limit := coordinatorToolResultBytes
	if call.Name == "get_project_integration" || call.Name == "update_project_integration" {
		limit = coordinatorWorkflowArgumentBytes
	}
	if len(encoded) > limit {
		return coordinatorToolError(fmt.Errorf("%s result exceeds %d bytes; narrow the request", call.Name, limit)), nil
	}
	return runner.AgentToolResult{Content: string(encoded), Success: true}, nil
}

func coordinatorToolError(err error) runner.AgentToolResult {
	message := err.Error()
	var native *nativeError
	var request *operatortool.RequestError
	switch {
	case errors.As(err, &native) && native.Code == "invalid_request":
		message = native.Message
	case errors.As(err, &request) && request.Message != "":
		message = request.Message
	}
	encoded, encodeErr := json.Marshal(map[string]string{"error": boundRunes(message, coordinatorErrorRunes)})
	if encodeErr != nil {
		encoded = []byte(`{"error":"tool failed"}`)
	}
	return runner.AgentToolResult{Content: string(encoded), Success: false}
}

func (t *coordinatorToolset) execute(ctx context.Context, call runner.AgentToolCall) (any, error) {
	record, err := t.coordinator.readConversation(ctx, nil, t.state.conversationID)
	if err != nil {
		return nil, err
	}
	if coordinatorScreenRead(call.Name) {
		return t.screenRead(ctx, record, call)
	}
	if actorCtx, contextErr := t.actionContext(ctx, record); contextErr == nil {
		credential, authErr := currentHubOperator(actorCtx)
		if authErr != nil {
			return nil, authErr
		}
		if err := t.coordinator.authorizeRecord(actorCtx, nativeScope{organization: record.OrganizationID, project: record.ProjectID, credential: credential}, record, false); err != nil {
			return nil, err
		}
		record.OwnerPrincipalID = credential.ID
		record.OwnerSubject = ""
		if credential.Hosted != nil {
			record.OwnerSubject = credential.Hosted.Subject
		}
	} else if record.ReadAudience != "" {
		return nil, contextErr
	}
	readable, err := t.readableProjects(ctx, record)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(readable, record.ProjectID) {
		return nil, errCoordinatorProjectUnreadable
	}
	if coordinatorSettingsTool(call.Name) {
		return t.settingsTool(ctx, record, call)
	}
	if coordinatorWorkflowTool(call.Name) {
		return t.workflowTool(ctx, record, call)
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
	case "get_runners", "set_runner_tier", "convert_runner_scope":
		return t.runnerTool(ctx, record, call)
	case "promote_sprites_token":
		return t.promoteSpritesTokenTool(ctx, record, call)
	case "get_sprite_pool", "set_sprites_token", "set_sprite_pool", "scale_up_sprite_pool", "get_sprite_bootstrap_log":
		return t.spriteTool(ctx, record, call)
	case "read_issue_history":
		return t.readIssueHistory(ctx, record, call)
	case "get_project_integration", "update_project_integration", "move_item", "edit_item", "add_comment":
		return t.projectAction(ctx, record, call)
	case "propose_stop_run":
		return t.proposeStopRun(ctx, record, call)
	case "propose_file_issue", "propose_maintenance_issue", "propose_move_item", "propose_set_priority", "propose_backlog_admission":
		return t.slashAction(ctx, record, call)
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
	case coordinatorToolFindTools:
		return t.findTools(ctx, record, call)
	case coordinatorToolCallTool:
		return t.callTool(ctx, record, call)
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
	return decodeCoordinatorArgumentsWithLimit(raw, target, coordinatorToolArgumentBytes)
}

func decodeCoordinatorArgumentsWithLimit(raw json.RawMessage, target any, limit int) error {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		raw = json.RawMessage(`{}`)
	}
	if len(raw) > limit {
		return fmt.Errorf("%w: payload exceeds %d bytes", errCoordinatorToolArguments, limit)
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
	actorCtx, err := t.actionContext(ctx, record)
	if err != nil {
		return nil, err
	}
	credential, err := currentHubOperator(actorCtx)
	if err != nil {
		return nil, err
	}
	projects := []tracker.ProjectID{}
	err = t.coordinator.service.server.config.TenantRepository.ReadTenant(actorCtx, record.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		if err := t.coordinator.service.server.authorizeAccessScope(actorCtx, r, nativeScope{organization: record.OrganizationID, project: record.ProjectID, credential: credential}, false); err != nil {
			return err
		}
		var after tracker.ProjectID
		for {
			var ids []tracker.ProjectID
			if credential.Hosted != nil {
				grants, err := r.AccessProjectGrants(ctx, credential.Hosted.Subject, after, 100)
				if err != nil {
					return err
				}
				for _, grant := range grants {
					ids = append(ids, grant.ProjectID)
				}
			} else if credential.Scope == apiScopeAdmin && !credential.NativeOnly {
				rows, err := r.Projects(ctx, after, 100)
				if err != nil {
					return err
				}
				for _, project := range rows {
					if project.DeletedAt == nil {
						ids = append(ids, project.ID)
					}
				}
				if len(rows) > 0 {
					after = rows[len(rows)-1].ID
				}
				allowed, err := t.coordinator.service.server.coordinatorAllowedProjects(actorCtx, r, nativeScope{organization: record.OrganizationID, credential: credential}, ids)
				if err != nil {
					return err
				}
				projects = append(projects, allowed...)
				if len(rows) < 100 {
					return nil
				}
				continue
			} else {
				grants, err := r.AccessTokenGrants(ctx, credential.ID, after, 100)
				if err != nil {
					return err
				}
				for _, grant := range grants {
					ids = append(ids, grant.ProjectID)
				}
			}
			allowed, err := t.coordinator.service.server.coordinatorAllowedProjects(actorCtx, r, nativeScope{organization: record.OrganizationID, credential: credential}, ids)
			if err != nil {
				return err
			}
			projects = append(projects, allowed...)
			if len(ids) < 100 {
				return nil
			}
			after = ids[len(ids)-1]
		}
	})
	return projects, err
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
	if scope == "" {
		scope = "project"
	}
	if scope != "project" && scope != "all_projects" {
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
	result := coordinatorAttention{Scope: scope, Projects: projects, Blocked: []coordinatorAttentionItem{}, WaitingForInput: []coordinatorAttentionItem{}, Running: []coordinatorAttentionItem{}, Review: []coordinatorAttentionItem{}, Unavailable: []coordinatorAttentionItem{}}
	s := t.coordinator.service.server
	err := s.config.TenantRepository.ReadTenant(ctx, record.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		owner, ok := r.(conversationstore.AttentionReader)
		if !ok {
			return conversationContentUnavailable()
		}
		rows, err := owner.CoordinatorAttentionItems(ctx, projects, coordinatorAttentionScan+1)
		if err != nil {
			return err
		}
		if len(rows) > coordinatorAttentionScan {
			result.Truncated = true
			rows = rows[:coordinatorAttentionScan]
		}
		org, err := r.AccessOrganization(ctx)
		if err != nil {
			return err
		}
		for _, candidate := range rows {
			row, err := r.WorkItem(ctx, candidate.ProjectID, candidate.WorkItemID)
			if err != nil {
				return err
			}
			project, err := r.Project(ctx, row.ProjectID)
			if err != nil {
				return err
			}
			item := coordinatorAttentionItem{WorkItemID: string(row.NativeID), ProjectID: row.ProjectID, Number: row.Number, Title: boundRunes(row.Title, coordinatorToolSummaryRunes), State: stringValue(row.WorkflowStateName), UpdatedAt: formatHubTime(row.NativeUpdatedAt), URL: issueBrowserPath(org.Slug, project.Slug, int(row.Number))}
			waiting := false
			linked, err := r.ConversationByWorkItem(ctx, row.ProjectID, string(row.NativeID))
			if err != nil && !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			if err == nil {
				questions, err := r.OutstandingQuestions(ctx, linked.ID, "", "", "", 1)
				if err != nil {
					return err
				}
				waiting = len(questions.Items) > 0
			}
			lease, err := r.ActiveLeaseForIssue(ctx, row.ID)
			if err != nil && !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			running := err == nil && lease.ReleasedAt == nil && s.config.now().Before(lease.ExpiresAt)
			var blockers []string
			relationships, err := r.Relationships(ctx, row.ProjectID, row.NativeID, corestore.RelationshipCursor{}, corestore.MaxPageSize)
			if err != nil {
				return err
			}
			if relationships.Next != (corestore.RelationshipCursor{}) {
				result.Truncated = true
			}
			for _, relationship := range relationships.Items {
				if relationship.Type == "dependency" && relationship.Direction == "blocked_by" && !relationship.Terminal && slices.Contains(readable, relationship.RelatedProjectID) {
					blockers = append(blockers, string(relationship.RelatedWorkItemID))
				}
			}
			var group *[]coordinatorAttentionItem
			switch {
			case waiting:
				item.Reason = "a runner question is waiting for an answer"
				group = &result.WaitingForInput
			case running:
				attempt, err := r.LatestAttempt(ctx, row.ID)
				if err != nil {
					return err
				}
				item.Reason = "attempt " + attempt.ID + " holds a live lease"
				group = &result.Running
			case row.WorkflowStateName == nil:
				item.Reason = "no workflow state mapping"
				group = &result.Unavailable
			case len(blockers) > 0:
				item.Reason = "blocked by " + strings.Join(blockers, ", ")
				group = &result.Blocked
			case strings.Contains(strings.ToLower(item.State), "block"):
				item.Reason = "workflow state " + item.State
				group = &result.Blocked
			case strings.Contains(strings.ToLower(item.State), "review"):
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
		return nil
	})
	return result, err
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
	ctx, scope, err := t.workReadContext(ctx, record)
	if err != nil {
		return "", err
	}
	scope.project = record.ProjectID
	s := t.coordinator.service.server
	issue, err := s.resolveOperatorNativeItem(ctx, nil, scope, reference)
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, corestore.ErrNotFound) || errors.Is(err, explain.ErrNotFound) || isNativeNotFound(err) {
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
	actorCtx, err := t.actionContext(ctx, record)
	if err != nil {
		return nil, err
	}
	credential, err := currentHubOperator(actorCtx)
	if err != nil {
		return nil, err
	}
	s := t.coordinator.service.server
	scope := nativeScope{organization: record.OrganizationID, credential: credential}
	result := coordinatorIssue{Comments: []coordinatorComment{}}
	var attemptID string
	err = s.config.TenantRepository.ReadTenant(actorCtx, record.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		row, err := r.WorkItemByNativeID(ctx, tracker.NativeWorkItemID(workItemID))
		if err != nil {
			return err
		}
		if !slices.Contains(readable, row.ProjectID) {
			return nativeNotFound()
		}
		scope.project = row.ProjectID
		project, err := r.ProjectRecord(ctx, row.ProjectID)
		if err != nil {
			return err
		}
		org, err := r.AccessOrganization(ctx)
		if err != nil {
			return err
		}
		result.URL = issueBrowserPath(org.Slug, project.Core.Slug, int(row.Number))
		result.WorkItemID, result.ProjectID, result.Number, result.Title, result.State = string(row.NativeID), row.ProjectID, row.Number, row.Title, stringValue(row.WorkflowStateName)
		result.UpdatedAt = formatHubTime(row.NativeUpdatedAt)
		for _, state := range project.States {
			if state.Name == result.State {
				result.Terminal = state.Terminal
			}
		}
		latest, err := r.LatestAttempt(ctx, row.ID)
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		attemptID = latest.ID
		linked, err := r.ConversationByWorkItem(ctx, row.ProjectID, string(row.NativeID))
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if err == nil {
			result.ConversationID = &linked.ID
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	row, version, err := s.readWorkItemRecord(actorCtx, scope, workItemID)
	if err != nil {
		return nil, err
	}
	issue, err := s.hydrateWorkItemRecord(actorCtx, row, version)
	if err != nil {
		return nil, err
	}
	result.Body = boundRunes(issue.Body, coordinatorIssueBodyRunes)
	if attemptID != "" {
		attempt, err := s.readNativeAttemptTenant(actorCtx, scope, workItemID, attemptID)
		if err != nil {
			return nil, err
		}
		status := attempt.Status
		if status == "running" && (attempt.ClaimReleasedAt != nil || !s.config.now().Before(attempt.LeaseExpiresAt)) {
			status = "interrupted"
		}
		result.LatestAttempt = &coordinatorAttempt{AttemptID: attemptID, Status: status, Outcome: attempt.Outcome, StartedAt: formatHubTime(attempt.StartedAt), UpdatedAt: formatHubTime(attempt.UpdatedAt)}
	}
	comments, err := s.readCommentRecords(actorCtx, scope, workItemID, url.Values{"limit": []string{strconv.Itoa(coordinatorCommentCount)}, "direction": []string{"newest"}})
	if err != nil {
		return nil, err
	}
	for _, comment := range comments.Items {
		result.Comments = append(result.Comments, coordinatorComment{Author: strings.TrimSpace(comment.Actor.Kind + " " + comment.Actor.PrincipalID), Body: boundRunes(comment.Body, coordinatorCommentRunes), CreatedAt: formatHubTime(comment.CreatedAt)})
	}
	slices.Reverse(result.Comments)
	return result, nil
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
	message := conversationMessageRecord{
		Role:     conversation.RoleAssistant,
		Kind:     conversation.MessageStatus,
		Text:     "Proposed issue: " + title,
		Data:     data,
		Delivery: conversation.DeliveryCompleted,
		ThreadID: t.state.threadID,
		Actor:    conversation.Actor{Kind: conversation.ActorCoordinator},
	}
	err = t.coordinator.appendPreparedMessage(ctx, record.ID, message)
	if err != nil {
		return nil, fmt.Errorf("record proposal: %w", err)
	}
	return map[string]any{"proposal": proposal}, nil
}

func (s *Service) coordinatorAllowedProjects(ctx context.Context, r tenantstore.Reader, scope nativeScope, ids []tracker.ProjectID) ([]tracker.ProjectID, error) {
	allowed := make([]tracker.ProjectID, 0, len(ids))
	for _, id := range ids {
		scope.project = id
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			if errors.Is(err, accessstore.ErrDenied) || errors.Is(err, auth.ErrHostedIdentity) {
				continue
			}
			return nil, err
		}
		allowed = append(allowed, id)
	}
	return allowed, nil
}
