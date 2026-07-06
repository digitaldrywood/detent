package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/store/sqlc"
)

const apiKeyLastUsedThrottle = time.Minute

func (s *sqliteStore) CreateAPIKey(ctx context.Context, attrs APIKeyCreate) (APIKey, error) {
	id := strings.TrimSpace(attrs.ID)
	if id == "" {
		return APIKey{}, errors.New("id is required")
	}
	name := strings.TrimSpace(attrs.Name)
	if name == "" {
		return APIKey{}, errors.New("name is required")
	}
	prefixLast4 := strings.TrimSpace(attrs.PrefixLast4)
	if prefixLast4 == "" {
		return APIKey{}, errors.New("prefix_last4 is required")
	}
	keyHash := strings.TrimSpace(attrs.KeyHash)
	if keyHash == "" {
		return APIKey{}, errors.New("key_hash is required")
	}
	scopes, err := jsonStringList("scopes", attrs.Scopes)
	if err != nil {
		return APIKey{}, err
	}
	projectIDs, err := jsonStringList("project_ids", attrs.ProjectIDs)
	if err != nil {
		return APIKey{}, err
	}
	createdAt, err := requiredTimestamp("created_at", attrs.CreatedAt)
	if err != nil {
		return APIKey{}, err
	}
	expiresAt, err := nullableTimestamp("expires_at", attrs.ExpiresAt)
	if err != nil {
		return APIKey{}, err
	}

	row, err := s.queries.CreateAPIKey(ctx, sqlc.CreateAPIKeyParams{
		ID:          id,
		Name:        name,
		PrefixLast4: prefixLast4,
		KeyHash:     keyHash,
		Scopes:      scopes,
		ProjectIds:  projectIDs,
		CreatedAt:   createdAt,
		ExpiresAt:   expiresAt,
		LastUsedAt:  sql.NullString{},
		RevokedAt:   sql.NullString{},
	})
	if err != nil {
		return APIKey{}, fmt.Errorf("creating api key: %w", err)
	}
	return apiKeyFromRow(row)
}

func (s *sqliteStore) APIKey(ctx context.Context, id string) (APIKey, error) {
	row, err := s.queries.GetAPIKey(ctx, strings.TrimSpace(id))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return APIKey{}, ErrNotFound
		}
		return APIKey{}, fmt.Errorf("reading api key: %w", err)
	}
	return apiKeyFromRow(row)
}

func (s *sqliteStore) APIKeyByHash(ctx context.Context, hash string) (APIKey, error) {
	row, err := s.queries.GetAPIKeyByHash(ctx, strings.TrimSpace(hash))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return APIKey{}, ErrNotFound
		}
		return APIKey{}, fmt.Errorf("reading api key by hash: %w", err)
	}
	return apiKeyFromRow(row)
}

func (s *sqliteStore) ListAPIKeys(ctx context.Context) ([]APIKey, error) {
	rows, err := s.queries.ListAPIKeys(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing api keys: %w", err)
	}
	keys := make([]APIKey, 0, len(rows))
	for _, row := range rows {
		key, err := apiKeyFromRow(row)
		if err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, nil
}

func (s *sqliteStore) CountActiveAPIKeys(ctx context.Context) (int64, error) {
	count, err := s.queries.CountActiveAPIKeys(ctx)
	if err != nil {
		return 0, fmt.Errorf("counting active api keys: %w", err)
	}
	return count, nil
}

func (s *sqliteStore) SetAPIKeyExpiresAt(ctx context.Context, id string, expiresAt time.Time) error {
	expiresAtText, err := requiredTimestamp("expires_at", expiresAt)
	if err != nil {
		return err
	}
	rows, err := s.queries.SetAPIKeyExpiresAt(ctx, sqlc.SetAPIKeyExpiresAtParams{
		ExpiresAt: sql.NullString{String: expiresAtText, Valid: true},
		ID:        strings.TrimSpace(id),
	})
	if err != nil {
		return fmt.Errorf("setting api key expiry: %w", err)
	}
	return requireStringAffected(rows, "api key", id)
}

func (s *sqliteStore) RevokeAPIKey(ctx context.Context, id string, revokedAt time.Time) error {
	revokedAtText, err := requiredTimestamp("revoked_at", revokedAt)
	if err != nil {
		return err
	}
	rows, err := s.queries.RevokeAPIKey(ctx, sqlc.RevokeAPIKeyParams{
		RevokedAt: sql.NullString{String: revokedAtText, Valid: true},
		ID:        strings.TrimSpace(id),
	})
	if err != nil {
		return fmt.Errorf("revoking api key: %w", err)
	}
	return requireStringAffected(rows, "api key", id)
}

func (s *sqliteStore) MarkAPIKeyLastUsed(ctx context.Context, id string, at time.Time) error {
	lastUsedAt, err := requiredTimestamp("last_used_at", at)
	if err != nil {
		return err
	}
	threshold, err := requiredTimestamp("last_used_at threshold", at.Add(-apiKeyLastUsedThrottle))
	if err != nil {
		return err
	}
	_, err = s.queries.UpdateAPIKeyLastUsed(ctx, sqlc.UpdateAPIKeyLastUsedParams{
		LastUsedAt: sql.NullString{String: lastUsedAt, Valid: true},
		ID:         strings.TrimSpace(id),
		Threshold:  sql.NullString{String: threshold, Valid: true},
	})
	if err != nil {
		return fmt.Errorf("marking api key last used: %w", err)
	}
	return nil
}

func (s *sqliteStore) RecordAPIUsageLog(ctx context.Context, attrs APIUsageLog) error {
	apiKeyID := strings.TrimSpace(attrs.APIKeyID)
	if apiKeyID == "" {
		return errors.New("api_key_id is required")
	}
	method := strings.TrimSpace(attrs.Method)
	if method == "" {
		return errors.New("method is required")
	}
	path := strings.TrimSpace(attrs.Path)
	if path == "" {
		return errors.New("path is required")
	}
	createdAt, err := requiredTimestamp("created_at", attrs.CreatedAt)
	if err != nil {
		return err
	}
	if err := s.queries.CreateAPIUsageLog(ctx, sqlc.CreateAPIUsageLogParams{
		ApiKeyID:   apiKeyID,
		Method:     method,
		Path:       path,
		StatusCode: nonNegative(int64(attrs.StatusCode)),
		LatencyMs:  nonNegative(int64(attrs.LatencyMS)),
		Ip:         strings.TrimSpace(attrs.IP),
		UserAgent:  strings.TrimSpace(attrs.UserAgent),
		CreatedAt:  createdAt,
	}); err != nil {
		return fmt.Errorf("recording api usage log: %w", err)
	}
	return nil
}

func (s *sqliteStore) CountAPIUsageLogsByKey(ctx context.Context, apiKeyID string) (int64, error) {
	count, err := s.queries.CountAPIUsageLogsByKey(ctx, strings.TrimSpace(apiKeyID))
	if err != nil {
		return 0, fmt.Errorf("counting api usage logs: %w", err)
	}
	return count, nil
}

func apiKeyFromRow(row sqlc.ApiKey) (APIKey, error) {
	scopes, err := parseJSONStringList("scopes", row.Scopes)
	if err != nil {
		return APIKey{}, err
	}
	projectIDs, err := parseJSONStringList("project_ids", row.ProjectIds)
	if err != nil {
		return APIKey{}, err
	}
	createdAt, err := parseTimestamp("created_at", row.CreatedAt)
	if err != nil {
		return APIKey{}, err
	}
	return APIKey{
		ID:          row.ID,
		Name:        row.Name,
		PrefixLast4: row.PrefixLast4,
		KeyHash:     row.KeyHash,
		Scopes:      scopes,
		ProjectIDs:  projectIDs,
		CreatedAt:   createdAt,
		ExpiresAt:   nullableTime(row.ExpiresAt),
		LastUsedAt:  nullableTime(row.LastUsedAt),
		RevokedAt:   nullableTime(row.RevokedAt),
	}, nil
}

func jsonStringList(name string, values []string) (string, error) {
	normalized := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" || seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		normalized = append(normalized, trimmed)
	}
	raw, err := json.Marshal(normalized)
	if err != nil {
		return "", fmt.Errorf("marshal %s: %w", name, err)
	}
	return string(raw), nil
}

func parseJSONStringList(name string, raw string) ([]string, error) {
	values := []string{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &values); err != nil {
		return nil, fmt.Errorf("parse %s: %w", name, err)
	}
	return values, nil
}

func nullableTimestamp(name string, value *time.Time) (sql.NullString, error) {
	if value == nil || value.IsZero() {
		return sql.NullString{}, nil
	}
	text, err := requiredTimestamp(name, *value)
	if err != nil {
		return sql.NullString{}, err
	}
	return sql.NullString{String: text, Valid: true}, nil
}

func nullableTime(value sql.NullString) *time.Time {
	if !value.Valid || strings.TrimSpace(value.String) == "" {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(value.String))
	if err != nil {
		return nil
	}
	return &parsed
}

func requireStringAffected(rows int64, name string, id string) error {
	if rows == 0 {
		return fmt.Errorf("%w: %s %s", ErrNotFound, name, strings.TrimSpace(id))
	}
	return nil
}
