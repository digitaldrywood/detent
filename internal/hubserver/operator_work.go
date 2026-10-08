package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type nativeOperatorScopeKey struct{}
type nativeOperatorExecutor struct{ service *Service }

func (e nativeOperatorExecutor) ListTools(ctx context.Context) ([]operatortool.Definition, error) {
	if _, err := e.service.authorizeCatalog(ctx, operatortool.Requirement{Scope: apikey.ScopeRead}); err != nil {
		return nil, err
	}
	writable := false
	if _, err := e.service.authorizeCatalog(ctx, operatortool.Requirement{Scope: apikey.ScopeWrite, ResourceKind: "work_item"}); err == nil {
		writable = true
	}
	definitions := []operatortool.Definition{}
	catalog, err := operatorCatalog(ctx)
	if err != nil {
		return nil, err
	}
	if catalog.credential.HostedRole != "account" {
		names := (operatorWorkReads{}).WorkReadNames(ctx)
		for _, definition := range operatortool.WorkReadCatalog() {
			if slices.Contains(names, definition.Name) {
				definitions = append(definitions, definition)
			}
		}
		definition, _ := operatortool.Lookup(operatortool.ExplainItem)
		definitions = append(definitions, definition)
		for _, name := range []string{operatortool.BoardState, operatortool.Dashboard} {
			definition, _ := operatortool.Lookup(name)
			definitions = append(definitions, definition)
		}
	}
	for _, definition := range operatortool.CommandCatalog() {
		switch definition.Name {
		case operatortool.MoveItem, operatortool.FileIssue, operatortool.EditItem, operatortool.AddComment, operatortool.EditComment, operatortool.SetDependency, operatortool.RestoreItem, operatortool.OrderItem, operatortool.SetQueuePriority:
			if writable {
				definitions = append(definitions, definition)
			}
		case operatortool.ListComments:
			definitions = append(definitions, definition)
		}
	}
	if e.service.hostedShared() {
		for _, definition := range operatortool.AttachmentCatalog() {
			if writable || definition.Annotations.ReadOnly {
				definitions = append(definitions, definition)
			}
		}
	}
	return definitions, nil
}

func (e nativeOperatorExecutor) Execute(ctx context.Context, call operatortool.Call) (operatortool.Result, error) {
	if call.Name == operatortool.BoardState || call.Name == operatortool.Dashboard {
		return e.service.executeBoardRead(ctx, call)
	}
	if operatortool.IsAttachmentTool(call.Name) && call.Name != operatortool.UploadAttachment {
		return e.attachmentOperation(ctx, call)
	}
	if call.Name == operatortool.MoveItem {
		return e.workflowTransition(ctx, call)
	}
	if operatortool.IsWorkRead(call.Name) || call.Name == operatortool.ExplainItem {
		return operatortool.NewAuthorizedExecutor(operatortool.NewExecutor(operatortool.Dependencies{})).Execute(ctx, call)
	}
	definition, ok := operatortool.Lookup(call.Name)
	if !ok {
		return operatortool.Result{}, operatortool.ErrUnknownTool
	}
	resolver, ok := ctx.Value(nativeOperatorScopeKey{}).(func(context.Context) (nativeScope, error))
	if !ok {
		return operatortool.Result{}, operatortool.ErrAccessDenied
	}
	scopeRequired := apikey.ScopeWrite
	if definition.Annotations.ReadOnly {
		scopeRequired = apikey.ScopeRead
	}
	var selector struct {
		ProjectID string `json:"project_id"`
		RequestID string `json:"request_id"`
	}
	if len(call.Arguments) > operatortool.MaxArgumentBytes || json.Unmarshal(call.Arguments, &selector) != nil || selector.ProjectID == "" || len(selector.ProjectID) > 256 {
		return operatortool.Result{}, operatortool.ErrInvalidArguments
	}
	identity := operatortool.ConnectionIdentity(ctx)
	metadata := mutation.Metadata{PrincipalID: identity.PrincipalID, OrganizationID: identity.OrganizationID, ProjectID: selector.ProjectID, Action: call.Name, Source: "mcp", Confirmation: "none", CorrelationID: uuid.NewString()}
	outcome := "failed"
	if !definition.Annotations.ReadOnly {
		defer func() {
			metadata.RetryIdentity, metadata.InputHash = "", ""
			audit, _ := json.Marshal(struct { //nolint:errcheck // Audit metadata and outcome contain only strings.
				mutation.Metadata
				Outcome string `json:"outcome"`
			}{metadata, outcome})
			e.service.config.Logger.InfoContext(ctx, "operator mutation", "audit", string(audit))
		}()
	}
	ctx, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: scopeRequired, ProjectID: selector.ProjectID})
	if err != nil {
		outcome = "denied"
		return operatortool.Result{}, err
	}
	scope, err := resolver(ctx)
	if err != nil {
		outcome = "denied"
		return operatortool.Result{}, operatortool.ErrAccessDenied
	}
	scope.project = tracker.ProjectID(selector.ProjectID)
	if call.Name == operatortool.UploadAttachment {
		if !e.service.hostedShared() {
			return operatortool.Result{}, operatortool.ErrAccessDenied
		}
		if _, _, err := operatortool.DecodeAttachmentArguments(call.Arguments); err != nil {
			return operatortool.Result{}, err
		}
		outcome = "authorized"
		return operatortool.EncodeResult(struct {
			EntryUpload bool              `json:"entry_upload"`
			Mutation    mutation.Metadata `json:"mutation"`
		}{true, metadata})
	}
	var request operatortool.WorkArguments
	arguments := call.Arguments
	var fields map[string]json.RawMessage
	if json.Unmarshal(arguments, &fields) != nil || fields == nil {
		return operatortool.Result{}, operatortool.ErrInvalidArguments
	}
	if !definition.Annotations.ReadOnly {
		if strings.TrimSpace(selector.RequestID) == "" || len(selector.RequestID) > 128 {
			return operatortool.Result{}, operatortool.ErrInvalidArguments
		}
		delete(fields, "request_id")
		arguments, err = json.Marshal(fields)
		if err != nil {
			return operatortool.Result{}, operatortool.ErrInvalidArguments
		}
	}
	if operatortool.IsWorkTool(call.Name) && call.Name != operatortool.FileIssue {
		request, err = operatortool.DecodeWorkArguments(call.Name, arguments)
		if err != nil {
			return operatortool.Result{}, err
		}
		if (call.Name == operatortool.EditItem || call.Name == operatortool.SetDependency) && fields["expected_revision"] != nil {
			fields["expected_revision"], err = json.Marshal(strconv.FormatInt(request.ExpectedRevision, 10))
			if err != nil {
				return operatortool.Result{}, operatortool.ErrInvalidArguments
			}
		}
		if request.Target != "pr" {
			resolve := e.service.resolveOperatorNativeItem
			if definition.Annotations.ReadOnly {
				resolve = e.service.resolveOperatorNativeReadItem
			}
			item, resolveErr := resolve(ctx, e.service.database.db, scope, request.Identifier)
			if resolveErr != nil {
				if definition.Annotations.ReadOnly {
					return operatortool.Result{}, safeWorkReadError(resolveErr)
				}
				return operatortool.Result{}, hubSafeChangeError(resolveErr)
			}
			request.Identifier = string(item.WorkItemID)
			fields["identifier"], err = json.Marshal(request.Identifier)
			if err != nil {
				return operatortool.Result{}, operatortool.ErrInvalidArguments
			}
			if request.Related != "" && !strings.HasPrefix(request.Related, "wi_") {
				related, resolveErr := e.service.resolveOperatorNativeItem(ctx, e.service.database.db, scope, request.Related)
				if resolveErr != nil {
					return operatortool.Result{}, hubSafeChangeError(resolveErr)
				}
				request.Related = string(related.WorkItemID)
				fields["related"], err = json.Marshal(request.Related)
				if err != nil {
					return operatortool.Result{}, operatortool.ErrInvalidArguments
				}
			}
		}
	}
	if !definition.Annotations.ReadOnly {
		arguments, err = json.Marshal(fields)
		if err != nil {
			return operatortool.Result{}, operatortool.ErrInvalidArguments
		}
		metadata, err = metadata.Bind(selector.RequestID, arguments)
		if err != nil {
			return operatortool.Result{}, operatortool.ErrInvalidArguments
		}
		// Audit is content-free. The command receipt, not this adapter, owns replay.
		ctx = mutation.WithContext(ctx, metadata)
	}
	var raw json.RawMessage
	if call.Name == operatortool.FileIssue {
		create, decodeErr := operatortool.DecodeFileIssue(arguments)
		if decodeErr != nil {
			return operatortool.Result{}, decodeErr
		}
		var priority *int
		if create.Priority != nil {
			level := *create.Priority - 1
			if level < 0 || level > 3 {
				return operatortool.Result{}, operatortool.ErrInvalidArguments
			}
			priority = &level
		}
		raw, err = e.service.operatorCreateNativeWork(ctx, scope, tracker.CreateIssue{Mutation: tracker.MutationForContext(ctx, ""), GitHubIssueURL: create.GitHubIssueURL, Title: create.Title, Body: create.Description, State: create.State, Labels: create.Labels, Priority: priority})
	} else if operatortool.IsWorkTool(call.Name) {
		metadata.ResourceID = request.Identifier
		if definition.Annotations.ReadOnly {
			if call.Name != operatortool.ListComments {
				return operatortool.Result{}, operatortool.ErrSnapshotUnavailable
			}
			if request.Limit == 0 {
				request.Limit = 50
			}
			page, readErr := e.service.nativeWorkComments(ctx, scope, request.Identifier, request.Limit, request.Cursor)
			if readErr != nil {
				return operatortool.Result{}, safeWorkReadError(readErr)
			}
			raw, err = json.Marshal(page)
		} else {
			raw, err = e.service.operatorNativeWork(ctx, scope, call.Name, request)
		}
	} else {
		return operatortool.Result{}, operatortool.ErrSnapshotUnavailable
	}
	if err != nil {
		return operatortool.Result{}, safeNativeWorkError(err)
	}
	outcome = "succeeded"
	metadata.RetryIdentity, metadata.InputHash = "", ""
	var resource struct {
		WorkItemID json.RawMessage  `json:"work_item_id"`
		Revision   tracker.Revision `json:"revision,string"`
		ID         string           `json:"comment_id"`
	}
	if err := json.Unmarshal(raw, &resource); err != nil {
		return operatortool.Result{}, operatortool.ErrSnapshotUnavailable
	}
	// Compatibility command receipts carry their internal numeric identity;
	// the caller's already-owned native alias remains the portable resource ID.
	var resourceID string
	if err := json.Unmarshal(resource.WorkItemID, &resourceID); err != nil {
		resourceID = request.Identifier
	}
	if resourceID == "" {
		resourceID = request.Identifier
	}
	resourceURL := "/api/v2/organizations/" + url.PathEscape(string(scope.organization)) + "/projects/" + url.PathEscape(string(scope.project)) + "/work-items/" + url.PathEscape(resourceID)
	if resource.ID != "" {
		resourceURL += "/comments#" + url.PathEscape(resource.ID)
	}
	content, err := json.Marshal(struct {
		CorrelationID string           `json:"correlation_id,omitempty"`
		ObservedAt    time.Time        `json:"observed_at"`
		ResourceID    string           `json:"resource_id"`
		URL           string           `json:"url"`
		Revision      tracker.Revision `json:"revision,omitempty,string"`
		CommentID     string           `json:"comment_id,omitempty"`
		Data          json.RawMessage  `json:"data"`
	}{metadata.CorrelationID, e.service.config.now(), resourceID, resourceURL, resource.Revision, resource.ID, raw})
	if err != nil || len(content) > operatortool.MaxResultBytes {
		return operatortool.Result{}, operatortool.ErrSnapshotUnavailable
	}
	return operatortool.Result{Content: content}, nil
}

func operatorNativeConflict(err error) error {
	var projected *operatortool.ConflictError
	if errors.As(err, &projected) {
		return projected
	}
	var failure *nativeError
	if !errors.As(err, &failure) || failure.status != http.StatusConflict {
		return nil
	}
	switch failure.Code {
	case "revision_conflict":
		return &operatortool.ConflictError{Code: failure.Code, CurrentRevision: int64(failure.CurrentRevision)}
	case "stale_execution":
		if len(failure.Details) == 0 {
			return &operatortool.ConflictError{Code: failure.Code}
		}
		expected, current, _ := staleExecutionAttempts(err)
		return &operatortool.ConflictError{Code: failure.Code, Details: &operatortool.ConflictDetails{ExpectedAttemptID: conversationOptional(expected), CurrentAttemptID: conversationOptional(current)}}
	}
	return nil
}

func safeNativeWorkError(err error) error {
	var refusal *nativeError
	if errors.As(err, &refusal) && refusal.Code == "invalid_request" {
		switch refusal.Message {
		case linkedIssueRepositoryRequired, linkedIssueRepositoryMismatch:
			return &operatortool.RequestError{Code: refusal.Code, Message: refusal.Message}
		}
	}
	if conflict := operatorNativeConflict(err); conflict != nil {
		return conflict
	}
	if safe := hubSafeChangeError(err); !errors.Is(safe, operatortool.ErrServiceUnavailable) {
		return safe
	}
	return fmt.Errorf("%w: %w", operatortool.ErrSnapshotUnavailable, err)
}
