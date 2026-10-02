package hubserver

import (
	"context"
	"encoding/json"
	"net/url"
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
	if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead}); err != nil {
		return nil, err
	}
	writable := false
	if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeWrite}); err == nil {
		writable = true
	}
	definitions, err := operatortool.NewAuthorizedExecutor(operatortool.NewExecutor(operatortool.Dependencies{})).ListTools(ctx)
	if err != nil {
		return nil, err
	}
	for _, definition := range operatortool.CommandCatalog() {
		switch definition.Name {
		case operatortool.FileIssue, operatortool.EditItem, operatortool.AddComment, operatortool.EditComment, operatortool.SetDependency, operatortool.RestoreItem, operatortool.OrderItem, operatortool.SetQueuePriority:
			if writable {
				definitions = append(definitions, definition)
			}
		case operatortool.ListComments:
			definitions = append(definitions, definition)
		}
	}
	if writable && e.service.hostedShared() {
		definitions = append(definitions, operatortool.AttachmentCatalog()...)
	}
	return definitions, nil
}

func (e nativeOperatorExecutor) Execute(ctx context.Context, call operatortool.Call) (operatortool.Result, error) {
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
	metadata := mutation.Metadata{PrincipalID: identity.PrincipalID, OrganizationID: identity.OrganizationID, ProjectID: selector.ProjectID, Action: call.Name, Source: "mcp", Mode: "confirmation", Confirmation: "none", CorrelationID: uuid.NewString()}
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
		return operatortool.EncodeResult(map[string]bool{"entry_upload": true})
	}
	var request operatortool.WorkArguments
	arguments := call.Arguments
	var fields map[string]json.RawMessage
	if json.Unmarshal(arguments, &fields) != nil {
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
		metadata, err = metadata.Bind(selector.RequestID, arguments)
		if err != nil {
			return operatortool.Result{}, operatortool.ErrInvalidArguments
		}
		// Audit is content-free. The command receipt, not this adapter, owns replay.
		ctx = mutation.WithContext(ctx, metadata)
	}
	var raw json.RawMessage
	if call.Name == operatortool.FileIssue {
		var create struct {
			ProjectID   string   `json:"project_id"`
			Title       string   `json:"title"`
			Description string   `json:"description"`
			State       string   `json:"state"`
			Labels      []string `json:"labels"`
			Priority    *int     `json:"priority"`
		}
		if operatortool.DecodeArguments(arguments, &create) != nil || strings.TrimSpace(create.Title) == "" || len(create.Title) > 256 || strings.TrimSpace(create.Description) == "" || len(create.Description) > 32768 || len(create.State) > 256 || len(create.Labels) > 64 {
			return operatortool.Result{}, operatortool.ErrInvalidArguments
		}
		for _, label := range create.Labels {
			if label == "" || len(label) > 200 {
				return operatortool.Result{}, operatortool.ErrInvalidArguments
			}
		}
		var priority *int
		if create.Priority != nil {
			level := *create.Priority - 1
			if level < 0 || level > 3 {
				return operatortool.Result{}, operatortool.ErrInvalidArguments
			}
			priority = &level
		}
		raw, err = e.service.operatorCreateNativeWork(ctx, scope, tracker.CreateIssue{Mutation: tracker.MutationForContext(ctx, ""), Title: create.Title, Body: create.Description, State: create.State, Labels: create.Labels, Priority: priority})
	} else if operatortool.IsWorkTool(call.Name) {
		request, err = operatortool.DecodeWorkArguments(call.Name, arguments)
		if err != nil {
			return operatortool.Result{}, err
		}
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
				return operatortool.Result{}, operatortool.ErrSnapshotUnavailable
			}
			raw, err = json.Marshal(page)
		} else {
			raw, err = e.service.operatorNativeWork(ctx, scope, call.Name, request)
		}
	} else {
		// Hub-only deployments have no daemon explainer, approval service or
		// orchestrator command owner. They never fall back to compatibility APIs.
		return operatortool.Result{}, operatortool.ErrSnapshotUnavailable
	}
	if err != nil {
		return operatortool.Result{}, operatortool.ErrSnapshotUnavailable
	}
	outcome = "succeeded"
	metadata.RetryIdentity, metadata.InputHash = "", ""
	var resource struct {
		WorkItemID json.RawMessage  `json:"work_item_id"`
		Revision   tracker.Revision `json:"revision,string"`
		ID         string           `json:"id"`
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
