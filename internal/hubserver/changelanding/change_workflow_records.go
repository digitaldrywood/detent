package changelanding

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
	"github.com/digitaldrywood/detent/internal/hubserver/changerules"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubcontent"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type WorkflowTransition struct {
	Eligible     bool
	Plan         hubrecords.WorkItemRecordPlan
	fields       hubrecords.WorkItemRecordFields
	Version      eventstore.CollaborationVersion
	Event        eventstore.Event
	policy       policystore.Revision
	policyFound  bool
	contract     *eventstore.Receipt
	attempt      *leasestore.Attempt
	cancellation LeaseCancellation
	linked       *eventstore.LinkedSource
	Request      tracker.Transition
	stopState    string
	dependencies bool
	Outbound     *projectstore.GitHubOutbound
}

type ReviewWorkflow struct {
	Transition  *WorkflowTransition
	current     eventstore.Change
	audits      map[int64][]eventstore.AuditDisposition
	until       time.Time
	evaluatedAt time.Time
}

func (s *Workflow) PrepareChangeReviewWorkflow(ctx context.Context, scope hubscope.NativeScope, change tracker.ChangeRequest, version tracker.ChangeVersion, review tracker.ChangeReview, approval hubrecords.ChangeVersionApproval, now time.Time) (*ReviewWorkflow, error) {
	if scope.Credential.Scope == hubapi.APIScopeWorker || review.VersionID != change.CurrentVersion || review.Decision == "commented" {
		return nil, nil
	}
	if review.Decision == "changes_requested" {
		transition, err := s.PrepareChangeWorkflowTransition(ctx, scope, change, approval, true, now)
		if err != nil || transition == nil {
			return nil, err
		}
		return &ReviewWorkflow{Transition: transition, evaluatedAt: now}, nil
	}
	if approval.Policy.Gates.SecurityAudit {
		return nil, hubapi.NativeWorkItemStorageUnavailable()
	}
	plan := &ReviewWorkflow{audits: map[int64][]eventstore.AuditDisposition{}, evaluatedAt: now}
	var evidence []eventstore.Evidence
	var prior bool
	var checks []string
	for _, check := range version.Checks {
		checks = append(checks, check.CheckRunID)
	}
	err := s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
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
	content := eventstore.NewContent(s.deps.RunDetailStore())
	for _, row := range evidence {
		if row.OrganizationID != scope.Organization || row.ChangeID != change.ID || hubapi.ValueOrEmpty(row.VersionID) != version.ID || row.Sequence > plan.current.EvidenceSequence {
			return nil, eventstore.ErrInvalidRecord
		}
		raw, err := content.Evidence(ctx, row)
		if err != nil {
			return nil, err
		}
		if err := s.deps.AppendBoundedChangeEvidence(&detail, row, raw); err != nil {
			return nil, err
		}
	}
	detail.Reviews = append(detail.Reviews, review)
	return s.prepareChangeEvidenceWorkflow(ctx, scope, detail, approval, plan, prior, now)
}

func (s *Workflow) PrepareChangeCheckWorkflow(ctx context.Context, scope hubscope.NativeScope, change tracker.ChangeRequest, version tracker.ChangeVersion, check tracker.ChangeCheck, approval hubrecords.ChangeVersionApproval, now time.Time) (*ReviewWorkflow, error) {
	if scope.Credential.Scope == hubapi.APIScopeWorker || version.ID != change.CurrentVersion {
		return nil, nil
	}
	plan := &ReviewWorkflow{audits: map[int64][]eventstore.AuditDisposition{}, evaluatedAt: now}
	var evidence []eventstore.Evidence
	var prior bool
	checks := make([]string, 0, len(version.Checks))
	for _, expected := range version.Checks {
		checks = append(checks, expected.CheckRunID)
	}
	err := s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
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
	content := eventstore.NewContent(s.deps.RunDetailStore())
	for _, row := range evidence {
		if row.OrganizationID != scope.Organization || row.ChangeID != change.ID || hubapi.ValueOrEmpty(row.VersionID) != version.ID || row.Sequence > plan.current.EvidenceSequence {
			return nil, eventstore.ErrInvalidRecord
		}
		raw, err := content.Evidence(ctx, row)
		if err != nil {
			return nil, err
		}
		if err := s.deps.AppendBoundedChangeEvidence(&detail, row, raw); err != nil {
			return nil, err
		}
	}
	detail.Checks = slices.DeleteFunc(detail.Checks, func(old tracker.ChangeCheck) bool { return old.CheckRunID == check.CheckRunID })
	detail.Checks = append(detail.Checks, check)
	return s.prepareChangeEvidenceWorkflow(ctx, scope, detail, approval, plan, prior, now)
}

func (s *Workflow) prepareChangeEvidenceWorkflow(ctx context.Context, scope hubscope.NativeScope, detail tracker.ChangeDetail, approval hubrecords.ChangeVersionApproval, plan *ReviewWorkflow, prior bool, now time.Time) (*ReviewWorkflow, error) {
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
		values, rows, err := s.deps.PrepareValidatorAuditDispositions(ctx, scope, change.ID, run)
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
	plan.Transition, err = s.PrepareChangeWorkflowTransition(ctx, scope, change, approval, false, now)
	if err != nil || plan.Transition == nil {
		return nil, err
	}
	return plan, nil
}

func (s *Workflow) RecheckChangeReviewWorkflow(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope, p *ReviewWorkflow, current eventstore.Change) (bool, error) {
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
	now := s.deps.Clock().UTC()
	return !now.Before(p.evaluatedAt) && (p.until.IsZero() || now.Before(p.until)), nil
}

type workflowRoute struct {
	rework bool
	reason string
	target func(tracker.NativeProject, string) string
}

func (s *Workflow) PrepareChangeWorkflowTransition(ctx context.Context, scope hubscope.NativeScope, change tracker.ChangeRequest, approval hubrecords.ChangeVersionApproval, rework bool, now time.Time) (*WorkflowTransition, error) {
	return s.prepareChangeWorkflowRoute(ctx, scope, change, approval, workflowRoute{rework: rework, reason: "user_requested"}, now)
}

func (s *Workflow) prepareChangeWorkflowRoute(ctx context.Context, scope hubscope.NativeScope, change tracker.ChangeRequest, approval hubrecords.ChangeVersionApproval, route workflowRoute, now time.Time) (*WorkflowTransition, error) {
	rework := route.rework
	if scope.Credential.Scope == hubapi.APIScopeWorker {
		return nil, nil
	}
	row, snapshot, err := s.deps.ReadWorkItemRecord(ctx, scope, string(change.WorkItemID))
	if err != nil {
		return nil, err
	}
	issue, err := s.deps.HydrateWorkItemRecord(ctx, row, snapshot)
	if err != nil {
		return nil, err
	}
	project, err := s.deps.ReadProjectRecord(ctx, scope)
	if err != nil {
		return nil, err
	}
	if err := s.deps.ValidateConfiguredWorkflowPolicy(project, approval.Policy, true); err != nil {
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
	p := &WorkflowTransition{Plan: hubrecords.WorkItemRecordPlan{Project: project, Current: row}}
	landing := rework && strings.EqualFold(issue.State, changerules.LandingLane)
	if issue.Archived || hubrecords.TenantProjectStateTerminal(project, issue.State) || !source && !landing || hubrecords.DispatchableProjectState(project, issue.State) && !landing {
		return p, nil
	}
	target := changerules.LandingTarget(tracker.NativeProject{States: project.States}, issue.State)
	if rework {
		target = changerules.ReworkTarget(tracker.NativeProject{States: project.States}, issue.State)
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
	p = &WorkflowTransition{Eligible: true, Plan: hubrecords.WorkItemRecordPlan{Project: project, Current: row, EventType: "workflow.transitioned"}, fields: hubrecords.WorkItemRecordFields{Body: hubapi.StringValue(row.BodyObjectKey), Contract: row.IssueContractObjectKey, Report: row.ReportContextObjectKey}, dependencies: !rework}
	sourceIssue := issue
	content := eventstore.NewContent(s.deps.RunDetailStore())
	err = s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
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
		p.cancellation, err = s.deps.PrepareLeaseCancellation(ctx, scope, row.ID)
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
	p.Request = tracker.Transition{State: target, ExpectedRevision: tracker.Revision(row.Revision), Reason: "user_requested"}
	var workflowLabel string
	p.policy, p.policyFound, err = s.deps.ValidateTenantTransitionIssueContract(ctx, scope, &issue, &project, previous, p.Request, &p.stopState, &workflowLabel)
	if err != nil {
		return nil, err
	}
	p.Request.WorkflowLabel = workflowLabel
	if issue.Profile == "github_compatible" && workflowLabel != "" {
		repository, _, sourceErr := s.deps.CutoverSourceReference(sourceIssue)
		if sourceErr == nil && project.Settings.RepositoryEnabled && strings.EqualFold(repository, strings.TrimSpace(project.Settings.Repository)) {
			key, err := hubapi.CommandHash(struct {
				Organization, Project, Change, Item, State, Policy string
				Revision                                           int64
			}{string(scope.Organization), string(scope.Project), change.ID, string(issue.WorkItemID), target, p.policy.PolicyID, int64(issue.Revision)})
			if err != nil {
				return nil, err
			}
			p.Outbound, err = s.deps.PrepareCompatibilityWorkflowLabel(ctx, scope, sourceIssue, project, workflowLabel, "change-workflow:"+key, now)
			if err != nil {
				return nil, err
			}
		}
	}
	p.Plan.Issue = issue
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
	raw, err := json.Marshal(s.deps.NativeIssueResponse(scope.ReporterIssue(issue)))
	if err != nil {
		return nil, err
	}
	p.Version, err = content.PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: row.NativeID, RecordID: string(row.NativeID), Revision: row.Revision + 1}, raw)
	if err != nil {
		return nil, err
	}
	raw, err = json.Marshal(tracker.CollaborationEvent{ID: hubcontent.PayloadNativeID(ctx, "evt"), OrganizationID: scope.Organization, ProjectID: scope.Project, AggregateType: "issue", AggregateID: row.NativeID, Type: "workflow.transitioned", SchemaVersion: 1, Actor: scope.Actor(), RecordedAt: now, Data: tracker.CollaborationData{Revision: issue.Revision, FromState: previous, ToState: target, Reason: route.reason}})
	if err != nil {
		return nil, err
	}
	p.Event, err = content.PrepareEvent(ctx, scope.Organization, raw)
	if err != nil {
		return nil, err
	}
	return p, err
}

func (s *Workflow) LockChangeWorkflowTransition(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope, p *WorkflowTransition) (bool, error) {
	if p == nil {
		return false, nil
	}
	if err := s.deps.AuthorizeWorkItemRecordScope(ctx, w, scope); err != nil {
		return false, err
	}
	if err := w.LockRelationshipGraph(ctx); err != nil {
		return false, err
	}
	refs := []corestore.WorkItemReference{{ProjectID: scope.Project, WorkItemID: p.Plan.Current.NativeID}}
	if p.dependencies {
		cursor := corestore.WorkItemBlockerCursor{}
		complete := false
		for range corestore.MaxRelationshipPreparationPages {
			page, err := w.WorkItemBlockers(ctx, scope.Project, p.Plan.Current.NativeID, cursor, corestore.MaxPageSize)
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
		if ref.ProjectID == scope.Project && !hubrecords.SameWorkItemProjectPlan(p.Plan.Project, project) {
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
			if row.ProjectID == scope.Project && row.NativeID == p.Plan.Current.NativeID {
				if row.ID != p.Plan.Current.ID || row.Revision != p.Plan.Current.Revision || row.EventSequence != p.Plan.Current.EventSequence || row.Archived != p.Plan.Current.Archived {
					return false, corestore.ErrRevisionConflict
				}
			} else if !projects[row.ProjectID][hubapi.StringValue(row.WorkflowStateName)] {
				ready = false
			}
		}
	}
	if !p.Eligible {
		return false, nil
	}
	policy, err := w.ProjectPolicy(ctx, scope.Project)
	if err != nil || !p.policyFound || !s.deps.SameTenantProjectPolicyRevisionContent(p.policy, policy) {
		if err != nil {
			return false, err
		}
		return false, corestore.ErrRevisionConflict
	}
	linked, linkedErr := w.LinkedSource(ctx, scope.Project, p.Plan.Current.NativeID)
	if linkedErr == nil && (p.linked == nil || !s.deps.SameNativeClaimLinkedSource(linked, *p.linked)) || (errors.Is(linkedErr, corestore.ErrNotFound) || errors.Is(linkedErr, sql.ErrNoRows)) && p.linked != nil {
		return false, corestore.ErrRevisionConflict
	}
	if linkedErr != nil && !errors.Is(linkedErr, corestore.ErrNotFound) && !errors.Is(linkedErr, sql.ErrNoRows) {
		return false, linkedErr
	}
	attempt, attemptErr := w.LatestAttempt(ctx, p.Plan.Current.ID)
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
	lease, err := locker.LockUnreleasedIssueLease(ctx, p.Plan.Current.ID)
	if err != nil && !errors.Is(err, corestore.ErrNotFound) && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if err == nil {
		if err := s.deps.SettleLease(ctx, w, scope, lease, p.Request, p.cancellation, p.stopState, s.deps.Clock); err != nil {
			return false, err
		}
	}
	return ready, nil
}

func (s *Workflow) ApplyChangeWorkflowTransition(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope, p *WorkflowTransition) error {
	if p.contract != nil {
		ref := eventstore.Reference(p.contract.PayloadObjectKey, p.contract.PayloadSHA256, p.contract.PayloadBytes, p.contract.PayloadContentBytes)
		id := "att_" + artifact.Digest([]byte(string(scope.Organization) + "\n" + ref.Key))[:32]
		object, err := w.RegisterArchiveObject(ctx, artifactstore.ArchiveObject{OrganizationID: scope.Organization, ID: id, Reference: ref, CreatedAt: p.Plan.Issue.UpdatedAt})
		if err != nil {
			return err
		}
		if err := w.AddArchiveOwner(ctx, artifactstore.ArchiveOwner{OrganizationID: scope.Organization, ArchiveID: object.ID, Kind: "work-item-contract", ID: string(p.Plan.Current.NativeID) + ":" + strconv.FormatInt(int64(p.Plan.Issue.Revision), 10), CreatedAt: p.Plan.Issue.UpdatedAt}); err != nil {
			return err
		}
	}
	if err := hubrecords.WriteWorkItemRecord(ctx, w, p.Plan, p.fields); err != nil {
		return err
	}
	if hubrecords.DispatchableProjectState(p.Plan.Project, p.Plan.Issue.State) {
		if s.deps.ConversationDispatch == nil {
			return s.deps.ConversationDispatchUnavailable()
		}
		if err := s.deps.ConversationDispatch.RequestDispatch(ctx, w, scope.Project, string(p.Plan.Current.NativeID), leasestore.Fence{}, s.deps.Clock().UTC()); err != nil {
			return err
		}
	}
	if _, err := w.CreateCollaborationVersion(ctx, p.Version); err != nil {
		return err
	}
	if _, err := w.AppendEvent(ctx, p.Event); err != nil {
		return err
	}
	if p.Outbound != nil {
		owner, ok := w.(projectstore.GitHubOutboxWriter)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		return owner.EnqueueGitHubOutbound(ctx, *p.Outbound)
	}
	return nil
}
