package hubserver

import "time"

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
	NextCursor   string                  `json:"next_cursor,omitempty"`
}
