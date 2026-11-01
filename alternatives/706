package platformstore

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
)

func (r *Repository) StoreGitHubToken(ctx context.Context, owner, id, token string, expires *time.Time) error {
	if owner == "" || id == "" || token == "" {
		return ErrInvalid
	}
	sealed, err := r.secrets.seal("github-user-access", owner, token)
	if err != nil {
		return err
	}
	return r.transaction(ctx, corestore.Serving, false, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO platform.github_user_connections(owner_subject,id,access_token,expires_at,created_at) VALUES($1,$2,$3,$4,$5) ON CONFLICT(owner_subject) DO UPDATE SET id=excluded.id,access_token=excluded.access_token,expires_at=excluded.expires_at,created_at=excluded.created_at,revoked_at=NULL`, owner, id, sealed, expires, r.now())
		return err
	})
}

func (r *Repository) GitHubToken(ctx context.Context, owner string) (string, error) {
	var token string
	err := r.transaction(ctx, corestore.Serving, true, func(tx *sql.Tx) error {
		var sealed string
		err := tx.QueryRowContext(ctx, `SELECT access_token FROM platform.github_user_connections WHERE owner_subject=$1 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at>$2)`, owner, r.now()).Scan(&sealed)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNoSession
		}
		if err != nil {
			return err
		}
		token, err = r.secrets.open("github-user-access", owner, sealed)
		return err
	})
	return token, err
}

func (r *Repository) RevokeGitHubToken(ctx context.Context, owner, id string) error {
	return r.transaction(ctx, corestore.Serving, false, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE platform.github_user_connections SET revoked_at=$1 WHERE owner_subject=$2 AND id=$3 AND revoked_at IS NULL`, r.now(), owner, id)
		return err
	})
}
