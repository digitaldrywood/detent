package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/backendcapacity"
	"github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

const doctorBackendCapacitySampleLimit = 20

type doctorBackendCapacityDiagnostic struct {
	ProjectID       string     `json:"project_id,omitempty"`
	BackendID       string     `json:"backend_id"`
	BackendKind     string     `json:"backend_kind,omitempty"`
	Provider        string     `json:"provider,omitempty"`
	DetectedAt      time.Time  `json:"detected_at"`
	LastObservedAt  time.Time  `json:"last_observed_at"`
	ResetAt         *time.Time `json:"reset_at,omitempty"`
	ResumeAt        time.Time  `json:"resume_at"`
	NextProbeAt     *time.Time `json:"next_probe_at,omitempty"`
	LastProbeAt     *time.Time `json:"last_probe_at,omitempty"`
	LastProbeResult string     `json:"last_probe_result,omitempty"`
	LastProbeDetail string     `json:"last_probe_detail,omitempty"`
	ProbeAttempts   int        `json:"probe_attempts,omitempty"`
	Active          bool       `json:"active"`
	Enforced        bool       `json:"enforced"`
	AffectedIssues  []string   `json:"affected_issues,omitempty"`
	ParkedIssues    []string   `json:"parked_issues,omitempty"`
}

type doctorBackendCapacitySnapshot struct {
	BackendOutages []doctorBackendCapacitySnapshotOutage `json:"backend_outages"`
}

type doctorBackendCapacitySnapshotOutage struct {
	BackendID       string     `json:"backend_id"`
	BackendKind     string     `json:"backend_kind"`
	Provider        string     `json:"provider"`
	DetectedAt      time.Time  `json:"detected_at"`
	LastObservedAt  time.Time  `json:"last_observed_at"`
	ResetAt         *time.Time `json:"reset_at"`
	ResumeAt        time.Time  `json:"resume_at"`
	NextProbeAt     *time.Time `json:"next_probe_at"`
	LastProbeAt     *time.Time `json:"last_probe_at"`
	LastProbeResult string     `json:"last_probe_result"`
	LastProbeDetail string     `json:"last_probe_detail"`
	ProbeAttempts   int        `json:"probe_attempts"`
}

func checkDoctorBackendCapacity(
	ctx context.Context,
	resolution global.PathResolution,
	boot BootConfig,
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
	diagnostics := []doctorBackendCapacityDiagnostic{}
	overloadRetries := 0
	db, err := deps.openSQLiteReadOnly(ctx, storePath)
	if err != nil {
		if !errors.Is(err, errDoctorTelemetryStoreUnavailable) {
			return doctorCheck{Name: name, Status: doctorWarn, Detail: fmt.Sprintf("%s capacity telemetry could not be read: %v", storePath, err)}
		}
	} else {
		var queryErr error
		diagnostics, queryErr = doctorBackendCapacityDiagnostics(ctx, db, projectID, now)
		var overloadQueryErr error
		overloadRetries, overloadQueryErr = doctorOverloadRetryCount(ctx, db, projectID, now)
		closeErr := db.Close()
		if queryErr != nil {
			return doctorCheck{Name: name, Status: doctorWarn, Detail: fmt.Sprintf("capacity telemetry query failed: %v", queryErr)}
		}
		if overloadQueryErr != nil {
			return doctorCheck{Name: name, Status: doctorWarn, Detail: fmt.Sprintf("overload retry telemetry query failed: %v", overloadQueryErr)}
		}
		if closeErr != nil {
			return doctorCheck{Name: name, Status: doctorWarn, Detail: fmt.Sprintf("capacity telemetry close failed: %v", closeErr)}
		}
	}
	liveOutages, liveAvailable := readDoctorBackendCapacityLive(ctx, boot, projectID, deps)
	diagnostics = reconcileDoctorBackendCapacity(diagnostics, liveOutages, liveAvailable)
	check := doctorCheck{
		Name:                    name,
		Status:                  doctorOK,
		Detail:                  "no provider capacity outages recorded",
		BackendCapacity:         diagnostics,
		OverloadRetriesLastHour: overloadRetries,
	}
	if len(diagnostics) == 0 {
		if err != nil {
			check.Detail = storePath + " has no capacity telemetry yet; live orchestrator state unavailable"
			if liveAvailable {
				check.Detail = storePath + " has no capacity telemetry yet; live orchestrator reports no enforced outage"
			}
		} else if overloadRetries > 0 {
			check.Detail = fmt.Sprintf("%d overload retries last hour; no provider usage-limit outages recorded", overloadRetries)
		}
		return check
	}
	active := 0
	for _, diagnostic := range diagnostics {
		if diagnostic.Active {
			active++
		}
	}
	check.Detail = doctorBackendCapacityDetail(diagnostics, active, liveAvailable)
	if overloadRetries > 0 {
		check.Detail = fmt.Sprintf("%d overload retries last hour; %s", overloadRetries, check.Detail)
	}
	if active > 0 {
		check.Status = doctorWarn
		check.Hint = "Recorded provider capacity state may still be pausing dispatch; check the live orchestrator before intervening."
		if liveAvailable {
			check.Hint = "Detent is pausing the affected backend while bounded early canaries and live status checks probe for recovery."
		}
	}
	return check
}

func doctorOverloadRetryCount(ctx context.Context, db doctorTelemetryStore, projectID string, now time.Time) (int, error) {
	row := db.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM work_attempts
WHERE error_class = ?
  AND datetime(completed_at) >= datetime(?)
  AND datetime(completed_at) <= datetime(?)
  AND (? = '' OR project_id = ?)`,
		backendcapacity.TransientOverloadErrorClass,
		now.Add(-time.Hour).UTC().Format(time.RFC3339Nano),
		now.UTC().Format(time.RFC3339Nano),
		strings.TrimSpace(projectID),
		strings.TrimSpace(projectID),
	)
	var count int
	if err := row.Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
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
			key := doctorBackendCapacityKey(rowProjectID, outage.BackendID, outage.Provider)
			index, ok := byKey[key]
			if !ok {
				diagnostics = append(diagnostics, doctorBackendCapacityDiagnostic{
					ProjectID:       strings.TrimSpace(rowProjectID),
					BackendID:       strings.TrimSpace(outage.BackendID),
					BackendKind:     strings.TrimSpace(outage.BackendKind),
					Provider:        strings.TrimSpace(outage.Provider),
					DetectedAt:      outage.DetectedAt,
					LastObservedAt:  outage.LastObservedAt,
					ResetAt:         outage.ResetAt,
					ResumeAt:        outage.ResumeAt,
					NextProbeAt:     outage.NextProbeAt,
					LastProbeAt:     outage.LastProbeAt,
					LastProbeResult: outage.LastProbeResult,
					LastProbeDetail: outage.LastProbeDetail,
					ProbeAttempts:   outage.ProbeAttempts,
					Active:          doctorBackendCapacityRecordedActive(outage, now),
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

func doctorBackendCapacityDetail(diagnostics []doctorBackendCapacityDiagnostic, active int, liveAvailable bool) string {
	latest := diagnostics[0]
	window := latest.DetectedAt.UTC().Format(time.RFC3339) + " to provider-recorded " + latest.ResumeAt.UTC().Format(time.RFC3339)
	probe := "last probe: none"
	if latest.LastProbeAt != nil {
		probe = "last probe: " + latest.LastProbeAt.UTC().Format(time.RFC3339) + " (" + strings.ReplaceAll(latest.LastProbeResult, "_", " ") + ")"
	}
	affectedIssues := []string{}
	for _, diagnostic := range diagnostics {
		for _, issue := range diagnostic.AffectedIssues {
			if !slices.Contains(affectedIssues, issue) {
				affectedIssues = append(affectedIssues, issue)
			}
		}
	}
	if active > 0 {
		if !liveAvailable {
			return fmt.Sprintf("%d recorded provider capacity outage(s) may still be active; latest recorded window %s; %s; live orchestrator enforcement unknown; affected issues: %s", active, window, probe, doctorBackendCapacityAffectedIssues(affectedIssues))
		}
		return fmt.Sprintf("%d enforced provider capacity outage(s); latest recorded window %s; %s; affected issues: %s", active, window, probe, doctorBackendCapacityAffectedIssues(affectedIssues))
	}
	liveState := "live orchestrator state unavailable"
	if liveAvailable {
		liveState = "not enforced by the live orchestrator"
	}
	return "latest recorded provider capacity outage window " + window + "; " + probe + "; " + liveState + "; affected issues: " + doctorBackendCapacityAffectedIssues(affectedIssues)
}

func doctorBackendCapacityRecordedActive(outage doctorBackendCapacitySnapshotOutage, now time.Time) bool {
	return outage.ResumeAt.After(now) || outage.NextProbeAt != nil && outage.NextProbeAt.After(now)
}

func doctorBackendCapacityAffectedIssues(issues []string) string {
	if len(issues) == 0 {
		return "none"
	}
	return strings.Join(issues, ", ")
}

func readDoctorBackendCapacityLive(
	ctx context.Context,
	boot BootConfig,
	projectID string,
	deps doctorDeps,
) ([]telemetry.BackendOutage, bool) {
	port := defaultWebPort
	if boot.Port != nil {
		port = *boot.Port
	}
	if port <= 0 || deps.httpDo == nil {
		return nil, false
	}
	host := unbracketIPv6Host(strings.TrimSpace(boot.Host))
	switch host {
	case "", "0.0.0.0", "::":
		host = defaultWebHost
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(host, strconv.Itoa(port))+"/health", nil)
	if err != nil {
		return nil, false
	}
	response, err := deps.httpDo(request)
	if err != nil {
		return nil, false
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, false
	}
	var payload struct {
		BackendOutages []telemetry.BackendOutage `json:"backend_outages"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return nil, false
	}
	filtered := make([]telemetry.BackendOutage, 0, len(payload.BackendOutages))
	for _, outage := range payload.BackendOutages {
		if strings.TrimSpace(projectID) == "" || strings.EqualFold(strings.TrimSpace(projectID), strings.TrimSpace(outage.ProjectID)) {
			filtered = append(filtered, outage)
		}
	}
	return filtered, true
}

func reconcileDoctorBackendCapacity(
	history []doctorBackendCapacityDiagnostic,
	live []telemetry.BackendOutage,
	liveAvailable bool,
) []doctorBackendCapacityDiagnostic {
	if !liveAvailable {
		return history
	}
	byKey := map[string]int{}
	for index := range history {
		history[index].Active = false
		history[index].Enforced = false
		byKey[doctorBackendCapacityKey(history[index].ProjectID, history[index].BackendID, history[index].Provider)] = index
	}
	for _, outage := range live {
		key := doctorBackendCapacityKey(outage.ProjectID, outage.BackendID, outage.Provider)
		index, ok := byKey[key]
		if !ok {
			history = append(history, doctorBackendCapacityDiagnostic{})
			index = len(history) - 1
			byKey[key] = index
		}
		diagnostic := &history[index]
		diagnostic.ProjectID = outage.ProjectID
		diagnostic.BackendID = outage.BackendID
		diagnostic.BackendKind = outage.BackendKind
		diagnostic.Provider = outage.Provider
		diagnostic.DetectedAt = outage.DetectedAt
		diagnostic.LastObservedAt = outage.LastObservedAt
		diagnostic.ResetAt = outage.ResetAt
		diagnostic.ResumeAt = outage.ResumeAt
		diagnostic.NextProbeAt = outage.NextProbeAt
		diagnostic.LastProbeAt = outage.LastProbeAt
		diagnostic.LastProbeResult = outage.LastProbeResult
		diagnostic.LastProbeDetail = outage.LastProbeDetail
		diagnostic.ProbeAttempts = outage.ProbeAttempts
		diagnostic.Active = true
		diagnostic.Enforced = true
	}
	return history
}

func doctorBackendCapacityKey(projectID string, backendID string, provider string) string {
	return strings.ToLower(strings.TrimSpace(projectID) + "\x00" + strings.TrimSpace(backendID) + "\x00" + strings.TrimSpace(provider))
}
