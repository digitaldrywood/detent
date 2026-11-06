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
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type changeWorkflowTransition struct {
	eligible     bool
	plan         workItemRecordPlan
	fields       workItemRecordFields
	version      eventstore.CollaborationVersion
	event        eventstore.Event
	search       searchstore.Document
	policy       policystore.Revision
	policyFound  bool
	contract     *eventstore.Receipt
	attempt      *leasestore.Attempt
	linked       *eventstore.LinkedSource
	request      tracker.Transition
	stopState    string
	dependencies bool
}

type changeReviewWorkflow struct {
	transition  *changeWorkflowTransition
	current     eventstore.Change
	audits      map[int64][]eventstore.AuditDisposition
	until       time.Time
	evaluatedAt time.Time
}

func (s *Service) prepareChangeReviewWorkflow(ctx context.Context, scope nativeScope, change tracker.ChangeRequest, version tracker.ChangeVersion, review tracker.ChangeReview, approval ChangeVersionApproval, now time.Time) (*changeReviewWorkflow, error) {
	if scope.credential.Scope == apiScopeWorker || review.VersionID != change.CurrentVersion || review.Decision == "commented" {
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
		return nil, workItemStorageUnavailable()
	}
	plan := &changeReviewWorkflow{audits: map[int64][]eventstore.AuditDisposition{}, evaluatedAt: now}
	var evidence []eventstore.Evidence
	var prior bool
	var checks []string
	for _, check := range version.Checks {
		checks = append(checks, check.CheckRunID)
	}
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		plan.current, err = r.Change(ctx, change.ID)
		if err != nil {
			return err
		}
		if plan.current.Revision != int64(change.Revision) || valueOrEmpty(plan.current.CurrentVersionID) != version.ID {
			return corestore.ErrRevisionConflict
		}
		reader, ok := r.(eventstore.ChangeReviewEvidenceReader)
		if !ok {
			return workItemStorageUnavailable()
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
		if row.OrganizationID != scope.organization || row.ChangeID != change.ID || valueOrEmpty(row.VersionID) != version.ID || row.Sequence > plan.current.EvidenceSequence {
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
	plan.transition, err = s.prepareChangeWorkflowTransition(ctx, scope, change, approval, false, now)
	if err != nil || plan.transition == nil {
		return nil, err
	}
	return plan, nil
}

func (s *Service) recheckChangeReviewWorkflow(ctx context.Context, w tenantstore.Writer, scope nativeScope, p *changeReviewWorkflow, current eventstore.Change) (bool, error) {
	if p == nil {
		return false, nil
	}
	if p.current.ID != "" && !reflect.DeepEqual(current, p.current) {
		return false, corestore.ErrRevisionConflict
	}
	if len(p.audits) > 0 {
		reader, ok := w.(eventstore.AuditDispositionReader)
		if !ok {
			return false, workItemStorageUnavailable()
		}
		for run, expected := range p.audits {
			rows, err := reader.AuditDispositions(ctx, scope.project, run)
			if err != nil {
				return false, err
			}
			if !reflect.DeepEqual(rows, expected) {
				return false, corestore.ErrRevisionConflict
			}
		}
	}
	now := s.config.now().UTC()
	return !now.Before(p.evaluatedAt) && (p.until.IsZero() || now.Before(p.until)), nil
}

func (s *Service) prepareChangeWorkflowTransition(ctx context.Context, scope nativeScope, change tracker.ChangeRequest, approval ChangeVersionApproval, rework bool, now time.Time) (*changeWorkflowTransition, error) {
	if scope.credential.Scope == apiScopeWorker {
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
	workflow, err := projectPolicyWorkflow(ctx, string(scope.organization)+"/"+string(scope.project), approval.Policy)
	if err != nil {
		return nil, err
	}
	source := strings.EqualFold(issue.State, workflow.Config.Agent.AutoPromote.SourceState)
	if approval.Policy.Configuration == nil && approval.Policy.Authored == nil && (strings.EqualFold(issue.State, "Review") || strings.EqualFold(issue.State, "In Review")) {
		source = true
	}
	p := &changeWorkflowTransition{plan: workItemRecordPlan{project: project, current: row}}
	landing := rework && strings.EqualFold(issue.State, landingLane)
	if issue.Archived || tenantProjectStateTerminal(project, issue.State) || !source && !landing || dispatchableProjectState(project, issue.State) && !landing {
		return p, nil
	}
	target := landingTarget(tracker.NativeProject{States: project.States}, issue.State)
	if rework {
		target = reworkTarget(tracker.NativeProject{States: project.States}, issue.State)
	}
	if target == "" || target == issue.State {
		return p, nil
	}
	if err := validateNativeWorkflowTransition(project.States, issue.State, target, scope.credential.Scope); err != nil {
		return nil, err
	}
	p = &changeWorkflowTransition{eligible: true, plan: workItemRecordPlan{project: project, current: row, eventType: "workflow.transitioned"}, fields: workItemRecordFields{Body: stringValue(row.BodyObjectKey), Contract: row.IssueContractObjectKey, Report: row.ReportContextObjectKey}, dependencies: !rework}
	content := eventstore.NewContent(s.runDetailStore())
	err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		attempt, err := r.LatestAttempt(ctx, row.ID)
		if err == nil {
			p.attempt = &attempt
		} else if !errors.Is(err, corestore.ErrNotFound) && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if issue.LinkedSource != nil {
			linked, err := r.LinkedSource(ctx, scope.project, row.NativeID)
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
	if p.linked != nil {
		issue.LinkedSource = &tracker.LinkedIssueSource{URL: p.linked.URL, Status: "pending"}
		if p.linked.SnapshotObjectKey != "" {
			linked, err := content.LinkedIssueSnapshot(ctx, scope.organization, eventstore.Reference(p.linked.SnapshotObjectKey, p.linked.SnapshotSHA256, p.linked.SnapshotBytes, p.linked.SnapshotContentBytes))
			if err != nil {
				return nil, err
			}
			if linked.URL != p.linked.URL {
				return nil, artifact.ErrIntegrity
			}
			issue.LinkedSource.Status, issue.LinkedSource.Snapshot = "complete", &linked
		}
		if issue.LinkedSource.Status != "complete" && dispatchableProjectState(project, target) {
			return nil, nativeWorkflowInvalid("Linked source intake must complete before executable work")
		}
		issue.ExternalReferences = slices.DeleteFunc(slices.Clone(issue.ExternalReferences), func(ref tracker.ExternalReference) bool {
			return ref.Provider == "github" && ref.Kind == "issue" && ref.URL == p.linked.URL
		})
	}
	previous := issue.State
	issue.State, issue.Revision, issue.UpdatedAt, issue.LastActivityAt, issue.Terminal = target, tracker.Revision(row.Revision+1), now, now, tenantProjectStateTerminal(project, target)
	if issue.Terminal {
		if issue.ClosedAt == nil {
			issue.ClosedAt = &now
		}
	} else {
		issue.ClosedAt = nil
	}
	p.request = tracker.Transition{State: target, ExpectedRevision: tracker.Revision(row.Revision), Reason: "user_requested"}
	p.policy, p.policyFound, err = s.validateTenantTransitionIssueContract(ctx, scope, &issue, &project, previous, p.request, &p.stopState)
	if err != nil {
		return nil, err
	}
	p.plan.issue = issue
	if issue.IssueContract != nil {
		raw, err := json.Marshal(issue.IssueContract)
		if err != nil {
			return nil, err
		}
		prepared, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.organization}, raw)
		if err != nil {
			return nil, err
		}
		p.contract = &prepared
		p.fields.Contract = &prepared.PayloadObjectKey
	}
	raw, err := json.Marshal(s.nativeIssueResponse(scope.reporterIssue(issue)))
	if err != nil {
		return nil, err
	}
	p.version, err = content.PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: row.NativeID, RecordID: string(row.NativeID), Revision: row.Revision + 1}, raw)
	if err != nil {
		return nil, err
	}
	raw, err = json.Marshal(tracker.CollaborationEvent{ID: payloadNativeID(ctx, "evt"), OrganizationID: scope.organization, ProjectID: scope.project, AggregateType: "issue", AggregateID: row.NativeID, Type: "workflow.transitioned", SchemaVersion: 1, Actor: scope.actor(), RecordedAt: now, Data: tracker.CollaborationData{Revision: issue.Revision, FromState: previous, ToState: target, Reason: "user_requested"}})
	if err != nil {
		return nil, err
	}
	p.event, err = content.PrepareEvent(ctx, scope.organization, raw)
	if err != nil {
		return nil, err
	}
	p.search, err = searchstore.NewContent(s.runDetailStore()).Prepare(ctx, searchstore.Document{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: row.NativeID, ID: "issue:" + string(row.NativeID), SourceID: string(row.NativeID), Kind: "issue", Scope: "issue", Revision: row.Revision + 1, UpdatedAt: now}, searchstore.Text{Title: issue.Title, Body: issue.Body, Labels: issue.Labels})
	return p, err
}

func (s *Service) lockChangeWorkflowTransition(ctx context.Context, w tenantstore.Writer, scope nativeScope, p *changeWorkflowTransition) (bool, error) {
	if p == nil {
		return false, nil
	}
	if err := s.authorizeWorkItemRecordScope(ctx, w, scope); err != nil {
		return false, err
	}
	if err := w.LockRelationshipGraph(ctx); err != nil {
		return false, err
	}
	refs := []corestore.WorkItemReference{{ProjectID: scope.project, WorkItemID: p.plan.current.NativeID}}
	if p.dependencies {
		cursor := corestore.WorkItemBlockerCursor{}
		complete := false
		for range claimPreparationLimit {
			page, err := w.WorkItemBlockers(ctx, scope.project, p.plan.current.NativeID, cursor, corestore.MaxPageSize)
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
		if ref.ProjectID == scope.project && !sameWorkItemProjectPlan(p.plan.project, project) {
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
			if row.ProjectID == scope.project && row.NativeID == p.plan.current.NativeID {
				if row.ID != p.plan.current.ID || row.Revision != p.plan.current.Revision || row.EventSequence != p.plan.current.EventSequence || row.Archived != p.plan.current.Archived {
					return false, corestore.ErrRevisionConflict
				}
			} else if !projects[row.ProjectID][stringValue(row.WorkflowStateName)] {
				ready = false
			}
		}
	}
	if !p.eligible {
		return false, nil
	}
	policy, err := w.ProjectPolicy(ctx, scope.project)
	if err != nil || !p.policyFound || !sameTenantProjectPolicyRevisionContent(p.policy, policy) {
		if err != nil {
			return false, err
		}
		return false, corestore.ErrRevisionConflict
	}
	linked, linkedErr := w.LinkedSource(ctx, scope.project, p.plan.current.NativeID)
	if linkedErr == nil && (p.linked == nil || !sameNativeClaimLinkedSource(linked, *p.linked)) || (errors.Is(linkedErr, corestore.ErrNotFound) || errors.Is(linkedErr, sql.ErrNoRows)) && p.linked != nil {
		return false, corestore.ErrRevisionConflict
	}
	if linkedErr != nil && !errors.Is(linkedErr, corestore.ErrNotFound) && !errors.Is(linkedErr, sql.ErrNoRows) {
		return false, linkedErr
	}
	attempt, attemptErr := w.LatestAttempt(ctx, p.plan.current.ID)
	if attemptErr == nil && (p.attempt == nil || !reflect.DeepEqual(attempt, *p.attempt)) || (errors.Is(attemptErr, corestore.ErrNotFound) || errors.Is(attemptErr, sql.ErrNoRows)) && p.attempt != nil {
		return false, corestore.ErrRevisionConflict
	}
	if attemptErr != nil && !errors.Is(attemptErr, corestore.ErrNotFound) && !errors.Is(attemptErr, sql.ErrNoRows) {
		return false, attemptErr
	}
	locker, ok := w.(leasestore.UnreleasedIssueLeaseLocker)
	if !ok {
		return false, workItemStorageUnavailable()
	}
	lease, err := locker.LockUnreleasedIssueLease(ctx, p.plan.current.ID)
	if err != nil && !errors.Is(err, corestore.ErrNotFound) && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if err == nil {
		if lease.ExpiresAt.After(s.config.now().UTC()) {
			return false, tracker.ErrLeaseConflict
		}
		if err := s.settleHumanWorkflowLease(ctx, w, scope, lease, p.request, nil, p.stopState); err != nil {
			return false, err
		}
	}
	return ready, nil
}

func (s *Service) applyChangeWorkflowTransition(ctx context.Context, w tenantstore.Writer, scope nativeScope, p *changeWorkflowTransition) error {
	if p.contract != nil {
		ref := eventstore.Reference(p.contract.PayloadObjectKey, p.contract.PayloadSHA256, p.contract.PayloadBytes, p.contract.PayloadContentBytes)
		id := "att_" + artifact.Digest([]byte(string(scope.organization) + "\n" + ref.Key))[:32]
		object, err := w.RegisterArchiveObject(ctx, artifactstore.ArchiveObject{OrganizationID: scope.organization, ID: id, Reference: ref, CreatedAt: p.plan.issue.UpdatedAt})
		if err != nil {
			return err
		}
		if err := w.AddArchiveOwner(ctx, artifactstore.ArchiveOwner{OrganizationID: scope.organization, ArchiveID: object.ID, Kind: "work-item-contract", ID: string(p.plan.current.NativeID) + ":" + strconv.FormatInt(int64(p.plan.issue.Revision), 10), CreatedAt: p.plan.issue.UpdatedAt}); err != nil {
			return err
		}
	}
	if err := writeWorkItemRecord(ctx, w, p.plan, p.fields); err != nil {
		return err
	}
	if dispatchableProjectState(p.plan.project, p.plan.issue.State) {
		if s.config.ConversationDispatch == nil {
			return conversationDispatchUnavailable()
		}
		if err := s.config.ConversationDispatch.RequestDispatch(ctx, w, scope.project, string(p.plan.current.NativeID), leasestore.Fence{}, s.config.now().UTC()); err != nil {
			return err
		}
	}
	if err := w.PutSearch(ctx, p.search, p.plan.current.Revision); err != nil {
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
	_, err := w.AppendEvent(ctx, p.event)
	return err
}
