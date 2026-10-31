package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"net"
)

type secretRewrapListener struct {
	net.Listener
	serving func()
}

func (l secretRewrapListener) Accept() (net.Conn, error) {
	l.serving()
	return l.Listener.Accept()
}

func (s *Service) signalSecretRewrap() {
	s.secretRewrapOnce.Do(func() { close(s.secretRewrapReady) })
}

func (s *Service) runSecretRewrap(ctx context.Context) {
	defer close(s.secretRewrapDone)
	select {
	case <-ctx.Done():
		return
	case <-s.secretRewrapReady:
	}
	s.rewrapSecrets(ctx)
}

func (s *Service) rewrapSecrets(ctx context.Context) {
	if s.config.SecretKeys == nil {
		return
	}
	rows, err := s.database.reader.QueryContext(ctx, `SELECT organization_id, project_id, kind FROM project_secrets WHERE master_key_version<>? UNION ALL SELECT organization_id, '', kind FROM organization_secrets WHERE master_key_version<>? ORDER BY organization_id, project_id, kind`, s.config.SecretKeys.Version(), s.config.SecretKeys.Version())
	if err != nil {
		if ctx.Err() == nil {
			s.config.Logger.Error("select secrets to rewrap", "error", err)
		}
		return
	}
	defer func() { _ = rows.Close() }()
	var candidates []projectSecretRow
	for rows.Next() {
		var row projectSecretRow
		if err = rows.Scan(&row.organization, &row.project, &row.kind); err != nil {
			break
		}
		candidates = append(candidates, row)
	}
	err = errors.Join(err, rows.Err())
	if err != nil {
		if ctx.Err() == nil {
			s.config.Logger.Error("select secrets to rewrap", "error", err)
		}
		return
	}
	for _, row := range candidates {
		if ctx.Err() != nil {
			return
		}
		if row.project == "" {
			err = s.database.reader.QueryRowContext(ctx, `SELECT ciphertext,nonce,wrapped_data_key,master_key_version FROM organization_secrets WHERE organization_id=? AND kind=?`, row.organization, row.kind).Scan(&row.envelope.Ciphertext, &row.envelope.Nonce, &row.envelope.WrappedKey, &row.envelope.Version)
		} else {
			err = s.database.reader.QueryRowContext(ctx, `SELECT ciphertext,nonce,wrapped_data_key,master_key_version FROM project_secrets WHERE organization_id=? AND project_id=? AND kind=?`, row.organization, row.project, row.kind).Scan(&row.envelope.Ciphertext, &row.envelope.Nonce, &row.envelope.WrappedKey, &row.envelope.Version)
		}
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err == nil && row.envelope.Version != s.config.SecretKeys.Version() {
			err = s.rewrapSecret(ctx, row)
		}
		if err != nil && ctx.Err() == nil {
			s.config.Logger.Error("rewrap provider secret", "error", err, "organization_id", row.organization, "project_id", row.project, "kind", row.kind, "master_key_version", row.envelope.Version)
		}
	}
}

func (s *Service) rewrapSecret(ctx context.Context, row projectSecretRow) (err error) {
	wrapped, err := s.config.SecretKeys.Rewrap(row.envelope, secretAAD(row.organization, row.project, row.kind))
	if err != nil {
		return err
	}
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if rollback := tx.Rollback(); rollback != nil && !errors.Is(rollback, sql.ErrTxDone) {
			err = errors.Join(err, rollback)
		}
	}()
	var result sql.Result
	if row.project == "" {
		result, err = tx.ExecContext(ctx, `UPDATE organization_secrets SET wrapped_data_key=?,master_key_version=? WHERE organization_id=? AND kind=? AND master_key_version=? AND wrapped_data_key=?`, wrapped.WrappedKey, wrapped.Version, row.organization, row.kind, row.envelope.Version, row.envelope.WrappedKey)
	} else {
		result, err = tx.ExecContext(ctx, `UPDATE project_secrets SET wrapped_data_key=?,master_key_version=? WHERE organization_id=? AND project_id=? AND kind=? AND master_key_version=? AND wrapped_data_key=?`, wrapped.WrappedKey, wrapped.Version, row.organization, row.project, row.kind, row.envelope.Version, row.envelope.WrappedKey)
	}
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed == 0 {
		return err
	}
	if err := secretAudit(ctx, tx, row.organization, row.project, "system:rewrap", row.kind, "rotate", wrapped.Version, formatHubTime(s.config.now())); err != nil {
		return err
	}
	return tx.Commit()
}
