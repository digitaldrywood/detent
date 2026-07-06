package apikey

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/digitaldrywood/detent/internal/store"
)

const (
	DefaultExpiresIn = "90d"
	DefaultMaxKeys   = 25
	StaticKeyID      = "static"
)

var (
	ErrNameRequired     = errors.New("name is required")
	ErrInvalidExpiry    = errors.New("invalid expiry")
	ErrInvalidGrace     = errors.New("invalid grace")
	ErrKeyLimitExceeded = errors.New("api key limit exceeded")
	ErrKeyRevoked       = errors.New("api key is revoked")
	ErrKeyExpired       = errors.New("api key is expired")
)

type Store interface {
	store.APIKeyStore
}

type Service struct {
	store         Store
	now           func() time.Time
	generateToken func() (string, error)
	maxActiveKeys int
}

type Option func(*Service)

type CreateRequest struct {
	Name       string
	Scopes     []string
	ProjectIDs []string
	ExpiresIn  string
}

type CreatedKey struct {
	Key   store.APIKey `json:"key"`
	Token string       `json:"token"`
}

type Credential struct {
	ID          string
	Name        string
	PrefixLast4 string
	Scopes      []string
	ProjectIDs  []string
	Static      bool
}

type AuthError struct {
	Code    string
	Message string
}

func (e *AuthError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

func NewService(store Store, opts ...Option) *Service {
	service := &Service{
		store:         store,
		now:           time.Now,
		generateToken: GenerateToken,
		maxActiveKeys: DefaultMaxKeys,
	}
	for _, opt := range opts {
		opt(service)
	}
	if service.maxActiveKeys <= 0 {
		service.maxActiveKeys = DefaultMaxKeys
	}
	return service
}

func WithNow(now func() time.Time) Option {
	return func(service *Service) {
		if now != nil {
			service.now = now
		}
	}
}

func WithTokenGenerator(generate func() (string, error)) Option {
	return func(service *Service) {
		if generate != nil {
			service.generateToken = generate
		}
	}
}

func WithMaxActiveKeys(maxKeys int) Option {
	return func(service *Service) {
		service.maxActiveKeys = maxKeys
	}
}

func (s *Service) Create(ctx context.Context, req CreateRequest) (CreatedKey, error) {
	expiresAt, err := ExpiresAt(req.ExpiresIn, s.currentTime())
	if err != nil {
		return CreatedKey{}, err
	}
	return s.create(ctx, req.Name, req.Scopes, req.ProjectIDs, expiresAt)
}

func (s *Service) Rotate(ctx context.Context, id string, grace string) (CreatedKey, error) {
	key, err := s.store.APIKey(ctx, strings.TrimSpace(id))
	if err != nil {
		return CreatedKey{}, err
	}
	if key.RevokedAt != nil {
		return CreatedKey{}, ErrKeyRevoked
	}
	now := s.currentTime()
	if key.ExpiresAt != nil && !key.ExpiresAt.After(now) {
		return CreatedKey{}, ErrKeyExpired
	}
	graceDuration, err := GraceDuration(grace)
	if err != nil {
		return CreatedKey{}, err
	}
	created, err := s.createWithExpiry(ctx, key.Name+" rotated", key.Scopes, key.ProjectIDs, key.ExpiresAt)
	if err != nil {
		return CreatedKey{}, err
	}
	graceExpiresAt := now.Add(graceDuration)
	if key.ExpiresAt != nil && key.ExpiresAt.Before(graceExpiresAt) {
		graceExpiresAt = *key.ExpiresAt
	}
	if err := s.store.SetAPIKeyExpiresAt(ctx, key.ID, graceExpiresAt); err != nil {
		return CreatedKey{}, err
	}
	return created, nil
}

func (s *Service) Revoke(ctx context.Context, id string) error {
	return s.store.RevokeAPIKey(ctx, strings.TrimSpace(id), s.currentTime())
}

func (s *Service) Authenticate(ctx context.Context, token string, staticToken string) (Credential, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return Credential{}, &AuthError{Code: "unauthorized", Message: "Valid API token is required"}
	}
	if constantTimeTokenEqual(token, staticToken) {
		return StaticCredential(), nil
	}
	if err := ValidateTokenFormat(token); err != nil {
		return Credential{}, &AuthError{Code: "invalid_token", Message: "Invalid API token"}
	}

	hash := HashToken(token)
	key, err := s.store.APIKeyByHash(ctx, hash)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return Credential{}, &AuthError{Code: "unauthorized", Message: "Valid API token is required"}
		}
		return Credential{}, fmt.Errorf("lookup api key: %w", err)
	}
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(key.KeyHash)), []byte(hash)) != 1 {
		return Credential{}, &AuthError{Code: "unauthorized", Message: "Valid API token is required"}
	}
	if key.RevokedAt != nil {
		return Credential{}, &AuthError{Code: "token_revoked", Message: "API token has been revoked"}
	}
	if key.ExpiresAt != nil && !key.ExpiresAt.After(s.currentTime()) {
		return Credential{}, &AuthError{Code: "token_expired", Message: "API token has expired"}
	}
	return Credential{
		ID:          key.ID,
		Name:        key.Name,
		PrefixLast4: key.PrefixLast4,
		Scopes:      append([]string(nil), key.Scopes...),
		ProjectIDs:  append([]string(nil), key.ProjectIDs...),
	}, nil
}

func StaticCredential() Credential {
	return Credential{
		ID:          StaticKeyID,
		Name:        "Static API token",
		PrefixLast4: StaticKeyID,
		Scopes:      []string{string(ScopeAdmin)},
		Static:      true,
	}
}

func ExpiresAt(choice string, now time.Time) (*time.Time, error) {
	choice = strings.ToLower(strings.TrimSpace(choice))
	if choice == "" {
		choice = DefaultExpiresIn
	}
	if choice == "never" {
		return nil, nil //nolint:nilnil // nil expiry means the key never expires.
	}
	var days int
	switch choice {
	case "30d", "30":
		days = 30
	case "90d", "90":
		days = 90
	case "365d", "365":
		days = 365
	default:
		return nil, ErrInvalidExpiry
	}
	expiresAt := now.UTC().Truncate(time.Second).Add(time.Duration(days) * 24 * time.Hour)
	return &expiresAt, nil
}

func GraceDuration(choice string) (time.Duration, error) {
	switch strings.ToLower(strings.TrimSpace(choice)) {
	case "", "24h":
		return 24 * time.Hour, nil
	case "1h":
		return time.Hour, nil
	case "7d":
		return 7 * 24 * time.Hour, nil
	default:
		return 0, ErrInvalidGrace
	}
}

func (s *Service) create(ctx context.Context, name string, scopes []string, projectIDs []string, expiresAt *time.Time) (CreatedKey, error) {
	return s.createWithExpiry(ctx, name, scopes, projectIDs, expiresAt)
}

func (s *Service) createWithExpiry(ctx context.Context, name string, scopes []string, projectIDs []string, expiresAt *time.Time) (CreatedKey, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return CreatedKey{}, ErrNameRequired
	}
	normalizedScopes, err := NormalizeScopes(scopes)
	if err != nil {
		return CreatedKey{}, err
	}
	count, err := s.store.CountActiveAPIKeys(ctx)
	if err != nil {
		return CreatedKey{}, err
	}
	if count >= int64(s.maxActiveKeys) {
		return CreatedKey{}, ErrKeyLimitExceeded
	}
	token, err := s.generateToken()
	if err != nil {
		return CreatedKey{}, err
	}
	key, err := s.store.CreateAPIKey(ctx, store.APIKeyCreate{
		ID:          uuid.NewString(),
		Name:        name,
		PrefixLast4: PrefixLast4(token),
		KeyHash:     HashToken(token),
		Scopes:      normalizedScopes,
		ProjectIDs:  NormalizeProjectIDs(projectIDs),
		CreatedAt:   s.currentTime(),
		ExpiresAt:   expiresAt,
	})
	if err != nil {
		return CreatedKey{}, err
	}
	return CreatedKey{Key: key, Token: token}, nil
}

func (s *Service) currentTime() time.Time {
	return s.now().UTC().Truncate(time.Second)
}

func constantTimeTokenEqual(candidate string, token string) bool {
	candidate = strings.TrimSpace(candidate)
	token = strings.TrimSpace(token)
	if candidate == "" || token == "" {
		return false
	}
	candidateHash := sha256.Sum256([]byte(candidate))
	tokenHash := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(candidateHash[:], tokenHash[:]) == 1
}
