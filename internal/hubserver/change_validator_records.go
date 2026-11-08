package hubserver

import (
	"context"
	"fmt"
	"reflect"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnersession"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type preparedValidatorReview struct {
	attempt  leasestore.Attempt
	event    eventstore.Event
	session  runnersession.Record
	change   eventstore.Change
	issue    corestore.WorkItem
	evidence validatorReviewEvidence
}

func (s *Service) prepareValidatorReviewRecord(ctx context.Context, scope nativeScope, change tracker.ChangeRequest, version tracker.ChangeVersion, source eventstore.Version, request tracker.ReviewChange) (*preparedValidatorReview, tracker.MachineID, error) {
	if scope.credential.Scope != apiScopeWorker {
		machine, err := validateValidatorReviewEvidence(scope, change, version, request, validatorReviewEvidence{})
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
	if resultID <= 0 || source.ImplementingSessionID <= 0 || source.SourceObjectKey == "" {
		return nil, "", nativePublicInvalid("worker review requires recorded source and review sessions")
	}
	p := &preparedValidatorReview{}
	var rows []eventstore.Evidence
	var itemVersion eventstore.CollaborationVersion
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		attempt, approved, err := s.runnerSessionFence(ctx, r, scope, runnersession.Mutation{LeaseID: request.LeaseID, FencingToken: request.FencingToken}, string(change.WorkItemID), false)
		if err != nil {
			return fmt.Errorf("read review authority: %w", err)
		}
		p.attempt = attempt
		p.change, err = r.Change(ctx, change.ID)
		if err != nil {
			return fmt.Errorf("read review change: %w", err)
		}
		p.issue, err = r.WorkItem(ctx, scope.project, change.WorkItemID)
		if err != nil {
			return fmt.Errorf("read review issue: %w", err)
		}
		itemVersion, err = r.CollaborationVersion(ctx, string(change.WorkItemID), p.issue.Revision)
		if err != nil {
			return fmt.Errorf("read review issue version: %w", err)
		}
		p.event, err = r.LatestRunEvent(ctx, scope.project, p.attempt.ID)
		if err != nil {
			return fmt.Errorf("read review event: %w", err)
		}
		reader, ok := r.(runnersession.Reader)
		if !ok {
			return nativeRunEventStorageUnavailable()
		}
		p.session, err = reader.RunnerSession(ctx, scope.project, resultID)
		if err != nil {
			return fmt.Errorf("read review session: %w", err)
		}
		if p.session.AttemptID != p.attempt.ID || p.session.IssueID != string(change.WorkItemID) || p.session.PolicyID != approved.PolicyID || p.session.CompletedAt == nil || p.session.FinalState != "completed" {
			return tracker.ErrStaleFencingToken
		}
		reviews, ok := r.(eventstore.ValidatorReviewReader)
		if !ok {
			return nativeRunEventStorageUnavailable()
		}
		var complete bool
		rows, complete, err = reviews.ValidatorReviewEvidence(ctx, change.ID, version.ID, p.change.EvidenceSequence, scope.credential.Runner.MachineID, resultID)
		if err != nil {
			return fmt.Errorf("read review evidence: %w", err)
		}
		if !complete {
			return nativeRunEventStorageUnavailable()
		}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	p.evidence.issue, err = s.hydrateWorkItemRecord(ctx, p.issue, itemVersion)
	if err != nil {
		return nil, "", err
	}
	content := eventstore.NewContent(s.runDetailStoreFor(ctx))
	event, err := content.Event(ctx, p.event)
	if err != nil {
		return nil, "", err
	}
	if event.Data.Run == nil || event.Data.Run.AttemptID != p.attempt.ID || event.Data.Run.Sequence != p.attempt.Sequence || event.Data.Run.MachineID != scope.credential.Runner.MachineID {
		return nil, "", tracker.ErrStaleFencingToken
	}
	p.evidence.current = *event.Data.Run
	p.evidence.sourceSession, p.evidence.sourceMachine = source.ImplementingSessionID, source.ImplementingMachineID
	for _, row := range rows {
		if row.OrganizationID != scope.organization || row.ChangeID != change.ID || row.Kind != "review" || row.VersionID == nil || *row.VersionID != version.ID || row.Sequence > p.change.EvidenceSequence {
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
		p.evidence.reviews = append(p.evidence.reviews, review)
	}
	if request.SecurityAudit != nil {
		p.evidence.auditDispositions, p.evidence.auditDispositionRows, err = s.prepareValidatorAuditDispositions(ctx, scope, change.ID, request.SecurityAudit.Run)
		if err != nil {
			return nil, "", err
		}
	}
	machine, err := validateValidatorReviewEvidence(scope, change, version, request, p.evidence)
	return p, machine, err
}

func (s *Service) lockValidatorReviewRecord(ctx context.Context, w tenantstore.Writer, scope nativeScope, change tracker.ChangeRequest, request tracker.ReviewChange, p *preparedValidatorReview) error {
	if p == nil {
		return nil
	}
	attempt, _, err := s.runnerSessionFence(ctx, w, scope, runnersession.Mutation{LeaseID: request.LeaseID, FencingToken: request.FencingToken}, string(change.WorkItemID), true)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(attempt, p.attempt) {
		return corestore.ErrRevisionConflict
	}
	current, err := w.Change(ctx, change.ID)
	if err != nil {
		return err
	}
	if current.Revision != p.change.Revision || current.EvidenceSequence != p.change.EvidenceSequence || !reflect.DeepEqual(current.CurrentVersionID, p.change.CurrentVersionID) {
		return corestore.ErrRevisionConflict
	}
	item, err := w.WorkItem(ctx, scope.project, change.WorkItemID)
	if err != nil {
		return err
	}
	if item.Revision != p.issue.Revision {
		return corestore.ErrRevisionConflict
	}
	reader, ok := w.(runnersession.Reader)
	if !ok {
		return nativeRunEventStorageUnavailable()
	}
	session, err := reader.RunnerSession(ctx, scope.project, p.session.ID)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(session, p.session) {
		return corestore.ErrRevisionConflict
	}
	if request.SecurityAudit != nil {
		return lockValidatorAuditDispositions(ctx, w, scope, request.SecurityAudit.Run.ID, p.evidence.auditDispositionRows)
	}
	return nil
}
