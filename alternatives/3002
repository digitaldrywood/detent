package hubserver

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) readReleaseDeliveryIssue(ctx context.Context, scope nativeScope, item string) (tracker.NativeIssue, error) {
	if s.config.TenantRepository == nil {
		return tracker.NativeIssue{}, workItemStorageUnavailable()
	}
	row, _, err := s.readWorkItemRecord(ctx, scope, item)
	if err != nil {
		return tracker.NativeIssue{}, err
	}
	delivery, found, err := s.readReleaseDeliveryRecord(ctx, scope, item)
	var issue tracker.NativeIssue
	issue.Revision = tracker.Revision(row.Revision)
	if found {
		issue.ReleaseDelivery = &delivery
	}
	return issue, err
}

func (s *Service) replayReleaseAssociation(ctx context.Context, scope nativeScope, item, operation string, request tracker.AssociateReleasePick, hash string, delivery tracker.ReleaseDelivery) ([]byte, error) {
	raw, err := json.Marshal(delivery)
	if err != nil {
		return nil, err
	}
	receipt, err := eventstore.NewContent(s.runDetailStore()).PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.organization, ActorID: scope.credential.ID, Operation: operation, CommandKey: request.IdempotencyKey, RequestHash: hash, CreatedAt: s.config.now()}, raw)
	if err != nil {
		return nil, err
	}
	if err := s.recheckProjectRecordMutation(ctx, scope); err != nil {
		return nil, err
	}
	err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.authorizeReleaseScope(ctx, w, scope); err != nil {
			return err
		}
		if err := s.database.requireHostedFeatureRecord(ctx, w, "collaboration", s.config.now()); err != nil {
			return err
		}
		if _, err := w.LockWorkItem(ctx, scope.project, tracker.NativeWorkItemID(item)); err != nil {
			return err
		}
		reader, ok := w.(eventstore.ReleaseDeliveryReader)
		if !ok {
			return workItemStorageUnavailable()
		}
		current, err := reader.ReleaseDelivery(ctx, scope.project, tracker.NativeWorkItemID(item))
		if err != nil {
			return err
		}
		if current.ReleaseDelivery != delivery {
			return nativeConflict(0)
		}
		if prior, err := w.Receipt(ctx, scope.credential.ID, operation, request.IdempotencyKey); err == nil {
			if prior.RequestHash != hash {
				return nativeConflict(0)
			}
			return corestore.ErrSerializationFailure
		} else if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if err := s.recordReleaseAssociationAudit(ctx, w, scope, operation); err != nil {
			return err
		}
		_, err = w.PutReceipt(ctx, receipt)
		return err
	})
	if errors.Is(err, corestore.ErrSerializationFailure) {
		if replay, found, replayErr := s.releaseAssociationReceipt(ctx, scope, operation, request.IdempotencyKey, hash); replayErr != nil || found {
			return replay, replayErr
		}
	}
	return raw, releaseAssociationError(err)
}

func (s *Service) releaseAssociationReceipt(ctx context.Context, scope nativeScope, operation, key, hash string) ([]byte, bool, error) {
	if err := s.recheckProjectRecordMutation(ctx, scope); err != nil {
		return nil, false, err
	}
	var receipt eventstore.Receipt
	err := s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.authorizeReleaseScope(ctx, w, scope); err != nil {
			return err
		}
		var err error
		receipt, err = w.Receipt(ctx, scope.credential.ID, operation, key)
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, releaseAssociationError(err)
	}
	if receipt.RequestHash != hash {
		return nil, false, nativeConflict(0)
	}
	if receipt.ResponseReplayUntilNs > 0 && s.config.now().UnixNano() >= receipt.ResponseReplayUntilNs {
		return nil, false, receiptstore.ErrExpired
	}
	raw, err := eventstore.NewContent(s.runDetailStore()).Receipt(ctx, receipt)
	return raw, err == nil, err
}

func releaseAssociationError(err error) error {
	if errors.Is(err, corestore.ErrRevisionConflict) || errors.Is(err, corestore.ErrUniqueViolation) || errors.Is(err, corestore.ErrSerializationFailure) {
		return nativeConflict(0)
	}
	return projectRecordError(err)
}

func (s *Service) recordReleaseAssociationAudit(ctx context.Context, w tenantstore.Writer, scope nativeScope, operation string) error {
	if scope.credential.Hosted == nil {
		return nil
	}
	audit := projectRecordActor(scope).Audit
	audit.Event, audit.Route, audit.Status = "action", operation, 200
	audit.ProjectID = scope.project
	audit.RecordedAt = s.config.now().UTC()
	audit.RecordedAtNS = audit.RecordedAt.UnixNano()
	_, err := w.RecordAccessAudit(ctx, audit)
	return err
}
