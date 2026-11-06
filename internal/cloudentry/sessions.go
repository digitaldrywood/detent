package cloudentry

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudentry/platformstore"
	"github.com/digitaldrywood/detent/internal/hubsecrets"
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
	keys  *hubsecrets.Keyring
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

func (a *authStore) sealToken(purpose, binding, token string) (string, error) {
	if token == "" {
		return "", nil
	}
	if a.keys != nil {
		envelope, err := a.keys.Seal([]byte(token), entrySecretAAD(purpose, binding))
		if err != nil {
			return "", err
		}
		return encodeEntrySecret(envelope), nil
	}
	nonce := make([]byte, a.seal.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.RawStdEncoding.EncodeToString(a.seal.Seal(nonce, nonce, []byte(token), []byte(binding))), nil
}

func (a *authStore) openToken(purpose, binding, sealed string) (string, error) {
	if strings.HasPrefix(sealed, "hk1.") {
		envelope, err := decodeEntrySecret(sealed)
		if err != nil {
			return "", errNoSession
		}
		plain, err := a.keys.Open(envelope, entrySecretAAD(purpose, binding))
		if err != nil {
			return "", errNoSession
		}
		return string(plain), nil
	}
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
	access, err := a.openToken("provider-access", item.Binding, item.sealedAccess)
	if err != nil {
		return auth.HostedTokens{}, err
	}
	refresh, err := a.openToken("provider-refresh", item.Binding, item.sealedRefresh)
	if err != nil {
		return auth.HostedTokens{}, err
	}
	return auth.HostedTokens{AccessToken: access, RefreshToken: refresh}, nil
}

func (a *authStore) storeTokens(ctx context.Context, binding string, tokens auth.HostedTokens, previous ...auth.HostedTokens) error {
	return a.postgresStoreTokens(ctx, binding, tokens, previous...)
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
	return a.store.db.postgres.CreateSession(ctx, platformstore.Session{Hash: hash, Subject: identity.Subject, Email: identity.Email, CSRFSecret: csrfSecret, Identity: *identity.Hosted})
}

func (a *authStore) session(ctx context.Context, hash string) (accountSession, error) {
	session, expiry, err := a.storedSession(ctx, hash)
	if err != nil || !expiry.After(a.now()) {
		return accountSession{}, errNoSession
	}
	return session, nil
}

func (a *authStore) storedSession(ctx context.Context, hash string) (accountSession, time.Time, error) {
	return a.postgresSession(ctx, hash)
}

func (a *authStore) authorize(ctx context.Context, session accountSession, organization string, identity auth.HostedIdentity, tokens auth.HostedTokens, effectiveEmail string) (_ authorization, _ []authorization, resultErr error) {
	return a.postgresAuthorize(ctx, session, organization, identity, tokens, effectiveEmail)
}

type queryer interface {
	QueryContext(context.Context, string, ...any) (entryRows, error)
}

func activeAuthorizations(ctx context.Context, query queryer, where string, args ...any) (_ []authorization, resultErr error) {
	return postgresAuthorizations(ctx, query, where, args...)
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

func (a *authStore) revokeSession(ctx context.Context, hash string) (_ []authorization, resultErr error) {
	return a.postgresRevokeSession(ctx, hash)
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
	return a.postgresLogin(ctx, hash, transaction)
}

func (a *authStore) consumeTransaction(ctx context.Context, hash, id string) (_ loginTransaction, resultErr error) {
	return a.postgresConsumeLogin(ctx, hash, id)
}

func (a *authStore) audit(ctx context.Context, subject, organization, event string) error {
	if event == "attachment_read" || event == "platform_opened" || event == "platform_users_searched" || strings.HasPrefix(event, "platform_") && strings.HasSuffix(event, "_viewed") {
		return nil
	}
	_, err := a.store.db.ExecContext(ctx, "INSERT INTO audit(subject,organization_id,event,recorded_at) VALUES (?,?,?,?)", subject, organization, event, formatTime(a.now()))
	return err
}
