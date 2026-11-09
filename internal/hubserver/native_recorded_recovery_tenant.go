package hubserver

import (
	"context"
	"slices"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func (s *Service) prepareRecordedRecoveryTenant(ctx context.Context, scope nativeScope, row corestore.WorkItem, issue tracker.NativeIssue, request tracker.Transition) (*leasestore.RecordedRecovery, error) {
	if request.BlockerAttemptID == "" || request.PolicyID == "" {
		return nil, tracker.ErrStaleFencingToken
	}
	snapshot, err := s.readNativeRecoveryAttemptSnapshot(ctx, scope, string(issue.WorkItemID), row.ID, request.BlockerAttemptID)
	if err != nil {
		return nil, err
	}
	attempt, err := s.hydrateNativeAttemptTenant(ctx, snapshot, s.config.now())
	if err != nil {
		return nil, err
	}
	var lane eventstore.Event
	err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		reader, ok := r.(eventstore.WorkflowTransitionReader)
		if !ok {
			return workItemStorageUnavailable()
		}
		bound, err := r.LeasePolicy(ctx, attempt.LeaseID)
		if err != nil {
			return err
		}
		current, err := r.ProjectPolicy(ctx, scope.project)
		if err != nil {
			return err
		}
		if bound.ProjectID != scope.project || bound.PolicyID != attempt.PolicyID || request.PolicyID != bound.PolicyID && request.PolicyID != current.PolicyID {
			return tracker.ErrStaleFencingToken
		}
		lane, err = reader.LatestWorkflowTransition(ctx, scope.project, issue.WorkItemID)
		return err
	})
	if err != nil {
		return nil, err
	}
	content := eventstore.NewContent(s.runDetailStore())
	report, err := content.Event(ctx, snapshot.event)
	if err != nil {
		return nil, err
	}
	transition, err := content.Event(ctx, lane)
	if err != nil {
		return nil, err
	}
	_, prior, valid := tracker.RecordedNativeBlockers(issue, []tracker.NativeAttempt{attempt}, []tracker.CollaborationEvent{report, transition})
	if !valid || prior != request.State || request.LeaseID != attempt.LeaseID || request.FencingToken != attempt.FencingToken {
		return nil, tracker.ErrStaleFencingToken
	}
	disposition := attempt.Disposition
	if disposition.HumanAction || len(disposition.BlockerEvidence) == 0 {
		return nil, corestore.ErrRevisionConflict
	}
	instance := disposition.ReasonCode == "" || disposition.ReasonCode == "instance_limitation"
	for _, blocker := range disposition.BlockerEvidence {
		instance = instance && blocker.Owner == workpad.BlockerOwnerInstance
	}
	refs := workpad.IssueDependencyRefs(&workpad.Signal{Status: disposition.Status, ReasonCode: disposition.ReasonCode, Blockers: disposition.BlockerEvidence}, string(scope.project))
	if !instance && len(refs) == 0 || len(refs) > corestore.MaxPageSize {
		return nil, corestore.ErrRevisionConflict
	}
	prepared := &leasestore.RecordedRecovery{ProjectID: scope.project, WorkItemID: issue.WorkItemID, Attempt: snapshot.attempt, ReportEventID: snapshot.event.ID, ReportSequence: snapshot.event.Sequence, ReportRevision: max(snapshot.event.EventRevision, snapshot.attempt.WorkItemRevision), WorkflowEventID: lane.ID, WorkflowSequence: lane.Sequence, WorkflowObjectKey: lane.PayloadObjectKey, DependencyNumbers: []int64{}}
	for _, ref := range refs {
		number, err := strconv.ParseInt(strings.TrimPrefix(ref, string(scope.project)+"#"), 10, 64)
		if err != nil || number <= 0 {
			return nil, corestore.ErrRevisionConflict
		}
		prepared.DependencyNumbers = append(prepared.DependencyNumbers, number)
	}
	return prepared, nil
}

func (s *Service) authorizeRecordedRecoveryTenant(ctx context.Context, w tenantstore.Writer, scope nativeScope, recovery leasestore.RecordedRecovery) error {
	now := s.config.now().UTC()
	credential, err := w.AccessCredential(ctx, scope.credential.ID)
	if err != nil {
		return err
	}
	if credential.OrganizationID != scope.organization || credential.Scope != string(apiScopeWorker) || credential.Kind != "service" || !credential.Valid(scope.credential.Hash, now) {
		return runnerUnauthorized()
	}
	identity, err := w.RunnerIdentityByCredential(ctx, credential.ID)
	if err != nil {
		return err
	}
	if !runnerIdentityMatchesHeldLease(identity, scope.organization, scope.credential.Runner.RunnerID, credential.ID, scope.credential.Runner.MachineID, now) || identity.State != "active" || !slices.Contains(identity.Operations, runnerauth.Claim) || !slices.Contains(identity.Operations, runnerauth.Collaborate) {
		return tracker.ErrMachineNotFound
	}
	allowed, err := w.RunnerProjectAuthorized(ctx, identity.RunnerID, scope.project, now)
	if err != nil {
		return err
	}
	if !allowed {
		return tracker.ErrMachineNotFound
	}
	writer, ok := w.(leasestore.RecordedRecoveryWriter)
	if !ok {
		return workItemStorageUnavailable()
	}
	recovery.Now = now
	return writer.AuthorizeRecordedRecovery(ctx, recovery)
}
