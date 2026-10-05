package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/skills"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const coordinatorSplitMaxChildren = 10

func coordinatorIssueSplitTool() runner.AgentTool {
	return coordinatorTool(string(chat.ActionIssueSplit), "Propose an entire issue split for one client submission. The client controls inline confirmation; no issues are created by the proposal. Children use positions 1 through 10; position 0 is the parent. Each edge means dependent is blocked by blocker. Priority is 0 urgent through 3 low, omitted for unset.", `{"type":"object","required":["parent_work_item_id","children","edges"],"properties":{"parent_work_item_id":{"type":"string"},"children":{"type":"array","minItems":1,"maxItems":10,"items":{"type":"object","required":["title","description","state"],"properties":{"title":{"type":"string","maxLength":500},"description":{"type":"string","maxLength":4000},"priority":{"type":"integer","minimum":0,"maximum":3},"state":{"type":"string"}},"additionalProperties":false}},"edges":{"type":"array","maxItems":100,"items":{"type":"object","required":["dependent","blocker"],"properties":{"dependent":{"type":"integer","minimum":0,"maximum":10},"blocker":{"type":"integer","minimum":0,"maximum":10}},"additionalProperties":false}}},"additionalProperties":false}`)
}

func (t *coordinatorToolset) loadSplitIssueSkill(raw json.RawMessage) (any, error) {
	var args struct{}
	if err := decodeCoordinatorArguments(raw, &args); err != nil {
		return nil, err
	}
	skill, body, err := skills.ReadBuiltin("split-issue")
	if err != nil {
		return nil, err
	}
	if len(body) > coordinatorToolArgumentBytes {
		return nil, fmt.Errorf("split-issue skill exceeds %d bytes", coordinatorToolArgumentBytes)
	}
	return map[string]string{"name": skill.Name, "instructions": body}, nil
}

func validateCoordinatorIssueSplit(split chat.IssueSplit) error {
	if !strings.HasPrefix(split.ParentID, "wi_") || len(split.ParentID) > 256 {
		return nativeInvalid("A native parent work item is required")
	}
	if len(split.Children) < 1 || len(split.Children) > coordinatorSplitMaxChildren {
		return nativeInvalid("A split must contain 1 to 10 children")
	}
	for _, child := range split.Children {
		if strings.TrimSpace(child.Title) == "" || len(child.Title) > coordinatorProposalTitleBytes || strings.TrimSpace(child.Description) == "" || utf8.RuneCountInString(child.Description) > coordinatorProposalBodyRunes || strings.TrimSpace(child.State) == "" || len(child.State) > 256 {
			return nativeInvalid("Each child requires a bounded title, description and target state")
		}
		if child.Priority != nil && (*child.Priority < 0 || *child.Priority > 3) {
			return nativeInvalid("Child priority must be 0 through 3")
		}
	}
	if len(split.Edges) > 100 {
		return nativeInvalid("A split may contain at most 100 dependency edges")
	}
	graph := make([][]int, len(split.Children)+1)
	seen := map[chat.IssueSplitEdge]bool{}
	for _, edge := range split.Edges {
		if edge.Dependent < 0 || edge.Dependent > len(split.Children) || edge.Blocker < 0 || edge.Blocker > len(split.Children) {
			return nativeInvalid("Dependency edge references an unknown child")
		}
		if seen[edge] {
			return nativeInvalid("Dependency edges must be unique")
		}
		seen[edge] = true
		graph[edge.Dependent] = append(graph[edge.Dependent], edge.Blocker)
	}
	visited := make([]int, len(graph))
	var visit func(int) bool
	visit = func(node int) bool {
		if visited[node] == 1 {
			return false
		}
		if visited[node] == 2 {
			return true
		}
		visited[node] = 1
		for _, blocker := range graph[node] {
			if !visit(blocker) {
				return false
			}
		}
		visited[node] = 2
		return true
	}
	for node := range graph {
		if !visit(node) {
			return nativeInvalid("Dependencies cannot form a cycle")
		}
	}
	return nil
}

func validateCoordinatorSplitDrafts(ctx context.Context, tx *sql.Tx, scope nativeScope, split chat.IssueSplit) (tracker.NativeIssue, error) {
	if err := validateCoordinatorIssueSplit(split); err != nil {
		return tracker.NativeIssue{}, err
	}
	parent, _, err := readNativeIssue(ctx, tx, scope, split.ParentID)
	if err != nil {
		return tracker.NativeIssue{}, err
	}
	if err := requireNativeEdit(parent, tracker.Revision(split.Revision)); err != nil {
		return tracker.NativeIssue{}, err
	}
	for _, child := range split.Children {
		project, err := validateNativeIssueDraft(ctx, tx, scope, tracker.CreateIssue{Title: child.Title, Body: child.Description, Priority: child.Priority})
		if err != nil {
			return tracker.NativeIssue{}, err
		}
		found := false
		for _, state := range project.States {
			if state.Name == child.State {
				found = true
				break
			}
		}
		if !found {
			return tracker.NativeIssue{}, nativeInvalid("Workflow state does not exist")
		}
	}
	return parent, nil
}

func (t *coordinatorToolset) proposeIssueSplit(ctx context.Context, record conversationRecord, call runner.AgentToolCall) (any, error) {
	var split chat.IssueSplit
	if err := decodeCoordinatorArguments(call.Arguments, &split); err != nil {
		return nil, err
	}
	if split.Revision != 0 {
		return nil, operatortool.ErrInvalidArguments
	}
	if err := validateCoordinatorIssueSplit(split); err != nil {
		return nil, err
	}
	ctx, err := t.actionContext(ctx, record)
	if err != nil {
		return nil, err
	}
	ctx, err = operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeWrite, ProjectID: string(record.ProjectID), ResourceKind: "work_item", ResourceID: split.ParentID})
	if err != nil {
		return nil, err
	}
	resolve, ok := ctx.Value(nativeOperatorScopeKey{}).(func(context.Context) (nativeScope, error))
	if !ok {
		return nil, operatortool.ErrAccessDenied
	}
	scope, err := resolve(ctx)
	if err != nil {
		return nil, err
	}
	scope.project = record.ProjectID
	s := t.coordinator.service.server
	tx, err := s.database.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	parent, _, err := readNativeIssue(ctx, tx, scope, split.ParentID)
	if err != nil {
		return nil, err
	}
	split.Revision = int64(parent.Revision)
	if _, err := validateCoordinatorSplitDrafts(ctx, tx, scope, split); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	arguments, err := json.Marshal(split)
	if err != nil {
		return nil, err
	}
	if len(arguments) > coordinatorToolArgumentBytes {
		return nil, fmt.Errorf("%w: approved split exceeds %d bytes", errCoordinatorToolArguments, coordinatorToolArgumentBytes)
	}
	action := chat.Action{ConversationID: record.ID, Kind: chat.ActionIssueSplit, ProjectID: string(record.ProjectID), IssueID: split.ParentID, Identifier: fmt.Sprintf("#%d", parent.Number), Revision: split.Revision, Title: fmt.Sprintf("Split #%d: %s into %d issues", parent.Number, parent.Title, len(split.Children)), Arguments: arguments, Material: true}
	return t.submitCoordinatorAction(ctx, record, call, action)
}

type coordinatorSplitResult struct {
	Children []tracker.NativeIssue `json:"children"`
	Comment  tracker.NativeComment `json:"comment"`
}

func (s *Service) executeCoordinatorIssueSplit(ctx context.Context, action chat.Action) (chat.ActionExecution, error) {
	var split chat.IssueSplit
	if err := decodeCoordinatorArguments(action.Arguments, &split); err != nil {
		return chat.ActionExecution{}, err
	}
	if split.ParentID != action.IssueID || split.Revision != action.Revision {
		return chat.ActionExecution{}, operatortool.ErrAccessDenied
	}
	resolve, ok := ctx.Value(nativeOperatorScopeKey{}).(func(context.Context) (nativeScope, error))
	if !ok {
		return chat.ActionExecution{}, operatortool.ErrAccessDenied
	}
	scope, err := resolve(ctx)
	if err != nil {
		return chat.ActionExecution{}, err
	}
	scope.project = tracker.ProjectID(action.ProjectID)
	options := nativeCommandOptions{OperationID: nativeOperation(scope, "POST", "/work-items/"+split.ParentID+"/split"), Item: split.ParentID, RequireLease: true, Feature: "collaboration"}
	raw, err := s.executeNativeMutation(ctx, scope, options, tracker.MutationForContext(ctx, action.RequestID), split, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		parent, err := validateCoordinatorSplitDrafts(ctx, tx, scope, split)
		if err != nil {
			return nil, err
		}
		children := make([]tracker.NativeIssue, 0, len(split.Children))
		for _, child := range split.Children {
			issue, err := createNativeIssueTx(ctx, tx, scope, tracker.CreateIssue{Title: child.Title, Body: child.Description, Priority: child.Priority, State: child.State}, now)
			if err != nil {
				return nil, err
			}
			children = append(children, issue)
		}
		nodes := append([]tracker.NativeIssue{parent}, children...)
		for _, edge := range split.Edges {
			dependent, blocker := nodes[edge.Dependent], nodes[edge.Blocker]
			issue, err := changeNativeDependencyTx(ctx, tx, scope, string(dependent.WorkItemID), tracker.DependencyMutation{ExpectedRevision: dependent.Revision, RelatedWorkItemID: blocker.WorkItemID, Operation: "add"}, now)
			if err != nil {
				return nil, err
			}
			nodes[edge.Dependent] = issue
		}
		var body strings.Builder
		body.WriteString("Approved issue split:\n")
		for _, child := range nodes[1:] {
			fmt.Fprintf(&body, "\n- #%d (%s): %s", child.Number, child.WorkItemID, child.Title)
		}
		if len(split.Edges) > 0 {
			body.WriteString("\n\nDependencies:\n")
			for _, edge := range split.Edges {
				fmt.Fprintf(&body, "\n- #%d is blocked by #%d", nodes[edge.Dependent].Number, nodes[edge.Blocker].Number)
			}
		}
		comment, err := insertNativeComment(ctx, tx, scope, parent, body.String(), nil, now)
		if err != nil {
			return nil, err
		}
		return coordinatorSplitResult{Children: nodes[1:], Comment: comment}, nil
	})
	if err != nil {
		return chat.ActionExecution{}, coordinatorActionError(err)
	}
	var result coordinatorSplitResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return chat.ActionExecution{}, err
	}
	for _, child := range result.Children {
		encoded, err := json.Marshal(child)
		if err != nil {
			return chat.ActionExecution{}, err
		}
		s.wakeSpriteRunnersAfter(scope, encoded)
	}
	var names []string
	for _, child := range result.Children {
		names = append(names, fmt.Sprintf("#%d (%s)", child.Number, child.WorkItemID))
	}
	return chat.ActionExecution{Message: "Created " + strings.Join(names, ", ") + " with their dependencies and parent comment.", ResourceID: split.ParentID, Identifier: action.Identifier, Revision: split.Revision, Data: raw}, nil
}
