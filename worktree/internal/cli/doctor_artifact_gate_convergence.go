package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
)

const doctorArtifactGateConvergenceSampleLimit = 200

type doctorArtifactGateConvergenceDiagnostic struct {
	ProjectID          string `json:"project_id"`
	IssueID            string `json:"issue_id,omitempty"`
	Identifier         string `json:"identifier,omitempty"`
	StatusField        string `json:"status_field"`
	UnchangedStatus    string `json:"unchanged_status"`
	ConsecutiveSuccess int    `json:"consecutive_successes"`
	Limit              int    `json:"limit"`
	CompletedAt        string `json:"completed_at"`
}

type doctorArtifactGateConvergenceRecord struct {
	StatusField          string `json:"status_field"`
	CompletionStatus     string `json:"completion_status"`
	ConsecutiveUnchanged int    `json:"consecutive_unchanged"`
	Limit                int    `json:"limit"`
	Tripped              bool   `json:"tripped"`
}

func checkDoctorArtifactGateConvergence(
	ctx context.Context,
	resolution globalconfig.PathResolution,
	projectID string,
	deps doctorDeps,
) doctorCheck {
	const name = "Artifact gate convergence"
	storePath := doctorRuntimeStorePath(resolution.Path)
	if storePath == "" {
		return doctorCheck{Name: name, Status: doctorOK, Detail: "artifact gate convergence telemetry is unavailable without a runtime store"}
	}
	deps = deps.withDefaults()
	db, err := deps.openSQLiteReadOnly(ctx, storePath)
	if err != nil {
		if errors.Is(err, errDoctorTelemetryStoreUnavailable) {
			return doctorCheck{Name: name, Status: doctorOK, Detail: "no artifact gate convergence telemetry recorded yet"}
		}
		return doctorCheck{Name: name, Status: doctorWarn, Detail: fmt.Sprintf("artifact gate convergence telemetry could not be read: %v", err)}
	}
	diagnostics, queryErr := doctorArtifactGateConvergenceDiagnostics(ctx, db, projectID)
	closeErr := db.Close()
	if queryErr != nil {
		if strings.Contains(strings.ToLower(queryErr.Error()), "no such table") {
			return doctorCheck{Name: name, Status: doctorOK, Detail: "no artifact gate convergence telemetry recorded yet"}
		}
		return doctorCheck{Name: name, Status: doctorWarn, Detail: fmt.Sprintf("artifact gate convergence telemetry query failed: %v", queryErr)}
	}
	if closeErr != nil {
		return doctorCheck{Name: name, Status: doctorWarn, Detail: fmt.Sprintf("artifact gate convergence telemetry close failed: %v", closeErr)}
	}
	if len(diagnostics) == 0 {
		return doctorCheck{Name: name, Status: doctorOK, Detail: "no latest artifact gate convergence outcomes are breaker trips"}
	}
	latest := diagnostics[0]
	latestIssue := strings.TrimSpace(latest.Identifier)
	if latestIssue == "" {
		latestIssue = strings.TrimSpace(latest.IssueID)
	}
	return doctorCheck{
		Name:   name,
		Status: doctorWarn,
		Detail: fmt.Sprintf(
			"the latest artifact gate convergence outcome for %d item(s) is a breaker trip; latest %s at %s after %d unchanged successful attempts",
			len(diagnostics),
			latestIssue,
			latest.CompletedAt,
			latest.ConsecutiveSuccess,
		),
		Hint:                    "Verify each item state; when still Blocked, review the artifact and update its configured gate status before recovery.",
		ArtifactGateConvergence: diagnostics,
	}
}

func doctorArtifactGateConvergenceDiagnostics(
	ctx context.Context,
	db doctorTelemetryStore,
	projectID string,
) ([]doctorArtifactGateConvergenceDiagnostic, error) {
	projectID = strings.TrimSpace(projectID)
	rows, err := db.QueryContext(ctx, `
WITH ranked AS (
  SELECT project_id, issue_id, identifier, completed_at, worker_metadata_json,
    ROW_NUMBER() OVER (
      PARTITION BY project_id, COALESCE(NULLIF(TRIM(issue_id), ''), NULLIF(TRIM(identifier), ''), CAST(id AS TEXT))
      ORDER BY completed_at DESC, id DESC
    ) AS row_num
  FROM work_attempts
  WHERE terminal_state = 'success'
    AND worker_metadata_json LIKE '%"artifact_gate_convergence"%'
    AND (? = '' OR project_id = ?)
)
SELECT project_id, issue_id, identifier, completed_at, worker_metadata_json
FROM ranked
WHERE row_num = 1
ORDER BY completed_at DESC
LIMIT ?`, projectID, projectID, doctorArtifactGateConvergenceSampleLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	diagnostics := []doctorArtifactGateConvergenceDiagnostic{}
	seen := map[string]struct{}{}
	for rows.Next() {
		var rowProjectID string
		var issueID sql.NullString
		var identifier sql.NullString
		var completedAt sql.NullString
		var metadataJSON string
		if err := rows.Scan(&rowProjectID, &issueID, &identifier, &completedAt, &metadataJSON); err != nil {
			return nil, err
		}
		identity := strings.TrimSpace(issueID.String)
		if identity == "" {
			identity = strings.TrimSpace(identifier.String)
		}
		key := strings.TrimSpace(rowProjectID) + "\x00" + identity
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		var root struct {
			Record doctorArtifactGateConvergenceRecord `json:"artifact_gate_convergence"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(metadataJSON)), &root); err != nil || strings.TrimSpace(root.Record.StatusField) == "" {
			continue
		}
		if !root.Record.Tripped {
			continue
		}
		diagnostics = append(diagnostics, doctorArtifactGateConvergenceDiagnostic{
			ProjectID:          strings.TrimSpace(rowProjectID),
			IssueID:            strings.TrimSpace(issueID.String),
			Identifier:         strings.TrimSpace(identifier.String),
			StatusField:        strings.TrimSpace(root.Record.StatusField),
			UnchangedStatus:    strings.TrimSpace(root.Record.CompletionStatus),
			ConsecutiveSuccess: root.Record.ConsecutiveUnchanged,
			Limit:              root.Record.Limit,
			CompletedAt:        strings.TrimSpace(completedAt.String),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return diagnostics, nil
}
