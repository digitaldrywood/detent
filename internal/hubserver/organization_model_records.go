package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) readOrganizationModels(ctx context.Context, organization tracker.OrganizationID) (cloudModelSelection, error) {
	var row policystore.OrganizationModels
	err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		row, err = r.OrganizationModels(ctx)
		return err
	})
	if err != nil {
		return cloudModelSelection{}, projectRecordError(err)
	}
	selection := config.ModelSelection{}
	if row.Revision > 0 {
		raw, err := eventstore.NewContent(s.runDetailStore()).Receipt(ctx, eventstore.Receipt{OrganizationID: organization, PayloadObjectKey: row.Payload.Key, PayloadSHA256: row.Payload.SHA256, PayloadBytes: row.Payload.CompressedSize, PayloadContentBytes: row.Payload.UncompressedSize})
		if err != nil {
			return cloudModelSelection{}, err
		}
		if err := json.Unmarshal(raw, &selection); err != nil {
			return cloudModelSelection{}, err
		}
	}
	return cloudModelSelection{Revision: tracker.Revision(row.Revision), Selection: &selection, Effective: config.ResolveCloudModelSelection(selection, config.ModelSelection{})}, nil
}

func (s *Service) updateOrganizationModelRecord(ctx context.Context, scope nativeScope, operation string, request cloudModelSelectionRequest) (json.RawMessage, error) {
	if strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 128 || request.ExpectedRevision < 0 || request.Selection == nil {
		return nil, nativePublicInvalid("An idempotency key, nonnegative revision and model selection are required")
	}
	if s.config.AccessRepository == nil {
		return nil, nativeNotFound()
	}
	if err := s.recheckProjectRecordMutation(ctx, scope); err != nil {
		return nil, err
	}
	hash, err := nativeCommandHash(request)
	if err != nil {
		return nil, err
	}
	if replay, found, err := s.changeRecordReceipt(ctx, scope, operation, request.IdempotencyKey, hash); found || err != nil {
		return replay, err
	}
	effective := config.ResolveCloudModelSelection(*request.Selection, config.ModelSelection{})
	if len(effective.Validate()) > 0 {
		return nil, nativePublicInvalid("Model selection is invalid")
	}
	value := cloudModelSelection{Revision: request.ExpectedRevision + 1, Selection: request.Selection, Effective: effective}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	selection, err := json.Marshal(request.Selection)
	if err != nil {
		return nil, err
	}
	content := eventstore.NewContent(s.runDetailStore())
	payload, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.organization}, selection)
	if err != nil {
		return nil, err
	}
	row := policystore.OrganizationModels{OrganizationID: scope.organization, Revision: int64(value.Revision), Payload: eventstore.Reference(payload.PayloadObjectKey, payload.PayloadSHA256, payload.PayloadBytes, payload.PayloadContentBytes)}
	receipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.organization, ActorID: scope.credential.ID, Operation: operation, CommandKey: request.IdempotencyKey, RequestHash: hash, CreatedAt: s.config.now()}, raw)
	if err != nil {
		return nil, err
	}
	var existing *eventstore.Receipt
	err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		previous, err := w.Receipt(ctx, scope.credential.ID, operation, request.IdempotencyKey)
		if err == nil {
			if previous.RequestHash != hash {
				return nativeConflict(0)
			}
			existing = &previous
			return nil
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if err := w.SetOrganizationModels(ctx, row, int64(request.ExpectedRevision)); err != nil {
			return err
		}
		_, err = w.PutReceipt(ctx, receipt)
		return err
	})
	if errors.Is(err, corestore.ErrRevisionConflict) || errors.Is(err, corestore.ErrUniqueViolation) {
		if authErr := s.recheckProjectRecordMutation(ctx, scope); authErr != nil {
			return nil, authErr
		}
		if replay, found, readErr := s.changeRecordReceipt(ctx, scope, operation, request.IdempotencyKey, hash); found || readErr != nil {
			return replay, readErr
		}
		current, readErr := s.readOrganizationModels(ctx, scope.organization)
		if readErr != nil {
			return nil, readErr
		}
		return nil, nativeConflict(current.Revision)
	}
	if err != nil {
		return nil, projectRecordError(err)
	}
	if existing != nil {
		return content.Receipt(ctx, *existing)
	}
	return raw, nil
}
