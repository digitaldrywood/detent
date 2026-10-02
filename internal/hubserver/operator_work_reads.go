package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// The scope is reconstructed by operatorCurrentAuthority on every invocation.
// These are the same reads used by the native HTTP application, not a proxy.
type operatorWorkReads struct {
	service *Service
	scope   nativeScope
}

func (r operatorWorkReads) ReadWork(ctx context.Context, name string, request operatortool.WorkReadRequest) (operatortool.Result, error) {
	s := r.service
	scope := r.scope
	scope.project = tracker.ProjectID(request.ProjectID)
	ctx, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead, ProjectID: request.ProjectID})
	if err != nil {
		return operatortool.Result{}, err
	}
	params := url.Values{"limit": {strconv.Itoa(request.Limit)}}
	if request.Cursor != "" {
		params.Set("cursor", request.Cursor)
	}
	if request.Offset != 0 && name != operatortool.WorkReferences && name != operatortool.BoardSessionHistory {
		return operatortool.Result{}, operatortool.ErrInvalidArguments
	}
	if name == operatortool.WorkList {
		params.Set("include", "summary")
		if request.State != "" {
			params.Set("state", request.State)
		}
		if request.Label != "" {
			params.Set("label", request.Label)
		}
		if request.Query != "" {
			params.Set("q", request.Query)
		}
		page, err := s.readIssues(ctx, scope, params)
		return hubWorkResult(r, request, operatortool.NativeItemPage(request.ProjectID, page.Page), err)
	}
	if name == operatortool.WorkConfig {
		project, err := readNativeProject(ctx, s.database.db, scope)
		if err != nil {
			return operatortool.Result{}, safeWorkReadError(err)
		}
		labels, err := readNativeLabels(ctx, s.database.db, scope)
		return hubWorkResult(r, request, struct {
			Project    tracker.NativeProject `json:"project"`
			Priorities []int                 `json:"priorities"`
			Labels     []nativeLabel         `json:"labels"`
		}{project, []int{0, 1, 2, 3}, labels}, err)
	}
	item, err := s.resolveOperatorNativeItem(ctx, scope, request.Reference)
	if err != nil {
		return operatortool.Result{}, safeWorkReadError(err)
	}
	request.Reference = string(item.WorkItemID)
	switch name {
	case operatortool.BoardReceipt, operatortool.BoardSession, operatortool.BoardSessionHistory, operatortool.WorkAttemptReceipt:
		if request.Cursor != "" {
			return operatortool.Result{}, operatortool.ErrInvalidArguments
		}
		selector := request.NativeAttemptID
		if request.AttemptID > 0 {
			selector = strconv.FormatInt(request.AttemptID, 10)
		}
		evidence, err := s.readNativeRuntime(ctx, scope, request.Reference, selector)
		if err != nil {
			return operatortool.Result{}, safeWorkReadError(err)
		}
		return operatortool.NativeRuntimeResult(name, request, evidence)
	case operatortool.WorkItem:
		return hubWorkResult(r, request, operatortool.NativeItemView(request.ProjectID, item), nil)
	case operatortool.WorkExport:
		return hubWorkResult(r, request, item, nil)
	case operatortool.WorkRelationships:
		return hubWorkResult(r, request, struct {
			Dependencies []tracker.NativeWorkItemID  `json:"dependencies"`
			Blockers     []tracker.NativeDependency  `json:"blockers"`
			References   []tracker.ExternalReference `json:"references"`
		}{item.Dependencies, item.Blockers, item.ExternalReferences}, nil)
	case operatortool.WorkComments:
		page, err := s.readComments(ctx, scope, request.Reference, params)
		return hubWorkResult(r, request, page, err)
	case operatortool.WorkHistory, operatortool.BoardActivity:
		page, err := s.readHistory(ctx, scope, request.Reference, params)
		return hubWorkResult(r, request, page, err)
	case operatortool.WorkVersion:
		version, err := s.readVersion(ctx, scope, request.Reference, request.CommentID, request.Revision)
		return hubWorkResult(r, request, version, err)
	case operatortool.WorkRuns:
		page, err := s.readAttempts(ctx, scope, request.Reference, params)
		return hubWorkResult(r, request, page, err)
	case operatortool.WorkReferences:
		// References deliberately do not read file/diff content or change bodies.
		// The review/artifacts child owns those application detail operations.
		changes, err := s.readChanges(ctx, scope, request.Reference)
		if err != nil {
			return operatortool.Result{}, safeWorkReadError(err)
		}
		refs := make([]operatortool.WorkReference, 0, len(changes))
		for _, change := range changes {
			refs = append(refs, operatortool.WorkReference{Kind: "change", ID: change.ID, VersionID: change.CurrentVersion, URL: "/projects/" + url.PathEscape(request.ProjectID) + "/issues/" + url.PathEscape(request.Reference) + "/changes/" + url.PathEscape(change.ID)})
		}
		if request.Cursor != "" {
			return operatortool.Result{}, operatortool.ErrInvalidArguments
		}
		artifacts, err := s.readArtifactReferences(ctx, scope, request.Reference)
		if err != nil {
			return operatortool.Result{}, err
		}
		for _, ref := range artifacts {
			refs = append(refs, operatortool.ArtifactWorkReference(request.ProjectID, request.Reference, ref))
		}
		prs, err := s.readWorkItemPullRequests(ctx, scope, request.Reference, "")
		if err != nil {
			return operatortool.Result{}, err
		}
		for _, pr := range prs {
			if pr.Number > 0 && pr.URL != "" {
				refs = append(refs, operatortool.WorkReference{Kind: "pull_request", State: pr.State, ID: strconv.Itoa(pr.Number), URL: pr.URL, ObservedAt: &pr.FetchedAt})
			}
		}
		page := operatortool.OffsetPage(refs, request.Offset, request.Limit)
		return hubWorkResult(r, request, page, nil)
	default:
		return operatortool.Result{}, operatortool.ErrReadUnavailable
	}
}

func hubWorkResult[T any](r operatorWorkReads, request operatortool.WorkReadRequest, data T, err error) (operatortool.Result, error) {
	if err != nil {
		return operatortool.Result{}, safeWorkReadError(err)
	}
	return operatortool.EncodeResult(operatortool.WorkReadResult[T]{ProjectID: request.ProjectID, Reference: request.Reference, URL: operatortool.WorkItemURL(request.ProjectID, request.Reference), GeneratedAt: r.service.config.now(), Freshness: explain.SourceAvailable, Data: data})
}

func safeWorkReadError(err error) error {
	var ambiguous *explain.AmbiguousIdentityError
	if errors.Is(err, explain.ErrNotFound) || errors.Is(err, operatortool.ErrAccessDenied) || errors.Is(err, operatortool.ErrInvalidArguments) || errors.As(err, &ambiguous) {
		return err
	}
	if errors.Is(err, sql.ErrNoRows) || isNativeNotFound(err) {
		return explain.ErrNotFound
	}
	var native *nativeError
	if errors.As(err, &native) && native.status == 422 {
		return operatortool.ErrInvalidArguments
	}
	return operatortool.ErrReadUnavailable
}

func (s *Service) resolveOperatorNativeItem(ctx context.Context, scope nativeScope, reference string) (tracker.NativeIssue, error) {
	if strings.HasPrefix(reference, "wi_") {
		item, _, err := readNativeIssue(ctx, s.database.db, scope, reference)
		return s.nativeIssueResponse(item), err
	}
	value := reference
	if prefix, number, ok := strings.Cut(reference, "#"); ok {
		if prefix != "" && prefix != string(scope.project) {
			return tracker.NativeIssue{}, explain.ErrNotFound
		}
		value = number
	}
	number, err := strconv.Atoi(value)
	if err != nil || number <= 0 {
		return tracker.NativeIssue{}, explain.ErrNotFound
	}
	var id string
	if err := s.database.db.QueryRowContext(ctx, "SELECT native_id FROM issues WHERE organization_id=? AND project_id=? AND number=?", scope.organization, scope.project, number).Scan(&id); err != nil {
		return tracker.NativeIssue{}, err
	}
	item, _, err := readNativeIssue(ctx, s.database.db, scope, id)
	return s.nativeIssueResponse(item), err
}

func (operatorWorkReads) WorkReadNames(context.Context) []string {
	return []string{operatortool.WorkList, operatortool.WorkItem, operatortool.WorkConfig, operatortool.WorkComments, operatortool.WorkHistory, operatortool.WorkVersion, operatortool.WorkRelationships, operatortool.WorkRuns, operatortool.WorkReferences, operatortool.WorkExport, operatortool.BoardActivity, operatortool.BoardReceipt, operatortool.BoardSession, operatortool.BoardSessionHistory, operatortool.WorkAttemptReceipt}
}

func (r operatorWorkReads) Explain(ctx context.Context, query explain.Query) (explain.IssueExplanation, error) {
	ctx, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead, ProjectID: query.ProjectID})
	if err != nil {
		return explain.IssueExplanation{}, err
	}
	scope := r.scope
	scope.project = tracker.ProjectID(query.ProjectID)
	issue, err := r.service.resolveOperatorNativeItem(ctx, scope, query.Reference)
	if err != nil {
		return explain.IssueExplanation{}, safeWorkReadError(err)
	}
	evidence, err := r.service.readNativeRuntime(ctx, scope, string(issue.WorkItemID), "")
	if err != nil {
		return explain.IssueExplanation{}, safeWorkReadError(err)
	}
	return explain.FromNativeEvidence(evidence), nil
}
