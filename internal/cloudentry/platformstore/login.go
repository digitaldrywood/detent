package platformstore

import (
	"context"
	"database/sql"
	"errors"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
)

func (r *Repository) CreateLogin(ctx context.Context, item LoginTransaction) error {
	if item.Hash == "" || item.ID == "" || item.State == "" || item.Verifier == "" || !item.ExpiresAt.After(r.now()) {
		return ErrInvalid
	}
	verifier, err := r.secrets.seal("login-verifier", item.Hash, item.Verifier)
	if err != nil {
		return err
	}
	invitation, err := r.secrets.seal("login-invitation", item.Hash, item.InvitationToken)
	if err != nil {
		return err
	}
	return r.transaction(ctx, corestore.Serving, false, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO platform.transactions(token_hash,transaction_id,state,verifier,organization_id,return_path,invitation_token,invitation_organization,support_actor,support_session,support_reason,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, item.Hash, item.ID, item.State, verifier, item.OrganizationID, item.ReturnPath, invitation, item.InvitationOrganization, item.SupportActor, item.SupportSession, item.SupportReason, item.ExpiresAt)
		return err
	})
}

func (r *Repository) ConsumeLogin(ctx context.Context, hash, id string) (LoginTransaction, error) {
	var item LoginTransaction
	err := r.transaction(ctx, corestore.Serving, false, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `WITH candidate AS (
 SELECT * FROM platform.transactions WHERE token_hash=$1 AND transaction_id=$2 AND consumed_at IS NULL AND expires_at>$3 FOR UPDATE
 ) UPDATE platform.transactions t SET consumed_at=$3,verifier='',invitation_token='' FROM candidate c WHERE t.token_hash=c.token_hash
 RETURNING c.token_hash,c.transaction_id,c.state,c.verifier,c.organization_id,c.return_path,c.invitation_token,c.invitation_organization,c.support_actor,c.support_session,c.support_reason,c.expires_at`, hash, id, r.now()).Scan(&item.Hash, &item.ID, &item.State, &item.Verifier, &item.OrganizationID, &item.ReturnPath, &item.InvitationToken, &item.InvitationOrganization, &item.SupportActor, &item.SupportSession, &item.SupportReason, &item.ExpiresAt)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNoSession
		}
		if err != nil {
			return err
		}
		item.Verifier, err = r.secrets.open("login-verifier", hash, item.Verifier)
		if err != nil {
			return err
		}
		item.InvitationToken, err = r.secrets.open("login-invitation", hash, item.InvitationToken)
		return err
	})
	if err != nil {
		return LoginTransaction{}, err
	}
	return item, nil
}
