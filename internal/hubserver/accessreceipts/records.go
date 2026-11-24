package accessreceipts

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) AuthorizeActor(ctx context.Context, r tenantstore.Reader, actor string) error {
	principal, err := r.AccessCredential(ctx, actor)
	if err != nil {
		if errors.Is(err, corestore.ErrNotFound) {
			return accessstore.ErrDenied
		}
		return err
	}
	if principal.RevokedAt != nil || principal.ExpiresAt != nil && !principal.ExpiresAt.After(s.deps.Now()) {
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
	if canonical.RevokedAt != nil || canonical.ExpiresAt != nil && !canonical.ExpiresAt.After(s.deps.Now()) {
		return accessstore.ErrDenied
	}
	return nil
}

func (s *Service) Read(ctx context.Context, command Command) (json.RawMessage, bool, error) {
	hash, err := hubapi.CommandHash(command.Input)
	if err != nil {
		return nil, false, err
	}
	organization := command.Organization
	var row eventstore.Receipt
	err = s.read(ctx, organization, func(r tenantstore.Reader) error {
		if err := s.AuthorizeActor(ctx, r, command.Actor); err != nil {
			return err
		}
		var err error
		row, err = r.Receipt(ctx, command.Actor, command.Operation, command.Key)
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
	raw, err := eventstore.NewContent(s.deps.Details).Receipt(ctx, row)
	if err != nil {
		return nil, true, err
	}
	raw, err = s.deps.ResolveJSON(ctx, raw)
	return raw, true, err
}

func (s *Service) Claim(ctx context.Context, command Command) (bool, json.RawMessage, error) {
	if raw, found, err := s.Read(ctx, command); found || err != nil {
		return false, raw, err
	}
	hash, err := hubapi.CommandHash(command.Input)
	if err != nil {
		return false, nil, err
	}
	organization := command.Organization
	content := eventstore.NewContent(s.deps.Details)
	record := eventstore.Receipt{OrganizationID: organization, ActorID: command.Actor, Operation: command.Operation, CommandKey: command.Key, RequestHash: hash, CreatedAt: s.deps.Now().UTC(), ResponseClaimed: true, ResponsePending: true}
	var stored eventstore.Receipt
	var claimed bool
	err = s.write(ctx, organization, func(w tenantstore.Writer) error {
		if err := s.AuthorizeActor(ctx, w, command.Actor); err != nil {
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
	raw, err = s.deps.ResolveJSON(ctx, raw)
	return false, raw, err
}

func (s *Service) Complete(ctx context.Context, command Command, value any) (json.RawMessage, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, errors.Join(mutation.ErrUncertain, err)
	}
	hash, err := hubapi.CommandHash(command.Input)
	if err != nil {
		return nil, errors.Join(mutation.ErrUncertain, err)
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), hubapi.DefaultCleanupBudget)
	defer cancel()
	organization := command.Organization
	content := eventstore.NewContent(s.deps.Details)
	record, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: organization, ActorID: command.Actor, Operation: command.Operation, CommandKey: command.Key, RequestHash: hash, CreatedAt: s.deps.Now().UTC()}, raw)
	if err != nil {
		return nil, errors.Join(mutation.ErrUncertain, err)
	}
	var stored eventstore.Receipt
	err = s.write(ctx, organization, func(w tenantstore.Writer) error {
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

func (s *Service) Abandon(ctx context.Context, command Command) error {
	hash, err := hubapi.CommandHash(command.Input)
	if err != nil {
		return err
	}
	organization := command.Organization
	return s.write(ctx, organization, func(w tenantstore.Writer) error {
		writer, ok := w.(receiptstore.Writer)
		if !ok {
			return eventstore.ErrInvalidRecord
		}
		return writer.AbandonReceipt(ctx, command.Actor, command.Operation, command.Key, hash)
	})
}

func (s *Service) Retain(ctx context.Context, cutoff time.Time) error {
	organization := s.deps.Organization
	var page []receiptstore.Record
	if err := s.deps.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(r tenantstore.Reader) error {
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
		if err := s.deps.TenantRepository.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
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

type Deps struct {
	Repository       tenantstore.Repository
	TenantRepository tenantstore.Repository
	Organization     tracker.OrganizationID
	Now              func() time.Time
	Details          attachment.DetailStore
	ResolveJSON      func(context.Context, []byte) ([]byte, error)
}

type Command struct {
	Organization          tracker.OrganizationID
	Actor, Operation, Key string
	Input                 any
}

type Service struct{ deps Deps }

func New(deps Deps) *Service { return &Service{deps: deps} }

func (s *Service) read(ctx context.Context, organization tracker.OrganizationID, read func(tenantstore.Reader) error) error {
	if s.deps.Repository == nil {
		return hubapi.WorkItemStorageUnavailable()
	}
	return s.deps.Repository.ReadTenant(ctx, organization, corestore.Serving, read)
}

func (s *Service) write(ctx context.Context, organization tracker.OrganizationID, write func(tenantstore.Writer) error) error {
	if s.deps.Repository == nil {
		return hubapi.WorkItemStorageUnavailable()
	}
	return s.deps.Repository.WriteTenant(ctx, organization, corestore.Serving, write)
}

func ResponseExpired() error {
	return &hubapi.Error{Code: "idempotency_expired", Message: receiptstore.ErrExpired.Error(), Status: http.StatusGone, Public: true}
}
