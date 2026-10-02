package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

// SaveWorkflowActivityProfile checkpoints a batch of safe observations in the
// existing phase ledger. The runner calls this only from its telemetry consumer.
func (s *sqliteStore) SaveWorkflowActivityProfile(ctx context.Context, id int64, event WorkflowPhaseEvent, profile workflowmetrics.ActivityProfile) (int64, error) {
	if event.PhaseType != workflowmetrics.PhaseTypeAgentActivity || event.SessionID <= 0 {
		return 0, errors.New("activity profile requires an agent session")
	}
	if profile.Schema != 1 || profile.SessionID != event.SessionID {
		return 0, errors.New("activity profile identity differs from its session")
	}
	instance := sha256.Sum256([]byte(s.path))
	profile.Instance = hex.EncodeToString(instance[:])
	data, err := json.Marshal(profile)
	if err != nil {
		return 0, err
	}
	event.MetadataJSON = string(data)
	if id == 0 {
		return s.RecordWorkflowPhaseEvent(ctx, event)
	}
	finishedAt, err := optionalTimestamp("finished_at", event.FinishedAt)
	if err != nil {
		return 0, err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE workflow_phase_events SET metadata_json = ?, status = ?, finished_at = ?, event_day = ? WHERE id = ? AND session_id = ? AND phase_type = 'agent_activity'`, event.MetadataJSON, event.Status, finishedAt, profile.AsOf.UTC().Format("2006-01-02"), id, event.SessionID)
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if n != 1 {
		return 0, errors.New("activity profile checkpoint not found")
	}
	return id, nil
}
