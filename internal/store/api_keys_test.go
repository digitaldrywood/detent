package store

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestAPIKeysPersistLifecycleAndUsage(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	backend, err := Open(ctx, Config{
		Backend: BackendSQLite,
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

	createdAt := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	expiresAt := createdAt.Add(90 * 24 * time.Hour)
	key, err := backend.CreateAPIKey(ctx, APIKeyCreate{
		ID:          "key-1",
		Name:        "Video Studio",
		PrefixLast4: "detent_ab...9xzq",
		KeyHash:     "hash-1",
		Scopes:      []string{"write", "write"},
		ProjectIDs:  []string{"digitaldrywood-video", "digitaldrywood-video"},
		CreatedAt:   createdAt,
		ExpiresAt:   &expiresAt,
	})
	if err != nil {
		t.Fatalf("CreateAPIKey() error = %v", err)
	}
	if !reflect.DeepEqual(key.Scopes, []string{"write"}) {
		t.Fatalf("Scopes = %#v, want deduplicated write", key.Scopes)
	}
	if !reflect.DeepEqual(key.ProjectIDs, []string{"digitaldrywood-video"}) {
		t.Fatalf("ProjectIDs = %#v, want deduplicated project", key.ProjectIDs)
	}

	byHash, err := backend.APIKeyByHash(ctx, "hash-1")
	if err != nil {
		t.Fatalf("APIKeyByHash() error = %v", err)
	}
	if byHash.ID != key.ID || byHash.KeyHash != "hash-1" {
		t.Fatalf("APIKeyByHash() = %#v, want key-1", byHash)
	}

	lastUsedAt := createdAt.Add(time.Minute)
	if err := backend.MarkAPIKeyLastUsed(ctx, key.ID, lastUsedAt); err != nil {
		t.Fatalf("MarkAPIKeyLastUsed(first) error = %v", err)
	}
	if err := backend.MarkAPIKeyLastUsed(ctx, key.ID, lastUsedAt.Add(30*time.Second)); err != nil {
		t.Fatalf("MarkAPIKeyLastUsed(throttled) error = %v", err)
	}
	got, err := backend.APIKey(ctx, key.ID)
	if err != nil {
		t.Fatalf("APIKey() error = %v", err)
	}
	if got.LastUsedAt == nil || !got.LastUsedAt.Equal(lastUsedAt) {
		t.Fatalf("LastUsedAt after throttled update = %v, want %v", got.LastUsedAt, lastUsedAt)
	}
	if err := backend.MarkAPIKeyLastUsed(ctx, key.ID, lastUsedAt.Add(2*time.Minute)); err != nil {
		t.Fatalf("MarkAPIKeyLastUsed(after threshold) error = %v", err)
	}
	got, err = backend.APIKey(ctx, key.ID)
	if err != nil {
		t.Fatalf("APIKey() error = %v", err)
	}
	if got.LastUsedAt == nil || !got.LastUsedAt.Equal(lastUsedAt.Add(2*time.Minute)) {
		t.Fatalf("LastUsedAt after threshold = %v, want %v", got.LastUsedAt, lastUsedAt.Add(2*time.Minute))
	}

	if err := backend.RecordAPIUsageLog(ctx, APIUsageLog{
		APIKeyID:   key.ID,
		Method:     "POST",
		Path:       "/api/v1/projects/digitaldrywood-video/work-items",
		StatusCode: 201,
		LatencyMS:  12,
		IP:         "127.0.0.1",
		UserAgent:  "detent-test",
		CreatedAt:  createdAt.Add(3 * time.Minute),
	}); err != nil {
		t.Fatalf("RecordAPIUsageLog() error = %v", err)
	}
	count, err := backend.CountAPIUsageLogsByKey(ctx, key.ID)
	if err != nil {
		t.Fatalf("CountAPIUsageLogsByKey() error = %v", err)
	}
	if count != 1 {
		t.Fatalf("usage log count = %d, want 1", count)
	}

	if err := backend.RevokeAPIKey(ctx, key.ID, createdAt.Add(4*time.Minute)); err != nil {
		t.Fatalf("RevokeAPIKey() error = %v", err)
	}
	got, err = backend.APIKey(ctx, key.ID)
	if err != nil {
		t.Fatalf("APIKey() after revoke error = %v", err)
	}
	if got.RevokedAt == nil {
		t.Fatalf("RevokedAt = nil, want timestamp")
	}
	active, err := backend.CountActiveAPIKeys(ctx)
	if err != nil {
		t.Fatalf("CountActiveAPIKeys() error = %v", err)
	}
	if active != 0 {
		t.Fatalf("active key count = %d, want 0 after revoke", active)
	}
}

func TestExpiredAPIKeysDoNotCountActive(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	backend, err := Open(ctx, Config{
		Backend: BackendSQLite,
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

	createdAt := time.Date(2000, 1, 1, 12, 0, 0, 0, time.UTC)
	expiredAt := createdAt.Add(time.Hour)
	if _, err := backend.CreateAPIKey(ctx, APIKeyCreate{
		ID:          "expired-key",
		Name:        "Expired",
		PrefixLast4: "detent_ab...dead",
		KeyHash:     "hash-expired",
		Scopes:      []string{"read"},
		CreatedAt:   createdAt,
		ExpiresAt:   &expiredAt,
	}); err != nil {
		t.Fatalf("CreateAPIKey(expired) error = %v", err)
	}
	active, err := backend.CountActiveAPIKeys(ctx)
	if err != nil {
		t.Fatalf("CountActiveAPIKeys() error = %v", err)
	}
	if active != 0 {
		t.Fatalf("active key count = %d, want 0 for expired key", active)
	}
}
