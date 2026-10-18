package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// This adapter forwards only the connection's own authenticated member token.
// A project's configured producer credential never supplies operator authority.
type dashboardChangeApplication struct {
	server *Server
	token  string
}

func (a dashboardChangeApplication) client(args operatortool.ChangeArguments) (*hubclient.NativeClient, error) {
	if a.token == "" || a.server.registry == nil {
		return nil, operatortool.ErrServiceUnavailable
	}
	tracked, ok := a.server.registry.Get(project.ID(args.ProjectID))
	if !ok {
		return nil, operatortool.ErrAccessDenied
	}
	source, ok := tracked.Connector().(nativeClientSource)
	if !ok || source.NativeClient() == nil {
		return nil, operatortool.ErrServiceUnavailable
	}
	return source.NativeClient().ArtifactReader(a.token), nil
}
func (a dashboardChangeApplication) result(args operatortool.ChangeArguments) operatortool.ChangeResult {
	result := operatortool.ChangeResult{ProjectID: args.ProjectID, WorkItemID: args.ItemID, ChangeID: args.ChangeID, URL: "/projects/" + url.PathEscape(args.ProjectID), GeneratedAt: time.Now().UTC(), Freshness: "live"}
	if args.ItemID != "" {
		result.URL += "/issues/" + url.PathEscape(args.ItemID)
	}
	if args.ChangeID != "" {
		result.URL += "/changes/" + url.PathEscape(args.ChangeID)
	}
	if args.VersionID != "" {
		result.URL += "?version=" + url.QueryEscape(args.VersionID)
	}
	return result
}
func (a dashboardChangeApplication) ReadChange(ctx context.Context, name string, args operatortool.ChangeArguments) (operatortool.ChangeResult, error) {
	result := a.result(args)
	result.OrganizationID = operatortool.ConnectionIdentity(ctx).OrganizationID
	if name == operatortool.ArtifactLibrary {
		result.URL = "/library?project=" + url.QueryEscape(args.ProjectID)
		return a.readLibrary(ctx, args, result)
	}
	client, err := a.client(args)
	if err != nil {
		return result, err
	}
	item := tracker.NativeWorkItemID(args.ItemID)
	switch name {
	case operatortool.GetAttemptDiff, operatortool.GetWorkItemDiff:
		attempt := args.AttemptID
		if name == operatortool.GetWorkItemDiff {
			attempt = ""
		}
		diff, err := client.StoredDiff(ctx, item, attempt, args.Source, args.Sequence)
		result.Diff, result.NextOffset = operatortool.ChangeDiffPage(diff, args)
		return result, safeChangeError(err)
	case operatortool.ListWorkItemPullRequests:
		rows, err := client.PullRequestDetails(ctx, item)
		result.PullRequests, result.NextOffset = operatortool.ChangePage(rows, args)
		return result, safeChangeError(err)
	case operatortool.GetNativeRun:
		attempt, err := client.NativeAttempt(ctx, item, args.AttemptID)
		if err != nil {
			return result, safeChangeError(err)
		}
		result.Attempt = &attempt
		changes, err := client.Changes(ctx, item)
		if err != nil {
			return result, safeChangeError(err)
		}
		result.Changes = changes
		refs, err := client.Artifacts(ctx, item)
		for _, ref := range refs {
			if ref.AttemptID == args.AttemptID {
				result.Artifacts = append(result.Artifacts, ref)
			}
		}
		return result, safeChangeError(err)
	case operatortool.ListChanges:
		rows, err := client.Changes(ctx, item)
		result.Changes, result.NextOffset = operatortool.ChangePage(rows, args)
		return result, safeChangeError(err)
	case operatortool.GetChange, operatortool.GetChangeVersion:
		detail, err := client.Change(ctx, item, args.ChangeID)
		if err != nil {
			return result, safeChangeError(err)
		}
		if name == operatortool.GetChange {
			result.Detail = &detail
		} else {
			for _, v := range detail.Versions {
				if v.ID == args.VersionID {
					result.Version = &v
				}
			}
			if result.Version == nil {
				return result, operatortool.ErrAccessDenied
			}
		}
	case operatortool.GetChangeReviewPolicy:
		policy, err := client.ChangeReviewPolicy(ctx)
		result.Policy = &policy
		return result, safeChangeError(err)
	case operatortool.ChangeViewedFiles:
		rows, err := client.ChangeViewedFiles(ctx, item, args.ChangeID, args.VersionID)
		result.Viewed, result.NextOffset = operatortool.ChangePage(rows, args)
		return result, safeChangeError(err)
	case operatortool.GetArtifactReference:
		ref, err := client.ArtifactReference(ctx, item, args.ArtifactID, args.Revision)
		result.Artifacts = []artifact.Reference{ref}
		return result, safeChangeError(err)
	case operatortool.ArtifactServices:
		result.Services, err = client.ArtifactServices(ctx)
		return result, safeChangeError(err)
	case operatortool.ArtifactReferences:
		rows, err := client.Artifacts(ctx, item)
		result.Artifacts, result.NextOffset = operatortool.ChangePage(rows, args)
		return result, safeChangeError(err)
	default:
		return result, operatortool.ErrUnknownTool
	}
	return result, nil
}
func (a dashboardChangeApplication) MutateChange(ctx context.Context, name string, args operatortool.ChangeArguments) (operatortool.ChangeResult, error) {
	result := a.result(args)
	result.OrganizationID = operatortool.ConnectionIdentity(ctx).OrganizationID
	client, err := a.client(args)
	if err != nil {
		return result, err
	}
	item := tracker.NativeWorkItemID(args.ItemID)
	m := tracker.Mutation{IdempotencyKey: args.RequestID}
	var receipt any
	switch name {
	case operatortool.PublishChangeVersion:
		if args.ExpectedVersionID == nil || args.Code == nil {
			return result, operatortool.ErrInvalidArguments
		}
		version, publishErr := client.PublishChangeVersion(ctx, item, args.ChangeID, tracker.PublishChangeVersion{Mutation: m, ExpectedVersionID: *args.ExpectedVersionID, ChangeVersionInput: tracker.ChangeVersionInput{BaseSHA: args.BaseSHA, HeadSHA: args.HeadSHA, MergeBaseSHA: args.MergeBaseSHA, Repository: args.Repository, Code: *args.Code, Artifacts: args.Artifacts, PolicyID: args.PolicyID, External: args.External}})
		if publishErr != nil {
			return result, safeChangeError(publishErr)
		}
		result.Version = &version
		receipt = version
		detail, readErr := client.Change(ctx, item, args.ChangeID)
		if readErr != nil {
			return result, safeChangeError(readErr)
		}
		detail.Versions, detail.Reviews, detail.Checks, detail.Discussion = nil, nil, nil, nil
		result.Detail = &detail
		issue, readErr := client.Issue(ctx, item)
		if readErr != nil {
			return result, safeChangeError(readErr)
		}
		result.WorkItemState = issue.State
	case operatortool.CreateChange:
		receipt, err = client.CreateChange(ctx, item, tracker.CreateChange{Mutation: m, Title: args.Title, Body: args.Body, LinkedIssues: args.LinkedIssues})
	case operatortool.DiscussChange:
		receipt, err = client.DiscussChange(ctx, item, args.ChangeID, tracker.DiscussChange{Mutation: m, VersionID: args.VersionID, Body: args.Body})
	case operatortool.ViewChangeFile:
		if args.Bundle == nil {
			return result, operatortool.ErrInvalidArguments
		}
		receipt, err = client.ViewChangeFile(ctx, item, args.ChangeID, args.VersionID, tracker.ViewChangeFile{Mutation: m, Bundle: *args.Bundle, FileSHA256: args.FileSHA256, Viewed: args.Viewed})
	case operatortool.ReviewChange:
		receipt, err = client.ReviewChange(ctx, item, args.ChangeID, args.VersionID, tracker.ReviewChange{Mutation: m, ExpectedRevision: tracker.Revision(args.ExpectedRevision), ExpectedVersionID: args.VersionID, Decision: args.Decision, Body: args.Body, Bundle: args.Bundle})
	case operatortool.ApproveChangeReviewPolicy:
		if args.Policy == nil {
			return result, operatortool.ErrInvalidArguments
		}
		receipt, err = client.ApproveChangeReviewPolicy(ctx, tracker.ApproveChangeReviewPolicy{Mutation: m, ExpectedID: args.ExpectedPolicyID, Policy: *args.Policy})
	case operatortool.BindArtifactService:
		if args.Binding == nil {
			return result, operatortool.ErrInvalidArguments
		}
		receipt, err = client.BindArtifactService(ctx, *args.Binding, args.RequestID)
	case operatortool.ArtifactAccess:
		access, err := client.ArtifactDownload(ctx, item, args.ArtifactID, args.Revision, args.SHA256)
		if err != nil {
			return result, safeChangeError(err)
		}
		result.Access = &access
		return result, nil
	default:
		return result, operatortool.ErrUnknownTool
	}
	if err != nil {
		return result, safeChangeError(err)
	}
	result.Receipt, err = json.Marshal(receipt)
	if err == nil && name == operatortool.CreateChange {
		var change tracker.ChangeRequest
		if err = json.Unmarshal(result.Receipt, &change); err == nil {
			args.ChangeID = change.ID
			result.ChangeID = change.ID
			result.URL = a.result(args).URL
		}
	}
	return result, err
}

func (a dashboardChangeApplication) readLibrary(ctx context.Context, args operatortool.ChangeArguments, result operatortool.ChangeResult) (operatortool.ChangeResult, error) {
	ctx, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: "read", ProjectID: args.ProjectID})
	if err != nil {
		return result, err
	}
	s := a.server
	if s.registry == nil {
		return result, operatortool.ErrServiceUnavailable
	}
	if _, ok := s.registry.Get(project.ID(args.ProjectID)); !ok {
		return result, operatortool.ErrAccessDenied
	}
	filters := libraryFilters{ProjectID: args.ProjectID, Kind: args.Kind, Status: args.Status}
	rows, _, err := s.libraryRows(ctx, filters, s.libraryProjectSources(nil))
	if err != nil {
		return result, operatortool.ErrServiceUnavailable
	}
	rows = filterLibraryRows(rows, filters)
	rows, result.NextOffset = operatortool.ChangePage(rows, args)
	for _, row := range rows {
		if row.ProjectID != args.ProjectID {
			return result, operatortool.ErrAccessDenied
		}
		result.Library = append(result.Library, operatortool.ArtifactLibraryRow{ID: row.ID, ProjectID: row.ProjectID, Kind: row.Kind, Title: row.Title, State: row.State, ValidationStatus: row.ValidationStatus, ReviewURL: row.ReviewURL, SourceURL: row.SourceURL, ArtifactPath: row.ArtifactPath, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt})
	}
	return result, nil
}
func safeChangeError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, operatortool.ErrAccessDenied) {
		return operatortool.ErrAccessDenied
	}
	var failure *hubclient.APIError
	if errors.As(err, &failure) {
		switch failure.Status {
		case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
			return operatortool.ErrAccessDenied
		case http.StatusConflict:
			return mutation.ErrConflict
		}
	}
	return operatortool.ErrServiceUnavailable
}
func (s *Server) operatorChangeProposal(ctx context.Context, name string, raw json.RawMessage) (chat.Action, error) {
	var args operatortool.ChangeArguments
	if operatortool.DecodeArguments(raw, &args) != nil {
		return chat.Action{}, operatortool.ErrInvalidArguments
	}
	app, err := operatortool.CurrentChanges(ctx)
	if err != nil {
		return chat.Action{}, err
	}
	read := operatortool.GetChangeReviewPolicy
	if args.ItemID != "" {
		read = operatortool.ListChanges
	}
	if args.ChangeID != "" {
		read = operatortool.GetChange
	}
	if name == operatortool.ArtifactAccess {
		read = operatortool.GetArtifactReference
	}
	current := operatortool.ChangeResult{}
	if args.ItemID != "" || name == operatortool.ApproveChangeReviewPolicy {
		current, err = app.ReadChange(ctx, read, args)
	}
	if err != nil {
		return chat.Action{}, err
	}
	if name == operatortool.ReviewChange {
		if current.Detail == nil || int64(current.Detail.Change.Revision) != args.ExpectedRevision || current.Detail.Change.CurrentVersion != args.VersionID {
			return chat.Action{}, operatortool.ErrInvalidArguments
		}
	}
	if name == operatortool.ArtifactAccess {
		found := false
		for _, ref := range current.Artifacts {
			if ref.ArtifactID == args.ArtifactID && ref.Revision == args.Revision && ref.SHA256 == args.SHA256 {
				found = true
			}
		}
		if !found {
			return chat.Action{}, operatortool.ErrAccessDenied
		}
	}
	policyID := ""
	if current.Policy != nil {
		policyID = current.Policy.ID
	}
	return chat.Action{CurrentState: policyID, Kind: chat.ActionKind(name), ProjectID: args.ProjectID, IssueID: args.ItemID, Identifier: args.ChangeID, ResourceURL: current.URL, Title: args.ChangeID, Description: args.Body}, nil
}
func (s *Server) executeChangeAction(ctx context.Context, action chat.Action) (chat.ActionExecution, error) {
	app, err := operatortool.CurrentChanges(ctx)
	if err != nil {
		return chat.ActionExecution{}, err
	}
	var args operatortool.ChangeArguments
	if operatortool.DecodeArguments(action.Arguments, &args) != nil {
		return chat.ActionExecution{}, operatortool.ErrInvalidArguments
	}
	args.RequestID = action.RequestID
	result, err := app.MutateChange(ctx, string(action.Kind), args)
	if err != nil {
		return chat.ActionExecution{}, err
	}
	return chat.ActionExecution{Message: "Change command completed.", ResourceID: args.ItemID, Identifier: result.ChangeID, URL: result.URL}, nil
}

// Ephemeral downloads use the same durable operator retry/audit receipt, while
// tokens stay only in this call's response. Every replay checks exact ownership
// and obtains fresh service access instead of persisting a bearer secret.
func (s *Server) executeOperatorArtifactAccess(ctx context.Context, app operatortool.ChangeApplication, args operatortool.ChangeArguments, raw json.RawMessage) (operatortool.Result, error) {
	correlation, err := randomMutationCorrelation()
	if err != nil {
		return operatortool.Result{}, errOperatorCommandUnavailable
	}
	identity := operatortool.ConnectionIdentity(ctx)
	m := mutation.Metadata{PrincipalID: identity.PrincipalID, OrganizationID: identity.OrganizationID, ProjectID: args.ProjectID, ResourceID: args.ItemID, Action: operatortool.ArtifactAccess, Source: "mcp", Confirmation: "none", CorrelationID: correlation}
	m, err = m.Bind(args.RequestID, raw)
	if err != nil {
		return operatortool.Result{}, operatortool.ErrInvalidArguments
	}
	outcome := "failed"
	defer func() { s.auditMutation(ctx, m, outcome) }()
	current, err := app.ReadChange(ctx, operatortool.GetArtifactReference, args)
	if err != nil {
		return operatortool.Result{}, err
	}
	if len(current.Artifacts) != 1 || current.Artifacts[0].SHA256 != args.SHA256 || current.Artifacts[0].Availability != "available" {
		return operatortool.Result{}, operatortool.ErrAccessDenied
	}
	records, ok := s.store.(store.OperatorMutations)
	if !ok {
		return operatortool.Result{}, errOperatorCommandUnavailable
	}
	receipt, found, err := records.OperatorMutation(ctx, m)
	if err != nil {
		return operatortool.Result{}, safeMutationError(err)
	}
	if found && receipt.Outcome != "succeeded" {
		return operatortool.Result{}, mutation.ErrUncertain
	}
	if !found {
		reserved, err := records.ReserveOperatorMutation(ctx, m)
		if err != nil {
			return operatortool.Result{}, safeMutationError(err)
		}
		if !reserved {
			return operatortool.Result{}, mutation.ErrUncertain
		}
		claimed, err := records.ClaimOperatorMutation(ctx, m)
		if err != nil || !claimed {
			return operatortool.Result{}, mutation.ErrUncertain
		}
	}
	value, err := app.MutateChange(mutation.WithContext(ctx, m), operatortool.ArtifactAccess, args)
	if err != nil {
		return operatortool.Result{}, err
	}
	if !found {
		if err := records.CompleteOperatorMutation(ctx, store.OperatorReceipt{Metadata: m, Outcome: "succeeded", URL: value.URL}); err != nil {
			return operatortool.Result{}, errOperatorCommandUnavailable
		}
	}
	outcome = "succeeded"
	return operatortool.BoundedChangeResult(value)
}
