package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type nativeWorkIssueContract struct {
	RequiredSections []string                         `json:"required_sections"`
	ScheduledRepair  *nativeWorkScheduledRepair       `json:"scheduled_repair,omitempty"`
	ReleaseDelivery  *nativeReleaseDeliveryDelegation `json:"release_delivery,omitempty"`
}

type nativeWorkScheduledRepair struct {
	PolicyID       string   `json:"policy_id"`
	Repository     string   `json:"repository"`
	Workflow       string   `json:"workflow"`
	Branches       []string `json:"branches"`
	Body           string   `json:"body"`
	EvidenceSchema int      `json:"evidence_schema"`
	Priority       int      `json:"priority"`
}

// The scope is reconstructed by operatorCurrentAuthority on every invocation.
// These are the same reads used by the native HTTP application, not a proxy.
type operatorWorkReads struct {
	service *Service
	scope   nativeScope
}

func (r operatorWorkReads) ReadWork(ctx context.Context, name string, request operatortool.WorkReadRequest) (result operatortool.Result, resultErr error) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	defer func() {
		if resultErr != nil {
			resultErr = fmt.Errorf("%s: %w", strings.ReplaceAll(name, "_", " "), resultErr)
		}
	}()
	s := r.service
	scope := r.scope
	scope.project = tracker.ProjectID(request.ProjectID)
	ctx = s.archiveContext(ctx, scope, request.ArchiveURL)
	defer func() {
		if resultErr == nil && len(result.Content) > 0 {
			result.Content, resultErr = withNativeArchive(result.Content, nativeArchiveDescriptor(ctx))
		}
	}()
	ctx, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead, ProjectID: request.ProjectID})
	if err != nil {
		return operatortool.Result{}, err
	}
	params := url.Values{"limit": {strconv.Itoa(request.Limit)}}
	if request.Cursor != "" {
		params.Set("cursor", request.Cursor)
	}
	if request.Offset != 0 && name != operatortool.WorkReferences && name != operatortool.BoardSessionHistory {
		return operatortool.Result{}, &operatortool.RequestError{Code: "invalid_request", Message: "offset is not supported here; page with the returned next_cursor"}
	}
	if name == operatortool.HealthFindings {
		page, err := s.readHealthFindings(ctx, scope, request.State, request.Since, request.Cursor, min(request.Limit, 100))
		return hubWorkResult(r, request, page, err)
	}
	if name == operatortool.WorkBulkExport {
		params := request.NativeWorkQuery()
		params.Del("include")
		page, err := s.readBulkExport(ctx, scope, params)
		if err != nil {
			return operatortool.Result{}, safeWorkReadError(err)
		}
		return operatortool.EncodeBulkExport(page)
	}
	if name == operatortool.WorkList {
		params := request.NativeWorkQuery()
		if _, err := nativeCreatedBy(scope, params); err != nil {
			return operatortool.Result{}, &operatortool.RequestError{Code: "invalid_request", Message: "created_by=me requires a human identity"}
		}
		compact := slices.Contains(request.Include, "summary")
		page, err := s.readIssuesProjection(ctx, scope, params, compact)
		if compact {
			return hubWorkResult(r, request, operatortool.NativeSummaryPageView(page), err)
		}
		return hubWorkResult(r, request, operatortool.NativeWorkPageView(request.ProjectID, page), err)
	}
	if name == operatortool.WorkConfig {
		project, err := readNativeProject(ctx, s.database.reader, scope)
		if err != nil {
			return operatortool.Result{}, safeWorkReadError(err)
		}
		contract, err := nativeIssueContract(ctx, s.database.reader, scope)
		if err != nil {
			return operatortool.Result{}, safeWorkReadError(err)
		}
		templates, err := config.ResolveReporterContracts(contract.Context)
		if err != nil {
			return operatortool.Result{}, safeWorkReadError(err)
		}
		issueContract := nativeWorkIssueContract{RequiredSections: contract.Sections}
		delivery, err := readReleaseDeliveryDelegation(ctx, s.database.reader, scope)
		if err != nil {
			return operatortool.Result{}, safeWorkReadError(err)
		}
		if delivery.Body != "" {
			issueContract.ReleaseDelivery = &delivery
		}
		approval, found, approvalErr := readProjectPolicy(ctx, s.database.reader, string(scope.organization)+"/"+string(scope.project))
		if approvalErr == nil && found && approval.Policy.Authored != nil {
			repairContract, err := config.ResolvePolicyIssueContract(approval.Policy)
			if err != nil {
				return operatortool.Result{}, safeWorkReadError(err)
			}
			if workflow, branch, priority, authorized := repairContract.ScheduledRepairWorkflow(); authorized {
				repository, err := policyRepository(ctx, s.database.reader, string(scope.organization)+"/"+string(scope.project))
				if err != nil {
					return operatortool.Result{}, safeWorkReadError(err)
				}
				branches := []string{branch}
				if branch == "" {
					branches = []string{"develop", "release/v*"}
				}
				issueContract.ScheduledRepair = &nativeWorkScheduledRepair{PolicyID: approval.Policy.ID, Repository: repository, Workflow: workflow, Branches: branches, Body: repairContract.ScheduledRepairBody(), EvidenceSchema: 1, Priority: priority}
			}
		}
		labels, err := readNativeLabels(ctx, s.database.reader, scope)
		return hubWorkResult(r, request, struct {
			Project           tracker.NativeProject    `json:"project"`
			Priorities        []int                    `json:"priorities"`
			Labels            []nativeLabel            `json:"labels"`
			ReporterTemplates config.ReporterContracts `json:"reporter_templates"`
			IssueContract     nativeWorkIssueContract  `json:"issue_contract"`
		}{project, []int{0, 1, 2, 3}, labels, templates, issueContract}, err)
	}
	item, err := s.resolveOperatorNativeReadItem(ctx, s.database.reader, scope, request.Reference)
	if err != nil {
		return operatortool.Result{}, safeWorkReadError(err)
	}
	request.Reference = string(item.WorkItemID)
	switch name {
	case operatortool.BoardReceipt, operatortool.BoardSession, operatortool.BoardSessionHistory, operatortool.WorkAttemptReceipt, operatortool.GitHubScopeTimings:
		if name == operatortool.GitHubScopeTimings && scope.credential.Runner.RunnerID != "" && scope.credential.Runner.RunnerID != request.RunnerID {
			return operatortool.Result{}, explain.ErrNotFound
		}
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
		if name == operatortool.WorkAttemptReceipt && evidence.Attempt != nil {
			evidence.ValidationAudit, err = readValidationAudit(ctx, s.database.reader, scope, "", request.Reference, "", evidence.Attempt.AttemptID)
			if err != nil {
				return operatortool.Result{}, safeWorkReadError(err)
			}
		}
		return operatortool.NativeRuntimeResult(name, request, evidence)
	case operatortool.WorkItem:
		if request.View == "recovery" {
			params := url.Values{"view": {"recovery"}, "limit": {"1"}, "cursor": {request.Cursor}}
			recovery, err := s.readNativeRecoveryPage(ctx, scope, request.Reference, "/api/v2/organizations/"+url.PathEscape(string(scope.organization))+"/projects/"+url.PathEscape(string(scope.project))+"/work-items/"+url.PathEscape(request.Reference), params)
			return hubWorkResult(r, request, recovery.Continuation, err)
		}
		view := operatortool.NativeItemView(request.ProjectID, item)
		if strings.Contains(item.Body, "detent-checks") {
			view.ValidationAudit, err = readValidationAudit(ctx, s.database.reader, scope, request.Reference, "", "", "")
		}
		return hubWorkResult(r, request, view, err)
	case operatortool.WorkExport:
		return hubWorkResult(r, request, item, nil)
	case operatortool.WorkRelationships:
		related, err := readNativeRelated(ctx, s.database.reader, scope, item.WorkItemID)
		if err != nil {
			return operatortool.Result{}, safeWorkReadError(err)
		}
		return hubWorkResult(r, request, struct {
			Dependencies []tracker.NativeWorkItemID  `json:"dependencies"`
			Blockers     []tracker.NativeDependency  `json:"blockers"`
			References   []tracker.ExternalReference `json:"references"`
			Related      []tracker.NativeDependency  `json:"related"`
		}{item.Dependencies, item.Blockers, item.ExternalReferences, related}, nil)
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
		params.Set("view", "blockers")
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
			refs = append(refs, operatortool.WorkReference{Kind: "change", ID: change.ID, VersionID: change.CurrentVersion, URL: item.WebURL + "/changes/" + url.PathEscape(change.ID)})
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
	view := operatortool.WorkReadResult[T]{ProjectID: request.ProjectID, Reference: request.Reference, URL: operatortool.WorkItemURL(request.ProjectID, request.Reference), GeneratedAt: r.service.config.now(), Freshness: explain.SourceAvailable, Data: data}
	if _, ok := any(data).(operatortool.NativeItem); ok {
		return operatortool.EncodeWorkItemResult(view)
	}
	return operatortool.EncodeResult(view)
}

func safeWorkReadError(err error) error {
	var export *operatortool.WorkBulkExportItemError
	if errors.As(err, &export) {
		return err
	}
	var ambiguous *explain.AmbiguousIdentityError
	if errors.Is(err, explain.ErrNotFound) || errors.Is(err, operatortool.ErrProjectScopeRequired) || errors.Is(err, operatortool.ErrAccessDenied) || errors.Is(err, operatortool.ErrInvalidArguments) || errors.As(err, &ambiguous) {
		return err
	}
	if errors.Is(err, sql.ErrNoRows) || isNativeNotFound(err) {
		return explain.ErrNotFound
	}
	if archive := nativeArchiveReadError(err); archive != nil {
		return archive
	}
	var native *nativeError
	if errors.As(err, &native) && native.status == 422 {
		return &operatortool.RequestError{Code: native.Code, Message: native.Message}
	}
	return &operatortool.ReadUnavailableError{Err: err}
}

func (s *Service) resolveOperatorNativeReadItem(ctx context.Context, query nativeQueryer, scope nativeScope, reference string) (tracker.NativeIssue, error) {
	item, err := s.resolveOperatorNativeItem(ctx, query, scope, reference)
	if scope.credential.HostedRole == "reporter" || !errors.Is(err, sql.ErrNoRows) || !strings.HasPrefix(reference, "wi_") {
		return item, err
	}
	var project string
	lookupErr := query.QueryRowContext(ctx, "SELECT project_id FROM issues WHERE organization_id=? AND native_id=?", scope.organization, reference).Scan(&project)
	if errors.Is(lookupErr, sql.ErrNoRows) {
		return tracker.NativeIssue{}, explain.ErrNotFound
	}
	if lookupErr != nil {
		return tracker.NativeIssue{}, lookupErr
	}
	if project == string(scope.project) {
		return tracker.NativeIssue{}, err
	}
	otherScope := scope
	otherScope.project = tracker.ProjectID(project)
	authErr := s.requireHostedProject(ctx, query, otherScope, false)
	if authErr == nil {
		authErr = authorizeNativeProject(ctx, query, otherScope)
	}
	if isNativeNotFound(authErr) || errors.Is(authErr, operatortool.ErrAccessDenied) {
		return tracker.NativeIssue{}, operatortool.ErrProjectScopeRequired
	}
	if authErr != nil {
		return tracker.NativeIssue{}, authErr
	}
	return tracker.NativeIssue{}, explain.ErrNotFound
}

func (s *Service) resolveOperatorNativeItem(ctx context.Context, query nativeQueryer, scope nativeScope, reference string) (tracker.NativeIssue, error) {
	if strings.Contains(reference, "/") {
		id, err := s.readableReference(ctx, query, scope, reference)
		if err != nil {
			return tracker.NativeIssue{}, err
		}
		reference = id
	}
	if strings.HasPrefix(reference, "wi_") {
		item, _, err := readNativeIssue(ctx, query, scope, reference)
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
	if err := query.QueryRowContext(ctx, "SELECT native_id FROM issues WHERE organization_id=? AND project_id=? AND number=?", scope.organization, scope.project, number).Scan(&id); err != nil {
		return tracker.NativeIssue{}, err
	}
	item, _, err := readNativeIssue(ctx, query, scope, id)
	return s.nativeIssueResponse(item), err
}

func (operatorWorkReads) WorkReadNames(context.Context) []string {
	return []string{operatortool.HealthFindings, operatortool.WorkList, operatortool.WorkItem, operatortool.WorkConfig, operatortool.WorkComments, operatortool.WorkHistory, operatortool.WorkVersion, operatortool.WorkRelationships, operatortool.WorkRuns, operatortool.WorkReferences, operatortool.WorkExport, operatortool.WorkBulkExport, operatortool.BoardActivity, operatortool.BoardReceipt, operatortool.BoardSession, operatortool.BoardSessionHistory, operatortool.WorkAttemptReceipt, operatortool.GitHubScopeTimings}
}

func (r operatorWorkReads) Explain(ctx context.Context, query explain.Query) (explain.IssueExplanation, error) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	ctx, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead, ProjectID: query.ProjectID})
	if err != nil {
		return explain.IssueExplanation{}, err
	}
	scope := r.scope
	scope.project = tracker.ProjectID(query.ProjectID)
	issue, err := r.service.resolveOperatorNativeReadItem(ctx, r.service.database.reader, scope, query.Reference)
	if err != nil {
		return explain.IssueExplanation{}, safeWorkReadError(err)
	}
	evidence, err := r.service.readNativeRuntime(ctx, scope, string(issue.WorkItemID), "")
	if err != nil {
		return explain.IssueExplanation{}, safeWorkReadError(err)
	}
	failures, partial, err := readNativeFailures(ctx, r.service.database.reader, scope, string(issue.WorkItemID), nil)
	if err != nil {
		return explain.IssueExplanation{}, safeWorkReadError(err)
	}
	out := explain.FromNativeEvidence(evidence)
	out.FailureSignature = latestNativeFailureSignature(failures, partial)
	return out, nil
}
