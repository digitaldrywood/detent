package changeapi

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/changerules"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/releaseaudits"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnersession"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type preparedValidatorReview struct {
	attempt              leasestore.Attempt
	event                eventstore.Event
	session              runnersession.Record
	change               eventstore.Change
	issue                corestore.WorkItem
	evidence             changerules.ValidatorReviewEvidence
	auditDispositionRows []eventstore.AuditDisposition
}

func (s *validator) prepareValidatorReviewRecord(ctx context.Context, scope hubscope.NativeScope, change tracker.ChangeRequest, version tracker.ChangeVersion, source eventstore.Version, request tracker.ReviewChange) (*preparedValidatorReview, tracker.MachineID, error) {
	if scope.Credential.Scope != hubapi.APIScopeWorker || request.Validator == nil && request.ImplementationReview == nil && request.SecurityAudit == nil {
		machine, err := changerules.ValidateValidatorReviewEvidence(scope, change, version, request, changerules.ValidatorReviewEvidence{}, s.deps.NativeRejection)
		return nil, machine, err
	}
	resultID := int64(0)
	for _, result := range []*gate.ValidatorResult{request.Validator, request.ImplementationReview} {
		if result != nil {
			resultID = result.SessionID
		}
	}
	if request.SecurityAudit != nil {
		resultID = request.SecurityAudit.SessionID
	}
	if source.ImplementingSessionID < 0 || (source.SourceObjectKey == "" && version.GitSource == nil) || source.ImplementingMachineID == "" {
		return nil, "", hubapi.NativePublicInvalid("worker review requires recorded source and review sessions")
	}
	p := &preparedValidatorReview{}
	var rows []eventstore.Evidence
	var itemVersion eventstore.CollaborationVersion
	err := s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		attempt, approved, err := s.deps.RunnerSessionFence(ctx, r, scope, runnersession.Mutation{LeaseID: request.LeaseID, FencingToken: request.FencingToken}, string(change.WorkItemID), false)
		if err != nil {
			return fmt.Errorf("read review authority: %w", err)
		}
		p.attempt = attempt
		if resultID <= 0 {
			if _, err := changerules.ValidateValidatorReviewEvidence(scope, change, version, request, changerules.ValidatorReviewEvidence{}, s.deps.NativeRejection); err != nil {
				return err
			}
			return hubapi.NativePublicInvalid("worker review requires a recorded review session")
		}
		p.change, err = r.Change(ctx, change.ID)
		if err != nil {
			return fmt.Errorf("read review change: %w", err)
		}
		p.issue, err = r.WorkItem(ctx, scope.Project, change.WorkItemID)
		if err != nil {
			return fmt.Errorf("read review issue: %w", err)
		}
		itemVersion, err = r.CollaborationVersion(ctx, string(change.WorkItemID), p.issue.Revision)
		if err != nil {
			return fmt.Errorf("read review issue version: %w", err)
		}
		p.event, err = r.LatestRunEvent(ctx, scope.Project, p.attempt.ID)
		if err != nil {
			return fmt.Errorf("read review event: %w", err)
		}
		reader, ok := r.(runnersession.Reader)
		if !ok {
			return hubapi.NativeRunEventStorageUnavailable()
		}
		p.session, err = reader.RunnerSession(ctx, scope.Project, resultID)
		if err != nil {
			return fmt.Errorf("read review session: %w", err)
		}
		if p.session.AttemptID != p.attempt.ID && p.session.ID == source.ImplementingSessionID && source.ImplementingMachineID == string(scope.Credential.Runner.MachineID) {
			if request.SecurityAudit != nil {
				return s.deps.SecurityAuditRejection("security audit must come from a session separate from the implementing session under its active execution authority", "execution_session")
			}
			refusal := hubapi.NativeInvalid("validator review must come from a session separate from the implementing session under its active execution authority")
			if request.Validator != nil && request.ImplementationReview == nil {
				return s.deps.NativeRejection(refusal, "acceptance_validation", "validator.session_id")
			}
			return refusal
		}
		if p.session.AttemptID != p.attempt.ID || p.session.IssueID != string(change.WorkItemID) || p.session.PolicyID != approved.PolicyID || p.session.CompletedAt == nil || p.session.FinalState != "completed" {
			return tracker.ErrStaleFencingToken
		}
		if request.SecurityAudit != nil && p.session.Role != "security_audit" {
			return s.deps.SecurityAuditRejection("security audit requires a recorded audit session", "execution_session")
		}
		reviews, ok := r.(eventstore.ValidatorReviewReader)
		if !ok {
			return hubapi.NativeRunEventStorageUnavailable()
		}
		var complete bool
		rows, complete, err = reviews.ValidatorReviewEvidence(ctx, change.ID, version.ID, p.change.EvidenceSequence, scope.Credential.Runner.MachineID, resultID)
		if err != nil {
			return fmt.Errorf("read review evidence: %w", err)
		}
		if !complete {
			return hubapi.NativeRunEventStorageUnavailable()
		}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	session, err := s.deps.ReadRunnerSessionPayload(ctx, p.session)
	if err != nil {
		return nil, "", err
	}
	if err := s.deps.VerifyRunnerSessionFinalization(ctx, scope, p.session, session); err != nil {
		if errors.Is(err, runnersession.ErrFinalizationIncomplete) {
			return nil, "", hubapi.NativePublicInvalid("session finalization is incomplete")
		}
		return nil, "", err
	}
	p.evidence.Issue, err = s.deps.HydrateWorkItemRecord(ctx, p.issue, itemVersion)
	if err != nil {
		return nil, "", err
	}
	content := eventstore.NewContent(s.deps.RunDetailStoreFor(ctx))
	event, err := content.Event(ctx, p.event)
	if err != nil {
		return nil, "", err
	}
	if event.Data.Run == nil || event.Data.Run.AttemptID != p.attempt.ID || event.Data.Run.Sequence != p.attempt.Sequence || event.Data.Run.MachineID != scope.Credential.Runner.MachineID {
		return nil, "", tracker.ErrStaleFencingToken
	}
	p.evidence.Current = *event.Data.Run
	p.evidence.SourceSession, p.evidence.SourceMachine = source.ImplementingSessionID, source.ImplementingMachineID
	for _, row := range rows {
		if row.OrganizationID != scope.Organization || row.ChangeID != change.ID || row.Kind != "review" || row.VersionID == nil || *row.VersionID != version.ID || row.Sequence > p.change.EvidenceSequence {
			return nil, "", eventstore.ErrInvalidRecord
		}
		raw, err := content.Evidence(ctx, row)
		if err != nil {
			return nil, "", err
		}
		review, err := eventstore.DecodeChangeReview(row, raw)
		if err != nil {
			return nil, "", err
		}
		p.evidence.Reviews = append(p.evidence.Reviews, review)
	}
	if request.SecurityAudit != nil {
		p.evidence.AuditDispositions, p.auditDispositionRows, err = s.deps.PrepareValidatorAuditDispositions(ctx, scope, change.ID, request.SecurityAudit.Run)
		if err != nil {
			return nil, "", err
		}
	}
	machine, err := changerules.ValidateValidatorReviewEvidence(scope, change, version, request, p.evidence, s.deps.NativeRejection)
	return p, machine, err
}

func (s *validator) lockValidatorReviewRecord(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope, change tracker.ChangeRequest, request tracker.ReviewChange, p *preparedValidatorReview, held runnerstore.Identity, current eventstore.Change) error {
	if p == nil {
		return nil
	}
	attempt, item, _, err := s.deps.RunnerSessionFenceWithOwner(ctx, w, scope, runnersession.Mutation{LeaseID: request.LeaseID, FencingToken: request.FencingToken}, string(change.WorkItemID), held)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(attempt, p.attempt) {
		return corestore.ErrRevisionConflict
	}
	if current.Revision != p.change.Revision || current.EvidenceSequence != p.change.EvidenceSequence || !reflect.DeepEqual(current.CurrentVersionID, p.change.CurrentVersionID) {
		return corestore.ErrRevisionConflict
	}
	if item.Revision != p.issue.Revision {
		return corestore.ErrRevisionConflict
	}
	reader, ok := w.(runnersession.Reader)
	if !ok {
		return hubapi.NativeRunEventStorageUnavailable()
	}
	session, err := reader.RunnerSession(ctx, scope.Project, p.session.ID)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(session, p.session) {
		return corestore.ErrRevisionConflict
	}
	if request.SecurityAudit != nil {
		return releaseaudits.LockValidatorAuditDispositions(ctx, w, scope, request.SecurityAudit.Run.ID, p.auditDispositionRows)
	}
	return nil
}
