package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) organizationVisibleMutation(ctx context.Context, scope nativeScope, operation string, command tracker.Mutation, input, value any, apply func(tenantstore.Writer) error) (json.RawMessage, error) {
	if strings.TrimSpace(command.IdempotencyKey) == "" || len(command.IdempotencyKey) > 128 {
		return nil, nativePublicInvalid("An idempotency key of at most 128 bytes is required")
	}
	scope.requireHostedAdmin = true
	if err := s.recheckProjectRecordMutation(ctx, scope); err != nil {
		return nil, err
	}
	hash, err := nativeCommandHash(input)
	if err != nil {
		return nil, err
	}
	if raw, found, err := s.changeRecordReceipt(ctx, scope, operation, command.IdempotencyKey, hash); found || err != nil {
		return raw, err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	content := eventstore.NewContent(s.runDetailStore())
	now := s.config.now()
	receipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.organization, ActorID: scope.credential.ID, Operation: operation, CommandKey: command.IdempotencyKey, RequestHash: hash, CreatedAt: now, ResponseReplayUntilNs: now.Add(receiptstore.ReplayRetention).UnixNano()}, raw)
	if err != nil {
		return nil, err
	}
	var existing *eventstore.Receipt
	err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.LockAccessOrganization(ctx); err != nil {
			return err
		}
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		previous, err := w.Receipt(ctx, scope.credential.ID, operation, command.IdempotencyKey)
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
		if err := apply(w); err != nil {
			return err
		}
		if scope.credential.Hosted != nil {
			audit := projectRecordActor(scope).Audit
			audit.Event, audit.Route, audit.Status = "action", operation, http.StatusOK
			audit.RecordedAt = s.config.now()
			audit.RecordedAtNS = audit.RecordedAt.UnixNano()
			if _, err := w.RecordAccessAudit(ctx, audit); err != nil {
				return err
			}
		}
		_, err = w.PutReceipt(ctx, receipt)
		return err
	})
	if err != nil {
		return nil, projectRecordError(err)
	}
	if existing != nil {
		return content.Receipt(ctx, *existing)
	}
	return raw, nil
}

func (s *Service) renameOrganizationRecord(ctx context.Context, scope nativeScope, request renameOrganizationRequest) (json.RawMessage, error) {
	return s.organizationVisibleMutation(ctx, scope, "PUT /api/v2/organizations/"+string(scope.organization)+"/name", request.Mutation, request, request.organizationNameSettings, func(w tenantstore.Writer) error { return w.UpdateAccessOrganizationName(ctx, request.Name) })
}

func organizationVisibleReader(r tenantstore.Reader) (projectstore.OrganizationReader, error) {
	owner, ok := r.(projectstore.OrganizationReader)
	if !ok {
		return nil, ErrProjectParticipants
	}
	return owner, nil
}
func organizationVisibleWriter(w tenantstore.Writer) (projectstore.OrganizationWriter, error) {
	owner, ok := w.(projectstore.OrganizationWriter)
	if !ok {
		return nil, ErrProjectParticipants
	}
	return owner, nil
}
