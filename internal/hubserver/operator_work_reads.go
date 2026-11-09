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
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
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
		if err := s.authorizeProjectRecord(ctx, scope, false); err != nil {
			return operatortool.Result{}, safeWorkReadError(err)
		}
		record, err := s.readProjectRecord(ctx, scope)
		if err != nil {
			return operatortool.Result{}, safeWorkReadError(err)
		}
		project, err := s.projectRecordNative(ctx, record)
		if err != nil {
			return operatortool.Result{}, safeWorkReadError(err)
		}
		approval, err := s.readTenantProjectPolicy(ctx, scope.organization, scope.project)
		if err != nil {
			return operatortool.Result{}, safeWorkReadError(err)
		}
		contract, err := config.ResolvePolicyIssueContract(approval.Policy)
		if approval.Policy.Authored != nil && len(approval.Policy.Authored.Files) > 0 {
			workflow, workflowErr := projectPolicyWorkflow(ctx, string(scope.organization)+"/"+string(scope.project), approval.Policy)
			if workflowErr != nil {
				return operatortool.Result{}, safeWorkReadError(workflowErr)
			}
			contract, err = config.ResolveIssueContract(workflow.SharedPrompt)
		}
		if err != nil {
			return operatortool.Result{}, safeWorkReadError(err)
		}
		templates, err := config.ResolveReporterContracts(contract.Context)
		if err != nil {
			return operatortool.Result{}, safeWorkReadError(err)
		}
		issueContract := nativeWorkIssueContract{RequiredSections: contract.Sections}
		if approval.Policy.Authored != nil {
			repairContract, err := config.ResolvePolicyIssueContract(approval.Policy)
			if err != nil {
				return operatortool.Result{}, safeWorkReadError(err)
			}
			if body := repairContract.ReleaseDeliveryBody(); body != "" {
				issueContract.ReleaseDelivery = &nativeReleaseDeliveryDelegation{PolicyID: approval.Policy.ID, Body: body}
			}
			if workflow, branch, priority, authorized := repairContract.ScheduledRepairWorkflow(); authorized {
				repository := record.Settings.CheckoutRepository
				if repository == "" {
					repository = record.Settings.Repository
				}
				branches := []string{branch}
				if branch == "" {
					branches = []string{"develop", "release/v*"}
				}
				issueContract.ScheduledRepair = &nativeWorkScheduledRepair{PolicyID: approval.Policy.ID, Repository: repository, Workflow: workflow, Branches: branches, Body: repairContract.ScheduledRepairBody(), EvidenceSchema: 1, Priority: priority}
			}
		}
		labels, err := s.readNativeLabels(ctx, scope)
		if err == nil {
			err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
				return s.authorizeBulkExportItem(ctx, r, scope, "")
			})
		}
		return hubWorkResult(r, request, struct {
			Project           tracker.NativeProject    `json:"project"`
			Priorities        []int                    `json:"priorities"`
			Labels            []nativeLabel            `json:"labels"`
			ReporterTemplates config.ReporterContracts `json:"reporter_templates"`
			IssueContract     nativeWorkIssueContract  `json:"issue_contract"`
		}{project, []int{0, 1, 2, 3}, labels, templates, issueContract}, err)
	}
	item, err := s.resolveOperatorRecordReadItem(ctx, scope, request.Reference)
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
			evidence.ValidationAudit, err = s.readValidationAuditRecord(ctx, scope, item, evidence.Attempt)
			if err != nil {
				return operatortool.Result{}, safeWorkReadError(err)
			}
		}
		return operatortool.NativeRuntimeResult(name, request, evidence)
	case operatortool.WorkItem:
		if request.View == "recovery" {
			params := url.Values{"view": {"recovery"}, "limit": {"1"}, "cursor": {request.Cursor}}
			recovery, err := s.readNativeRecoveryPageTenant(ctx, scope, request.Reference, "/api/v2/organizations/"+url.PathEscape(string(scope.organization))+"/projects/"+url.PathEscape(string(scope.project))+"/work-items/"+url.PathEscape(request.Reference), params)
			return hubWorkResult(r, request, recovery.Continuation, err)
		}
		view := operatortool.NativeItemView(request.ProjectID, item)
		if strings.Contains(item.Body, "detent-checks") {
			view.ValidationAudit, err = s.readValidationAuditRecord(ctx, scope, item, nil)
		}
		return hubWorkResult(r, request, view, err)
	case operatortool.WorkExport:
		return hubWorkResult(r, request, item, nil)
	case operatortool.WorkRelationships:
		related, err := s.readWorkItemRelationshipPage(ctx, scope, request, item)
		if err != nil {
			return operatortool.Result{}, safeWorkReadError(err)
		}
		return hubWorkResult(r, request, related, nil)
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

func (s *Service) resolveOperatorNativeReadItem(ctx context.Context, _ nativeQueryer, scope nativeScope, reference string) (tracker.NativeIssue, error) {
	return s.resolveOperatorRecordReadItem(ctx, scope, reference)
}

func (s *Service) resolveOperatorNativeItem(ctx context.Context, _ nativeQueryer, scope nativeScope, reference string) (tracker.NativeIssue, error) {
	item, err := s.resolveOperatorRecordReadItem(ctx, scope, reference)
	if errors.Is(err, explain.ErrNotFound) || errors.Is(err, operatortool.ErrProjectScopeRequired) {
		return tracker.NativeIssue{}, nativeNotFound()
	}
	return item, err
}

func (operatorWorkReads) WorkReadNames(context.Context) []string {
	return []string{operatortool.HealthFindings, operatortool.WorkList, operatortool.WorkItem, operatortool.WorkConfig, operatortool.WorkComments, operatortool.WorkHistory, operatortool.WorkVersion, operatortool.WorkRelationships, operatortool.WorkRuns, operatortool.WorkReferences, operatortool.WorkExport, operatortool.WorkBulkExport, operatortool.BoardActivity, operatortool.BoardReceipt, operatortool.BoardSession, operatortool.BoardSessionHistory, operatortool.WorkAttemptReceipt, operatortool.GitHubScopeTimings}
}

func (r operatorWorkReads) Explain(ctx context.Context, query explain.Query) (explain.IssueExplanation, error) {
	ctx = context.WithValue(ctx, explanationReadKey{}, true)
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	ctx, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead, ProjectID: query.ProjectID})
	if err != nil {
		return explain.IssueExplanation{}, err
	}
	scope := r.scope
	scope.project = tracker.ProjectID(query.ProjectID)
	issue, err := r.service.resolveOperatorRecordReadItem(ctx, scope, query.Reference)
	if err != nil {
		return explain.IssueExplanation{}, safeWorkReadError(err)
	}
	evidence, failures, partial, err := r.service.readOperatorExplanation(ctx, scope, issue)
	if err != nil {
		return explain.IssueExplanation{}, safeWorkReadError(err)
	}
	out, err := boundedNativeExplanation(evidence, latestNativeFailureSignature(failures, partial))
	if err != nil {
		return explain.IssueExplanation{}, safeWorkReadError(err)
	}
	return out, nil
}

func (s *Service) readOperatorExplanation(ctx context.Context, scope nativeScope, issue tracker.NativeIssue) (tracker.NativeRuntimeEvidence, []nativeFailure, bool, error) {
	item := string(issue.WorkItemID)
	now := s.config.now().UTC()
	evidence := tracker.NativeRuntimeEvidence{Issue: issue.RuntimeReference(), Selection: "latest", ObservedAt: now,
		Unavailable: []string{"efficiency_receipt", "scheduler_history_projection", "runner_capacity_projection", "native_admission_projection"}, Capacity: []tracker.NativeRuntimeCapacity{},
		Scheduling: tracker.NativeSchedulerDecision{Source: "native_claim_eligibility", Outcome: "unknown", At: now, WorkItemRevision: issue.Revision, Reason: "Runner capacity and admission projection are not available from this typed read"}}
	snapshots, sequence, partial, err := s.readOperatorAttemptSnapshots(ctx, scope, item)
	if err != nil {
		return evidence, nil, false, err
	}
	evidence.ChangeSequence, evidence.Issue.ChangeSequence = sequence, sequence
	if len(snapshots) > 0 {
		evidence, err = s.readNativeRuntime(ctx, scope, item, snapshots[0].attempt.ID)
		if err != nil {
			return evidence, nil, false, err
		}
		evidence.Selection = "latest"
	} else {
		evidence.Unavailable = append(evidence.Unavailable, "native_attempt")
	}
	failures := make([]nativeFailure, 0, len(snapshots))
	for _, snapshot := range snapshots {
		attempt, err := s.hydrateNativeAttemptTenant(ctx, snapshot, now)
		if err != nil {
			return evidence, nil, false, err
		}
		example, class := failureFromAttempt(attempt.NativeRunData, snapshot.attempt.Status)
		if example == "" {
			continue
		}
		at := attempt.UpdatedAt
		if attempt.Runtime != nil && attempt.Runtime.Landing != nil && !attempt.Runtime.Landing.ObservedAt.IsZero() {
			at = attempt.Runtime.Landing.ObservedAt
		}
		failures = append(failures, nativeFailure{attemptID: attempt.AttemptID, workItem: item, at: at, started: attempt.StartedAt, finished: attempt.UpdatedAt, example: example, class: class})
	}
	var decisions eventstore.SchedulerDecisionRows
	err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeBulkExportItem(ctx, r, scope, issue.WorkItemID); err != nil {
			return err
		}
		reader, ok := r.(eventstore.SchedulerDecisionReader)
		if !ok {
			return nativeRunEventStorageUnavailable()
		}
		var err error
		decisions, err = reader.SchedulerDecisions(ctx, scope.project, issue.WorkItemID)
		return err
	})
	if err == nil {
		evidence, err = s.hydrateSchedulerDecisionRows(ctx, scope, issue.WorkItemID, evidence, decisions)
	}
	return evidence, failures, partial, err
}

func (s *Service) readOperatorAttemptSnapshots(ctx context.Context, scope nativeScope, item string) ([]tenantAttemptSnapshot, int64, bool, error) {
	repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return nil, 0, false, nativeRunEventStorageUnavailable()
	}
	var snapshots []tenantAttemptSnapshot
	var sequence int64
	var partial bool
	err := repository.ReadTenantSnapshot(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		project, err := r.Project(ctx, scope.project)
		if err != nil {
			return err
		}
		if project.DeletedAt != nil {
			return corestore.ErrNotFound
		}
		work, err := r.WorkItem(ctx, scope.project, tracker.NativeWorkItemID(item))
		if err != nil {
			return err
		}
		if !scope.reporterOwns(tracker.Actor{Kind: stringValue(work.ActorKind), PrincipalID: stringValue(work.ActorPrincipalID)}) {
			return nativeNotFound()
		}
		board, err := r.BoardSequence(ctx, scope.project)
		if err != nil {
			return err
		}
		sequence = board.Sequence
		page, err := r.AttemptPage(ctx, work.ID, leasestore.AttemptCursor{}, 64)
		if err != nil {
			return err
		}
		partial = page.Next != (leasestore.AttemptCursor{})
		for _, attempt := range page.Items {
			if attempt.OrganizationID != scope.organization || attempt.IssueID != work.ID {
				return corestore.ErrNotFound
			}
			lease, err := r.Lease(ctx, attempt.LeaseID)
			if err != nil {
				return err
			}
			if lease.OrganizationID != scope.organization || lease.IssueID != work.ID || lease.FencingToken != attempt.FencingToken {
				return corestore.ErrNotFound
			}
			event, err := r.LatestRunEvent(ctx, scope.project, attempt.ID)
			if err != nil {
				return err
			}
			if event.OrganizationID != scope.organization || event.ProjectID != scope.project || event.WorkItemID != work.NativeID || event.AttemptID != attempt.ID {
				return corestore.ErrNotFound
			}
			snapshot := tenantAttemptSnapshot{attempt: attempt, lease: lease, event: event}
			if err := readTenantAttemptPayload(ctx, r, &snapshot); err != nil {
				return err
			}
			snapshots = append(snapshots, snapshot)
		}
		return nil
	})
	return snapshots, sequence, partial, err
}
