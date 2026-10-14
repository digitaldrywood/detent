package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/digitaldrywood/detent/internal/apikey"
	chatpkg "github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/web/templates"
)

type workCommandTarget struct {
	issue  telemetry.Issue
	native tracker.NativeIssue
	client *hubclient.NativeClient
}

func (s *Server) workCommandTarget(ctx context.Context, projectID, identifier string) (workCommandTarget, error) {
	tracked, ok := s.registry.Get(project.ID(projectID))
	if !ok || tracked == nil {
		return workCommandTarget{}, errOperatorCommandUnavailable
	}
	issue, found := chatFindIssue(s.chatSnapshot(ctx), projectID, identifier)
	if source, ok := tracked.Connector().(nativeClientSource); ok && source.NativeClient() != nil {
		id := identifier
		if found {
			id = issue.ID
		}
		native, err := source.NativeClient().Issue(ctx, tracker.NativeWorkItemID(id))
		if err != nil {
			return workCommandTarget{}, errOperatorCommandUnavailable
		}
		// NativeClient binds this read to the configured organization and project.
		issue = telemetry.Issue{ID: string(native.WorkItemID), ProjectID: projectID, Identifier: identifier, State: native.State, URL: templates.NativeIssuePath(projectID, native.WorkItemID)}
		return workCommandTarget{issue: issue, native: native, client: source.NativeClient()}, nil
	}
	if !found {
		return workCommandTarget{}, errOperatorCommandUnavailable
	}
	return workCommandTarget{issue: issue}, nil
}

func (s *Server) workActionProposal(ctx context.Context, name string, arguments json.RawMessage) (chatpkg.Action, error) {
	request, err := operatortool.DecodeWorkArguments(name, arguments)
	if err != nil {
		return chatpkg.Action{}, err
	}
	target, err := s.workCommandTarget(ctx, request.ProjectID, request.Identifier)
	if err != nil {
		return chatpkg.Action{}, err
	}
	action := chatpkg.Action{Kind: chatpkg.ActionKind(name), Work: &request, ProjectID: request.ProjectID, IssueID: target.issue.ID, Identifier: target.issue.Identifier, ResourceURL: target.issue.URL, CurrentState: target.issue.State}
	if target.client != nil {
		if target.native.Profile != "native" || request.Target == "pr" {
			return chatpkg.Action{}, errOperatorCommandUnavailable
		}
		switch name {
		case operatortool.EditItem, operatortool.SetDependency, operatortool.ArchiveItem, operatortool.RestoreItem:
			if request.ExpectedRevision <= 0 || int64(target.native.Revision) != request.ExpectedRevision {
				return chatpkg.Action{}, errOperatorCommandUnavailable
			}
			action.Revision = int64(target.native.Revision)
		}
		switch name {
		case operatortool.EditItem:
			action.Material = request.Body != nil && strings.TrimSpace(*request.Body) == "" && target.native.Body != ""
			if request.Labels != nil {
				for _, label := range target.native.Labels {
					found := false
					for _, next := range *request.Labels {
						if next == label {
							found = true
							break
						}
					}
					if !found {
						action.Material = true
					}
				}
			}
		case operatortool.EditComment:
			if request.ExpectedRevision <= 0 {
				return chatpkg.Action{}, errOperatorCommandUnavailable
			}
			comment, err := nativeCommentForCommand(ctx, target.client, target.native.WorkItemID, request.CommentID)
			if err != nil || int64(comment.Revision) != request.ExpectedRevision {
				return chatpkg.Action{}, errOperatorCommandUnavailable
			}
			action.Revision = int64(comment.Revision)
		case operatortool.DeleteComment, operatortool.RemoveItem, operatortool.OrderItem:
			return chatpkg.Action{}, errOperatorCommandUnavailable
		case operatortool.SetDependency:
			// Resolve the related item under the same configured project; the hub also
			// enforces ownership, project grants and cycle restrictions transactionally.
			related, err := target.client.Issue(ctx, tracker.NativeWorkItemID(request.Related))
			if err != nil || related.WorkItemID == target.native.WorkItemID {
				return chatpkg.Action{}, errOperatorCommandUnavailable
			}
		}
	} else {
		switch name {
		case operatortool.EditItem, operatortool.SetDependency, operatortool.ArchiveItem, operatortool.RestoreItem, operatortool.OrderItem:
			return chatpkg.Action{}, errOperatorCommandUnavailable
		case operatortool.AddComment:
			board, message, _ := s.kanbanActionTarget(request.ProjectID)
			if message != "" || request.Target != "pr" && !connector.DetectCapabilities(board.connector).CreateComment || request.Target == "pr" && (!kanbanSupportsPullRequestComments(board.connector) || !s.kanbanCommentTargetKnown(workCommentRequest(request, target.issue.ID))) {
				return chatpkg.Action{}, errOperatorCommandUnavailable
			}
		case operatortool.EditComment, operatortool.DeleteComment:
			if !s.kanbanCommentCanMutate(kanbanCommentMutationRequest{projectID: request.ProjectID, issueID: target.issue.ID, commentID: request.CommentID}, name == operatortool.EditComment) {
				return chatpkg.Action{}, errOperatorCommandUnavailable
			}
		case operatortool.RemoveItem:
			board, message, _ := s.kanbanActionTarget(request.ProjectID)
			if message != "" || !kanbanCanRemoveCards(board) || s.operatorMoves == nil {
				return chatpkg.Action{}, errOperatorCommandUnavailable
			}
		}
	}
	if name == operatortool.AcknowledgeParks {
		explanation, err := s.workExplanation(ctx, request.ProjectID, target.issue.ID)
		if err != nil {
			return chatpkg.Action{}, err
		}
		// Pin the sequence the operator is acknowledging, including approval/replay.
		action.Attempt = int(explanation.ParkSummary.ParkCount)
	}
	if name == operatortool.DisposeSecurityFinding {
		if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeAdmin, ProjectID: request.ProjectID}); err != nil {
			return chatpkg.Action{}, err
		}
		if _, err := s.securityDispositionRun(ctx, request.ProjectID, workDispositionRequest(request)); err != nil {
			return chatpkg.Action{}, err
		}
	}
	return action, nil
}

func nativeCommentForCommand(ctx context.Context, client *hubclient.NativeClient, id tracker.NativeWorkItemID, commentID string) (tracker.NativeComment, error) {
	cursor := ""
	for range 20 {
		page, err := client.Comments(ctx, id, cursor)
		if err != nil {
			return tracker.NativeComment{}, err
		}
		for _, comment := range page.Items {
			if comment.ID == commentID {
				return comment, nil
			}
		}
		if page.NextCursor == "" || page.NextCursor == cursor {
			break
		}
		cursor = page.NextCursor
	}
	return tracker.NativeComment{}, errOperatorCommandUnavailable
}

func (s *Server) executeWorkAction(ctx context.Context, action chatpkg.Action) (chatpkg.ActionExecution, error) {
	if action.Work == nil {
		return chatpkg.ActionExecution{}, errOperatorCommandUnavailable
	}
	request := *action.Work
	target, err := s.workCommandTarget(ctx, action.ProjectID, action.IssueID)
	if err != nil {
		return chatpkg.ActionExecution{}, err
	}
	result := chatpkg.ActionExecution{Message: "Action completed.", ResourceID: action.IssueID, Identifier: action.Identifier, URL: action.ResourceURL}
	if target.client != nil {
		command := nativeWorkCommand{Kind: string(action.Kind), ID: target.native.WorkItemID, ExpectedRevision: tracker.Revision(request.ExpectedRevision), Title: request.Title, Body: request.Body, Labels: request.Labels, Priority: request.Priority, CommentID: request.CommentID, Related: tracker.NativeWorkItemID(request.Related), Operation: request.Operation}
		native, err := executeNativeWorkCommand(ctx, target.client, command)
		if err != nil {
			return chatpkg.ActionExecution{}, errOperatorCommandUnavailable
		}
		result.Revision = int64(native.Issue.Revision)
		if native.Comment.ID != "" {
			result.CommentID = native.Comment.ID
			result.Revision = int64(native.Comment.Revision)
		}
		if string(action.Kind) != operatortool.AcknowledgeParks && string(action.Kind) != operatortool.DisposeSecurityFinding {
			s.requestKanbanRefresh(ctx)
			return result, nil
		}
	}
	switch string(action.Kind) {
	case operatortool.AddComment:
		_, status, err := s.addKanbanComment(ctx, workCommentRequest(request, action.IssueID))
		if err != nil || status != http.StatusOK {
			return chatpkg.ActionExecution{}, errOperatorCommandUnavailable
		}
	case operatortool.EditComment, operatortool.DeleteComment:
		body := ""
		if request.Body != nil {
			body = *request.Body
		}
		if err := s.mutateKanbanComment(ctx, kanbanCommentMutationRequest{projectID: action.ProjectID, issueID: action.IssueID, commentID: request.CommentID, body: body}, string(action.Kind) == operatortool.EditComment); err != nil {
			return chatpkg.ActionExecution{}, errOperatorCommandUnavailable
		}
		result.CommentID = request.CommentID
	case operatortool.RemoveItem:
		_, status, err := s.removeKanbanCard(ctx, kanbanRemoveRequest{projectID: action.ProjectID, issueID: action.IssueID, currentState: action.CurrentState})
		if err != nil || status != http.StatusOK {
			return chatpkg.ActionExecution{}, errOperatorCommandUnavailable
		}
	case operatortool.AcknowledgeParks:
		explanation, err := s.workExplanation(ctx, action.ProjectID, action.IssueID)
		if err != nil || int(explanation.ParkSummary.ParkCount) != action.Attempt {
			return chatpkg.ActionExecution{}, errOperatorCommandUnavailable
		}
		if _, err := s.acknowledgeIssueParks(ctx, explanation); err != nil {
			return chatpkg.ActionExecution{}, errOperatorCommandUnavailable
		}
	case operatortool.DisposeSecurityFinding:
		if _, err := s.recordSecurityDisposition(ctx, action.ProjectID, workDispositionRequest(request)); err != nil {
			return chatpkg.ActionExecution{}, errOperatorCommandUnavailable
		}
	default:
		return chatpkg.ActionExecution{}, errOperatorCommandUnavailable
	}
	s.requestKanbanRefresh(ctx)
	return result, nil
}

func (s *Server) workExplanation(ctx context.Context, projectID, reference string) (explain.IssueExplanation, error) {
	if s.issueExplainer == nil {
		return explain.IssueExplanation{}, errOperatorCommandUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, issueExplanationTimeout)
	defer cancel()
	return s.issueExplainer.Explain(ctx, explain.Query{ProjectID: projectID, Reference: reference})
}

func (s *Server) executeWorkRead(ctx context.Context, call operatortool.Call) (operatortool.Result, error) {
	request, err := operatortool.DecodeWorkArguments(call.Name, call.Arguments)
	if err != nil {
		return operatortool.Result{}, err
	}
	ctx, err = operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead, ProjectID: request.ProjectID})
	if err != nil {
		return operatortool.Result{}, err
	}
	target, err := s.workCommandTarget(ctx, request.ProjectID, request.Identifier)
	if err != nil {
		return operatortool.Result{}, err
	}
	if request.Limit == 0 {
		request.Limit = 50
	}
	var data any
	truncated := false
	if call.Name == operatortool.ListComments {
		if target.client != nil {
			data, err = target.client.CommentsPage(ctx, target.native.WorkItemID, request.Cursor, request.Limit)
		} else {
			if request.Cursor != "" {
				return operatortool.Result{}, operatortool.ErrInvalidArguments
			}
			comments := target.issue.Comments
			if len(comments) > request.Limit {
				truncated = true
				comments = comments[:request.Limit]
			}
			data = comments
		}
	} else {
		if request.Cursor != "" {
			return operatortool.Result{}, operatortool.ErrInvalidArguments
		}
		timeline, readErr := s.workTimeline(ctx, store.IssueIdentity{ProjectID: request.ProjectID, IssueID: target.issue.ID, Identifier: target.issue.Identifier, IssueURL: target.issue.URL})
		err = readErr
		if len(timeline.Events) > request.Limit {
			timeline.Events = timeline.Events[:request.Limit]
			truncated = true
		}
		if len(timeline.Activity) > request.Limit {
			timeline.Activity = timeline.Activity[:request.Limit]
			truncated = true
		}
		data = timeline
	}
	if err != nil {
		return operatortool.Result{}, errOperatorCommandUnavailable
	}
	return operatorResult(struct {
		ResourceID string    `json:"resource_id"`
		URL        string    `json:"url"`
		ObservedAt time.Time `json:"observed_at"`
		Data       any       `json:"data"`
		Truncated  bool      `json:"truncated"`
	}{target.issue.ID, target.issue.URL, time.Now().UTC(), data, truncated})
}

type nativeWorkCommand struct {
	Kind             string
	Key              string
	ID               tracker.NativeWorkItemID
	ExpectedRevision tracker.Revision
	Title, Body      *string
	Labels           *[]string
	Priority         *int
	CommentID        string
	Related          tracker.NativeWorkItemID
	Operation        string
	Create           tracker.CreateIssue
}

// executeNativeWorkCommand is shared by browser forms, dashboard creation,
// and operator tools. The hub remains authoritative for revisions and workflow.
func executeNativeWorkCommand(ctx context.Context, client *hubclient.NativeClient, command nativeWorkCommand) (nativeFormResult, error) {
	var result nativeFormResult
	key := command.Key
	if key == "" {
		key = uuid.NewString()
	}
	mutation := tracker.MutationForContext(ctx, key)
	var err error
	switch command.Kind {
	case operatortool.FileIssue:
		command.Create.Mutation = mutation
		result.Issue, err = client.CreateIssue(ctx, command.Create)
	case operatortool.EditItem:
		result.Issue, err = client.UpdateIssue(ctx, command.ID, tracker.UpdateIssue{Mutation: mutation, ExpectedRevision: command.ExpectedRevision, Title: command.Title, Body: command.Body, Labels: command.Labels, Priority: tracker.SetPriority(command.Priority)})
	case operatortool.AddComment:
		if command.Body == nil {
			return result, errOperatorCommandUnavailable
		}
		result.Comment, err = client.CreateComment(ctx, command.ID, tracker.CreateComment{Mutation: mutation, Body: *command.Body})
	case operatortool.EditComment:
		if command.Body == nil {
			return result, errOperatorCommandUnavailable
		}
		result.Comment, err = client.UpdateComment(ctx, command.ID, command.CommentID, tracker.UpdateComment{Mutation: mutation, ExpectedRevision: command.ExpectedRevision, Body: *command.Body})
	case operatortool.SetDependency:
		result.Issue, err = client.Dependency(ctx, command.ID, tracker.DependencyMutation{Mutation: mutation, ExpectedRevision: command.ExpectedRevision, RelatedWorkItemID: command.Related, Operation: command.Operation})
	case operatortool.ArchiveItem, operatortool.RestoreItem:
		result.Issue, err = client.SetArchived(ctx, command.ID, command.ExpectedRevision, command.Kind == operatortool.ArchiveItem, mutation)
	case operatortool.AcknowledgeParks, operatortool.DisposeSecurityFinding:
		// These use the dashboard runtime command, not the native tracker.
	default:
		return result, errors.New("native work command is unavailable")
	}
	return result, err
}

func (s *Server) workTimeline(ctx context.Context, identity store.IssueIdentity) (workflowTimelineAPIResponse, error) {
	if s.store == nil {
		return workflowTimelineAPIResponse{}, errOperatorCommandUnavailable
	}
	timeline, err := s.store.IssueWorkflowTimeline(ctx, identity)
	return workflowTimelineResponse(timeline), err
}

func workDispositionRequest(request operatortool.WorkArguments) securityAuditDispositionRequest {
	return securityAuditDispositionRequest{Repository: request.Repository, PullRequest: request.PullRequest, BaseSHA: request.BaseSHA, HeadSHA: request.HeadSHA, FindingID: request.FindingID, Evidence: request.Evidence, Status: "false_positive"}
}

func (s *Server) pinNativeCommandRevision(ctx context.Context, action *chatpkg.Action, expected int64) error {
	tracked, ok := s.registry.Get(project.ID(action.ProjectID))
	if !ok || tracked == nil {
		return errOperatorCommandUnavailable
	}
	source, ok := tracked.Connector().(nativeClientSource)
	if !ok || source.NativeClient() == nil {
		if expected != 0 {
			return operatortool.ErrInvalidArguments
		}
		return nil
	}
	item, err := source.NativeClient().Issue(ctx, tracker.NativeWorkItemID(action.IssueID))
	if expected <= 0 && !operatortool.ConnectionIdentity(ctx).Valid() {
		expected = int64(item.Revision)
	}
	if err != nil || expected <= 0 || int64(item.Revision) != expected {
		return errOperatorCommandUnavailable
	}
	action.Work = &operatortool.WorkArguments{ProjectID: action.ProjectID, Identifier: action.Identifier, ExpectedRevision: expected}
	action.Revision = expected
	return nil
}

func (s *Server) workTerminalState(ctx context.Context, projectID, state string) bool {
	tracked, ok := s.registry.Get(project.ID(projectID))
	if !ok || tracked == nil {
		return false
	}
	if native, ok := tracked.Connector().(nativeClientSource); ok && native.NativeClient() != nil {
		cfg, err := native.NativeClient().Project(ctx)
		if err != nil {
			return true
		}
		if state == "" && len(cfg.States) > 0 {
			state = cfg.States[0].Name
		}
		for _, lane := range cfg.States {
			if strings.EqualFold(lane.Name, state) {
				return lane.Terminal
			}
		}
	}
	for _, lane := range tracked.Workflow().Config.Tracker.TerminalStates {
		if strings.EqualFold(lane, state) {
			return true
		}
	}
	return false
}

func workCommentRequest(request operatortool.WorkArguments, issueID string) kanbanCommentRequest {
	target := request.Target
	if target == "" {
		target = "issue"
	}
	body := ""
	if request.Body != nil {
		body = *request.Body
	}
	return kanbanCommentRequest{projectID: request.ProjectID, issueID: issueID, target: target, prRepository: request.Repository, prNumber: request.PullRequest, body: body}
}
