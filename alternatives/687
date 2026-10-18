package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
)

const doctorValidatorFailureWindow = 24 * time.Hour

type doctorValidatorFailureDiagnostic struct {
	IssueID        string `json:"issue_id,omitempty"`
	Identifier     string `json:"identifier,omitempty"`
	PRNumber       int64  `json:"pr_number,omitempty"`
	HeadSHA        string `json:"head_sha,omitempty"`
	Attempts       int64  `json:"attempts"`
	Summary        string `json:"summary"`
	NextRetryAt    string `json:"next_retry_at,omitempty"`
	LastObservedAt string `json:"last_observed_at"`
}

func checkDoctorValidatorHealth(ctx context.Context, projectID string, storePath string, deps doctorDeps, now time.Time) doctorCheck {
	name := "Project " + projectID + " validator health"
	if strings.TrimSpace(storePath) == "" {
		return doctorCheck{Name: name, Status: doctorOK, Detail: "validator failure telemetry is unavailable without a runtime store"}
	}
	deps = deps.withDefaults()
	db, err := deps.openSQLiteReadOnly(ctx, storePath)
	if err != nil {
		if errors.Is(err, errDoctorTelemetryStoreUnavailable) {
			return doctorCheck{Name: name, Status: doctorOK, Detail: "validator failure telemetry is not available yet"}
		}
		return doctorCheck{Name: name, Status: doctorWarn, Detail: fmt.Sprintf("validator failure telemetry could not be read: %v", err)}
	}
	failures, queryErr := doctorValidatorFailures(ctx, db, projectID, now)
	closeErr := db.Close()
	if queryErr != nil {
		return doctorCheck{Name: name, Status: doctorWarn, Detail: fmt.Sprintf("validator failure telemetry query failed: %v", queryErr)}
	}
	if closeErr != nil {
		return doctorCheck{Name: name, Status: doctorWarn, Detail: fmt.Sprintf("validator failure telemetry close failed: %v", closeErr)}
	}
	if len(failures) == 0 {
		return doctorCheck{Name: name, Status: doctorOK, Detail: "no validator production failures recorded in the last 24h"}
	}
	return doctorCheck{
		Name:              name,
		Status:            doctorWarn,
		Detail:            doctorValidatorFailureDetail(failures),
		Hint:              "Inspect the validator backend/model and the affected PRs; Detent will retry bounded failures and route exhausted validators to Rework.",
		ValidatorFailures: failures,
	}
}

func doctorValidatorFailures(ctx context.Context, db doctorTelemetryStore, projectID string, now time.Time) ([]doctorValidatorFailureDiagnostic, error) {
	rows, err := db.QueryContext(ctx, `
SELECT issue_id,
       COALESCE(identifier, ''),
       COALESCE(pr_number, 0),
       head_sha,
       failure_attempts,
       COALESCE(summary, ''),
       next_retry_at,
       updated_at
FROM validator_verdicts
WHERE project_id = ?
  AND submitted = 0
  AND verdict = ?
  AND datetime(updated_at) >= datetime(?)
  AND datetime(updated_at) <= datetime(?)
ORDER BY datetime(updated_at) DESC, id DESC
LIMIT 5`,
		strings.TrimSpace(projectID),
		gate.ValidatorVerdictError,
		now.Add(-doctorValidatorFailureWindow).UTC().Format(time.RFC3339Nano),
		now.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	failures := []doctorValidatorFailureDiagnostic{}
	for rows.Next() {
		var failure doctorValidatorFailureDiagnostic
		var nextRetryAt sql.NullString
		if err := rows.Scan(
			&failure.IssueID,
			&failure.Identifier,
			&failure.PRNumber,
			&failure.HeadSHA,
			&failure.Attempts,
			&failure.Summary,
			&nextRetryAt,
			&failure.LastObservedAt,
		); err != nil {
			return nil, err
		}
		failure.NextRetryAt = nextRetryAt.String
		failures = append(failures, failure)
	}
	return failures, rows.Err()
}

func doctorValidatorFailureDetail(failures []doctorValidatorFailureDiagnostic) string {
	parts := make([]string, 0, len(failures))
	for _, failure := range failures {
		issue := strings.TrimSpace(failure.Identifier)
		if issue == "" {
			issue = strings.TrimSpace(failure.IssueID)
		}
		if failure.PRNumber > 0 {
			issue += fmt.Sprintf(" PR #%d", failure.PRNumber)
		}
		parts = append(parts, fmt.Sprintf("%s attempt %d: %s", issue, failure.Attempts, strings.TrimSpace(failure.Summary)))
	}
	return fmt.Sprintf("%d validator production failure(s) in the last 24h: %s", len(failures), strings.Join(parts, "; "))
}
