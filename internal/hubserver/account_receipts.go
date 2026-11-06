package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"
)

func (s *Service) readAccessAccountReceipt(ctx context.Context, actor, retry string) (string, error) {
	var row accessstore.Audit
	err := s.accessRead(ctx, s.accessOrganization(), func(r tenantstore.Reader) error {
		var err error
		row, err = r.AccessAccountAudit(ctx, actor, retry)
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) {
		return "", sql.ErrNoRows
	}
	if err != nil {
		return "", err
	}
	if row.Event != "account_administration" {
		return row.Event, nil
	}
	store := s.runDetailStore()
	if row.Mutation == nil || store == nil {
		return "", artifact.ErrStorage
	}
	if err := row.Mutation.Validate(string(s.accessOrganization())); err != nil {
		return "", artifact.ErrIntegrity
	}
	raw, err := store.ReadDetail(ctx, string(s.accessOrganization()), *row.Mutation)
	return string(raw), err
}

func (s *Service) storeAccessAccountReceipt(ctx context.Context, credential apiCredential, receipt accountReceipt, metadata mutation.Metadata) error {
	store := s.runDetailStore()
	if store == nil {
		return artifact.ErrStorage
	}
	metadata.PrincipalID = credential.Hosted.Subject
	metadata.OrganizationID = string(s.accessOrganization())
	raw, err := json.Marshal(struct {
		accountReceipt
		Mutation mutation.Metadata `json:"mutation"`
	}{receipt, metadata})
	if err != nil {
		return err
	}
	ref, err := store.StoreDetail(ctx, string(s.accessOrganization()), raw)
	if err != nil {
		return err
	}
	if ref.Validate(string(s.accessOrganization())) != nil || ref.BindingID != "" || ref.CustomerOwned || ref.SchemaVersion != 1 || ref.UncompressedSize != int64(len(raw)) {
		return artifact.ErrIntegrity
	}
	verified, err := store.ReadDetail(ctx, string(s.accessOrganization()), ref)
	if err != nil {
		return err
	}
	if !bytes.Equal(verified, raw) {
		return artifact.ErrIntegrity
	}
	now := s.config.now()
	return s.accessWrite(ctx, s.accessOrganization(), func(w tenantstore.Writer) error {
		_, err := w.RecordAccessAudit(ctx, accessstore.Audit{OrganizationID: s.accessOrganization(), SessionID: credential.Hosted.SessionID, ActualActor: credential.Hosted.Subject, EffectiveUser: credential.Hosted.Subject, Event: "account_administration", Route: "mcp/account", StartedAt: credential.Hosted.CreatedAt, ExpiresAt: credential.Hosted.ExpiresAt, RecordedAt: now, RecordedAtNS: now.UnixNano(), AccountKind: receipt.Kind, AccountRetry: receipt.Retry, Mutation: &ref})
		return err
	})
}
