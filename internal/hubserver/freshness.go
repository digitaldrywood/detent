package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
)

type SyncError struct {
	Message string    `json:"message"`
	At      time.Time `json:"at"`
}

type RepositoryFreshness struct {
	ID                   int64      `json:"id"`
	GitHubNodeID         string     `json:"github_node_id"`
	Repository           string     `json:"repository"`
	Status               string     `json:"status"`
	LastSuccessfulSyncAt *time.Time `json:"last_successful_sync_at,omitempty"`
	LastWebhookAt        *time.Time `json:"last_webhook_at,omitempty"`
	LastReconciledAt     *time.Time `json:"last_reconciled_at,omitempty"`
	LastFullRepairAt     *time.Time `json:"last_full_repair_at,omitempty"`
	LastWebhookError     *SyncError `json:"last_webhook_error,omitempty"`
	LastReconcileError   *SyncError `json:"last_reconcile_error,omitempty"`
}

type RepositoryHealthSummary struct {
	Total int `json:"total"`
	Fresh int `json:"fresh"`
	Stale int `json:"stale"`
	Error int `json:"error"`
}

type repositoryFreshnessResponse struct {
	Repositories []RepositoryFreshness   `json:"repositories"`
	Summary      RepositoryHealthSummary `json:"summary"`
}

type checkpointFreshness struct {
	lastSuccessful *time.Time
	lastError      *SyncError
}

func (s *Service) repositoryFreshness(c echo.Context) error {
	result, err := s.database.repositoryFreshness(c.Request().Context(), s.config.now().UTC(), s.config.ReconcileInterval)
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, webhookErrorResponse{
			Code:    "freshness_unavailable",
			Message: "Repository freshness could not be read",
		})
	}
	return c.JSON(http.StatusOK, result)
}

func (d *database) repositoryFreshness(ctx context.Context, now time.Time, reconcileInterval time.Duration) (repositoryFreshnessResponse, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT r.id, r.github_node_id, r.github_owner, r.github_name,
			r.last_webhook_at, r.last_reconciled_at,
			c.checkpoint_name, c.last_successful_at, c.last_error, c.state_json
		FROM repositories r
		LEFT JOIN sync_checkpoints c ON c.repository_id = r.id
		ORDER BY lower(r.github_owner), lower(r.github_name), r.id, c.checkpoint_name
	`)
	if err != nil {
		return repositoryFreshnessResponse{}, fmt.Errorf("query repository freshness: %w", err)
	}
	defer rows.Close()
	type collected struct {
		freshness   RepositoryFreshness
		checkpoints map[string]checkpointFreshness
	}
	var repositories []*collected
	byID := make(map[int64]*collected)
	for rows.Next() {
		var id int64
		var nodeID string
		var owner string
		var name string
		var lastWebhook sql.NullString
		var lastReconciled sql.NullString
		var checkpointName sql.NullString
		var lastSuccessful sql.NullString
		var lastError sql.NullString
		var stateJSON sql.NullString
		if err := rows.Scan(&id, &nodeID, &owner, &name, &lastWebhook, &lastReconciled, &checkpointName, &lastSuccessful, &lastError, &stateJSON); err != nil {
			return repositoryFreshnessResponse{}, fmt.Errorf("scan repository freshness: %w", err)
		}
		entry := byID[id]
		if entry == nil {
			entry = &collected{
				freshness:   RepositoryFreshness{ID: id, GitHubNodeID: nodeID, Repository: owner + "/" + name},
				checkpoints: make(map[string]checkpointFreshness),
			}
			var parseErr error
			webhookAt, webhookAtValid, parseErr := parseCheckpointTime(lastWebhook)
			if parseErr != nil {
				return repositoryFreshnessResponse{}, fmt.Errorf("parse repository webhook freshness: %w", parseErr)
			}
			if webhookAtValid {
				entry.freshness.LastWebhookAt = &webhookAt
			}
			reconciledAt, reconciledAtValid, parseErr := parseCheckpointTime(lastReconciled)
			if parseErr != nil {
				return repositoryFreshnessResponse{}, fmt.Errorf("parse repository reconciliation freshness: %w", parseErr)
			}
			if reconciledAtValid {
				entry.freshness.LastReconciledAt = &reconciledAt
			}
			byID[id] = entry
			repositories = append(repositories, entry)
		}
		if checkpointName.Valid {
			checkpoint, err := parseCheckpointFreshness(lastSuccessful, lastError, stateJSON)
			if err != nil {
				return repositoryFreshnessResponse{}, fmt.Errorf("parse repository %s checkpoint: %w", checkpointName.String, err)
			}
			entry.checkpoints[checkpointName.String] = checkpoint
		}
	}
	if err := rows.Err(); err != nil {
		return repositoryFreshnessResponse{}, fmt.Errorf("iterate repository freshness: %w", err)
	}

	staleAfter := 2 * reconcileInterval
	if staleAfter <= 0 {
		staleAfter = 2 * DefaultReconcileInterval
	}
	result := repositoryFreshnessResponse{Repositories: make([]RepositoryFreshness, 0, len(repositories))}
	for _, entry := range repositories {
		freshness := entry.freshness
		freshness.LastWebhookAt = latestTime(freshness.LastWebhookAt, entry.checkpoints[checkpointWebhook].lastSuccessful)
		freshness.LastSuccessfulSyncAt = latestTime(freshness.LastWebhookAt, freshness.LastReconciledAt)
		if checkpoint := entry.checkpoints[checkpointFullRepair]; checkpoint.lastSuccessful != nil {
			freshness.LastFullRepairAt = checkpoint.lastSuccessful
		}
		freshness.LastWebhookError = entry.checkpoints[checkpointWebhook].lastError
		freshness.LastReconcileError = latestSyncError(
			entry.checkpoints[checkpointIncremental].lastError,
			entry.checkpoints[checkpointFullRepair].lastError,
		)
		freshness.Status = "fresh"
		if checkpointHasUnresolvedError(entry.checkpoints[checkpointWebhook]) ||
			checkpointHasUnresolvedError(entry.checkpoints[checkpointIncremental]) ||
			checkpointHasUnresolvedError(entry.checkpoints[checkpointFullRepair]) {
			freshness.Status = "error"
		} else if freshness.LastReconciledAt == nil || now.Sub(*freshness.LastReconciledAt) > staleAfter {
			freshness.Status = "stale"
		}
		result.Repositories = append(result.Repositories, freshness)
		result.Summary.Total++
		switch freshness.Status {
		case "fresh":
			result.Summary.Fresh++
		case "error":
			result.Summary.Error++
		default:
			result.Summary.Stale++
		}
	}
	return result, nil
}

func checkpointHasUnresolvedError(checkpoint checkpointFreshness) bool {
	return checkpoint.lastError != nil &&
		(checkpoint.lastSuccessful == nil || !checkpoint.lastError.At.Before(*checkpoint.lastSuccessful))
}

func parseCheckpointFreshness(lastSuccessful sql.NullString, lastError sql.NullString, stateJSON sql.NullString) (checkpointFreshness, error) {
	result := checkpointFreshness{}
	var err error
	lastSuccessfulAt, lastSuccessfulValid, err := parseCheckpointTime(lastSuccessful)
	if err != nil {
		return checkpointFreshness{}, err
	}
	if lastSuccessfulValid {
		result.lastSuccessful = &lastSuccessfulAt
	}
	if !lastError.Valid || lastError.String == "" {
		return result, nil
	}
	var state struct {
		LastErrorAt string `json:"last_error_at"`
	}
	if !stateJSON.Valid || json.Unmarshal([]byte(stateJSON.String), &state) != nil || state.LastErrorAt == "" {
		return result, nil
	}
	errorAt, err := time.Parse(time.RFC3339Nano, state.LastErrorAt)
	if err != nil {
		return checkpointFreshness{}, err
	}
	result.lastError = &SyncError{Message: lastError.String, At: errorAt.UTC()}
	return result, nil
}

func latestTime(values ...*time.Time) *time.Time {
	var latest *time.Time
	for _, value := range values {
		if value != nil && (latest == nil || value.After(*latest)) {
			copy := value.UTC()
			latest = &copy
		}
	}
	return latest
}

func latestSyncError(values ...*SyncError) *SyncError {
	var latest *SyncError
	for _, value := range values {
		if value != nil && (latest == nil || value.At.After(latest.At)) {
			copy := *value
			latest = &copy
		}
	}
	return latest
}
