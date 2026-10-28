package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type scheduledValidationRecord struct {
	gate.ScheduledValidationReport
	RecordedAt time.Time `json:"recorded_at"`
}

func (s *Service) recordScheduledValidation(ctx context.Context, scope nativeScope, request tracker.CreateIssue) (json.RawMessage, error) {
	report := request.ScheduledValidation
	if report == nil || !report.Valid() || request.Type != "" || request.ReportContext != nil || request.Title != "" || request.Body != "" || request.State != "" || request.Priority != nil || len(request.Labels) != 0 || len(request.Assignees) != 0 || request.Provenance != nil || request.GitHubIssueURL != "" {
		return nil, nativePublicInvalid("Invalid scheduled validation report")
	}
	if scope.credential.Scope == apiScopeWorker {
		return nil, &nativeError{Code: "insufficient_scope", Message: "Scheduled reporting requires operator authority", status: http.StatusForbidden, publicMessage: true}
	}
	options := nativeCommandOptions{OperationID: "POST /api/v2/organizations/" + string(scope.organization) + "/projects/" + string(scope.project) + "/work-items", Feature: "collaboration"}
	return s.executeNativeMutation(ctx, scope, options, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		repository, err := policyRepository(ctx, tx, string(scope.organization)+"/"+string(scope.project))
		if err != nil {
			return nil, err
		}
		if !strings.EqualFold(repository, report.Repository) {
			return nil, nativePublicInvalid("Scheduled validation repository does not match the project")
		}
		raw, err := json.Marshal(report)
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO scheduled_validation_reports (organization_id,project_id,repository,run_id,run_attempt,record_json,recorded_at,conclusion)
 VALUES (?,?,?,?,?,?,?,?) ON CONFLICT (organization_id,project_id,repository,run_id,run_attempt) DO NOTHING`, scope.organization, scope.project, report.Repository, report.RunID, report.RunAttempt, string(raw), formatHubTime(now), report.Conclusion); err != nil {
			return nil, err
		}
		var retained, at string
		if err := tx.QueryRowContext(ctx, `SELECT record_json,recorded_at FROM scheduled_validation_reports WHERE organization_id=? AND project_id=? AND repository=? AND run_id=? AND run_attempt=?`, scope.organization, scope.project, report.Repository, report.RunID, report.RunAttempt).Scan(&retained, &at); err != nil {
			return nil, err
		}
		if retained != string(raw) {
			return nil, nativePublicInvalid("Scheduled validation run already has a different report")
		}
		recordedAt, err := parseTimeValue(at)
		return scheduledValidationRecord{ScheduledValidationReport: *report, RecordedAt: recordedAt}, err
	})
}
