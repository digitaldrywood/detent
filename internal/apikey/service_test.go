package apikey

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/store"
)

func TestGenerateTokenFormatAndChecksum(t *testing.T) {
	t.Parallel()

	token, err := GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken() error = %v", err)
	}
	if len(token) != len(TokenPrefix)+43+6 {
		t.Fatalf("token length = %d, want %d", len(token), len(TokenPrefix)+43+6)
	}
	if err := ValidateTokenFormat(token); err != nil {
		t.Fatalf("ValidateTokenFormat(valid) error = %v", err)
	}
	if strings.Contains(strings.TrimPrefix(token, TokenPrefix), "_") {
		t.Fatalf("token body contains underscore: %q", token)
	}

	replacement := "0"
	if strings.HasSuffix(token, replacement) {
		replacement = "1"
	}
	tampered := token[:len(token)-1] + replacement
	if err := ValidateTokenFormat(tampered); err == nil {
		t.Fatalf("ValidateTokenFormat(tampered) error = nil, want checksum failure")
	}
}

func TestScopeHierarchyAndProjects(t *testing.T) {
	t.Parallel()

	if !HasScope([]string{"admin"}, ScopeRead) || !HasScope([]string{"admin"}, ScopeWrite) || !HasScope([]string{"admin"}, ScopeAdmin) {
		t.Fatalf("admin should satisfy read, write, and admin")
	}
	if !HasScope([]string{"write"}, ScopeRead) || !HasScope([]string{"write"}, ScopeWrite) || HasScope([]string{"write"}, ScopeAdmin) {
		t.Fatalf("write hierarchy mismatch")
	}
	if !AllowsProject(nil, "digitaldrywood-video") || !AllowsProject([]string{"digitaldrywood-video"}, "digitaldrywood-video") {
		t.Fatalf("project allowlist rejected an allowed project")
	}
	if AllowsProject([]string{"digitaldrywood-video"}, "detent") {
		t.Fatalf("project allowlist accepted a disallowed project")
	}
}

func TestLoopbackPeerReadCredential(t *testing.T) {
	t.Parallel()

	credential := LoopbackPeerReadCredential()
	if credential.ID != LoopbackPeerReadKeyID || !credential.Static {
		t.Fatalf("LoopbackPeerReadCredential() = %#v, want static credential with ID %q", credential, LoopbackPeerReadKeyID)
	}
	if !HasScope(credential.Scopes, ScopeRead) || HasScope(credential.Scopes, ScopeWrite) || HasScope(credential.Scopes, ScopeAdmin) {
		t.Fatalf("LoopbackPeerReadCredential() scopes = %v, want read only", credential.Scopes)
	}
}

func TestAuthenticateRejectsChecksumInvalidTokenWithoutLookup(t *testing.T) {
	t.Parallel()

	token, err := GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken() error = %v", err)
	}
	replacement := "0"
	if strings.HasSuffix(token, replacement) {
		replacement = "1"
	}
	badToken := token[:len(token)-1] + replacement
	store := &countingAPIKeyStore{}

	_, err = NewService(store).Authenticate(context.Background(), badToken, "")
	if err == nil {
		t.Fatal("Authenticate() error = nil, want invalid token")
	}
	var authErr *AuthError
	if !errors.As(err, &authErr) || authErr.Code != "invalid_token" {
		t.Fatalf("Authenticate() error = %v, want invalid_token", err)
	}
	if store.hashLookups != 0 {
		t.Fatalf("hash lookups = %d, want 0", store.hashLookups)
	}
}

func TestRotateKeepsOldKeyValidUntilGraceExpires(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	backend := openAPIKeyTestStore(t)
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	service := NewService(backend, WithNow(func() time.Time { return now }))
	created, err := service.Create(ctx, CreateRequest{
		Name:       "Example key",
		Scopes:     []string{"write"},
		ProjectIDs: []string{"example-project"},
		ExpiresIn:  "90d",
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	rotated, err := service.Rotate(ctx, created.Key.ID, "1h")
	if err != nil {
		t.Fatalf("Rotate() error = %v", err)
	}
	if rotated.Key.ID == created.Key.ID || rotated.Token == created.Token {
		t.Fatal("Rotate() reused the old key identity or token")
	}
	if !slices.Equal(rotated.Key.Scopes, []string{"write"}) || !slices.Equal(rotated.Key.ProjectIDs, []string{"example-project"}) {
		t.Fatalf("rotated permissions = %v, %v, want write for example-project", rotated.Key.Scopes, rotated.Key.ProjectIDs)
	}
	if rotated.Key.ExpiresAt == nil || !rotated.Key.ExpiresAt.Equal(*created.Key.ExpiresAt) {
		t.Fatalf("rotated expiry = %v, want %v", rotated.Key.ExpiresAt, created.Key.ExpiresAt)
	}

	now = now.Add(59 * time.Minute)
	for _, key := range []CreatedKey{created, rotated} {
		credential, err := service.Authenticate(ctx, key.Token, "")
		if err != nil || credential.ID != key.Key.ID {
			t.Fatalf("key %s before grace expiry Authenticate() = %v, error = %v", key.Key.ID, credential, err)
		}
	}

	now = now.Add(2 * time.Minute)
	_, err = service.Authenticate(ctx, created.Token, "")
	var authErr *AuthError
	if !errors.As(err, &authErr) || authErr.Code != "token_expired" {
		t.Fatalf("old key after grace Authenticate() error = %v, want token_expired", err)
	}
}

func TestRotateDoesNotExtendOldKeyBeyondOriginalExpiry(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	backend := openAPIKeyTestStore(t)
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	service := NewService(backend, WithNow(func() time.Time { return now }))
	created, err := service.Create(ctx, CreateRequest{
		Name:      "Near expiry",
		Scopes:    []string{"read"},
		ExpiresIn: "30d",
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	originalExpiresAt := *created.Key.ExpiresAt

	now = originalExpiresAt.Add(-30 * time.Minute)
	if _, err := service.Rotate(ctx, created.Key.ID, "1h"); err != nil {
		t.Fatalf("Rotate() error = %v", err)
	}
	key, err := backend.APIKey(ctx, created.Key.ID)
	if err != nil {
		t.Fatalf("APIKey() error = %v", err)
	}
	if key.ExpiresAt == nil || !key.ExpiresAt.Equal(originalExpiresAt) {
		t.Fatalf("old key expires_at = %v, want %v", key.ExpiresAt, originalExpiresAt)
	}
}

func TestRotateRejectsExpiredKey(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	backend := openAPIKeyTestStore(t)
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	service := NewService(backend, WithNow(func() time.Time { return now }))
	created, err := service.Create(ctx, CreateRequest{
		Name:      "Expired",
		Scopes:    []string{"read"},
		ExpiresIn: "30d",
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	now = created.Key.ExpiresAt.Add(time.Second)
	_, err = service.Rotate(ctx, created.Key.ID, "1h")
	if !errors.Is(err, ErrKeyExpired) {
		t.Fatalf("Rotate() error = %v, want ErrKeyExpired", err)
	}
}

func openAPIKeyTestStore(t *testing.T) *rotationKeyStore {
	t.Helper()
	return &rotationKeyStore{keys: make(map[string]store.APIKey)}
}

type rotationKeyStore struct {
	countingAPIKeyStore
	keys map[string]store.APIKey
}

func (s *rotationKeyStore) CreateAPIKey(_ context.Context, row store.APIKeyCreate) (store.APIKey, error) {
	if _, exists := s.keys[row.ID]; exists {
		return store.APIKey{}, errors.New("duplicate key identity")
	}
	key := store.APIKey{ID: row.ID, Name: row.Name, PrefixLast4: row.PrefixLast4, KeyHash: row.KeyHash, Scopes: slices.Clone(row.Scopes), ProjectIDs: slices.Clone(row.ProjectIDs), CreatedAt: row.CreatedAt, ExpiresAt: row.ExpiresAt}
	s.keys[key.ID] = key
	return key, nil
}

func (s *rotationKeyStore) APIKey(_ context.Context, id string) (store.APIKey, error) {
	key, exists := s.keys[id]
	if !exists {
		return store.APIKey{}, store.ErrNotFound
	}
	return key, nil
}

func (s *rotationKeyStore) APIKeyByHash(ctx context.Context, hash string) (store.APIKey, error) {
	for _, key := range s.keys {
		if key.KeyHash == hash {
			return s.APIKey(ctx, key.ID)
		}
	}
	return store.APIKey{}, store.ErrNotFound
}

func (s *rotationKeyStore) CountActiveAPIKeys(context.Context) (int64, error) {
	return int64(len(s.keys)), nil
}

func (s *rotationKeyStore) SetAPIKeyExpiresAt(ctx context.Context, id string, at time.Time) error {
	key, err := s.APIKey(ctx, id)
	if err != nil {
		return err
	}
	key.ExpiresAt = &at
	s.keys[id] = key
	return nil
}

type countingAPIKeyStore struct {
	hashLookups int
}

func (s *countingAPIKeyStore) CreateAPIKey(context.Context, store.APIKeyCreate) (store.APIKey, error) {
	return store.APIKey{}, errors.New("not implemented")
}

func (s *countingAPIKeyStore) APIKey(context.Context, string) (store.APIKey, error) {
	return store.APIKey{}, errors.New("not implemented")
}

func (s *countingAPIKeyStore) APIKeyByHash(context.Context, string) (store.APIKey, error) {
	s.hashLookups++
	return store.APIKey{}, store.ErrNotFound
}

func (s *countingAPIKeyStore) ListAPIKeys(context.Context) ([]store.APIKey, error) {
	return nil, errors.New("not implemented")
}

func (s *countingAPIKeyStore) CountActiveAPIKeys(context.Context) (int64, error) {
	return 0, nil
}

func (s *countingAPIKeyStore) SetAPIKeyExpiresAt(context.Context, string, time.Time) error {
	return errors.New("not implemented")
}

func (s *countingAPIKeyStore) RevokeAPIKey(context.Context, string, time.Time) error {
	return errors.New("not implemented")
}

func (s *countingAPIKeyStore) MarkAPIKeyLastUsed(context.Context, string, time.Time) error {
	return nil
}

func (s *countingAPIKeyStore) RecordAPIUsageLog(context.Context, store.APIUsageLog) error {
	return nil
}

func (s *countingAPIKeyStore) CountAPIUsageLogsByKey(context.Context, string) (int64, error) {
	return 0, nil
}
