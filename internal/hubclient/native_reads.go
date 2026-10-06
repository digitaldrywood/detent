package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// NativePullRequestReference projects identifiers from the existing PR panel.
// Change/review details remain owned by that application's detail reads.
type NativePullRequestReference struct {
	Number    int       `json:"number"`
	URL       string    `json:"url"`
	FetchedAt time.Time `json:"fetched_at"`
}

// SameOrganization binds local project mappings to the same hub and tenant.
func (c *NativeClient) SameOrganization(other *NativeClient) bool {
	return other != nil && c.organization == other.organization && c.client.baseURL.String() == other.client.baseURL.String()
}

func (c *NativeClient) ProjectID() tracker.ProjectID { return c.project }

func (c *NativeClient) PullRequestReferences(ctx context.Context, id tracker.NativeWorkItemID) ([]NativePullRequestReference, error) {
	path, err := nativeItemPath(id)
	if err != nil {
		return nil, err
	}
	var result []NativePullRequestReference
	err = c.client.request(ctx, http.MethodGet, c.base()+path+"/pull-requests", nil, &result)
	return result, err
}

func (c *NativeClient) CommentsPage(ctx context.Context, id tracker.NativeWorkItemID, cursor string, limit int) (tracker.Page[tracker.NativeComment], error) {
	var result tracker.Page[tracker.NativeComment]
	err := c.readItemPage(ctx, id, "comments", cursor, limit, &result)
	return result, err
}

func (c *NativeClient) HistoryPage(ctx context.Context, id tracker.NativeWorkItemID, cursor string, limit int) (tracker.Page[tracker.CollaborationEvent], error) {
	var result tracker.Page[tracker.CollaborationEvent]
	err := c.readItemPage(ctx, id, "history", cursor, limit, &result)
	return result, err
}

func (c *NativeClient) AttemptsPage(ctx context.Context, id tracker.NativeWorkItemID, cursor string, limit int) (tracker.Page[tracker.NativeAttempt], error) {
	var result tracker.Page[tracker.NativeAttempt]
	err := c.readItemPage(ctx, id, "attempts", cursor, limit, &result)
	return result, err
}

func (c *NativeClient) readItemPage(ctx context.Context, id tracker.NativeWorkItemID, kind, cursor string, limit int, target any) error {
	path, err := nativeItemPath(id)
	if err != nil {
		return err
	}
	if limit < 1 || limit > 200 {
		return tracker.ErrInvalidCandidateQuery
	}
	params := url.Values{"limit": {strconv.Itoa(limit)}, "cursor": {cursor}}
	return c.client.request(ctx, http.MethodGet, c.base()+path+"/"+kind+"?"+params.Encode(), nil, target)
}

func (c *NativeClient) Version(ctx context.Context, id tracker.NativeWorkItemID, comment string, revision int64) (json.RawMessage, error) {
	var result json.RawMessage
	path, err := nativeItemPath(id)
	if err != nil {
		return result, err
	}
	if revision <= 0 {
		return result, tracker.ErrInvalidCandidateQuery
	}
	if comment != "" {
		path += "/comments/" + url.PathEscape(comment)
	}
	err = c.client.request(ctx, http.MethodGet, c.base()+path+"/versions/"+strconv.FormatInt(revision, 10), nil, &result)
	return result, err
}

func (c *NativeClient) Labels(ctx context.Context) ([]tracker.NativeLabel, error) {
	var result struct {
		Items []tracker.NativeLabel `json:"items"`
	}
	err := c.client.request(ctx, http.MethodGet, c.base()+"/labels", nil, &result)
	return result.Items, err
}

func (e *nativeExecution) AgentTools() ([]runner.AgentTool, runner.AgentToolHandler) {
	var tools []runner.AgentTool
	definitions := append(operatortool.WorkReadCatalog(), operatortool.ChangeCatalog()...)
	definitions = append(definitions, operatortool.AttachmentCatalog()...)
	for _, definition := range definitions {
		switch definition.Name {
		case operatortool.WorkItem, operatortool.WorkComments, operatortool.WorkHistory, operatortool.WorkRuns, operatortool.BoardActivity, operatortool.WorkAttemptReceipt:
			switch definition.Name {
			case operatortool.WorkRuns:
				definition.Description = "Read a bounded page of native work-item attempts, including their recorded attempt IDs and runtime observations."
			case operatortool.BoardActivity:
				definition.Description = "Read a bounded page of durable native work-item activity from the existing history owner."
			case operatortool.WorkAttemptReceipt:
				definition.Description += " Returns recorded native runtime evidence with freshness and unavailable fields; no provider conversation transcript is supplied."
			}
			definition.Description += " reference must be a canonical native work-item ID beginning with wi_; numbers, titles and URLs are not supported. Use cursor, not offset, for paging."
			tools = append(tools, runner.AgentTool{Name: definition.Name, Description: definition.Description, InputSchema: definition.InputSchema})
		case operatortool.ListChanges, operatortool.GetChange, operatortool.ReadAttachmentMetadata, operatortool.ReadAttachment:
			tools = append(tools, runner.AgentTool{Name: definition.Name, Description: definition.Description, InputSchema: definition.InputSchema})
		}
	}
	tools = append(tools, evidenceTool())
	return tools, func(ctx context.Context, call runner.AgentToolCall) (runner.AgentToolResult, error) {
		if err := e.Validate(ctx); err != nil {
			return runner.AgentToolResult{Content: "Native execution authority is unavailable"}, err
		}
		if call.Name == "attach_evidence" {
			return e.attachEvidence(ctx, call.Arguments)
		}
		result, err := e.claim.source.client.readAgentTool(ctx, call, e.scheduler.now())
		if err != nil {
			return runner.AgentToolResult{Content: err.Error()}, err
		}
		return runner.AgentToolResult{Content: string(result.Content), Success: true}, nil
	}
}

func (c *NativeClient) readAgentTool(ctx context.Context, call runner.AgentToolCall, now time.Time) (operatortool.Result, error) {
	var value any
	var err error
	switch call.Name {
	case operatortool.ReadAttachmentMetadata, operatortool.ReadAttachment:
		request, decodeErr := operatortool.DecodeAttachmentOperation(call.Name, call.Arguments)
		if decodeErr != nil {
			return operatortool.Result{}, decodeErr
		}
		if request.ProjectID != string(c.project) {
			return operatortool.Result{}, operatortool.ErrAccessDenied
		}
		value, err = c.readAgentAttachment(ctx, call.Name, request)
		var refusal *APIError
		if errors.As(err, &refusal) && refusal.Status == http.StatusBadRequest {
			return operatortool.Result{}, operatortool.ErrInvalidArguments
		}
	case operatortool.WorkItem, operatortool.WorkComments, operatortool.WorkHistory, operatortool.WorkRuns, operatortool.BoardActivity, operatortool.WorkAttemptReceipt:
		request, decodeErr := operatortool.DecodeWorkRead(call.Name, call.Arguments)
		if decodeErr != nil {
			return operatortool.Result{}, decodeErr
		}
		if request.ProjectID != string(c.project) {
			return operatortool.Result{}, operatortool.ErrAccessDenied
		}
		id := tracker.NativeWorkItemID(request.Reference)
		if _, err := nativeItemPath(id); err != nil {
			return operatortool.Result{}, operatortool.ErrInvalidArguments
		}
		if request.Offset != 0 {
			return operatortool.Result{}, operatortool.ErrInvalidArguments
		}
		switch call.Name {
		case operatortool.WorkItem:
			var issue tracker.NativeIssue
			issue, err = c.Issue(ctx, id)
			if err == nil && (issue.ProjectID != c.project || issue.WorkItemID != id) {
				return operatortool.Result{}, operatortool.ErrAccessDenied
			}
			value = operatortool.NativeItemView(request.ProjectID, issue)
		case operatortool.WorkComments:
			value, err = c.CommentsPage(ctx, id, request.Cursor, request.Limit)
		case operatortool.WorkHistory, operatortool.BoardActivity:
			value, err = c.HistoryPage(ctx, id, request.Cursor, request.Limit)
		case operatortool.WorkRuns:
			value, err = c.AttemptsPage(ctx, id, request.Cursor, request.Limit)
		case operatortool.WorkAttemptReceipt:
			if request.Cursor != "" {
				return operatortool.Result{}, operatortool.ErrInvalidArguments
			}
			selector := request.NativeAttemptID
			if request.AttemptID > 0 {
				selector = strconv.FormatInt(request.AttemptID, 10)
			}
			var evidence tracker.NativeRuntimeEvidence
			evidence, err = c.RuntimeEvidence(ctx, id, selector)
			if err == nil {
				if evidence.Issue.OrganizationID != c.organization || evidence.Issue.ProjectID != c.project || evidence.Issue.WorkItemID != id {
					return operatortool.Result{}, operatortool.ErrAccessDenied
				}
				return operatortool.NativeRuntimeResult(call.Name, request, evidence)
			}
		}
		value = operatortool.WorkReadResult[any]{ProjectID: request.ProjectID, Reference: request.Reference, GeneratedAt: now, Freshness: explain.SourceAvailable, Data: value}
	case operatortool.ListChanges, operatortool.GetChange:
		request, decodeErr := operatortool.DecodeChangeArguments(call.Name, call.Arguments)
		if decodeErr != nil {
			return operatortool.Result{}, decodeErr
		}
		if request.ProjectID != string(c.project) {
			return operatortool.Result{}, operatortool.ErrAccessDenied
		}
		id := tracker.NativeWorkItemID(request.ItemID)
		result := operatortool.ChangeResult{OrganizationID: string(c.organization), ProjectID: request.ProjectID, WorkItemID: request.ItemID, GeneratedAt: now, Freshness: "available"}
		if call.Name == operatortool.ListChanges {
			var changes []tracker.ChangeRequest
			changes, err = c.Changes(ctx, id)
			for _, change := range changes {
				if change.ProjectID != c.project || change.WorkItemID != id {
					return operatortool.Result{}, operatortool.ErrAccessDenied
				}
			}
			page := operatortool.OffsetPage(changes, request.Offset, request.Limit)
			result.Changes, result.NextOffset = page.Items, page.NextOffset
		} else {
			var detail tracker.ChangeDetail
			detail, err = c.Change(ctx, id, request.ChangeID)
			if err == nil && (detail.Change.ProjectID != c.project || detail.Change.WorkItemID != id || detail.Change.ID != request.ChangeID) {
				return operatortool.Result{}, operatortool.ErrAccessDenied
			}
			result.ChangeID, result.Detail = request.ChangeID, &detail
		}
		value = result
	default:
		return operatortool.Result{}, operatortool.ErrUnknownTool
	}
	if err != nil {
		var refusal *APIError
		if errors.As(err, &refusal) && (refusal.Status == http.StatusForbidden || refusal.Status == http.StatusUnauthorized || refusal.Status == http.StatusNotFound) {
			return operatortool.Result{}, operatortool.ErrAccessDenied
		}
		return operatortool.Result{}, operatortool.ErrReadUnavailable
	}
	return operatortool.EncodeResult(value)
}
