package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/backendcapacity"
	"github.com/digitaldrywood/detent/internal/config/global"
)

const doctorBackendCapacitySampleLimit = 20

type doctorBackendCapacityDiagnostic struct {
	ProjectID      string     `json:"project_id,omitempty"`
	BackendID      string     `json:"backend_id"`
	BackendKind    string     `json:"backend_kind,omitempty"`
	Provider       string     `json:"provider,omitempty"`
	DetectedAt     time.Time  `json:"detected_at"`
	LastObservedAt time.Time  `json:"last_observed_at"`
	ResetAt        *time.Time `json:"reset_at,omitempty"`
	ResumeAt       time.Time  `json:"resume_at"`
	Active         bool       `json:"active"`
	AffectedIssues []string   `json:"affected_issues,omitempty"`
	ParkedIssues   []string   `json:"parked_issues,omitempty"`
}

type doctorBackendCapacitySnapshot struct {
	BackendOutages []doctorBackendCapacitySnapshotOutage `json:"backend_outages"`
}

type doctorBackendCapacitySnapshotOutage struct {
	BackendID      string     `json:"backend_id"`
	BackendKind    string     `json:"backend_kind"`
	Provider       string     `json:"provider"`
	DetectedAt     time.Time  `json:"detected_at"`
	LastObservedAt time.Time  `json:"last_observed_at"`
	ResetAt        *time.Time `json:"reset_at"`
	ResumeAt       time.Time  `json:"resume_at"`
}

func checkDoctorBackendCapacity(
	ctx context.Context,
	resolution global.PathResolution,
	projectID string,
	deps doctorDeps,
	now time.Time,
) doctorCheck {
	name := "Backend capacity"
	if strings.TrimSpace(resolution.Path) == "" {
		return doctorCheck{Name: name, Status: doctorOK, Detail: "skipped because global config path is unavailable"}
	}
	deps = deps.withDefaults()
	storePath := filepath.Join(filepath.Dir(resolution.Path), "detent.db")
	db, err := deps.openSQLiteReadOnly(ctx, storePath)
	if err != nil {
		if errors.Is(err, errDoctorTelemetryStoreUnavailable) {
			return doctorCheck{Name: name, Status: doctorOK, Detail: storePath + " has no capacity telemetry yet"}
		}
		return doctorCheck{Name: name, Status: doctorWarn, Detail: fmt.Sprintf("%s capacity telemetry could not be read: %v", storePath, err)}
	}
	diagnostics, queryErr := doctorBackendCapacityDiagnostics(ctx, db, projectID, now)
	closeErr := db.Close()
	if queryErr != nil {
		return doctorCheck{Name: name, Status: doctorWarn, Detail: fmt.Sprintf("capacity telemetry query failed: %v", queryErr)}
	}
	if closeErr != nil {
		return doctorCheck{Name: name, Status: doctorWarn, Detail: fmt.Sprintf("capacity telemetry close failed: %v", closeErr)}
	}
	check := doctorCheck{
		Name:            name,
		Status:          doctorOK,
		Detail:          "no provider capacity outages recorded",
		BackendCapacity: diagnostics,
	}
	if len(diagnostics) == 0 {
		return check
	}
	active := 0
	for _, diagnostic := range diagnostics {
		if diagnostic.Active {
			active++
		}
	}
	check.Detail = doctorBackendCapacityDetail(diagnostics, active)
	if active > 0 {
		check.Status = doctorWarn
		check.Hint = "Detent is pausing the affected backend until its reset-aware capacity probe succeeds."
	}
	return check
}

func doctorBackendCapacityDiagnostics(
	ctx context.Context,
	db doctorTelemetryStore,
	projectID string,
	now time.Time,
) ([]doctorBackendCapacityDiagnostic, error) {
	rows, err := db.QueryContext(ctx, `
SELECT project_id, identifier, issue_id, capacity_snapshot_json
FROM work_attempts
WHERE error_class = ?
  AND (? = '' OR project_id = ?)
ORDER BY completed_at DESC
LIMIT ?`, backendcapacity.ErrorClass, strings.TrimSpace(projectID), strings.TrimSpace(projectID), doctorBackendCapacitySampleLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	diagnostics := []doctorBackendCapacityDiagnostic{}
	byKey := map[string]int{}
	for rows.Next() {
		var rowProjectID string
		var identifier sql.NullString
		var issueID sql.NullString
		var snapshotJSON string
		if err := rows.Scan(&rowProjectID, &identifier, &issueID, &snapshotJSON); err != nil {
			return nil, err
		}
		var snapshot doctorBackendCapacitySnapshot
		if err := json.Unmarshal([]byte(snapshotJSON), &snapshot); err != nil {
			continue
		}
		issue := strings.TrimSpace(identifier.String)
		if issue == "" {
			issue = strings.TrimSpace(issueID.String)
		}
		for _, outage := range snapshot.BackendOutages {
			key := strings.ToLower(strings.TrimSpace(rowProjectID) + "\x00" + strings.TrimSpace(outage.BackendID) + "\x00" + strings.TrimSpace(outage.Provider))
			index, ok := byKey[key]
			if !ok {
				diagnostics = append(diagnostics, doctorBackendCapacityDiagnostic{
					ProjectID:      strings.TrimSpace(rowProjectID),
					BackendID:      strings.TrimSpace(outage.BackendID),
					BackendKind:    strings.TrimSpace(outage.BackendKind),
					Provider:       strings.TrimSpace(outage.Provider),
					DetectedAt:     outage.DetectedAt,
					LastObservedAt: outage.LastObservedAt,
					ResetAt:        outage.ResetAt,
					ResumeAt:       outage.ResumeAt,
					Active:         outage.ResumeAt.After(now),
				})
				index = len(diagnostics) - 1
				byKey[key] = index
			}
			if issue != "" && !slices.Contains(diagnostics[index].AffectedIssues, issue) {
				diagnostics[index].AffectedIssues = append(diagnostics[index].AffectedIssues, issue)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return diagnostics, nil
}

func doctorBackendCapacityDetail(diagnostics []doctorBackendCapacityDiagnostic, active int) string {
	latest := diagnostics[0]
	window := latest.DetectedAt.UTC().Format(time.RFC3339) + " to " + latest.ResumeAt.UTC().Format(time.RFC3339)
	affectedIssues := []string{}
	for _, diagnostic := range diagnostics {
		for _, issue := range diagnostic.AffectedIssues {
			if !slices.Contains(affectedIssues, issue) {
				affectedIssues = append(affectedIssues, issue)
			}
		}
	}
	if active > 0 {
		return fmt.Sprintf("%d active provider capacity outage(s); latest window %s; affected issues: %s", active, window, strings.Join(affectedIssues, ", "))
	}
	return "latest provider capacity outage window " + window + "; affected issues: " + strings.Join(affectedIssues, ", ")
}
