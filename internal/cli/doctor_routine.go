package cli

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
)

const doctorRoutineFailureThreshold = 3

type doctorRoutineDiagnostic struct {
	Name                   string `json:"name"`
	Schedule               string `json:"schedule"`
	MaxFindingsPerRun      int    `json:"max_findings_per_run"`
	MaxOpenFindings        int    `json:"max_open_findings"`
	NeverRun               bool   `json:"never_run,omitempty"`
	LastRun                string `json:"last_run,omitempty"`
	IssuesProposed         int    `json:"issues_proposed,omitempty"`
	IssuesFiled            int    `json:"issues_filed,omitempty"`
	IssuesDeduplicated     int    `json:"issues_deduplicated,omitempty"`
	IssuesLimited          int    `json:"issues_limited,omitempty"`
	ConsecutiveFailures    int    `json:"consecutive_failures,omitempty"`
	ConsecutiveLimitedRuns int    `json:"consecutive_limited_runs,omitempty"`
	LatestError            string `json:"latest_error,omitempty"`
}

func checkDoctorRoutines(ctx context.Context, projectID string, definitions []workflowconfig.Routine, storePath string, deps doctorDeps) doctorCheck {
	name := "Project " + projectID + " scheduled routines"
	diagnostics := initialDoctorRoutineDiagnostics(definitions)
	if len(diagnostics) == 0 {
		return doctorCheck{Name: name, Status: doctorOK, Detail: "no scheduled routines configured"}
	}
	if strings.TrimSpace(storePath) == "" {
		return doctorRoutineCheck(name, diagnostics, "runtime store is not configured")
	}
	deps = deps.withDefaults()
	db, err := deps.openSQLiteReadOnly(ctx, storePath)
	if err != nil {
		if errors.Is(err, errDoctorTelemetryStoreUnavailable) {
			return doctorRoutineCheck(name, diagnostics, "runtime store is not available yet")
		}
		return doctorCheck{Name: name, Status: doctorWarn, Detail: fmt.Sprintf("routine run telemetry could not be read: %v", err), Routines: diagnostics}
	}
	diagnostics, queryErr := doctorRoutineDiagnostics(ctx, db, projectID, definitions)
	closeErr := db.Close()
	if queryErr != nil {
		return doctorCheck{Name: name, Status: doctorWarn, Detail: fmt.Sprintf("routine run telemetry query failed: %v", queryErr), Routines: diagnostics}
	}
	if closeErr != nil {
		return doctorCheck{Name: name, Status: doctorWarn, Detail: fmt.Sprintf("routine run telemetry close failed: %v", closeErr), Routines: diagnostics}
	}
	return doctorRoutineCheck(name, diagnostics, "")
}

func doctorRoutineDiagnostics(ctx context.Context, db doctorTelemetryStore, projectID string, definitions []workflowconfig.Routine) ([]doctorRoutineDiagnostic, error) {
	diagnostics := initialDoctorRoutineDiagnostics(definitions)
	for index := range diagnostics {
		diagnostic, err := readDoctorRoutineDiagnostic(ctx, db, projectID, diagnostics[index])
		if err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "no such table: routine_runs") {
				return diagnostics, nil
			}
			return diagnostics, err
		}
		diagnostics[index] = diagnostic
	}
	return diagnostics, nil
}

func readDoctorRoutineDiagnostic(ctx context.Context, db doctorTelemetryStore, projectID string, diagnostic doctorRoutineDiagnostic) (_ doctorRoutineDiagnostic, resultErr error) {
	rows, err := db.QueryContext(ctx, `
SELECT completed_at, proposed_count, filed_count, deduplicated_count, limited_count,
       COALESCE(error, '')
FROM routine_runs
WHERE project_id = ? AND routine_name = ?
ORDER BY completed_at DESC, id DESC
LIMIT ?`, strings.TrimSpace(projectID), diagnostic.Name, doctorRoutineFailureThreshold)
	if err != nil {
		return diagnostic, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, rows.Close())
	}()
	rowIndex := 0
	failureStreak := true
	limitedStreak := true
	for rows.Next() {
		var completedAt string
		var proposed int
		var filed int
		var deduplicated int
		var limited int
		var runError string
		if err := rows.Scan(&completedAt, &proposed, &filed, &deduplicated, &limited, &runError); err != nil {
			return diagnostic, err
		}
		if rowIndex == 0 {
			diagnostic.NeverRun = false
			diagnostic.LastRun = completedAt
			diagnostic.IssuesProposed = proposed
			diagnostic.IssuesFiled = filed
			diagnostic.IssuesDeduplicated = deduplicated
			diagnostic.IssuesLimited = limited
			diagnostic.LatestError = strings.TrimSpace(runError)
		}
		if failureStreak {
			if strings.TrimSpace(runError) == "" {
				failureStreak = false
			} else {
				diagnostic.ConsecutiveFailures++
			}
		}
		if limitedStreak {
			if limited == 0 {
				limitedStreak = false
			} else {
				diagnostic.ConsecutiveLimitedRuns++
			}
		}
		rowIndex++
	}
	return diagnostic, rows.Err()
}

func initialDoctorRoutineDiagnostics(definitions []workflowconfig.Routine) []doctorRoutineDiagnostic {
	diagnostics := make([]doctorRoutineDiagnostic, 0, len(definitions))
	for _, definition := range workflowconfig.NormalizeRoutines(definitions) {
		diagnostics = append(diagnostics, doctorRoutineDiagnostic{
			Name:              definition.Name,
			Schedule:          definition.Schedule,
			MaxFindingsPerRun: definition.MaxFindingsPerRun,
			MaxOpenFindings:   definition.MaxOpenFindings,
			NeverRun:          true,
		})
	}
	sort.Slice(diagnostics, func(i, j int) bool { return diagnostics[i].Name < diagnostics[j].Name })
	return diagnostics
}

func doctorRoutineCheck(name string, diagnostics []doctorRoutineDiagnostic, unavailable string) doctorCheck {
	never := []string{}
	failing := []string{}
	limited := []string{}
	latest := []string{}
	for _, diagnostic := range diagnostics {
		if diagnostic.NeverRun {
			never = append(never, diagnostic.Name)
			continue
		}
		latestResult := fmt.Sprintf("%s at %s proposed=%d filed=%d deduplicated=%d limited=%d", diagnostic.Name, diagnostic.LastRun, diagnostic.IssuesProposed, diagnostic.IssuesFiled, diagnostic.IssuesDeduplicated, diagnostic.IssuesLimited)
		if diagnostic.LatestError != "" {
			latestResult += " error=" + diagnostic.LatestError
		}
		latest = append(latest, latestResult)
		if diagnostic.ConsecutiveFailures >= doctorRoutineFailureThreshold {
			failing = append(failing, fmt.Sprintf("%s (%d consecutive: %s)", diagnostic.Name, diagnostic.ConsecutiveFailures, diagnostic.LatestError))
		}
		if diagnostic.ConsecutiveLimitedRuns >= doctorRoutineFailureThreshold {
			limited = append(limited, fmt.Sprintf("%s (%d consecutive runs, max_findings_per_run=%d, max_open_findings=%d)", diagnostic.Name, diagnostic.ConsecutiveLimitedRuns, diagnostic.MaxFindingsPerRun, diagnostic.MaxOpenFindings))
		}
	}
	details := []string{fmt.Sprintf("%d configured routine(s)", len(diagnostics))}
	if unavailable != "" {
		details = append(details, unavailable)
	}
	if len(never) > 0 {
		details = append(details, "never run: "+strings.Join(never, ", "))
	}
	if len(failing) > 0 {
		details = append(details, "repeatedly failing: "+strings.Join(failing, ", "))
	}
	if len(limited) > 0 {
		details = append(details, "repeatedly hitting finding ceilings: "+strings.Join(limited, ", "))
	}
	if len(latest) > 0 {
		details = append(details, "latest: "+strings.Join(latest, "; "))
	}
	check := doctorCheck{Name: name, Status: doctorOK, Detail: strings.Join(details, "; "), Routines: diagnostics}
	if len(never) > 0 || len(failing) > 0 || len(limited) > 0 {
		check.Status = doctorWarn
		check.Hint = "Inspect the configured cron schedule, finding ceilings, and recent routine agent errors; successful runs remain visible in the routine run ledger."
	}
	return check
}
