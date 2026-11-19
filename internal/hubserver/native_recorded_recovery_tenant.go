package hubserver

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubserver/claimapi"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/releaseaudits"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) prepareRecordedRecoveryTenant(ctx context.Context, scope hubscope.NativeScope, row corestore.WorkItem, issue tracker.NativeIssue, request tracker.Transition) (*leasestore.RecordedRecovery, error) {
	if request.BlockerAttemptID == "" || request.PolicyID == "" {
		return nil, tracker.ErrStaleFencingToken
	}
	recovery, err := s.planRecordedRecoveryTenant(ctx, scope, row, issue, request.BlockerAttemptID, true, nil)
	if err != nil {
		return nil, err
	}
	if request.LeaseID != tracker.LeaseID(recovery.Attempt.LeaseID) || request.FencingToken != tracker.FencingToken(recovery.Attempt.FencingToken) ||
		request.PolicyID != recovery.Policy.PolicyID && request.PolicyID != recovery.AttemptPolicyID {
		return nil, tracker.ErrStaleFencingToken
	}
	if request.State != recovery.State {
		return nil, corestore.ErrRevisionConflict
	}
	return recovery, nil
}

func (s *Service) planRecordedRecoveryTenant(ctx context.Context, scope hubscope.NativeScope, row corestore.WorkItem, issue tracker.NativeIssue, attemptID string, advance bool, audit *releaseaudits.AuditHoldClearance) (*leasestore.RecordedRecovery, error) {
	if issue.Profile != "native" || issue.Archived || !strings.EqualFold(issue.State, "Blocked") {
		return nil, corestore.ErrRevisionConflict
	}
	var lane eventstore.Event
	var project projectstore.Record
	var approved policystore.Revision
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		reader, ok := r.(eventstore.WorkflowTransitionReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		var err error
		lane, err = reader.LatestWorkflowTransition(ctx, scope.Project, issue.WorkItemID)
		if err != nil {
			return err
		}
		if lane.ActorKind != "runner" {
			return corestore.ErrRevisionConflict
		}
		if attemptID == "" {
			attempt, err := r.LatestAttempt(ctx, row.ID)
			if err != nil {
				return err
			}
			attemptID = attempt.ID
		}
		project, err = r.ProjectRecord(ctx, scope.Project)
		if err != nil {
			return err
		}
		approved, err = r.ProjectPolicy(ctx, scope.Project)
		return err
	})
	if err != nil {
		return nil, err
	}
	content := eventstore.NewContent(s.runDetailStoreFor(ctx))
	transition, err := content.Event(ctx, lane)
	if err != nil {
		return nil, err
	}
	if transition.Actor.Kind != "runner" || transition.Data.Operation == "operator_stop" {
		return nil, corestore.ErrRevisionConflict
	}
	snapshot, err := s.readNativeRecoveryAttemptSnapshot(ctx, scope, string(issue.WorkItemID), row.ID, attemptID)
	if err != nil {
		return nil, err
	}
	attempt, err := s.hydrateNativeAttemptTenant(ctx, snapshot, s.config.Clock())
	if err != nil {
		return nil, err
	}
	report, err := content.Event(ctx, snapshot.Event)
	if err != nil {
		return nil, err
	}
	prior, valid := recordedBlockedLane(issue, attempt, report, transition)
	if !valid || issueContractWorkerHumanAction(row, issue) != nil {
		return nil, corestore.ErrRevisionConflict
	}
	var clearedQuestion recordedAuditQuestion
	if audit == nil && typedWorkerHumanAction(transition.Data.ReasonDetail) {
		audit, err = s.changeAuditEvidence().ClearedAuditHold(ctx, scope, row.NativeID, nil)
		if err != nil {
			return nil, err
		}
	}
	if audit == nil && claimapi.RecordedHumanHold(attempt.Status, attempt.TerminalFailure, attempt.Disposition, attempt.CompletionBody, transition.Data.ReasonDetail) {
		return nil, corestore.ErrRevisionConflict
	}
	if audit != nil {
		if approved.PolicyID != audit.PolicyID || !recordedAuditHold(*audit, transition, attempt) {
			return nil, corestore.ErrRevisionConflict
		}
		clearedQuestion = auditQuestion(*audit, transition.Actor.PrincipalID)
	}
	var bound policystore.LeasePolicy
	if err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		bound, err = r.LeasePolicy(ctx, attempt.LeaseID)
		return err
	}); err != nil {
		return nil, err
	}
	if bound.ProjectID != scope.Project || bound.PolicyID != attempt.PolicyID {
		return nil, corestore.ErrRevisionConflict
	}
	machine := claimapi.RecordedMachineFailure(attempt.Status, attempt.TerminalFailure, attempt.Disposition)
	dependencies, ok := claimapi.RecordedRecoveryDependencies(attempt.Disposition, string(scope.Project))
	staleBase := attempt.Disposition != nil && attempt.Disposition.ReasonCode == "stale_base"
	if !ok || audit == nil && !machine && !staleBase && len(dependencies) == 0 {
		return nil, corestore.ErrRevisionConflict
	}
	question, err := s.recordedWorkpadQuestion(ctx, scope, row, snapshot.Attempt, advance, clearedQuestion)
	if err != nil {
		return nil, err
	}
	if question {
		return nil, corestore.ErrRevisionConflict
	}
	var source *tracker.NativeChangeReference
	if audit != nil {
		source = &audit.Source
	} else if staleBase {
		source, err = s.verifyRecordedRecoveryBase(ctx, scope, row, project, attempt.Checkpoint)
		if err != nil {
			return nil, err
		}
	}
	state := prior
	if !recordedRecoveryStateAllowed(project, issue.State, state) {
		if audit != nil {
			return nil, corestore.ErrRevisionConflict
		}
		state, err = recordedRetryLane(ctx, content, approved)
		if err != nil {
			return nil, err
		}
		if machine && !staleBase && len(dependencies) == 0 {
			state = recordedMachineRetryLane(project, issue.State, state)
		}
		if !recordedRecoveryStateAllowed(project, issue.State, state) {
			return nil, corestore.ErrRevisionConflict
		}
	}
	prepared := &leasestore.RecordedRecovery{
		ProjectID: scope.Project, WorkItemID: issue.WorkItemID, Attempt: snapshot.Attempt,
		ReportEventID: snapshot.Event.ID, ReportSequence: snapshot.Event.Sequence, ReportRevision: max(snapshot.Event.EventRevision, snapshot.Attempt.WorkItemRevision),
		WorkflowEventID: lane.ID, WorkflowSequence: lane.Sequence, WorkflowObjectKey: lane.PayloadObjectKey,
		DependencyNumbers: []int64{}, MergedDependencyNumbers: []int64{}, Policy: approved, AttemptPolicyID: attempt.PolicyID, State: state, ExpectedRevision: row.Revision, EventSequence: row.EventSequence, Source: source, Now: s.config.Clock().UTC(),
	}
	if audit != nil {
		prepared.AuditReviewEvidenceRevision = audit.ReviewEvidenceRevision
		prepared.AuditEvidenceSequence = audit.EvidenceSequence
	}
	for number, merged := range dependencies {
		prepared.DependencyNumbers = append(prepared.DependencyNumbers, number)
		if merged {
			prepared.MergedDependencyNumbers = append(prepared.MergedDependencyNumbers, number)
		}
	}
	return prepared, nil
}

func (s *Service) recordedWorkerMachineHold(ctx context.Context, scope hubscope.NativeScope, row corestore.WorkItem, request tracker.Transition) (*leasestore.Attempt, error) {
	var attempt leasestore.Attempt
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		attempt, err = r.LatestAttempt(ctx, row.ID)
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if attempt.Status == "running" || attempt.LeaseID != string(request.LeaseID) || attempt.FencingToken != int64(request.FencingToken) {
		return nil, nil
	}
	snapshot, err := s.readNativeRecoveryAttemptSnapshot(ctx, scope, string(row.NativeID), row.ID, attempt.ID)
	if err != nil {
		return nil, err
	}
	run, err := s.readNativeAttemptRunTenant(ctx, snapshot)
	if err != nil {
		return nil, err
	}
	if claimapi.RecordedHumanHold(attempt.Status, run.TerminalFailure, run.Disposition, run.CompletionBody, request.ReasonDetail) {
		return nil, nil
	}
	dependencies, valid := claimapi.RecordedRecoveryDependencies(run.Disposition, string(scope.Project))
	if !claimapi.RecordedMachineFailure(attempt.Status, run.TerminalFailure, run.Disposition) || !valid || len(dependencies) != 0 || run.Disposition != nil && run.Disposition.ReasonCode == "stale_base" {
		return nil, nil
	}
	question, err := s.recordedWorkpadQuestion(ctx, scope, row, snapshot.Attempt, true, recordedAuditQuestion{})
	if err != nil || question {
		return nil, err
	}
	return &snapshot.Attempt, nil
}

func recordedRetryLane(ctx context.Context, content *eventstore.Content, approved policystore.Revision) (string, error) {
	descriptor, err := content.ProjectPolicy(ctx, approved)
	if err != nil {
		return "", err
	}
	workflow, err := config.ApplyNativePolicy(config.Workflow{Config: config.Default()}, descriptor)
	if err != nil {
		return "", err
	}
	return workflow.Config.Tracker.BlockedRecovery.TargetState, nil
}

func recordedMachineRetryLane(project projectstore.Record, from, preferred string) string {
	if recordedRecoveryStateAllowed(project, from, preferred) {
		return preferred
	}
	for _, lane := range project.States {
		if recordedRecoveryStateAllowed(project, from, lane.Name) {
			return lane.Name
		}
	}
	return ""
}

func recordedRecoveryStateAllowed(project projectstore.Record, from, to string) bool {
	return hubrecords.ValidateNativeWorkflowTransition(project.States, from, to, hubapi.APIScopeWorker) == nil &&
		slices.ContainsFunc(project.States, func(lane tracker.NativeState) bool {
			return lane.Name == to && lane.Dispatchable && !lane.Terminal && !lane.OperatorOnly
		})
}

func (s *Service) verifyRecordedRecoveryBase(ctx context.Context, scope hubscope.NativeScope, row corestore.WorkItem, project projectstore.Record, checkpoint *tracker.NativeCheckpoint) (*tracker.NativeChangeReference, error) {
	if checkpoint == nil || !checkpoint.GitRef() || checkpoint.Change == nil || !hubapi.ValidCommitID(checkpoint.BaseSHA) {
		return nil, corestore.ErrRevisionConflict
	}
	repository := project.Settings.CheckoutRepository
	if !runnerstore.ValidCheckoutRepository(repository) {
		return nil, corestore.ErrRevisionConflict
	}
	source := checkpoint.Change
	var publication eventstore.NativePRPublication
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		change, err := r.LatestChange(ctx, scope.Project, row.NativeID)
		if err != nil {
			return err
		}
		if change.ID != source.ChangeID || hubapi.StringValue(change.CurrentVersionID) != source.VersionID {
			return corestore.ErrRevisionConflict
		}
		version, err := r.Version(ctx, change.ID, source.VersionID)
		if err != nil {
			return err
		}
		if version.HeadSHA != source.HeadSHA {
			return corestore.ErrRevisionConflict
		}
		publication, err = r.PRPublication(ctx, source.VersionID)
		return err
	})
	if err != nil {
		return nil, err
	}
	if publication.ChangeID != source.ChangeID || publication.HeadSHA != source.HeadSHA ||
		!strings.EqualFold(strings.TrimPrefix(publication.Repository, "https://github.com/"), repository) || publication.BaseRef == "" {
		return nil, corestore.ErrRevisionConflict
	}
	backend, ok := s.config.ReconcileBackend.(interface {
		CommitOnCurrentBase(context.Context, string, string, string) (bool, error)
	})
	if !ok {
		return nil, corestore.ErrRevisionConflict
	}
	contains, err := backend.CommitOnCurrentBase(ctx, repository, publication.BaseRef, checkpoint.BaseSHA)
	if err != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil || !contains {
		return nil, corestore.ErrRevisionConflict
	}
	return source, nil
}

func recordedBlockedLane(issue tracker.NativeIssue, attempt tracker.NativeAttempt, report, lane tracker.CollaborationEvent) (string, bool) {
	if report.AggregateID != issue.WorkItemID || report.ProjectID != issue.ProjectID || report.OrganizationID != issue.OrganizationID ||
		report.Type != "run.finished" || report.Actor.Kind != "runner" || report.Data.Run == nil ||
		report.Data.Run.AttemptID != attempt.AttemptID || report.Data.Run.FencingToken != attempt.FencingToken ||
		lane.AggregateID != issue.WorkItemID || lane.ProjectID != issue.ProjectID || lane.OrganizationID != issue.OrganizationID ||
		lane.Data.Revision != issue.Revision || lane.Data.ToState != issue.State || lane.Data.Reason != "worker_progress" ||
		lane.Data.BlockerAttemptID != "" && lane.Data.BlockerAttemptID != attempt.AttemptID ||
		lane.RecordedAt.Before(attempt.StartedAt) || attempt.Identity == nil && !claimapi.RecordedMachineFailure(attempt.Status, attempt.TerminalFailure, nil) || attempt.WorkItemRevision <= 0 ||
		max(report.Data.Revision, attempt.WorkItemRevision) > issue.Revision {
		return "", false
	}
	switch attempt.Status {
	case "succeeded", "failed", "interrupted", "cancelled":
		return lane.Data.FromState, lane.Data.FromState != ""
	default:
		return "", false
	}
}

func (s *Service) readRecordedRecoveryIntent(ctx context.Context, scope hubscope.NativeScope, row corestore.WorkItem, issue tracker.NativeIssue, advance bool) (*leasestore.RecordedRecovery, error) {
	recovery, err := s.planRecordedRecoveryTenant(ctx, scope, row, issue, "", advance, nil)
	if err == nil {
		err = s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			owner, ok := r.(leasestore.RecordedRecoveryWriter)
			if !ok {
				return hubapi.NativeWorkItemStorageUnavailable()
			}
			return owner.AuthorizeRecordedRecovery(ctx, *recovery)
		})
	}
	if recordedRecoveryIneligible(err) {
		return nil, nil
	}
	return recovery, err
}

func (s *Service) authorizeRecordedRecoveryTenant(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope, recovery leasestore.RecordedRecovery) error {
	if err := s.authorizeRecordedRecoveryRunner(ctx, w, scope); err != nil {
		return err
	}
	return s.authorizeRecordedRecoveryState(ctx, w, scope, recovery)
}

func (s *Service) authorizeRecordedRecoveryRunner(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope) error {
	now := s.config.Clock().UTC()
	credential, err := w.AccessCredential(ctx, scope.Credential.ID)
	if err != nil {
		return err
	}
	if credential.OrganizationID != scope.Organization || credential.Scope != string(hubapi.APIScopeWorker) || credential.Kind != "service" || !credential.Valid(scope.Credential.Hash, now) {
		return hubscope.RunnerUnauthorized()
	}
	identity, err := w.RunnerIdentityByCredential(ctx, credential.ID)
	if err != nil {
		return err
	}
	if !runnerIdentityMatchesHeldLease(identity, scope.Organization, scope.Credential.Runner.RunnerID, credential.ID, scope.Credential.Runner.MachineID, now) || identity.State != "active" || !slices.Contains(identity.Operations, runnerauth.Claim) || !slices.Contains(identity.Operations, runnerauth.Collaborate) {
		return tracker.ErrMachineNotFound
	}
	allowed, err := w.RunnerProjectAuthorized(ctx, identity.RunnerID, scope.Project, now)
	if err != nil {
		return err
	}
	if !allowed {
		return tracker.ErrMachineNotFound
	}
	return nil
}

func (s *Service) authorizeRecordedRecoveryState(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope, recovery leasestore.RecordedRecovery) error {
	if recovery.Policy.PolicyID != "" {
		current, err := w.ProjectPolicy(ctx, scope.Project)
		if err != nil {
			return err
		}
		if !sameProjectPolicySnapshot(current, recovery.Policy) {
			return corestore.ErrRevisionConflict
		}
		bound, err := w.LeasePolicy(ctx, tracker.LeaseID(recovery.Attempt.LeaseID))
		if err != nil {
			return err
		}
		if bound.ProjectID != scope.Project || bound.PolicyID != recovery.AttemptPolicyID {
			return tracker.ErrStaleFencingToken
		}
	}
	if recovery.Source != nil {
		change, err := w.LatestChange(ctx, scope.Project, recovery.WorkItemID)
		if err != nil {
			return err
		}
		if change.ID != recovery.Source.ChangeID || hubapi.StringValue(change.CurrentVersionID) != recovery.Source.VersionID {
			return corestore.ErrRevisionConflict
		}
		if recovery.AuditEvidenceSequence != 0 && (change.ReviewEvidenceRevision != recovery.AuditReviewEvidenceRevision || change.EvidenceSequence != recovery.AuditEvidenceSequence) {
			return corestore.ErrRevisionConflict
		}
		version, err := w.Version(ctx, change.ID, recovery.Source.VersionID)
		if err != nil {
			return err
		}
		if version.HeadSHA != recovery.Source.HeadSHA {
			return corestore.ErrRevisionConflict
		}
	}
	writer, ok := w.(leasestore.RecordedRecoveryWriter)
	if !ok {
		return hubapi.NativeWorkItemStorageUnavailable()
	}
	recovery.Now = s.config.Clock().UTC()
	return writer.AuthorizeRecordedRecovery(ctx, recovery)
}

func (s *Service) readClaimRecoveryIntent(ctx context.Context, scope hubscope.NativeScope, row corestore.WorkItem, version eventstore.CollaborationVersion) (*leasestore.RecordedRecovery, error) {
	issue, err := s.hydrateWorkItemRecord(ctx, row, version)
	if err != nil {
		return nil, err
	}
	return s.readRecordedRecoveryIntent(ctx, scope, row, issue, true)
}

func (s *Service) prepareNativeClaimRecoveryWrite(ctx context.Context, scope hubscope.NativeScope, recovery *leasestore.RecordedRecovery) (func(context.Context, tenantstore.Writer) error, error) {
	return s.prepareNativeRecordedRecoveryWrite(ctx, scope, recovery, nil)
}

func (s *Service) prepareNativeRecordedRecoveryWrite(ctx context.Context, scope hubscope.NativeScope, recovery *leasestore.RecordedRecovery, audit *releaseaudits.AuditHoldClearance) (func(context.Context, tenantstore.Writer) error, error) {
	command := nativeRecordedRecoveryWrite{recovery: recovery, audit: audit}
	request := tracker.Transition{
		Mutation:         tracker.Mutation{IdempotencyKey: "recorded-recovery/" + recovery.WorkflowEventID, LeaseID: tracker.LeaseID(recovery.Attempt.LeaseID), FencingToken: tracker.FencingToken(recovery.Attempt.FencingToken)},
		ExpectedRevision: tracker.Revision(recovery.ExpectedRevision), State: recovery.State, Reason: "dependency_ready", BlockerAttemptID: recovery.Attempt.ID, PolicyID: recovery.Policy.PolicyID,
	}
	if audit != nil {
		request.ReasonDetail = "Security audit findings cleared."
	}
	_, err := s.prepareOrTransitionNativeIssue(ctx, scope, string(recovery.WorkItemID), request, nil, "", &command)
	var conflict *hubapi.NativeError
	if errors.As(err, &conflict) && conflict.Code == "revision_conflict" {
		err = ErrNoClaimableWork
	}
	return command.write, err
}
