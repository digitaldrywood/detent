package cloudassert

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	Header         = "Detent-Entry-Assertion"
	MaxLifetime    = 30 * time.Second
	MaxBodyBytes   = 2 << 20
	KindBrowser    = "browser"
	KindMachine    = "machine"
	KindService    = "service"
	maxReplayCache = 1 << 16
	maxHeaderBytes = 8 << 10
	clockSkew      = 5 * time.Second
)

var ErrInvalid = errors.New("entry assertion is missing, invalid, expired or replayed")

type Claims struct {
	Issuer               string    `json:"iss"`
	Audience             string    `json:"aud"`
	Generation           int64     `json:"gen"`
	Kind                 string    `json:"kind"`
	Subject              string    `json:"sub,omitempty"`
	Email                string    `json:"email,omitempty"`
	ProviderOrganization string    `json:"provider_org,omitempty"`
	ProviderSession      string    `json:"provider_session,omitempty"`
	SessionCreatedAt     time.Time `json:"session_created_at,omitzero"`
	SessionExpiresAt     time.Time `json:"session_expires_at,omitzero"`
	SupportActor         string    `json:"support_actor,omitempty"`
	SupportReason        string    `json:"support_reason,omitempty"`
	Binding              string    `json:"binding,omitempty"`
	CSRF                 string    `json:"csrf,omitempty"`
	Method               string    `json:"method"`
	Path                 string    `json:"path"`
	BodyDigest           string    `json:"body"`
	IssuedAt             time.Time `json:"iat"`
	ExpiresAt            time.Time `json:"exp"`
	ID                   string    `json:"jti"`
}

func BodyDigest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func NewID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func Sign(key ed25519.PrivateKey, claims Claims) (string, error) {
	if len(key) != ed25519.PrivateKeySize || claims.ID == "" || !claims.ExpiresAt.After(claims.IssuedAt) || claims.ExpiresAt.Sub(claims.IssuedAt) > MaxLifetime {
		return "", ErrInvalid
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signature := ed25519.Sign(key, payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

type Verifier struct {
	PublicKeys []ed25519.PublicKey
	Issuer     string
	Audience   string
	Generation int64
	Now        func() time.Time

	mu   sync.Mutex
	seen map[string]time.Time
}

func (v *Verifier) Verify(value, method, path string, body []byte) (Claims, error) {
	if v == nil || len(value) == 0 || len(value) > maxHeaderBytes {
		return Claims{}, ErrInvalid
	}
	encodedPayload, encodedSignature, ok := strings.Cut(value, ".")
	if !ok {
		return Claims{}, ErrInvalid
	}
	payload, err := base64.RawURLEncoding.DecodeString(encodedPayload)
	if err != nil {
		return Claims{}, ErrInvalid
	}
	signature, err := base64.RawURLEncoding.DecodeString(encodedSignature)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return Claims{}, ErrInvalid
	}
	verified := false
	for _, key := range v.PublicKeys {
		if len(key) == ed25519.PublicKeySize && ed25519.Verify(key, payload, signature) {
			verified = true
			break
		}
	}
	if !verified {
		return Claims{}, ErrInvalid
	}
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	var claims Claims
	if err := decoder.Decode(&claims); err != nil {
		return Claims{}, ErrInvalid
	}
	now := time.Now()
	if v.Now != nil {
		now = v.Now()
	}
	if claims.Issuer != v.Issuer || claims.Audience != v.Audience || claims.Generation != v.Generation || claims.Method != method || claims.Path != path || claims.ID == "" || len(claims.ID) > 64 {
		return Claims{}, ErrInvalid
	}
	if subtle.ConstantTimeCompare([]byte(claims.BodyDigest), []byte(BodyDigest(body))) != 1 {
		return Claims{}, ErrInvalid
	}
	if !claims.ExpiresAt.After(claims.IssuedAt) || claims.ExpiresAt.Sub(claims.IssuedAt) > MaxLifetime || claims.IssuedAt.After(now.Add(clockSkew)) || !claims.ExpiresAt.After(now) {
		return Claims{}, ErrInvalid
	}
	if !validKind(claims) {
		return Claims{}, ErrInvalid
	}
	if !v.remember(claims.ID, claims.ExpiresAt, now) {
		return Claims{}, ErrInvalid
	}
	return claims, nil
}

func validKind(claims Claims) bool {
	browser := claims.Subject != "" || claims.Email != "" || claims.ProviderOrganization != "" || claims.ProviderSession != "" || claims.Binding != "" || claims.CSRF != "" || claims.SupportActor != "" || claims.SupportReason != "" || !claims.SessionCreatedAt.IsZero() || !claims.SessionExpiresAt.IsZero()
	switch claims.Kind {
	case KindBrowser:
		return claims.Subject != "" && claims.Email != "" && claims.ProviderOrganization != "" && claims.ProviderSession != "" && len(claims.Binding) == 64 && len(claims.CSRF) == 64 && !claims.SessionCreatedAt.IsZero() && claims.SessionExpiresAt.After(claims.SessionCreatedAt)
	case KindMachine:
		return !browser
	case KindService:
		return !browser || claims.Subject != "" && claims.Email != "" && claims.ProviderOrganization == "" && claims.Binding == "" && claims.CSRF == "" && claims.SupportActor == ""
	default:
		return false
	}
}

func (v *Verifier) remember(id string, expires, now time.Time) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.seen == nil {
		v.seen = make(map[string]time.Time)
	}
	if _, ok := v.seen[id]; ok {
		return false
	}
	if len(v.seen) >= maxReplayCache {
		for key, expiry := range v.seen {
			if !expiry.After(now) {
				delete(v.seen, key)
			}
		}
		if len(v.seen) >= maxReplayCache {
			return false
		}
	}
	v.seen[id] = expires.Add(clockSkew)
	return true
}

func ParsePrivateKey(value string) (ed25519.PrivateKey, error) {
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errors.New("entry assertion key must be a base64 Ed25519 seed")
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

func ParsePublicKey(value string) (ed25519.PublicKey, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil || len(key) != ed25519.PublicKeySize {
		return nil, errors.New("entry assertion public key must be a base64 Ed25519 public key")
	}
	return ed25519.PublicKey(key), nil
}

func EncodePublicKey(key ed25519.PublicKey) string {
	return base64.StdEncoding.EncodeToString(key)
}

func CSRFToken(sessionHash, organization string) string {
	sum := sha256.Sum256([]byte("detent-shared-csrf:" + sessionHash + ":" + organization))
	return hex.EncodeToString(sum[:])
}

func AuthorizationBinding(sessionHash, organization, providerSession string) string {
	sum := sha256.Sum256([]byte("detent-shared-authorization:" + sessionHash + ":" + organization + ":" + providerSession))
	return hex.EncodeToString(sum[:])
}

func CanonicalPath(u *url.URL) bool {
	if u == nil || u.RawPath != "" || u.Opaque != "" || !strings.HasPrefix(u.Path, "/") || strings.ContainsAny(u.Path, "\\\x00%") || strings.Contains(u.Path, "//") {
		return false
	}
	for segment := range strings.SplitSeq(strings.TrimPrefix(u.Path, "/"), "/") {
		if segment == "." || segment == ".." {
			return false
		}
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return false
	}
	for _, values := range query {
		if len(values) > 1 {
			return false
		}
	}
	return true
}
