package cloudassert

import (
	"container/heap"
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

	"github.com/digitaldrywood/detent/internal/apikey"
)

const (
	Header         = "Detent-Entry-Assertion"
	MaxLifetime    = 30 * time.Second
	MaxBodyBytes   = 2 << 20
	KindBrowser    = "browser"
	KindMachine    = "machine"
	KindService    = "service"
	KindKey        = "key"
	maxReplayCache = 1 << 16
	maxHeaderBytes = 8 << 10
	clockSkew      = 5 * time.Second
)

var ErrInvalid = errors.New("entry assertion is missing, invalid, expired or replayed")

type Claims struct {
	Key                  *apikey.KeyAuthority `json:"key,omitempty"`
	Issuer               string               `json:"iss"`
	Audience             string               `json:"aud"`
	OrganizationSlug     string               `json:"organization_slug,omitempty"`
	Generation           int64                `json:"gen"`
	Kind                 string               `json:"kind"`
	Background           bool                 `json:"background,omitempty"`
	Subject              string               `json:"sub,omitempty"`
	Email                string               `json:"email,omitempty"`
	ProviderOrganization string               `json:"provider_org,omitempty"`
	ProviderSession      string               `json:"provider_session,omitempty"`
	SessionCreatedAt     time.Time            `json:"session_created_at,omitzero"`
	SessionExpiresAt     time.Time            `json:"session_expires_at,omitzero"`
	SupportActor         string               `json:"support_actor,omitempty"`
	SupportReason        string               `json:"support_reason,omitempty"`
	PlatformRole         string               `json:"platform_role,omitempty"`
	Role                 string               `json:"role,omitempty"`
	AccessExpiresAt      time.Time            `json:"access_expires_at,omitzero"`
	Binding              string               `json:"binding,omitempty"`
	CSRF                 string               `json:"csrf,omitempty"`
	Method               string               `json:"method"`
	Path                 string               `json:"path"`
	BodyDigest           string               `json:"body"`
	IssuedAt             time.Time            `json:"iat"`
	ExpiresAt            time.Time            `json:"exp"`
	ID                   string               `json:"jti"`
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

	Replays *ReplayCache
	local   ReplayCache
}

type ReplayCache struct {
	mu      sync.Mutex
	seen    map[replayID]struct{}
	count   map[string]int
	expires replayExpirations
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
	cache := v.Replays
	audienceLimit := maxReplayCache / 2
	if cache == nil {
		cache = &v.local
		audienceLimit = maxReplayCache
	}
	if !cache.remember(claims.Audience, claims.ID, claims.ExpiresAt, now, audienceLimit) {
		return Claims{}, ErrInvalid
	}
	return claims, nil
}

func validKind(claims Claims) bool {
	if claims.Background && claims.Kind != KindService {
		return false
	}
	if claims.Key != nil {
		return claims.Kind == KindKey && claims.Key.Valid() && claims.Subject != "" && claims.ProviderOrganization != "" && validRole(claims.Role) && claims.SupportActor == "" && claims.PlatformRole == "" && claims.Binding == "" && claims.CSRF == "" && claims.ProviderSession == "" && claims.SupportReason == "" && claims.SessionCreatedAt.IsZero() && claims.SessionExpiresAt.IsZero() && claims.AccessExpiresAt.IsZero()
	}
	browser := claims.PlatformRole != "" || claims.Subject != "" || claims.Email != "" || claims.ProviderOrganization != "" || claims.ProviderSession != "" || claims.Binding != "" || claims.CSRF != "" || claims.SupportActor != "" || claims.SupportReason != "" || !claims.SessionCreatedAt.IsZero() || !claims.SessionExpiresAt.IsZero() || claims.Role != "" || !claims.AccessExpiresAt.IsZero()
	if !ValidPlatformRole(claims.PlatformRole) || claims.PlatformRole != "" && (claims.Kind != KindBrowser || claims.SupportActor != "") {
		return false
	}
	switch claims.Kind {
	case KindBrowser:
		return claims.Subject != "" && claims.Email != "" && claims.ProviderOrganization != "" && claims.ProviderSession != "" && len(claims.Binding) == 64 && len(claims.CSRF) == 64 && !claims.SessionCreatedAt.IsZero() && claims.SessionExpiresAt.After(claims.SessionCreatedAt) && validRole(claims.Role) && !claims.AccessExpiresAt.IsZero()
	case KindMachine:
		return !browser
	case KindService:
		return !browser || claims.Subject != "" && claims.Email != "" && claims.ProviderOrganization == "" && claims.Binding == "" && claims.CSRF == "" && claims.SupportActor == ""
	default:
		return false
	}
}

func validRole(role string) bool {
	switch role {
	case "owner", "admin", "member", "reporter", "viewer":
		return true
	default:
		return false
	}
}

type replayID struct{ audience, id string }
type replayExpiration struct {
	key replayID
	at  time.Time
}
type replayExpirations []replayExpiration

func (h replayExpirations) Len() int           { return len(h) }
func (h replayExpirations) Less(i, j int) bool { return h[i].at.Before(h[j].at) }
func (h replayExpirations) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *replayExpirations) Push(x any)        { *h = append(*h, x.(replayExpiration)) }
func (h *replayExpirations) Pop() any {
	last := len(*h) - 1
	v := (*h)[last]
	(*h)[last] = replayExpiration{}
	*h = (*h)[:last]
	return v
}

func (v *ReplayCache) remember(audience, id string, expires, now time.Time, audienceLimit int) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.seen == nil {
		v.seen = make(map[replayID]struct{})
		v.count = make(map[string]int)
	}
	for len(v.expires) > 0 && !v.expires[0].at.After(now) {
		key := heap.Pop(&v.expires).(replayExpiration).key
		delete(v.seen, key)
		v.count[key.audience]--
		if v.count[key.audience] == 0 {
			delete(v.count, key.audience)
		}
	}
	key := replayID{audience, id}
	if _, seen := v.seen[key]; seen || len(v.seen) >= maxReplayCache || v.count[audience] >= audienceLimit {
		return false
	}
	v.seen[key] = struct{}{}
	v.count[audience]++
	heap.Push(&v.expires, replayExpiration{key, expires.Add(clockSkew)})
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
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	for _, segment := range parts {
		if segment == "." || segment == ".." {
			return false
		}
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return false
	}
	workBoard := len(parts) == 5 && parts[0] == "api" && parts[1] == "v2" && parts[2] == "organizations" && parts[4] == "work-board"
	for key, values := range query {
		if len(values) > 1 && !repeatableQueryKeys[key] && (!workBoard || key != "project" && key != "item") {
			return false
		}
	}
	return true
}

var repeatableQueryKeys = map[string]bool{"state": true, "label": true, "assignee": true, "priority": true}

func PublicKeyOf(key ed25519.PrivateKey) string {
	if len(key) != ed25519.PrivateKeySize {
		return ""
	}
	return EncodePublicKey(ed25519.PublicKey(key[ed25519.SeedSize:]))
}

func ValidPlatformRole(role string) bool {
	switch role {
	case "", "admin", "support", "billing", "viewer":
		return true
	default:
		return false
	}
}
