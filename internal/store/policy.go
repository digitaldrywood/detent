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
	err := s.db.QueryRowContext(ctx, `SELECT w.worker_metadata_json FROM codex_sessions s
JOIN work_attempts w ON w.id = s.work_attempt_id
WHERE s.id = ? AND s.project_id = ? AND w.project_id = ?`, sessionID, projectID, projectID).Scan(&raw)
	if err != nil {
		return policy.Descriptor{}, fmt.Errorf("read session policy: %w", err)
	}
	var metadata struct {
		Policy policy.Descriptor `json:"policy"`
	}
	if err := json.Unmarshal([]byte(raw), &metadata); err != nil {
		return policy.Descriptor{}, fmt.Errorf("decode session policy: %w", err)
	}
	return metadata.Policy, metadata.Policy.Validate()
}
