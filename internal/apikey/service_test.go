package apikey

import (
	"context"
	"errors"
	"path/filepath"
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
	backend, err := store.Open(ctx, store.Config{
		Backend: store.BackendSQLite,
		Path:    filepath.Join(t.TempDir(), "detent.db"),
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})

	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	service := NewService(backend, WithNow(func() time.Time { return now }))
	created, err := service.Create(ctx, CreateRequest{
		Name:       "Video Studio",
		Scopes:     []string{"write"},
		ProjectIDs: []string{"digitaldrywood-video"},
		ExpiresIn:  "90d",
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	rotated, err := service.Rotate(ctx, created.Key.ID, "1h")
	if err != nil {
		t.Fatalf("Rotate() error = %v", err)
	}

	now = now.Add(59 * time.Minute)
	if _, err := service.Authenticate(ctx, created.Token, ""); err != nil {
		t.Fatalf("old key before grace expiry Authenticate() error = %v", err)
	}
	if _, err := service.Authenticate(ctx, rotated.Token, ""); err != nil {
		t.Fatalf("new key Authenticate() error = %v", err)
	}

	now = now.Add(2 * time.Minute)
	_, err = service.Authenticate(ctx, created.Token, "")
	var authErr *AuthError
	if !errors.As(err, &authErr) || authErr.Code != "token_expired" {
		t.Fatalf("old key after grace Authenticate() error = %v, want token_expired", err)
	}
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
