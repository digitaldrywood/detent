package cloudentry

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
)

var errNoSession = errors.New("shared session is missing, expired or revoked")

// providerSessionRecheck bounds how long a read-only request trusts the last
// successful provider verification of a stored session, so polling screens do
// not call the provider on every read while revocation still lands within it.
const providerSessionRecheck = 60 * time.Second

type sessionVerifications struct {
	mu       sync.Mutex
	verified map[string]time.Time
}

func (v *sessionVerifications) fresh(hash string, now time.Time) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	at, ok := v.verified[hash]
	return ok && !now.Before(at) && now.Sub(at) < providerSessionRecheck
}

func (v *sessionVerifications) record(hash string, now time.Time) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.verified == nil {
		v.verified = make(map[string]time.Time)
	}
	for key, at := range v.verified {
		if now.Sub(at) >= providerSessionRecheck || now.Before(at) {
			delete(v.verified, key)
		}
	}
	v.verified[hash] = now
}

func (v *sessionVerifications) forget(hash string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.verified, hash)
}

type accountSession struct {
	Hash       string
	CSRFSecret string
	Subject    string
	Email      string
	Identity   auth.HostedIdentity
}

type authorization struct {
	Binding        string
	Organization   string
	Identity       auth.HostedIdentity
	Support        bool
	EffectiveEmail string

	sealedAccess  string
	sealedRefresh string
}

type authStore struct {
	store *store
	now   func() time.Time
	seal  cipher.AEAD
}

func newTokenSeal(signingKey ed25519.PrivateKey) (cipher.AEAD, error) {
	mac := hmac.New(sha256.New, signingKey.Seed())
	mac.Write([]byte("detent-entry-provider-token-seal"))
	block, err := aes.NewCipher(mac.Sum(nil))
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (a *authStore) sealToken(binding, token string) (string, error) {
	if token == "" {
		return "", nil
	}
	nonce := make([]byte, a.seal.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.RawStdEncoding.EncodeToString(a.seal.Seal(nonce, nonce, []byte(token), []byte(binding))), nil
}

func (a *authStore) openToken(binding, sealed string) (string, error) {
	raw, err := base64.RawStdEncoding.DecodeString(sealed)
	if err != nil || len(raw) <= a.seal.NonceSize() {
		return "", errNoSession
	}
	plain, err := a.seal.Open(nil, raw[:a.seal.NonceSize()], raw[a.seal.NonceSize():], []byte(binding))
	if err != nil {
		return "", errNoSession
	}
	return string(plain), nil
}

// tokens opens an authorization's provider tokens. An authorization stored
// before tokens were kept, or sealed under another key, has none.
func (a *authStore) tokens(item authorization) (auth.HostedTokens, error) {
	access, err := a.openToken(item.Binding, item.sealedAccess)
	if err != nil {
		return auth.HostedTokens{}, err
	}
	refresh, err := a.openToken(item.Binding, item.sealedRefresh)
	if err != nil {
		return auth.HostedTokens{}, err
	}
	return auth.HostedTokens{AccessToken: access, RefreshToken: refresh}, nil
}

func (a *authStore) storeTokens(ctx context.Context, binding string, tokens auth.HostedTokens) error {
	access, err := a.sealToken(binding, tokens.AccessToken)
	if err != nil {
		return err
	}
	refresh, err := a.sealToken(binding, tokens.RefreshToken)
	if err != nil {
		return err
	}
	result, err := a.store.db.ExecContext(ctx, "UPDATE authorizations SET access_token = ?, refresh_token = ? WHERE binding = ? AND revoked_at IS NULL", access, refresh, binding)
	if err != nil {
		return err
	}
	if rows, err := result.RowsAffected(); err != nil || rows != 1 {
		return errNoSession
	}
	return nil
}

func (a *authStore) authorizationByBinding(ctx context.Context, binding string) (authorization, error) {
	items, err := activeAuthorizations(ctx, a.store.db, "binding = ? AND revoked_at IS NULL", binding)
	if err != nil || len(items) != 1 {
		return authorization{}, errNoSession
	}
	return items[0], nil
}

// refreshLocks serializes provider token refreshes per authorization: a
// WorkOS refresh token is single use, so two concurrent refreshes of the
// same session would revoke it.
type refreshLocks struct {
	mu    sync.Mutex
	locks map[string]*refreshLock
}

type refreshLock struct {
	sync.Mutex
	users int
}

func (r *refreshLocks) lock(key string) func() {
	r.mu.Lock()
	if r.locks == nil {
		r.locks = make(map[string]*refreshLock)
	}
	entry := r.locks[key]
	if entry == nil {
		entry = &refreshLock{}
		r.locks[key] = entry
	}
	entry.users++
	r.mu.Unlock()
	entry.Lock()
	return func() {
		entry.Unlock()
		r.mu.Lock()
		entry.users--
		if entry.users == 0 {
			delete(r.locks, key)
		}
		r.mu.Unlock()
	}
}

func (a *authStore) createSession(ctx context.Context, hash, csrfSecret string, identity auth.Identity) error {
	encoded, err := json.Marshal(identity.Hosted)
	if err != nil {
		return err
	}
	_, err = a.store.db.ExecContext(ctx, "INSERT INTO sessions(token_hash,subject,email,csrf_secret,identity_json,created_at,expires_at) VALUES (?,?,?,?,?,?,?)",
		hash, identity.Subject, identity.Email, csrfSecret, string(encoded), formatTime(a.now()), formatTime(identity.Hosted.ExpiresAt))
	return err
}

func (a *authStore) session(ctx context.Context, hash string) (accountSession, error) {
	session, expiry, err := a.storedSession(ctx, hash)
	if err != nil || !expiry.After(a.now()) {
		return accountSession{}, errNoSession
	}
	return session, nil
}

func (a *authStore) storedSession(ctx context.Context, hash string) (accountSession, time.Time, error) {
	var session accountSession
	var encoded, expires string
	var revoked sql.NullString
	err := a.store.db.QueryRowContext(ctx, "SELECT token_hash,subject,email,csrf_secret,identity_json,expires_at,revoked_at FROM sessions WHERE token_hash = ?", hash).
		Scan(&session.Hash, &session.Subject, &session.Email, &session.CSRFSecret, &encoded, &expires, &revoked)
	if err != nil || revoked.Valid {
		return accountSession{}, time.Time{}, errNoSession
	}
	expiry, err := parseTime(expires)
	if err != nil || json.Unmarshal([]byte(encoded), &session.Identity) != nil || session.Identity.Subject != session.Subject {
		return accountSession{}, time.Time{}, errNoSession
	}
	return session, expiry, nil
}

func (a *authStore) authorize(ctx context.Context, session accountSession, organization string, identity auth.HostedIdentity, tokens auth.HostedTokens, effectiveEmail string) (authorization, []authorization, error) {
	encoded, err := json.Marshal(identity)
	if err != nil {
		return authorization{}, nil, err
	}
	result := authorization{Binding: cloudassert.AuthorizationBinding(session.Hash, organization, identity.SessionID), Organization: organization, Identity: identity, Support: identity.SupportActor != "", EffectiveEmail: effectiveEmail}
	if result.sealedAccess, err = a.sealToken(result.Binding, tokens.AccessToken); err != nil {
		return authorization{}, nil, err
	}
	if result.sealedRefresh, err = a.sealToken(result.Binding, tokens.RefreshToken); err != nil {
		return authorization{}, nil, err
	}
	tx, err := a.store.db.BeginTx(ctx, nil)
	if err != nil {
		return authorization{}, nil, err
	}
	defer tx.Rollback()
	replaced, err := activeAuthorizations(ctx, tx, "session_hash = ? AND organization_id = ? AND revoked_at IS NULL", session.Hash, organization)
	if err != nil {
		return authorization{}, nil, err
	}
	now := formatTime(a.now())
	if _, err := tx.ExecContext(ctx, "UPDATE authorizations SET revoked_at = ? WHERE session_hash = ? AND organization_id = ? AND revoked_at IS NULL", now, session.Hash, organization); err != nil {
		return authorization{}, nil, err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO authorizations(binding,session_hash,organization_id,identity_json,created_at,expires_at,support,effective_email,access_token,refresh_token) VALUES (?,?,?,?,?,?,?,?,?,?) ON CONFLICT(binding) DO UPDATE SET identity_json = excluded.identity_json, expires_at = excluded.expires_at, support = excluded.support, effective_email = excluded.effective_email, access_token = excluded.access_token, refresh_token = excluded.refresh_token, revoked_at = NULL",
		result.Binding, session.Hash, organization, string(encoded), now, formatTime(identity.ExpiresAt), result.Support, effectiveEmail, result.sealedAccess, result.sealedRefresh); err != nil {
		return authorization{}, nil, err
	}
	var stale []authorization
	for _, previous := range replaced {
		if previous.Binding != result.Binding {
			stale = append(stale, previous)
		}
	}
	return result, stale, tx.Commit()
}

type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func activeAuthorizations(ctx context.Context, query queryer, where string, args ...any) ([]authorization, error) {
	rows, err := query.QueryContext(ctx, "SELECT binding,organization_id,identity_json,expires_at,support,effective_email,access_token,refresh_token FROM authorizations WHERE "+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []authorization
	for rows.Next() {
		var item authorization
		var encoded, expires string
		if err := rows.Scan(&item.Binding, &item.Organization, &encoded, &expires, &item.Support, &item.EffectiveEmail, &item.sealedAccess, &item.sealedRefresh); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(encoded), &item.Identity); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (a *authStore) authorization(ctx context.Context, session accountSession, organization string) (authorization, error) {
	items, err := activeAuthorizations(ctx, a.store.db, "session_hash = ? AND organization_id = ? AND revoked_at IS NULL", session.Hash, organization)
	if err != nil || len(items) != 1 || !items[0].Identity.ExpiresAt.After(a.now()) {
		return authorization{}, errNoSession
	}
	item := items[0]
	if item.Support != (item.Identity.SupportActor != "") || item.Support && !strings.EqualFold(item.Identity.SupportActor, session.Email) || !item.Support && item.Identity.Subject != session.Subject {
		return authorization{}, errNoSession
	}
	return items[0], nil
}

func (a *authStore) revokeAuthorization(ctx context.Context, binding string) error {
	_, err := a.store.db.ExecContext(ctx, "UPDATE authorizations SET revoked_at = ? WHERE binding = ? AND revoked_at IS NULL", formatTime(a.now()), binding)
	return err
}

func (a *authStore) revokeSession(ctx context.Context, hash string) ([]authorization, error) {
	tx, err := a.store.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	items, err := activeAuthorizations(ctx, tx, "session_hash = ? AND revoked_at IS NULL", hash)
	if err != nil {
		return nil, err
	}
	now := formatTime(a.now())
	if _, err := tx.ExecContext(ctx, "UPDATE authorizations SET revoked_at = ? WHERE session_hash = ? AND revoked_at IS NULL", now, hash); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE sessions SET revoked_at = ? WHERE token_hash = ? AND revoked_at IS NULL", now, hash); err != nil {
		return nil, err
	}
	return items, tx.Commit()
}

type loginTransaction struct {
	SupportActor           string
	SupportSession         string
	SupportReason          string
	ID                     string
	State                  string
	Verifier               string
	Organization           string
	ReturnPath             string
	InvitationToken        string
	InvitationOrganization string
}

func (a *authStore) createTransaction(ctx context.Context, hash string, transaction loginTransaction) error {
	_, err := a.store.db.ExecContext(ctx, "INSERT INTO transactions(token_hash,transaction_id,state,verifier,organization_id,return_path,invitation_token,invitation_organization,support_actor,support_session,support_reason,expires_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
		hash, transaction.ID, transaction.State, transaction.Verifier, transaction.Organization, transaction.ReturnPath, transaction.InvitationToken, transaction.InvitationOrganization, transaction.SupportActor, transaction.SupportSession, transaction.SupportReason, formatTime(a.now().Add(10*time.Minute)))
	return err
}

func (a *authStore) consumeTransaction(ctx context.Context, hash, id string) (loginTransaction, error) {
	tx, err := a.store.db.BeginTx(ctx, nil)
	if err != nil {
		return loginTransaction{}, err
	}
	defer tx.Rollback()
	var result loginTransaction
	var expires string
	var consumed sql.NullString
	err = tx.QueryRowContext(ctx, "SELECT transaction_id,state,verifier,organization_id,return_path,invitation_token,invitation_organization,support_actor,support_session,expires_at,consumed_at FROM transactions WHERE token_hash = ? AND transaction_id = ?", hash, id).
		Scan(&result.ID, &result.State, &result.Verifier, &result.Organization, &result.ReturnPath, &result.InvitationToken, &result.InvitationOrganization, &result.SupportActor, &result.SupportSession, &expires, &consumed)
	if err != nil || consumed.Valid {
		return loginTransaction{}, errNoSession
	}
	expiry, err := parseTime(expires)
	if err != nil || !expiry.After(a.now()) {
		return loginTransaction{}, errNoSession
	}
	if _, err := tx.ExecContext(ctx, "UPDATE transactions SET consumed_at = ?, invitation_token = '' WHERE token_hash = ?", formatTime(a.now()), hash); err != nil {
		return loginTransaction{}, err
	}
	return result, tx.Commit()
}

func (a *authStore) audit(ctx context.Context, subject, organization, event string) error {
	if event == "attachment_read" || event == "platform_opened" || event == "platform_users_searched" || strings.HasPrefix(event, "platform_") && strings.HasSuffix(event, "_viewed") {
		return nil
	}
	_, err := a.store.db.ExecContext(ctx, "INSERT INTO audit(subject,organization_id,event,recorded_at) VALUES (?,?,?,?)", subject, organization, event, formatTime(a.now()))
	return err
}
