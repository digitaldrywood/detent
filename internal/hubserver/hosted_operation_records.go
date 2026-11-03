package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) authorizeHostedReceiptActor(ctx context.Context, r tenantstore.Reader, actor string) error {
	principal, err := r.AccessCredential(ctx, actor)
	if err != nil {
		if errors.Is(err, corestore.ErrNotFound) {
			return accessstore.ErrDenied
		}
		return err
	}
	if principal.RevokedAt != nil || principal.ExpiresAt != nil && !principal.ExpiresAt.After(s.config.now()) {
		return accessstore.ErrDenied
	}
	var member accessstore.Member
	if principal.UserID != "" {
		member, err = r.AccessMember(ctx, principal.UserID)
	} else if reader, ok := r.(receiptstore.ActorReader); ok {
		member, err = reader.ReceiptMember(ctx, actor)
	} else {
		return accessstore.ErrDenied
	}
	if errors.Is(err, corestore.ErrNotFound) && principal.UserID == "" && principal.Kind != "personal" {
		return nil
	}
	if errors.Is(err, corestore.ErrNotFound) {
		return accessstore.ErrDenied
	}
	if err != nil {
		return err
	}
	if !member.Active || principal.MembershipID != "" && member.MembershipID != principal.MembershipID || !auth.ValidOrganizationRole(member.Role) {
		return accessstore.ErrDenied
	}
	canonical := principal
	if member.PrincipalID != actor {
		canonical, err = r.AccessCredential(ctx, member.PrincipalID)
	}
	if errors.Is(err, corestore.ErrNotFound) {
		return accessstore.ErrDenied
	}
	if err != nil {
		return err
	}
	if canonical.RevokedAt != nil || canonical.ExpiresAt != nil && !canonical.ExpiresAt.After(s.config.now()) {
		return accessstore.ErrDenied
	}
	return nil
}

func (s *Service) readHostedOperationRecord(ctx context.Context, command hostedCommand) (json.RawMessage, bool, error) {
	hash, err := command.hash()
	if err != nil {
		return nil, false, err
	}
	organization := tracker.OrganizationID(s.commandOrganization(ctx, command))
	var row eventstore.Receipt
	err = s.accessRead(ctx, organization, func(r tenantstore.Reader) error {
		if err := s.authorizeHostedReceiptActor(ctx, r, command.actor); err != nil {
			return err
		}
		var err error
		row, err = r.Receipt(ctx, command.actor, command.operation, command.key)
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if row.RequestHash != hash {
		return nil, true, mutation.ErrConflict
	}
	raw, err := eventstore.NewContent(s.runDetailStore()).Receipt(ctx, row)
	if err != nil {
		return nil, true, err
	}
	raw, err = resolveStorageJSON(s.contentContext(ctx), raw)
	return raw, true, err
}

func (s *Service) claimHostedOperationRecord(ctx context.Context, command hostedCommand) (bool, json.RawMessage, error) {
	if raw, found, err := s.readHostedOperationRecord(ctx, command); found || err != nil {
		return false, raw, err
	}
	hash, err := command.hash()
	if err != nil {
		return false, nil, err
	}
	organization := tracker.OrganizationID(s.commandOrganization(ctx, command))
	content := eventstore.NewContent(s.runDetailStore())
	record := eventstore.Receipt{OrganizationID: organization, ActorID: command.actor, Operation: command.operation, CommandKey: command.key, RequestHash: hash, CreatedAt: s.config.now().UTC(), ResponseClaimed: true, ResponsePending: true}
	var stored eventstore.Receipt
	var claimed bool
	err = s.accessWrite(ctx, organization, func(w tenantstore.Writer) error {
		if err := s.authorizeHostedReceiptActor(ctx, w, command.actor); err != nil {
			return err
		}
		writer, ok := w.(receiptstore.Writer)
		if !ok {
			return eventstore.ErrInvalidRecord
		}
		var err error
		stored, claimed, err = writer.ClaimReceipt(ctx, record)
		return err
	})
	if errors.Is(err, corestore.ErrRevisionConflict) {
		return false, nil, mutation.ErrConflict
	}
	if err != nil || claimed {
		return claimed, nil, err
	}
	raw, err := content.Receipt(ctx, stored)
	if err != nil {
		return false, nil, err
	}
	raw, err = resolveStorageJSON(s.contentContext(ctx), raw)
	return false, raw, err
}

func (s *Service) completeHostedOperationRecord(ctx context.Context, command hostedCommand, value any) (json.RawMessage, error) {
	raw, err := marshalNative(value)
	if err != nil {
		return nil, errors.Join(mutation.ErrUncertain, err)
	}
	hash, err := command.hash()
	if err != nil {
		return nil, errors.Join(mutation.ErrUncertain, err)
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), hubReadTimeout)
	defer cancel()
	organization := tracker.OrganizationID(s.commandOrganization(ctx, command))
	content := eventstore.NewContent(s.runDetailStore())
	record, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: organization, ActorID: command.actor, Operation: command.operation, CommandKey: command.key, RequestHash: hash, CreatedAt: s.config.now().UTC()}, []byte(raw))
	if err != nil {
		return nil, errors.Join(mutation.ErrUncertain, err)
	}
	var stored eventstore.Receipt
	err = s.accessWrite(ctx, organization, func(w tenantstore.Writer) error {
		writer, ok := w.(receiptstore.Writer)
		if !ok {
			return eventstore.ErrInvalidRecord
		}
		var err error
		stored, err = writer.CompleteReceipt(ctx, record)
		return err
	})
	if err != nil {
		return nil, errors.Join(mutation.ErrUncertain, err)
	}
	return content.Receipt(ctx, stored)
}

func (s *Service) abandonHostedOperationRecord(ctx context.Context, command hostedCommand) error {
	hash, err := command.hash()
	if err != nil {
		return err
	}
	organization := tracker.OrganizationID(s.commandOrganization(ctx, command))
	return s.accessWrite(ctx, organization, func(w tenantstore.Writer) error {
		writer, ok := w.(receiptstore.Writer)
		if !ok {
			return eventstore.ErrInvalidRecord
		}
		return writer.AbandonReceipt(ctx, command.actor, command.operation, command.key, hash)
	})
}

func (s *Service) retainHostedOperationRecords(ctx context.Context, cutoff time.Time) error {
	organization := tracker.OrganizationID(s.appOrganizationID())
	var page []receiptstore.Record
	if err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(r tenantstore.Reader) error {
		reader, ok := r.(receiptstore.RetentionReader)
		if !ok {
			return eventstore.ErrInvalidRecord
		}
		var err error
		page, err = reader.ReceiptExpiryPage(ctx, cutoff, receiptstore.MaxRetentionBatch)
		return err
	}); err != nil {
		return err
	}
	for _, record := range page {
		if err := s.config.TenantRepository.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
			writer, ok := w.(receiptstore.RetentionWriter)
			if !ok {
				return eventstore.ErrInvalidRecord
			}
			_, err := writer.ExpireReceipt(ctx, record, cutoff)
			return err
		}); err != nil {
			return err
		}
	}
	return nil
}
