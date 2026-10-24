package cloudentry

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

func (s *Service) oauthRegistration(ctx context.Context, id string) (oauthRegistration, error) {
	var registration oauthRegistration
	sealed, ok := strings.CutPrefix(id, "oauth_client_")
	if !ok || len(id) > 2048 {
		return registration, errNoSession
	}
	rows, err := s.auth.store.reader.QueryContext(ctx, `SELECT c.name,c.auth_method,c.secret_hash,u.uri FROM oauth_clients c JOIN oauth_client_redirect_uris u ON u.client_id=c.id WHERE c.id=? ORDER BY u.position`, id)
	if err != nil {
		return registration, err
	}
	defer rows.Close()
	for rows.Next() {
		var uri string
		if err := rows.Scan(&registration.Name, &registration.AuthMethod, &registration.SecretHash, &uri); err != nil {
			return registration, err
		}
		registration.RedirectURIs = append(registration.RedirectURIs, uri)
	}
	if err := rows.Err(); err != nil {
		return registration, err
	}
	if len(registration.RedirectURIs) > 0 {
		return registration, nil
	}
	err = s.openOAuth("oauth-client", sealed, &registration)
	return registration, err
}

func insertOAuthRedirectURIs(ctx context.Context, tx *sql.Tx, id string, uris []string) error {
	for position, uri := range uris {
		if _, err := tx.ExecContext(ctx, `INSERT INTO oauth_client_redirect_uris(client_id,position,uri) VALUES(?,?,?) ON CONFLICT DO NOTHING`, id, position, uri); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) startOAuthClientBackfill(parent context.Context) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	s.stopOAuthBackfill = cancel
	s.oauthBackfillDone = make(chan struct{})
	go func() {
		defer close(s.oauthBackfillDone)
		if err := s.backfillOAuthClients(ctx); err != nil && ctx.Err() == nil {
			s.config.Logger.ErrorContext(ctx, "backfill OAuth clients failed", "error", err)
		}
	}()
}

func (s *Service) backfillOAuthClients(ctx context.Context) error {
	var after string
	for {
		var id, created string
		err := s.auth.store.reader.QueryRowContext(ctx, `SELECT c.id,c.created_at FROM oauth_clients c WHERE c.id>? AND NOT EXISTS(SELECT 1 FROM oauth_client_redirect_uris u WHERE u.client_id=c.id) ORDER BY c.id LIMIT 1`, after).Scan(&id, &created)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		after = id
		var registration oauthRegistration
		sealed, ok := strings.CutPrefix(id, "oauth_client_")
		if !ok || s.openOAuth("oauth-client", sealed, &registration) != nil || len(registration.RedirectURIs) == 0 {
			s.config.Logger.ErrorContext(ctx, "backfill OAuth client unavailable", "error", errNoSession, "created_at", created)
			continue
		}
		if err := s.backfillOAuthClient(ctx, id, registration.RedirectURIs); err != nil {
			return err
		}
	}
}

func (s *Service) backfillOAuthClient(ctx context.Context, id string, uris []string) error {
	tx, err := s.auth.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := insertOAuthRedirectURIs(ctx, tx, id, uris); err != nil {
		return err
	}
	return tx.Commit()
}
