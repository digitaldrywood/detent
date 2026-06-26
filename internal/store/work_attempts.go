package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/store/sqlc"
)

const defaultSchedulerDecisionLimit = 50

func (s *sqliteStore) StartWorkAttempt(ctx context.Context, attrs WorkAttemptStart) (int64, error) {
	projectID := strings.TrimSpace(attrs.ProjectID)
	if projectID == "" {
		return 0, errors.New("project_id is required")
	}
	workerType := strings.TrimSpace(attrs.WorkerType)
	if workerType == "" {
		return 0, errors.New("worker_type is required")
	}
	startedAt, err := requiredTimestamp("started_at", attrs.StartedAt)
	if err != nil {
		return 0, err
	}
	leaseExpiresAt, err := optionalTimestamp("lease_expires_at", attrs.LeaseExpiresAt)
	if err != nil {
		return 0, err
	}
	attemptNumber := attrs.AttemptNumber
	if attemptNumber <= 0 {
		attemptNumber = 1
	}

	attempt, err := s.queries.CreateWorkAttempt(ctx, sqlc.CreateWorkAttemptParams{
		ProjectID:              projectID,
		IssueID:                nullString(attrs.IssueID),
		Identifier:             nullString(attrs.Identifier),
		IssueURL:               nullString(attrs.IssueURL),
		PrNumber:               nullOptionalInt64(attrs.PRNumber),
		Repo:                   nullString(attrs.Repo),
		WorkerType:             workerType,
		WorkerHost:             nullString(attrs.WorkerHost),
		Lane:                   nullString(attrs.Lane),
		AttemptNumber:          int64(attemptNumber),
		Status:                 string(WorkAttemptStatusActive),
		StartedAt:              startedAt,
		LeaseExpiresAt:         leaseExpiresAt,
		HeartbeatAt:            sql.NullString{String: startedAt, Valid: true},
		Phase:                  nullString(attrs.Phase),
		StatusMessage:          nullString(attrs.StatusMessage),
		CurrentStep:            nullOptionalInt64(attrs.CurrentStep),
		TotalSteps:             nullOptionalInt64(attrs.TotalSteps),
		ProgressPercent:        nullOptionalInt64(attrs.ProgressPercent),
		CurrentCommand:         nullString(attrs.CurrentCommand),
		WaitReason:             nullString(attrs.WaitReason),
		GithubRateSnapshotJson: jsonObjectOrDefault(attrs.GitHubRateSnapshotJSON),
		CiState:                nullString(attrs.CIState),
		CapacitySnapshotJson:   jsonObjectOrDefault(attrs.CapacitySnapshotJSON),
		WorkerMetadataJson:     jsonObjectOrDefault(attrs.WorkerMetadataJSON),
		MetricsJson:            jsonObjectOrDefault(attrs.MetricsJSON),
		NextAction:             nullString(attrs.NextAction),
	})
	if err != nil {
		return 0, fmt.Errorf("starting work attempt: %w", err)
	}
	return attempt.ID, nil
}

func (s *sqliteStore) RecordWorkAttemptHeartbeat(ctx context.Context, attrs WorkAttemptHeartbeat) error {
	if attrs.AttemptID <= 0 {
		return errors.New("attempt_id is required")
	}
	heartbeatAt, err := optionalTimestamp("heartbeat_at", attrs.HeartbeatAt)
	if err != nil {
		return err
	}
	leaseExpiresAt, err := optionalTimestamp("lease_expires_at", attrs.LeaseExpiresAt)
	if err != nil {
		return err
	}

	rows, err := s.queries.UpdateWorkAttemptHeartbeat(ctx, sqlc.UpdateWorkAttemptHeartbeatParams{
		HeartbeatAt:            heartbeatAt,
		LeaseExpiresAt:         leaseExpiresAt,
		Phase:                  nullString(attrs.Phase),
		StatusMessage:          nullString(attrs.StatusMessage),
		CurrentStep:            nullOptionalInt64(attrs.CurrentStep),
		TotalSteps:             nullOptionalInt64(attrs.TotalSteps),
		ProgressPercent:        nullOptionalInt64(attrs.ProgressPercent),
		CurrentCommand:         nullString(attrs.CurrentCommand),
		WaitReason:             nullString(attrs.WaitReason),
		GithubRateSnapshotJson: jsonObjectOrDefault(attrs.GitHubRateSnapshotJSON),
		CiState:                nullString(attrs.CIState),
		CapacitySnapshotJson:   jsonObjectOrDefault(attrs.CapacitySnapshotJSON),
		MetricsJson:            jsonObjectOrDefault(attrs.MetricsJSON),
		NextAction:             nullString(attrs.NextAction),
		ErrorClass:             nullString(attrs.ErrorClass),
		ErrorMessage:           nullString(attrs.ErrorMessage),
		ID:                     attrs.AttemptID,
	})
	if err != nil {
		return fmt.Errorf("recording work attempt heartbeat: %w", err)
	}
	return requireAffected(rows, "work attempt", attrs.AttemptID)
}

func (s *sqliteStore) CompleteWorkAttempt(ctx context.Context, attrs WorkAttemptCompletion) error {
	if attrs.AttemptID <= 0 {
		return errors.New("attempt_id is required")
	}
	completedAt, err := requiredTimestamp("completed_at", attrs.CompletedAt)
	if err != nil {
		return err
	}
	status := attrs.Status
	if status == "" {
		status = WorkAttemptStatusTerminal
	}
	if status != WorkAttemptStatusTerminal {
		return fmt.Errorf("status = %q, want %q", status, WorkAttemptStatusTerminal)
	}
	terminalState := attrs.TerminalState
	if terminalState == "" {
		return errors.New("terminal_state is required")
	}

	rows, err := s.queries.CompleteWorkAttempt(ctx, sqlc.CompleteWorkAttemptParams{
		Status:                 string(status),
		TerminalState:          nullString(string(terminalState)),
		CompletedAt:            sql.NullString{String: completedAt, Valid: true},
		HeartbeatAt:            sql.NullString{String: completedAt, Valid: true},
		LeaseExpiresAt:         sql.NullString{},
		ErrorClass:             nullString(attrs.ErrorClass),
		ErrorMessage:           nullString(attrs.ErrorMessage),
		Phase:                  nullString(attrs.Phase),
		StatusMessage:          nullString(attrs.StatusMessage),
		WaitReason:             nullString(attrs.WaitReason),
		GithubRateSnapshotJson: jsonObjectOrDefault(attrs.GitHubRateSnapshotJSON),
		CiState:                nullString(attrs.CIState),
		CapacitySnapshotJson:   jsonObjectOrDefault(attrs.CapacitySnapshotJSON),
		MetricsJson:            jsonObjectOrDefault(attrs.MetricsJSON),
		NextAction:             nullString(attrs.NextAction),
		ID:                     attrs.AttemptID,
	})
	if err != nil {
		return fmt.Errorf("completing work attempt: %w", err)
	}
	return requireAffected(rows, "work attempt", attrs.AttemptID)
}

func (s *sqliteStore) ListActiveWorkAttempts(ctx context.Context, query WorkAttemptQuery) ([]WorkAttempt, error) {
	rows, err := s.queries.ListActiveWorkAttempts(ctx, strings.TrimSpace(query.ProjectID))
	if err != nil {
		return nil, fmt.Errorf("listing active work attempts: %w", err)
	}
	attempts := make([]WorkAttempt, 0, len(rows))
	for _, row := range rows {
		attempt, err := workAttemptFromRow(row)
		if err != nil {
			return nil, err
		}
		attempts = append(attempts, attempt)
	}
	return attempts, nil
}

func (s *sqliteStore) TimeoutExpiredWorkAttempts(ctx context.Context, attrs WorkAttemptTimeout) ([]WorkAttempt, error) {
	now, err := requiredTimestamp("now", attrs.Now)
	if err != nil {
		return nil, err
	}
	terminalState := attrs.TerminalState
	if terminalState == "" {
		terminalState = WorkAttemptTerminalTimedOut
	}
	errorClass := attrs.ErrorClass
	if strings.TrimSpace(errorClass) == "" {
		errorClass = "lease_expired"
	}
	errorMessage := attrs.ErrorMessage
	if strings.TrimSpace(errorMessage) == "" {
		errorMessage = "work attempt lease expired"
	}
	rows, err := s.queries.TimeoutExpiredWorkAttempts(ctx, sqlc.TimeoutExpiredWorkAttemptsParams{
		Status:          string(WorkAttemptStatusTerminal),
		TerminalState:   nullString(string(terminalState)),
		CompletedAt:     sql.NullString{String: now, Valid: true},
		HeartbeatAt:     sql.NullString{String: now, Valid: true},
		ErrorClass:      nullString(errorClass),
		ErrorMessage:    nullString(errorMessage),
		Phase:           nullString("recovered"),
		StatusMessage:   nullString(errorMessage),
		FilterProjectID: strings.TrimSpace(attrs.ProjectID),
		LeaseExpiresAt:  sql.NullString{String: now, Valid: true},
	})
	if err != nil {
		return nil, fmt.Errorf("timing out expired work attempts: %w", err)
	}
	return workAttemptsFromRows(rows)
}

func (s *sqliteStore) ReclaimActiveWorkAttempts(ctx context.Context, attrs WorkAttemptReclaim) ([]WorkAttempt, error) {
	projectID := strings.TrimSpace(attrs.ProjectID)
	if projectID == "" {
		return nil, errors.New("project_id is required")
	}
	now, err := requiredTimestamp("now", attrs.Now)
	if err != nil {
		return nil, err
	}
	terminalState := attrs.TerminalState
	if terminalState == "" {
		terminalState = WorkAttemptTerminalAbandoned
	}
	errorClass := attrs.ErrorClass
	if strings.TrimSpace(errorClass) == "" {
		errorClass = "service_restart"
	}
	errorMessage := attrs.ErrorMessage
	if strings.TrimSpace(errorMessage) == "" {
		errorMessage = "active work attempt reclaimed after service restart"
	}
	rows, err := s.queries.ReclaimActiveWorkAttempts(ctx, sqlc.ReclaimActiveWorkAttemptsParams{
		Status:        string(WorkAttemptStatusTerminal),
		TerminalState: nullString(string(terminalState)),
		CompletedAt:   sql.NullString{String: now, Valid: true},
		HeartbeatAt:   sql.NullString{String: now, Valid: true},
		ErrorClass:    nullString(errorClass),
		ErrorMessage:  nullString(errorMessage),
		Phase:         nullString("recovered"),
		StatusMessage: nullString(errorMessage),
		ProjectID:     projectID,
	})
	if err != nil {
		return nil, fmt.Errorf("reclaiming active work attempts: %w", err)
	}
	return workAttemptsFromRows(rows)
}

func (s *sqliteStore) RecordSchedulerDecision(ctx context.Context, attrs SchedulerDecision) (int64, error) {
	projectID := strings.TrimSpace(attrs.ProjectID)
	if projectID == "" {
		return 0, errors.New("project_id is required")
	}
	decisionAt, err := requiredTimestamp("decision_at", attrs.DecisionAt)
	if err != nil {
		return 0, err
	}
	result := attrs.Result
	if result == "" {
		if attrs.Selected {
			result = SchedulerDecisionResultSelected
		} else {
			result = SchedulerDecisionResultSkipped
		}
	}
	selected := attrs.Selected || result == SchedulerDecisionResultSelected

	decision, err := s.queries.CreateSchedulerDecision(ctx, sqlc.CreateSchedulerDecisionParams{
		ProjectID:              projectID,
		IssueID:                nullString(attrs.IssueID),
		Identifier:             nullString(attrs.Identifier),
		IssueURL:               nullString(attrs.IssueURL),
		PrNumber:               nullOptionalInt64(attrs.PRNumber),
		Repo:                   nullString(attrs.Repo),
		Lane:                   nullString(attrs.Lane),
		QueuePosition:          nonNegative(int64(attrs.QueuePosition)),
		Result:                 string(result),
		Reason:                 nullString(attrs.Reason),
		Selected:               boolInt64(selected),
		Retry:                  boolInt64(attrs.Retry),
		AttemptNumber:          nonNegative(int64(attrs.AttemptNumber)),
		WorkerHost:             nullString(attrs.WorkerHost),
		DecisionAt:             decisionAt,
		WaitReason:             nullString(attrs.WaitReason),
		CapacitySnapshotJson:   jsonObjectOrDefault(attrs.CapacitySnapshotJSON),
		GithubRateSnapshotJson: jsonObjectOrDefault(attrs.GitHubRateSnapshotJSON),
		MetadataJson:           jsonObjectOrDefault(attrs.MetadataJSON),
	})
	if err != nil {
		return 0, fmt.Errorf("recording scheduler decision: %w", err)
	}
	return decision.ID, nil
}

func (s *sqliteStore) ListRecentSchedulerDecisions(ctx context.Context, query SchedulerDecisionQuery) ([]SchedulerDecision, error) {
	limit := query.Limit
	if limit <= 0 {
		limit = defaultSchedulerDecisionLimit
	}
	rows, err := s.queries.ListRecentSchedulerDecisions(ctx, sqlc.ListRecentSchedulerDecisionsParams{
		FilterProjectID: strings.TrimSpace(query.ProjectID),
		Limit:           int64(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("listing scheduler decisions: %w", err)
	}
	decisions := make([]SchedulerDecision, 0, len(rows))
	for _, row := range rows {
		decision, err := schedulerDecisionFromRow(row)
		if err != nil {
			return nil, err
		}
		decisions = append(decisions, decision)
	}
	return decisions, nil
}

func workAttemptsFromRows(rows []sqlc.WorkAttempt) ([]WorkAttempt, error) {
	attempts := make([]WorkAttempt, 0, len(rows))
	for _, row := range rows {
		attempt, err := workAttemptFromRow(row)
		if err != nil {
			return nil, err
		}
		attempts = append(attempts, attempt)
	}
	return attempts, nil
}

func workAttemptFromRow(row sqlc.WorkAttempt) (WorkAttempt, error) {
	startedAt, err := parseTimestamp("started_at", row.StartedAt)
	if err != nil {
		return WorkAttempt{}, err
	}
	leaseExpiresAt, err := parseOptionalTimestamp("lease_expires_at", row.LeaseExpiresAt)
	if err != nil {
		return WorkAttempt{}, err
	}
	heartbeatAt, err := parseOptionalTimestamp("heartbeat_at", row.HeartbeatAt)
	if err != nil {
		return WorkAttempt{}, err
	}
	completedAt, err := parseOptionalTimestamp("completed_at", row.CompletedAt)
	if err != nil {
		return WorkAttempt{}, err
	}

	return WorkAttempt{
		ID:                     row.ID,
		ProjectID:              row.ProjectID,
		IssueID:                row.IssueID.String,
		Identifier:             row.Identifier.String,
		IssueURL:               row.IssueURL.String,
		PRNumber:               optionalInt64Pointer(row.PrNumber),
		Repo:                   row.Repo.String,
		WorkerType:             row.WorkerType,
		WorkerHost:             row.WorkerHost.String,
		Lane:                   row.Lane.String,
		AttemptNumber:          int(row.AttemptNumber),
		Status:                 WorkAttemptStatus(row.Status),
		StartedAt:              startedAt,
		LeaseExpiresAt:         leaseExpiresAt,
		HeartbeatAt:            heartbeatAt,
		CompletedAt:            completedAt,
		TerminalState:          WorkAttemptTerminalState(row.TerminalState.String),
		ErrorClass:             row.ErrorClass.String,
		ErrorMessage:           row.ErrorMessage.String,
		Phase:                  row.Phase.String,
		StatusMessage:          row.StatusMessage.String,
		CurrentStep:            optionalInt64Pointer(row.CurrentStep),
		TotalSteps:             optionalInt64Pointer(row.TotalSteps),
		ProgressPercent:        optionalInt64Pointer(row.ProgressPercent),
		CurrentCommand:         row.CurrentCommand.String,
		WaitReason:             row.WaitReason.String,
		GitHubRateSnapshotJSON: jsonObjectOrDefault(row.GithubRateSnapshotJson),
		CIState:                row.CiState.String,
		CapacitySnapshotJSON:   jsonObjectOrDefault(row.CapacitySnapshotJson),
		WorkerMetadataJSON:     jsonObjectOrDefault(row.WorkerMetadataJson),
		MetricsJSON:            jsonObjectOrDefault(row.MetricsJson),
		NextAction:             row.NextAction.String,
	}, nil
}

func schedulerDecisionFromRow(row sqlc.SchedulerDecision) (SchedulerDecision, error) {
	decisionAt, err := parseTimestamp("decision_at", row.DecisionAt)
	if err != nil {
		return SchedulerDecision{}, err
	}

	return SchedulerDecision{
		ID:                     row.ID,
		ProjectID:              row.ProjectID,
		IssueID:                row.IssueID.String,
		Identifier:             row.Identifier.String,
		IssueURL:               row.IssueURL.String,
		PRNumber:               optionalInt64Pointer(row.PrNumber),
		Repo:                   row.Repo.String,
		Lane:                   row.Lane.String,
		QueuePosition:          int(row.QueuePosition),
		Result:                 SchedulerDecisionResult(row.Result),
		Reason:                 row.Reason.String,
		Selected:               row.Selected != 0,
		Retry:                  row.Retry != 0,
		AttemptNumber:          int(row.AttemptNumber),
		WorkerHost:             row.WorkerHost.String,
		DecisionAt:             decisionAt,
		WaitReason:             row.WaitReason.String,
		CapacitySnapshotJSON:   jsonObjectOrDefault(row.CapacitySnapshotJson),
		GitHubRateSnapshotJSON: jsonObjectOrDefault(row.GithubRateSnapshotJson),
		MetadataJSON:           jsonObjectOrDefault(row.MetadataJson),
	}, nil
}

func parseOptionalTimestamp(name string, value sql.NullString) (time.Time, error) {
	if !value.Valid || strings.TrimSpace(value.String) == "" {
		return time.Time{}, nil
	}
	return parseTimestamp(name, value.String)
}

func optionalInt64Pointer(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	v := value.Int64
	return &v
}

func jsonObjectOrDefault(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "{}"
	}
	return trimmed
}

func boolInt64(value bool) int64 {
	if value {
		return 1
	}
	return 0
}
