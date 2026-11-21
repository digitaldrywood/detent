package hubserver

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubcontent"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
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
	scope   hubscope.NativeScope
}

func (r operatorWorkReads) ReadWork(ctx context.Context, name string, request operatortool.WorkReadRequest) (result operatortool.Result, resultErr error) {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	defer func() {
		if resultErr != nil {
			resultErr = fmt.Errorf("%s: %w", strings.ReplaceAll(name, "_", " "), resultErr)
		}
	}()
	s := r.service
	scope := r.scope
	scope.Project = tracker.ProjectID(request.ProjectID)
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
			return operatortool.Result{}, hubapi.SafeWorkReadError(err)
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
			return operatortool.Result{}, hubapi.SafeWorkReadError(err)
		}
		record, err := s.readProjectRecord(ctx, scope)
		if err != nil {
			return operatortool.Result{}, hubapi.SafeWorkReadError(err)
		}
		project, err := s.projectRecordNative(ctx, record)
		if err != nil {
			return operatortool.Result{}, hubapi.SafeWorkReadError(err)
		}
		approval, err := s.readTenantProjectPolicy(ctx, scope.Organization, scope.Project)
		if err != nil {
			return operatortool.Result{}, hubapi.SafeWorkReadError(err)
		}
		contract, err := approvedProjectIssueContract(ctx, scope.Organization, scope.Project, approval)
		if err != nil {
			return operatortool.Result{}, hubapi.SafeWorkReadError(err)
		}
		templates, err := config.ResolveReporterContracts(contract.Context)
		if err != nil {
			return operatortool.Result{}, hubapi.SafeWorkReadError(err)
		}
		issueContract := nativeWorkIssueContract{RequiredSections: contract.Sections}
		if approval.Policy.Authored != nil {
			repairContract, err := config.ResolvePolicyIssueContract(approval.Policy)
			if err != nil {
				return operatortool.Result{}, hubapi.SafeWorkReadError(err)
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
			err = s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
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
		return operatortool.Result{}, hubapi.SafeWorkReadError(err)
	}
	request.Reference = string(item.WorkItemID)
	switch name {
	case operatortool.BoardReceipt, operatortool.BoardSession, operatortool.BoardSessionHistory, operatortool.WorkAttemptReceipt, operatortool.GitHubScopeTimings:
		if name == operatortool.GitHubScopeTimings && scope.Credential.Runner.RunnerID != "" && scope.Credential.Runner.RunnerID != request.RunnerID {
			return operatortool.Result{}, explain.ErrNotFound
		}
		if name == operatortool.WorkAttemptReceipt && request.Cursor != "" {
			evidence, _, _, err := s.readSelectedNativeRuntimeTenant(ctx, scope, string(item.WorkItemID), runtimeAttemptSelector{attemptID: request.NativeAttemptID, evidenceAfter: request.Cursor})
			if err != nil {
				return operatortool.Result{}, hubapi.SafeWorkReadError(err)
			}
			return operatortool.NativeRuntimeResult(name, request, evidence)
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
			return operatortool.Result{}, hubapi.SafeWorkReadError(err)
		}
		if name == operatortool.WorkAttemptReceipt && evidence.Attempt != nil {
			evidence.Checklist, err = s.readExecutionChecklistReport(ctx, scope, item.WorkItemID, evidence.Attempt.AttemptID)
			if err != nil {
				return operatortool.Result{}, hubapi.SafeWorkReadError(err)
			}
			evidence.ValidationAudit, err = s.readValidationAuditRecord(ctx, scope, item, evidence.Attempt)
			if err != nil {
				return operatortool.Result{}, hubapi.SafeWorkReadError(err)
			}
		}
		return operatortool.NativeRuntimeResult(name, request, evidence)
	case operatortool.WorkItem, operatortool.WorkExport:
		if err := s.readOperatorItemChangeSequence(ctx, scope, &item); err != nil {
			return operatortool.Result{}, hubapi.SafeWorkReadError(err)
		}
	}
	switch name {
	case operatortool.WorkItem:
		if request.View == "recovery" {
			params := url.Values{"view": {"recovery"}, "limit": {"1"}, "cursor": {request.Cursor}}
			recovery, err := s.readNativeRecoveryPageTenant(ctx, scope, request.Reference, "/api/v2/organizations/"+url.PathEscape(string(scope.Organization))+"/projects/"+url.PathEscape(string(scope.Project))+"/work-items/"+url.PathEscape(request.Reference), params)
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
			return operatortool.Result{}, hubapi.SafeWorkReadError(err)
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
			return operatortool.Result{}, hubapi.SafeWorkReadError(err)
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
		return operatortool.Result{}, hubapi.SafeWorkReadError(err)
	}
	view := operatortool.WorkReadResult[T]{ProjectID: request.ProjectID, Reference: request.Reference, URL: operatortool.WorkItemURL(request.ProjectID, request.Reference), GeneratedAt: r.service.config.Clock(), Freshness: explain.SourceAvailable, Data: data}
	if _, ok := any(data).(operatortool.NativeItem); ok {
		return operatortool.EncodeWorkItemResult(view)
	}
	return operatortool.EncodeResult(view)
}

func (s *Service) resolveOperatorNativeReadItem(ctx context.Context, _ hubapi.NativeQueryer, scope hubscope.NativeScope, reference string) (tracker.NativeIssue, error) {
	return s.resolveOperatorRecordReadItem(ctx, scope, reference)
}

func (s *Service) resolveOperatorNativeItem(ctx context.Context, _ hubapi.NativeQueryer, scope hubscope.NativeScope, reference string) (tracker.NativeIssue, error) {
	item, err := s.resolveOperatorRecordReadItem(ctx, scope, reference)
	if errors.Is(err, explain.ErrNotFound) || errors.Is(err, operatortool.ErrProjectScopeRequired) {
		return tracker.NativeIssue{}, hubapi.NativeNotFound()
	}
	return item, err
}

func (operatorWorkReads) WorkReadNames(context.Context) []string {
	return []string{operatortool.HealthFindings, operatortool.WorkList, operatortool.WorkItem, operatortool.WorkConfig, operatortool.WorkComments, operatortool.WorkHistory, operatortool.WorkVersion, operatortool.WorkRelationships, operatortool.WorkRuns, operatortool.WorkReferences, operatortool.WorkExport, operatortool.WorkBulkExport, operatortool.BoardActivity, operatortool.BoardReceipt, operatortool.BoardSession, operatortool.BoardSessionHistory, operatortool.WorkAttemptReceipt, operatortool.GitHubScopeTimings}
}

func (r operatorWorkReads) Explain(ctx context.Context, query explain.Query) (explain.IssueExplanation, error) {
	ctx = r.service.withRequestDetails(context.WithValue(ctx, hubcontent.ExplanationReadKey{}, true))
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	ctx, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead, ProjectID: query.ProjectID})
	if err != nil {
		return explain.IssueExplanation{}, err
	}
	scope := r.scope
	scope.Project = tracker.ProjectID(query.ProjectID)
	issue, err := r.service.resolveOperatorRecordReadItem(ctx, scope, query.Reference)
	if err != nil {
		return explain.IssueExplanation{}, hubapi.SafeWorkReadError(err)
	}
	evidence, failures, partial, err := r.service.readOperatorExplanation(ctx, scope, issue)
	if err != nil {
		return explain.IssueExplanation{}, hubapi.SafeWorkReadError(err)
	}
	if err := r.service.readRuntimeAdmissionTenant(ctx, scope, issue, &evidence, nil); err != nil {
		return explain.IssueExplanation{}, hubapi.SafeWorkReadError(err)
	}
	out, err := boundedNativeExplanation(evidence, latestNativeFailureSignature(failures, partial))
	if err != nil {
		return explain.IssueExplanation{}, hubapi.SafeWorkReadError(err)
	}
	return out, nil
}

func (s *Service) readOperatorExplanation(ctx context.Context, scope hubscope.NativeScope, issue tracker.NativeIssue) (tracker.NativeRuntimeEvidence, []nativeFailure, bool, error) {
	return s.readOperatorEvidence(ctx, scope, issue, true)
}

func (s *Service) readOperatorEvidence(ctx context.Context, scope hubscope.NativeScope, issue tracker.NativeIssue, withFailures bool) (tracker.NativeRuntimeEvidence, []nativeFailure, bool, error) {
	ctx = s.withRequestDetails(s.runDetailContext(ctx))
	item := string(issue.WorkItemID)
	now := s.config.Clock().UTC()
	evidence := tracker.NativeRuntimeEvidence{Issue: issue.RuntimeReference(), Selection: "latest", ObservedAt: now,
		Unavailable: []string{"efficiency_receipt", "scheduler_history_projection", "runner_capacity_projection", "native_admission_projection"}, Capacity: []tracker.NativeRuntimeCapacity{},
		Scheduling: tracker.NativeSchedulerDecision{Source: "native_claim_eligibility", Outcome: "unknown", At: now, WorkItemRevision: issue.Revision, Reason: "Runner capacity and admission projection are not available from this typed read"}}
	selector := runtimeAttemptSelector{latest: true}
	if withFailures {
		selector.failures = &runtimeFailureRows{}
	}
	latest, sequence, found, err := s.readSelectedNativeRuntimeTenant(ctx, scope, item, selector)
	if err != nil {
		return evidence, nil, false, err
	}
	if found {
		evidence = latest
		evidence.Selection = "latest"
	} else {
		evidence.ChangeSequence, evidence.Issue.ChangeSequence = sequence, sequence
		evidence.Unavailable = append(evidence.Unavailable, "native_attempt")
		detail, err := s.readExplanationCurrentChange(ctx, scope, issue.WorkItemID, now)
		if err != nil {
			return evidence, nil, false, err
		}
		if detail != nil {
			attachRuntimeChangeDetail(&evidence, *detail)
		}
	}
	var failures []nativeFailure
	var partial bool
	if withFailures && found {
		failures, partial, err = s.operatorFailures(ctx, item, *selector.failures, now)
		if err != nil {
			return evidence, nil, false, err
		}
	}
	var decisions eventstore.SchedulerDecisionRows
	err = s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeBulkExportItem(ctx, r, scope, issue.WorkItemID); err != nil {
			return err
		}
		reader, ok := r.(eventstore.SchedulerDecisionReader)
		if !ok {
			return hubapi.NativeRunEventStorageUnavailable()
		}
		var err error
		decisions, err = reader.SchedulerDecisions(ctx, scope.Project, issue.WorkItemID)
		return err
	})
	if err == nil {
		evidence, err = s.hydrateSchedulerDecisionRows(ctx, scope, issue.WorkItemID, evidence, decisions)
	}
	return evidence, failures, partial, err
}

func (s *Service) operatorFailures(ctx context.Context, item string, history runtimeFailureRows, now time.Time) ([]nativeFailure, bool, error) {
	partial := len(history.rows) > runtimeFailureHistory
	rows := history.rows[:min(len(history.rows), runtimeFailureHistory)]
	failures := make([]nativeFailure, 0, len(rows))
	for _, row := range rows {
		failure := nativeFailure{AttemptID: row.AttemptID, WorkItem: item, At: row.UpdatedAt, Started: row.StartedAt, Finished: row.UpdatedAt, Example: row.Failure.Example, Class: row.Failure.Class}
		if row.Failure.LandingObservedAt != nil {
			failure.At = *row.Failure.LandingObservedAt
		}
		if !row.Recorded {
			snapshot, ok := history.legacy[row.AttemptID]
			if !ok {
				return nil, false, artifact.ErrIntegrity
			}
			attempt, err := s.hydrateNativeAttemptTenant(ctx, snapshot, now)
			if err != nil {
				return nil, false, err
			}
			failure.Example, failure.Class = failureFromAttempt(attempt.NativeRunData, snapshot.Attempt.Status)
			failure.AttemptID, failure.At, failure.Started, failure.Finished = attempt.AttemptID, attempt.UpdatedAt, attempt.StartedAt, attempt.UpdatedAt
			if attempt.Runtime != nil && attempt.Runtime.Landing != nil && !attempt.Runtime.Landing.ObservedAt.IsZero() {
				failure.At = attempt.Runtime.Landing.ObservedAt
			}
		}
		if failure.Example != "" {
			failures = append(failures, failure)
		}
	}
	return failures, partial, nil
}

func (s *Service) readOperatorAttemptSnapshots(ctx context.Context, scope hubscope.NativeScope, item string) ([]hubrecords.TenantAttemptSnapshot, int64, bool, error) {
	repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return nil, 0, false, hubapi.NativeRunEventStorageUnavailable()
	}
	var snapshots []hubrecords.TenantAttemptSnapshot
	var sequence int64
	var partial bool
	err := repository.ReadTenantSnapshot(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		project, err := r.Project(ctx, scope.Project)
		if err != nil {
			return err
		}
		if project.DeletedAt != nil {
			return corestore.ErrNotFound
		}
		work, err := r.WorkItem(ctx, scope.Project, tracker.NativeWorkItemID(item))
		if err != nil {
			return err
		}
		if !scope.ReporterOwns(tracker.Actor{Kind: hubapi.StringValue(work.ActorKind), PrincipalID: hubapi.StringValue(work.ActorPrincipalID)}) {
			return hubapi.NativeNotFound()
		}
		board, err := r.BoardSequence(ctx, scope.Project)
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
			if attempt.OrganizationID != scope.Organization || attempt.IssueID != work.ID {
				return corestore.ErrNotFound
			}
			lease, err := r.Lease(ctx, attempt.LeaseID)
			if err != nil {
				return err
			}
			if lease.OrganizationID != scope.Organization || lease.IssueID != work.ID || lease.FencingToken != attempt.FencingToken {
				return corestore.ErrNotFound
			}
			event, err := r.LatestRunEvent(ctx, scope.Project, attempt.ID)
			if err != nil {
				return err
			}
			if event.OrganizationID != scope.Organization || event.ProjectID != scope.Project || event.WorkItemID != work.NativeID || event.AttemptID != attempt.ID {
				return corestore.ErrNotFound
			}
			snapshot := hubrecords.TenantAttemptSnapshot{Attempt: attempt, Lease: lease, Event: event}
			if err := hubrecords.ReadTenantAttemptPayload(ctx, r, &snapshot); err != nil {
				return err
			}
			snapshots = append(snapshots, snapshot)
		}
		return nil
	})
	return snapshots, sequence, partial, err
}

func approvedProjectIssueContract(ctx context.Context, organization tracker.OrganizationID, project tracker.ProjectID, approval policy.Approval) (config.IssueContract, error) {
	if approval.Policy.Authored != nil && len(approval.Policy.Authored.Files) > 0 {
		workflow, err := hubrecords.ProjectPolicyWorkflow(ctx, string(organization)+"/"+string(project), approval.Policy)
		if err != nil {
			return config.IssueContract{}, err
		}
		return config.ResolveIssueContract(workflow.SharedPrompt)
	}
	return config.ResolvePolicyIssueContract(approval.Policy)
}

func (s *Service) projectReporterTemplates(ctx context.Context, scope hubscope.NativeScope, page *projectstore.Page) (config.ReporterContracts, error) {
	if page != nil {
		templates := config.DefaultReporterContracts()
		for kind, sections := range page.ReporterTemplates {
			if len(sections) > 0 {
				templates[kind] = sections
			}
		}
		return templates, nil
	}
	approval, err := s.readTenantProjectPolicy(ctx, scope.Organization, scope.Project)
	if err != nil {
		return nil, err
	}
	return approvedReporterTemplates(ctx, scope.Organization, scope.Project, approval)
}

func approvedReporterTemplates(ctx context.Context, organization tracker.OrganizationID, project tracker.ProjectID, approval policy.Approval) (config.ReporterContracts, error) {
	contract, err := approvedProjectIssueContract(ctx, organization, project, approval)
	if err != nil {
		return nil, err
	}
	return config.ResolveReporterContracts(contract.Context)
}

func (s *Service) readOperatorItemChangeSequence(ctx context.Context, scope hubscope.NativeScope, item *tracker.NativeIssue) error {
	return s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		head, err := r.BoardSequence(ctx, scope.Project)
		if err != nil {
			return err
		}
		item.ChangeSequence = head.Sequence
		item.OmittedFields = slices.DeleteFunc(slices.Clone(item.OmittedFields), func(field string) bool { return field == "change_sequence" })
		return nil
	})
}
