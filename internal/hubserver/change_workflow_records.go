package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubcontent"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type changeWorkflowTransition struct {
	eligible     bool
	plan         hubrecords.WorkItemRecordPlan
	fields       hubrecords.WorkItemRecordFields
	version      eventstore.CollaborationVersion
	event        eventstore.Event
	search       searchstore.Document
	policy       policystore.Revision
	policyFound  bool
	contract     *eventstore.Receipt
	attempt      *leasestore.Attempt
	cancellation *preparedNativeLeaseCancellation
	linked       *eventstore.LinkedSource
	request      tracker.Transition
	stopState    string
	dependencies bool
	outbound     *projectstore.GitHubOutbound
}

type changeReviewWorkflow struct {
	transition  *changeWorkflowTransition
	current     eventstore.Change
	audits      map[int64][]eventstore.AuditDisposition
	until       time.Time
	evaluatedAt time.Time
}

func (s *Service) prepareChangeReviewWorkflow(ctx context.Context, scope hubscope.NativeScope, change tracker.ChangeRequest, version tracker.ChangeVersion, review tracker.ChangeReview, approval hubrecords.ChangeVersionApproval, now time.Time) (*changeReviewWorkflow, error) {
	if scope.Credential.Scope == hubapi.APIScopeWorker || review.VersionID != change.CurrentVersion || review.Decision == "commented" {
		return nil, nil
	}
	if review.Decision == "changes_requested" {
		transition, err := s.prepareChangeWorkflowTransition(ctx, scope, change, approval, true, now)
		if err != nil || transition == nil {
			return nil, err
		}
		return &changeReviewWorkflow{transition: transition, evaluatedAt: now}, nil
	}
	if approval.Policy.Gates.SecurityAudit {
		return nil, hubapi.NativeWorkItemStorageUnavailable()
	}
	plan := &changeReviewWorkflow{audits: map[int64][]eventstore.AuditDisposition{}, evaluatedAt: now}
	var evidence []eventstore.Evidence
	var prior bool
	var checks []string
	for _, check := range version.Checks {
		checks = append(checks, check.CheckRunID)
	}
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		plan.current, err = r.Change(ctx, change.ID)
		if err != nil {
			return err
		}
		if plan.current.Revision != int64(change.Revision) || hubapi.ValueOrEmpty(plan.current.CurrentVersionID) != version.ID {
			return corestore.ErrRevisionConflict
		}
		reader, ok := r.(eventstore.ChangeReviewEvidenceReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		evidence, prior, err = reader.ChangeReviewEvidence(ctx, change.ID, version.ID, plan.current.EvidenceSequence, checks, review.Actor.PrincipalID)
		return err
	})
	if err != nil {
		return nil, err
	}
	detail := tracker.ChangeDetail{Change: change, Versions: []tracker.ChangeVersion{version}}
	content := eventstore.NewContent(s.runDetailStore())
	for _, row := range evidence {
		if row.OrganizationID != scope.Organization || row.ChangeID != change.ID || hubapi.ValueOrEmpty(row.VersionID) != version.ID || row.Sequence > plan.current.EvidenceSequence {
			return nil, eventstore.ErrInvalidRecord
		}
		raw, err := content.Evidence(ctx, row)
		if err != nil {
			return nil, err
		}
		if err := appendBoundedChangeEvidence(&detail, row, raw); err != nil {
			return nil, err
		}
	}
	detail.Reviews = append(detail.Reviews, review)
	return s.prepareChangeEvidenceWorkflow(ctx, scope, detail, approval, plan, prior, now)
}

func (s *Service) prepareChangeCheckWorkflow(ctx context.Context, scope hubscope.NativeScope, change tracker.ChangeRequest, version tracker.ChangeVersion, check tracker.ChangeCheck, approval hubrecords.ChangeVersionApproval, now time.Time) (*changeReviewWorkflow, error) {
	if scope.Credential.Scope == hubapi.APIScopeWorker || version.ID != change.CurrentVersion {
		return nil, nil
	}
	plan := &changeReviewWorkflow{audits: map[int64][]eventstore.AuditDisposition{}, evaluatedAt: now}
	var evidence []eventstore.Evidence
	var prior bool
	checks := make([]string, 0, len(version.Checks))
	for _, expected := range version.Checks {
		checks = append(checks, expected.CheckRunID)
	}
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		plan.current, err = r.Change(ctx, change.ID)
		if err != nil {
			return err
		}
		if plan.current.Revision != int64(change.Revision) || hubapi.ValueOrEmpty(plan.current.CurrentVersionID) != version.ID {
			return corestore.ErrRevisionConflict
		}
		reader, ok := r.(eventstore.ChangeRecoveryReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		evidence, prior, err = reader.ChangeSummaryEvidence(ctx, change.ID, version.ID, plan.current.EvidenceSequence, checks)
		return err
	})
	if err != nil {
		return nil, err
	}
	detail := tracker.ChangeDetail{Change: change, Versions: []tracker.ChangeVersion{version}}
	content := eventstore.NewContent(s.runDetailStore())
	for _, row := range evidence {
		if row.OrganizationID != scope.Organization || row.ChangeID != change.ID || hubapi.ValueOrEmpty(row.VersionID) != version.ID || row.Sequence > plan.current.EvidenceSequence {
			return nil, eventstore.ErrInvalidRecord
		}
		raw, err := content.Evidence(ctx, row)
		if err != nil {
			return nil, err
		}
		if err := appendBoundedChangeEvidence(&detail, row, raw); err != nil {
			return nil, err
		}
	}
	detail.Checks = slices.DeleteFunc(detail.Checks, func(old tracker.ChangeCheck) bool { return old.CheckRunID == check.CheckRunID })
	detail.Checks = append(detail.Checks, check)
	return s.prepareChangeEvidenceWorkflow(ctx, scope, detail, approval, plan, prior, now)
}

func (s *Service) prepareChangeEvidenceWorkflow(ctx context.Context, scope hubscope.NativeScope, detail tracker.ChangeDetail, approval hubrecords.ChangeVersionApproval, plan *changeReviewWorkflow, prior bool, now time.Time) (*changeReviewWorkflow, error) {
	change, version := detail.Change, detail.Versions[0]
	dispositions := make(map[int64][]securityaudit.Disposition)
	for _, current := range detail.Reviews {
		if current.SecurityAudit == nil {
			continue
		}
		run := current.SecurityAudit.Run
		if _, found := dispositions[run.ID]; found {
			continue
		}
		values, rows, err := s.prepareValidatorAuditDispositions(ctx, scope, change.ID, run)
		if err != nil {
			return nil, err
		}
		dispositions[run.ID], plan.audits[run.ID] = values, rows
	}
	policyID, reviewID := changerequest.CurrentPolicyIdentities(&version, approval.Policy, approval.ReviewPolicy)
	if changerequest.SummarizeCurrentVersionWithAuditDispositions(detail, policyID, reviewID, now, prior, dispositions).Status != "reviewed" {
		return plan, nil
	}
	for _, expected := range version.Checks {
		for _, check := range detail.Checks {
			if check.CheckRunID != expected.CheckRunID {
				continue
			}
			expires := check.CompletedAt.Add(time.Duration(expected.MaxAgeSeconds) * time.Second)
			if plan.until.IsZero() || expires.Before(plan.until) {
				plan.until = expires
			}
		}
	}
	var err error
	plan.transition, err = s.prepareChangeWorkflowTransition(ctx, scope, change, approval, false, now)
	if err != nil || plan.transition == nil {
		return nil, err
	}
	return plan, nil
}

func (s *Service) recheckChangeReviewWorkflow(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope, p *changeReviewWorkflow, current eventstore.Change) (bool, error) {
	if p == nil {
		return false, nil
	}
	expected := p.current
	expected.ReviewSummarySequence = current.ReviewSummarySequence
	if p.current.ID != "" && !reflect.DeepEqual(current, expected) {
		return false, corestore.ErrRevisionConflict
	}
	if len(p.audits) > 0 {
		reader, ok := w.(eventstore.AuditDispositionReader)
		if !ok {
			return false, hubapi.NativeWorkItemStorageUnavailable()
		}
		for run, expected := range p.audits {
			rows, err := reader.AuditDispositions(ctx, scope.Project, run)
			if err != nil {
				return false, err
			}
			if !reflect.DeepEqual(rows, expected) {
				return false, corestore.ErrRevisionConflict
			}
		}
	}
	now := s.config.Clock().UTC()
	return !now.Before(p.evaluatedAt) && (p.until.IsZero() || now.Before(p.until)), nil
}

type changeWorkflowRoute struct {
	rework bool
	reason string
	target func(tracker.NativeProject, string) string
}

func (s *Service) prepareChangeWorkflowTransition(ctx context.Context, scope hubscope.NativeScope, change tracker.ChangeRequest, approval hubrecords.ChangeVersionApproval, rework bool, now time.Time) (*changeWorkflowTransition, error) {
	return s.prepareChangeWorkflowRoute(ctx, scope, change, approval, changeWorkflowRoute{rework: rework, reason: "user_requested"}, now)
}

func (s *Service) prepareChangeWorkflowRoute(ctx context.Context, scope hubscope.NativeScope, change tracker.ChangeRequest, approval hubrecords.ChangeVersionApproval, route changeWorkflowRoute, now time.Time) (*changeWorkflowTransition, error) {
	rework := route.rework
	if scope.Credential.Scope == hubapi.APIScopeWorker {
		return nil, nil
	}
	row, snapshot, err := s.readWorkItemRecord(ctx, scope, string(change.WorkItemID))
	if err != nil {
		return nil, err
	}
	issue, err := s.hydrateWorkItemRecord(ctx, row, snapshot)
	if err != nil {
		return nil, err
	}
	project, err := s.readTenantProjectRecord(ctx, scope)
	if err != nil {
		return nil, err
	}
	if err := validateConfiguredWorkflowPolicy(project, approval.Policy, true); err != nil {
		return nil, err
	}
	workflow, err := hubrecords.ProjectPolicyWorkflow(ctx, string(scope.Organization)+"/"+string(scope.Project), approval.Policy)
	if err != nil {
		return nil, err
	}
	source := strings.EqualFold(issue.State, workflow.Config.Agent.AutoPromote.SourceState)
	if approval.Policy.Configuration == nil && approval.Policy.Authored == nil && (strings.EqualFold(issue.State, "Review") || strings.EqualFold(issue.State, "In Review")) {
		source = true
	}
	p := &changeWorkflowTransition{plan: hubrecords.WorkItemRecordPlan{Project: project, Current: row}}
	landing := rework && strings.EqualFold(issue.State, landingLane)
	if issue.Archived || hubrecords.TenantProjectStateTerminal(project, issue.State) || !source && !landing || hubrecords.DispatchableProjectState(project, issue.State) && !landing {
		return p, nil
	}
	target := landingTarget(tracker.NativeProject{States: project.States}, issue.State)
	if rework {
		target = reworkTarget(tracker.NativeProject{States: project.States}, issue.State)
	}
	if route.target != nil {
		target = route.target(tracker.NativeProject{States: project.States}, issue.State)
	}
	if target == "" || target == issue.State {
		return p, nil
	}
	if err := hubrecords.ValidateNativeWorkflowTransition(project.States, issue.State, target, scope.Credential.Scope); err != nil {
		return nil, err
	}
	p = &changeWorkflowTransition{eligible: true, plan: hubrecords.WorkItemRecordPlan{Project: project, Current: row, EventType: "workflow.transitioned"}, fields: hubrecords.WorkItemRecordFields{Body: hubapi.StringValue(row.BodyObjectKey), Contract: row.IssueContractObjectKey, Report: row.ReportContextObjectKey}, dependencies: !rework}
	sourceIssue := issue
	content := eventstore.NewContent(s.runDetailStore())
	err = s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		attempt, err := r.LatestAttempt(ctx, row.ID)
		if err == nil {
			p.attempt = &attempt
		} else if !errors.Is(err, corestore.ErrNotFound) && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if issue.LinkedSource != nil {
			linked, err := r.LinkedSource(ctx, scope.Project, row.NativeID)
			if err != nil {
				return err
			}
			p.linked = &linked
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if p.attempt != nil {
		p.cancellation, err = s.prepareExpiredNativeLeaseCancellation(ctx, scope, row.ID, false)
		if err != nil {
			return nil, err
		}
	}
	if p.linked != nil {
		issue.LinkedSource = &tracker.LinkedIssueSource{URL: p.linked.URL, Status: "pending"}
		if p.linked.SnapshotObjectKey != "" {
			linked, err := content.LinkedIssueSnapshot(ctx, scope.Organization, eventstore.Reference(p.linked.SnapshotObjectKey, p.linked.SnapshotSHA256, p.linked.SnapshotBytes, p.linked.SnapshotContentBytes))
			if err != nil {
				return nil, err
			}
			if linked.URL != p.linked.URL {
				return nil, artifact.ErrIntegrity
			}
			issue.LinkedSource.Status, issue.LinkedSource.Snapshot = "complete", &linked
		}
		if issue.LinkedSource.Status != "complete" && hubrecords.DispatchableProjectState(project, target) {
			return nil, hubapi.NativeWorkflowInvalid("Linked source intake must complete before executable work")
		}
		issue.ExternalReferences = slices.DeleteFunc(slices.Clone(issue.ExternalReferences), func(ref tracker.ExternalReference) bool {
			return ref.Provider == "github" && ref.Kind == "issue" && ref.URL == p.linked.URL
		})
	}
	previous := issue.State
	issue.State, issue.Revision, issue.UpdatedAt, issue.LastActivityAt, issue.Terminal = target, tracker.Revision(row.Revision+1), now, now, hubrecords.TenantProjectStateTerminal(project, target)
	if issue.Terminal {
		if issue.ClosedAt == nil {
			issue.ClosedAt = &now
		}
	} else {
		issue.ClosedAt = nil
	}
	p.request = tracker.Transition{State: target, ExpectedRevision: tracker.Revision(row.Revision), Reason: "user_requested"}
	var workflowLabel string
	p.policy, p.policyFound, err = s.validateTenantTransitionIssueContract(ctx, scope, &issue, &project, previous, p.request, &p.stopState, &workflowLabel)
	if err != nil {
		return nil, err
	}
	p.request.WorkflowLabel = workflowLabel
	if issue.Profile == "github_compatible" && workflowLabel != "" {
		repository, _, sourceErr := cutoverSourceReference(sourceIssue)
		if sourceErr == nil && project.Settings.RepositoryEnabled && strings.EqualFold(repository, strings.TrimSpace(project.Settings.Repository)) {
			key, err := hubapi.CommandHash(struct {
				Organization, Project, Change, Item, State, Policy string
				Revision                                           int64
			}{string(scope.Organization), string(scope.Project), change.ID, string(issue.WorkItemID), target, p.policy.PolicyID, int64(issue.Revision)})
			if err != nil {
				return nil, err
			}
			p.outbound, err = s.prepareCompatibilityWorkflowLabel(ctx, scope, sourceIssue, project, workflowLabel, "change-workflow:"+key, now)
			if err != nil {
				return nil, err
			}
		}
	}
	p.plan.Issue = issue
	if issue.IssueContract != nil {
		raw, err := json.Marshal(issue.IssueContract)
		if err != nil {
			return nil, err
		}
		prepared, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.Organization}, raw)
		if err != nil {
			return nil, err
		}
		p.contract = &prepared
		p.fields.Contract = &prepared.PayloadObjectKey
	}
	raw, err := json.Marshal(s.nativeIssueResponse(scope.ReporterIssue(issue)))
	if err != nil {
		return nil, err
	}
	p.version, err = content.PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: row.NativeID, RecordID: string(row.NativeID), Revision: row.Revision + 1}, raw)
	if err != nil {
		return nil, err
	}
	raw, err = json.Marshal(tracker.CollaborationEvent{ID: hubcontent.PayloadNativeID(ctx, "evt"), OrganizationID: scope.Organization, ProjectID: scope.Project, AggregateType: "issue", AggregateID: row.NativeID, Type: "workflow.transitioned", SchemaVersion: 1, Actor: scope.Actor(), RecordedAt: now, Data: tracker.CollaborationData{Revision: issue.Revision, FromState: previous, ToState: target, Reason: route.reason}})
	if err != nil {
		return nil, err
	}
	p.event, err = content.PrepareEvent(ctx, scope.Organization, raw)
	if err != nil {
		return nil, err
	}
	p.search, err = searchstore.NewContent(s.runDetailStore()).Prepare(ctx, searchstore.Document{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: row.NativeID, ID: "issue:" + string(row.NativeID), SourceID: string(row.NativeID), Kind: "issue", Scope: "issue", Revision: row.Revision + 1, UpdatedAt: now}, searchstore.Text{Title: issue.Title, Body: issue.Body, Labels: issue.Labels})
	return p, err
}

func (s *Service) lockChangeWorkflowTransition(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope, p *changeWorkflowTransition) (bool, error) {
	if p == nil {
		return false, nil
	}
	if err := s.authorizeWorkItemRecordScope(ctx, w, scope); err != nil {
		return false, err
	}
	if err := w.LockRelationshipGraph(ctx); err != nil {
		return false, err
	}
	refs := []corestore.WorkItemReference{{ProjectID: scope.Project, WorkItemID: p.plan.Current.NativeID}}
	if p.dependencies {
		cursor := corestore.WorkItemBlockerCursor{}
		complete := false
		for range claimPreparationLimit {
			page, err := w.WorkItemBlockers(ctx, scope.Project, p.plan.Current.NativeID, cursor, corestore.MaxPageSize)
			if err != nil {
				return false, err
			}
			for _, row := range page[:min(corestore.MaxPageSize, len(page))] {
				refs = append(refs, row.WorkItemReference)
			}
			if len(page) <= corestore.MaxPageSize {
				complete = true
				break
			}
			last := page[corestore.MaxPageSize-1]
			cursor = corestore.WorkItemBlockerCursor{ProjectID: last.ProjectID, IssueID: last.IssueID}
		}
		if !complete {
			return false, corestore.ErrInvalidPage
		}
	}
	slices.SortFunc(refs, func(a, b corestore.WorkItemReference) int {
		if a.ProjectID != b.ProjectID {
			return strings.Compare(string(a.ProjectID), string(b.ProjectID))
		}
		return strings.Compare(string(a.WorkItemID), string(b.WorkItemID))
	})
	refs = slices.Compact(refs)
	projects := make(map[tracker.ProjectID]map[string]bool)
	for _, ref := range refs {
		if _, ok := projects[ref.ProjectID]; ok {
			continue
		}
		if err := w.LockWorkItemProject(ctx, ref.ProjectID); err != nil {
			return false, err
		}
		project, err := w.ProjectRecord(ctx, ref.ProjectID)
		if err != nil {
			return false, err
		}
		if project.Core.DeletedAt != nil {
			return false, corestore.ErrNotFound
		}
		if ref.ProjectID == scope.Project && !hubrecords.SameWorkItemProjectPlan(p.plan.Project, project) {
			return false, corestore.ErrRevisionConflict
		}
		states := make(map[string]bool, len(project.States))
		for _, state := range project.States {
			states[state.Name] = state.Terminal
		}
		projects[ref.ProjectID] = states
	}
	ready := true
	for start := 0; start < len(refs); start += corestore.MaxPageSize {
		rows, err := w.LockWorkItems(ctx, refs[start:min(start+corestore.MaxPageSize, len(refs))])
		if err != nil {
			return false, err
		}
		for _, row := range rows {
			if row.ProjectID == scope.Project && row.NativeID == p.plan.Current.NativeID {
				if row.ID != p.plan.Current.ID || row.Revision != p.plan.Current.Revision || row.EventSequence != p.plan.Current.EventSequence || row.Archived != p.plan.Current.Archived {
					return false, corestore.ErrRevisionConflict
				}
			} else if !projects[row.ProjectID][hubapi.StringValue(row.WorkflowStateName)] {
				ready = false
			}
		}
	}
	if !p.eligible {
		return false, nil
	}
	policy, err := w.ProjectPolicy(ctx, scope.Project)
	if err != nil || !p.policyFound || !sameTenantProjectPolicyRevisionContent(p.policy, policy) {
		if err != nil {
			return false, err
		}
		return false, corestore.ErrRevisionConflict
	}
	linked, linkedErr := w.LinkedSource(ctx, scope.Project, p.plan.Current.NativeID)
	if linkedErr == nil && (p.linked == nil || !sameNativeClaimLinkedSource(linked, *p.linked)) || (errors.Is(linkedErr, corestore.ErrNotFound) || errors.Is(linkedErr, sql.ErrNoRows)) && p.linked != nil {
		return false, corestore.ErrRevisionConflict
	}
	if linkedErr != nil && !errors.Is(linkedErr, corestore.ErrNotFound) && !errors.Is(linkedErr, sql.ErrNoRows) {
		return false, linkedErr
	}
	attempt, attemptErr := w.LatestAttempt(ctx, p.plan.Current.ID)
	if attemptErr == nil && (p.attempt == nil || !reflect.DeepEqual(attempt, *p.attempt)) || (errors.Is(attemptErr, corestore.ErrNotFound) || errors.Is(attemptErr, sql.ErrNoRows)) && p.attempt != nil {
		return false, corestore.ErrRevisionConflict
	}
	if attemptErr != nil && !errors.Is(attemptErr, corestore.ErrNotFound) && !errors.Is(attemptErr, sql.ErrNoRows) {
		return false, attemptErr
	}
	locker, ok := w.(leasestore.UnreleasedIssueLeaseLocker)
	if !ok {
		return false, hubapi.NativeWorkItemStorageUnavailable()
	}
	lease, err := locker.LockUnreleasedIssueLease(ctx, p.plan.Current.ID)
	if err != nil && !errors.Is(err, corestore.ErrNotFound) && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if err == nil {
		if !p.cancellation.expiresLease(lease) && lease.ExpiresAt.After(s.config.Clock().UTC()) {
			return false, tracker.ErrLeaseConflict
		}
		if err := s.settleHumanWorkflowLease(ctx, w, scope, lease, p.request, p.cancellation, p.stopState); err != nil {
			return false, err
		}
	}
	return ready, nil
}

func (s *Service) applyChangeWorkflowTransition(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope, p *changeWorkflowTransition) error {
	if p.contract != nil {
		ref := eventstore.Reference(p.contract.PayloadObjectKey, p.contract.PayloadSHA256, p.contract.PayloadBytes, p.contract.PayloadContentBytes)
		id := "att_" + artifact.Digest([]byte(string(scope.Organization) + "\n" + ref.Key))[:32]
		object, err := w.RegisterArchiveObject(ctx, artifactstore.ArchiveObject{OrganizationID: scope.Organization, ID: id, Reference: ref, CreatedAt: p.plan.Issue.UpdatedAt})
		if err != nil {
			return err
		}
		if err := w.AddArchiveOwner(ctx, artifactstore.ArchiveOwner{OrganizationID: scope.Organization, ArchiveID: object.ID, Kind: "work-item-contract", ID: string(p.plan.Current.NativeID) + ":" + strconv.FormatInt(int64(p.plan.Issue.Revision), 10), CreatedAt: p.plan.Issue.UpdatedAt}); err != nil {
			return err
		}
	}
	if err := hubrecords.WriteWorkItemRecord(ctx, w, p.plan, p.fields); err != nil {
		return err
	}
	if hubrecords.DispatchableProjectState(p.plan.Project, p.plan.Issue.State) {
		if s.config.ConversationDispatch == nil {
			return conversationDispatchUnavailable()
		}
		if err := s.config.ConversationDispatch.RequestDispatch(ctx, w, scope.Project, string(p.plan.Current.NativeID), leasestore.Fence{}, s.config.Clock().UTC()); err != nil {
			return err
		}
	}
	if err := w.PutSearch(ctx, p.search, p.plan.Current.Revision); err != nil {
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if err := w.PutSearch(ctx, p.search, 0); err != nil {
			return err
		}
	}
	if _, err := w.CreateCollaborationVersion(ctx, p.version); err != nil {
		return err
	}
	if _, err := w.AppendEvent(ctx, p.event); err != nil {
		return err
	}
	if p.outbound != nil {
		owner, ok := w.(projectstore.GitHubOutboxWriter)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		return owner.EnqueueGitHubOutbound(ctx, *p.outbound)
	}
	return nil
}
