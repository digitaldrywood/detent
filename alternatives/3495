package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/digitaldrywood/detent/internal/policy"
)

type SessionPolicyStore interface {
	SessionPolicy(context.Context, string, int64) (policy.Descriptor, error)
}

func (s *sqliteStore) SessionPolicy(ctx context.Context, projectID string, sessionID int64) (policy.Descriptor, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT coalesce(w.worker_metadata_json, '{}') FROM codex_sessions s
LEFT JOIN work_attempts w ON w.id = s.work_attempt_id
WHERE s.id = ? AND coalesce(s.project_id, '') = ? AND (s.work_attempt_id IS NULL OR w.project_id = ?)`, sessionID, projectID, projectID).Scan(&raw)
	if err != nil {
		return policy.Descriptor{}, fmt.Errorf("read session policy: %w", err)
	}
	var metadata struct {
		Policy *policy.Descriptor `json:"policy"`
	}
	if err := json.Unmarshal([]byte(raw), &metadata); err != nil {
		return policy.Descriptor{}, fmt.Errorf("decode session policy: %w", err)
	}
	if metadata.Policy == nil {
		return policy.Descriptor{}, nil
	}
	return *metadata.Policy, metadata.Policy.Validate()
}
