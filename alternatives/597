package cloudentry

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatoradmin"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

func (s *Service) storeEntryObject(ctx context.Context, data []byte) (string, error) {
	if len(data) > 1<<20 {
		return "", operatoradmin.ErrUnavailable
	}
	if len(data) == 0 {
		return "", nil
	}
	if s.attachments == nil {
		return "", operatoradmin.ErrUnavailable
	}
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(data); err != nil {
		return "", err
	}
	if err := writer.Close(); err != nil {
		return "", err
	}
	digest := sha256.Sum256(compressed.Bytes())
	hash := hex.EncodeToString(digest[:])
	key := "platform/entry/sha256/" + hash
	if err := s.attachments.Put(ctx, key, bytes.NewReader(compressed.Bytes()), int64(compressed.Len()), "application/gzip", hash); err != nil {
		return "", err
	}
	return key, nil
}

func (s *Service) readEntryObject(ctx context.Context, key string) ([]byte, error) {
	if key == "" {
		return nil, nil
	}
	if s.attachments == nil || !strings.HasPrefix(key, "platform/entry/sha256/") {
		return nil, operatoradmin.ErrUnavailable
	}
	body, err := s.attachments.Open(ctx, key)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	compressed, err := io.ReadAll(io.LimitReader(body, 1<<20+1))
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(compressed)
	if len(compressed) > 1<<20 || key != "platform/entry/sha256/"+hex.EncodeToString(digest[:]) {
		return nil, operatoradmin.ErrUnavailable
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(reader, 1<<20+1))
	if err := errors.Join(err, reader.Close()); err != nil {
		return nil, err
	}
	if len(data) > 1<<20 {
		return nil, operatoradmin.ErrUnavailable
	}
	return data, nil
}

func (s *Service) postgresAdministrationReceipt(ctx context.Context, session accountSession, m mutation.Metadata) (operatoradmin.Output, bool, error) {
	var hash, outcome, key string
	var out operatoradmin.Output
	var hasSignOut bool
	var signOut operatortool.SignOutResult
	err := s.auth.store.reader.QueryRowContext(ctx, `SELECT input_hash,outcome,resource_id,url,reconnect,sign_out,signed_out,provider_confirmed,audit_recorded,output_object_key FROM administration_receipts WHERE subject=? AND organization_id=? AND retry_identity=?`, session.Subject, m.OrganizationID, m.RetryIdentity).Scan(&hash, &outcome, &out.ResourceID, &out.URL, &out.Reconnect, &hasSignOut, &signOut.SignedOut, &signOut.ProviderConfirmed, &signOut.AuditRecorded, &key)
	if errors.Is(err, sql.ErrNoRows) {
		return out, false, nil
	}
	if err != nil {
		return out, false, err
	}
	if hash != m.InputHash {
		return operatoradmin.Output{}, false, mutation.ErrConflict
	}
	if outcome != "succeeded" {
		return operatoradmin.Output{}, false, mutation.ErrUncertain
	}
	if hasSignOut {
		out.SignOut = &signOut
	}
	out.Data, err = s.readEntryObject(ctx, key)
	return out, err == nil, err
}

func (s *Service) postgresRecordAdministrationReceipt(ctx context.Context, session accountSession, m mutation.Metadata, outcome string, out operatoradmin.Output) error {
	key, err := s.storeEntryObject(ctx, out.Data)
	if err != nil {
		return err
	}
	var signOut operatortool.SignOutResult
	if out.SignOut != nil {
		signOut = *out.SignOut
	}
	return s.writePostgresAdministrationReceipt(ctx, s.auth.store.db, session, m, outcome, out, key, signOut)
}

type entryExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func (s *Service) writePostgresAdministrationReceipt(ctx context.Context, target entryExecutor, session accountSession, m mutation.Metadata, outcome string, out operatoradmin.Output, key string, signOut operatortool.SignOutResult) error {
	result, err := target.ExecContext(ctx, `INSERT INTO administration_receipts(subject,organization_id,retry_identity,input_hash,outcome,resource_id,url,reconnect,sign_out,signed_out,provider_confirmed,audit_recorded,output_object_key,recorded_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(subject,organization_id,retry_identity) DO UPDATE SET outcome=excluded.outcome,resource_id=excluded.resource_id,url=excluded.url,reconnect=excluded.reconnect,sign_out=excluded.sign_out,signed_out=excluded.signed_out,provider_confirmed=excluded.provider_confirmed,audit_recorded=excluded.audit_recorded,output_object_key=excluded.output_object_key,recorded_at=excluded.recorded_at WHERE administration_receipts.input_hash=excluded.input_hash`, session.Subject, m.OrganizationID, m.RetryIdentity, m.InputHash, outcome, out.ResourceID, out.URL, out.Reconnect, out.SignOut != nil, signOut.SignedOut, signOut.ProviderConfirmed, signOut.AuditRecorded, key, s.config.now())
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return mutation.ErrConflict
	}
	return nil
}

func (s *Service) postgresCreditAudit(ctx context.Context, actor, organization, retry, amount, reason string) error {
	data, err := json.Marshal(map[string]string{"kind": "ai_credits_adjusted", "idempotency_key": retry, "amount_usd": amount, "reason": reason})
	if err != nil {
		return err
	}
	key, err := s.storeEntryObject(ctx, data)
	if err != nil {
		return err
	}
	_, err = s.auth.store.db.ExecContext(ctx, `INSERT INTO audit(subject,organization_id,event,event_object_key,retry_identity,amount_usd,reason,recorded_at) VALUES(?,?,'ai_credits_adjusted',?,?,?,?,?) ON CONFLICT DO NOTHING`, actor, organization, key, retry, amount, reason, s.config.now())
	return err
}

func (s *Service) postgresSupportRequest(ctx context.Context, session accountSession, organization, reason, detail string) error {
	key, err := s.storeEntryObject(ctx, []byte(detail))
	if err != nil {
		return err
	}
	_, err = s.auth.store.db.ExecContext(ctx, "INSERT INTO audit(subject,organization_id,event,event_object_key,reason,recorded_at) VALUES(?,?,'support_requested',?,?,?)", session.Subject, organization, key, reason, s.config.now())
	return err
}
