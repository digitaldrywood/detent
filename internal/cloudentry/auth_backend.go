package cloudentry

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/cloudentry/platformstore"
	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tenantauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func entryAuthStatus(err error, denied int) int {
	if errors.Is(err, tenantauth.ErrUnavailable) || errors.Is(err, hubsecrets.ErrUnavailable) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || auth.HostedIdentityReason(err) == auth.HostedReasonProviderUnavailable || auth.HostedIdentityReason(err) == auth.HostedReasonProviderInvalid {
		return http.StatusServiceUnavailable
	}
	var refusal *apikey.Refusal
	if err == nil || errors.Is(err, errNoSession) || errors.Is(err, platformstore.ErrNoSession) || errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrOrganizationNotFound) || errors.Is(err, auth.ErrHostedIdentity) || errors.Is(err, auth.ErrAccessExpired) || errors.Is(err, tenantauth.ErrDenied) || errors.Is(err, cloudassert.ErrInvalid) || errors.Is(err, operatortool.ErrAccessDenied) || errors.As(err, &refusal) {
		return denied
	}
	return http.StatusServiceUnavailable
}

func entryAuthError(err error) error {
	if err != nil && entryAuthStatus(err, http.StatusUnauthorized) == http.StatusServiceUnavailable {
		return err
	}
	if errors.Is(err, platformstore.ErrNoSession) || errors.Is(err, sql.ErrNoRows) {
		return errNoSession
	}
	return err
}

func (a *authStore) postgresAuthorize(ctx context.Context, session accountSession, organization string, identity auth.HostedIdentity, tokens auth.HostedTokens, email string) (authorization, []authorization, error) {
	item, previous, err := a.store.db.postgres.Authorize(ctx, session.Hash, tracker.OrganizationID(organization), identity, tokens, email)
	if err != nil {
		return authorization{}, nil, entryAuthError(err)
	}
	result := entryAuthorization(item)
	var stale []authorization
	if previous != nil && previous.Binding != result.Binding {
		stale = append(stale, entryAuthorization(*previous))
	}
	return result, stale, nil
}

func entryAuthorization(item platformstore.Authorization) authorization {
	return authorization{Binding: item.Binding, Organization: string(item.OrganizationID), Identity: item.Identity, Support: item.Support, EffectiveEmail: item.EffectiveEmail}
}

type authorizationLookupKind uint8

const (
	lookupBinding authorizationLookupKind = iota
	lookupSessionOrganization
	lookupSession
	lookupOrganization
)

type authorizationSelector struct {
	Binding, SessionHash, Organization string
	Kind                               authorizationLookupKind
}

func postgresAuthorizations(ctx context.Context, query queryer, selector authorizationSelector) (_ []authorization, resultErr error) {
	clock, ok := query.(interface{ entryNow() time.Time })
	if !ok {
		return nil, platformstore.ErrInvalid
	}
	predicate := `binding=$1 AND revoked_at IS NULL AND expires_at>$2 AND identity_expires_at>$2`
	expires := `$2`
	args := []any{selector.Binding, clock.entryNow()}
	switch selector.Kind {
	case lookupSessionOrganization:
		predicate = `session_hash=$1 AND organization_id=$2 AND revoked_at IS NULL AND expires_at>$3 AND identity_expires_at>$3`
		expires = `$3`
		args = []any{selector.SessionHash, selector.Organization, clock.entryNow()}
	case lookupSession:
		predicate = `session_hash=$1 AND revoked_at IS NULL`
		args = []any{selector.SessionHash, clock.entryNow()}
	case lookupOrganization:
		predicate = `organization_id=$1 AND revoked_at IS NULL`
		args = []any{selector.Organization, clock.entryNow()}
	}
	statement := `SELECT binding,organization_id,identity_subject,provider_organization_id,provider_session_id,identity_created_at,identity_expires_at,support_actor,support_reason,support,effective_email,access_token,refresh_token FROM platform.authorizations WHERE ` + predicate + ` AND EXISTS(SELECT 1 FROM platform.sessions s WHERE s.token_hash=platform.authorizations.session_hash AND s.revoked_at IS NULL AND s.expires_at>` + expires + `)`
	rows, err := query.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, err
	}
	defer closeEntryRows(rows, &resultErr)
	var result []authorization
	for rows.Next() {
		var item authorization
		if err := rows.Scan(&item.Binding, &item.Organization, &item.Identity.Subject, &item.Identity.OrganizationID, &item.Identity.SessionID, &item.Identity.CreatedAt, &item.Identity.ExpiresAt, &item.Identity.SupportActor, &item.Identity.SupportReason, &item.Support, &item.EffectiveEmail, &item.sealedAccess, &item.sealedRefresh); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (a *authStore) postgresSession(ctx context.Context, hash string) (accountSession, time.Time, error) {
	item, err := a.store.db.postgres.StoredSession(ctx, hash)
	if err != nil {
		return accountSession{}, time.Time{}, entryAuthError(err)
	}
	return accountSession{Hash: item.Hash, Subject: item.Subject, Email: item.Email, CSRFSecret: item.CSRFSecret, Identity: item.Identity}, item.ExpiresAt, nil
}

func (a *authStore) postgresLogin(ctx context.Context, hash string, item loginTransaction) error {
	return a.store.db.postgres.CreateLogin(ctx, platformstore.LoginTransaction{Hash: hash, ID: item.ID, State: item.State, Verifier: item.Verifier, OrganizationID: item.Organization, ReturnPath: item.ReturnPath, InvitationToken: item.InvitationToken, InvitationOrganization: item.InvitationOrganization, SupportActor: item.SupportActor, SupportSession: item.SupportSession, SupportReason: item.SupportReason, ExpiresAt: a.now().Add(10 * time.Minute)})
}

func (a *authStore) postgresConsumeLogin(ctx context.Context, hash, id string) (loginTransaction, error) {
	item, err := a.store.db.postgres.ConsumeLogin(ctx, hash, id)
	return loginTransaction{ID: item.ID, State: item.State, Verifier: item.Verifier, Organization: item.OrganizationID, ReturnPath: item.ReturnPath, InvitationToken: item.InvitationToken, InvitationOrganization: item.InvitationOrganization, SupportActor: item.SupportActor, SupportSession: item.SupportSession, SupportReason: item.SupportReason}, entryAuthError(err)
}

func (a *authStore) postgresRevokeSession(ctx context.Context, hash string, preserveAuthorizations bool) ([]authorization, error) {
	items, err := postgresAuthorizations(ctx, a.store.db, authorizationSelector{Kind: lookupSession, SessionHash: hash})
	if err != nil {
		return nil, err
	}
	if preserveAuthorizations {
		for _, item := range items {
			if item.Support || !item.Identity.ExpiresAt.After(a.now()) {
				continue
			}
			if _, err := a.tokens(item); err != nil && entryAuthStatus(err, http.StatusUnauthorized) == http.StatusServiceUnavailable {
				return nil, err
			}
		}
	}
	return items, a.store.db.postgres.RevokeSession(ctx, hash)
}

func (a *authStore) postgresStoreTokens(ctx context.Context, binding string, tokens auth.HostedTokens, expected ...auth.HostedTokens) error {
	var hash, organization string
	err := a.store.reader.QueryRowContext(ctx, "SELECT session_hash,organization_id FROM platform.authorizations WHERE binding=$1 AND revoked_at IS NULL", binding).Scan(&hash, &organization)
	if err != nil {
		return entryAuthError(err)
	}
	previous, err := a.store.db.postgres.Tokens(ctx, hash, tracker.OrganizationID(organization), binding)
	if err != nil {
		return entryAuthError(err)
	}
	if len(expected) > 0 {
		previous = expected[0]
	}
	return entryAuthError(a.store.db.postgres.RotateTokens(ctx, hash, tracker.OrganizationID(organization), binding, previous, tokens))
}

type entryOptionalTime struct{ value *string }

func (v entryOptionalTime) Scan(source any) error {
	switch at := source.(type) {
	case nil:
		*v.value = ""
	case time.Time:
		*v.value = formatTime(at)
	case string:
		*v.value = at
	case []byte:
		*v.value = string(at)
	default:
		return errors.New("invalid stored timestamp")
	}
	return nil
}

var _ sql.Scanner = entryOptionalTime{}
