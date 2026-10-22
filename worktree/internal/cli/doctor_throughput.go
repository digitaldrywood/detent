package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/store"
)

const (
	doctorThroughputWindow               = 24 * time.Hour
	doctorThroughputBucket               = time.Hour
	doctorConcurrencyFloor               = 1
	doctorConcurrencyMinimumHours        = 6
	doctorRedispatchThreshold            = 10
	doctorRedispatchDiagnosticLimit      = 20
	doctorRateWindowBackpressureWaitName = "provider_rate_window_backpressure"
)

type doctorConcurrencyDiagnostic struct {
	ProjectID             string  `json:"project_id"`
	Kind                  string  `json:"kind"`
	From                  string  `json:"from"`
	To                    string  `json:"to"`
	Hours                 int     `json:"hours"`
	ObservedWindowPercent float64 `json:"observed_window_percent"`
	ResultingCeiling      int     `json:"resulting_ceiling,omitempty"`
	ConfiguredConcurrency int     `json:"configured_concurrency,omitempty"`
	ObservedMax           int     `json:"observed_max"`
}

type doctorRedispatchDiagnostic struct {
	ProjectID  string `json:"project_id"`
	Issue      string `json:"issue"`
	Dispatches int    `json:"dispatches"`
	FirstAt    string `json:"first_at"`
	LastAt     string `json:"last_at"`
}

func checkDoctorHistoricalThroughput(
	ctx context.Context,
	projectID string,
	storePath string,
	configuredConcurrency int,
	deps doctorDeps,
) []doctorCheck {
	throughputName := "Project " + projectID + " historical concurrency"
	redispatchName := "Project " + projectID + " redispatch rate"
	deps = deps.withDefaults()
	db, err := deps.openSQLiteReadOnly(ctx, storePath)
	if err != nil {
		if errors.Is(err, errDoctorTelemetryStoreUnavailable) {
			return doctorHistoricalThroughputUnavailable(throughputName, redispatchName, "no work-attempt history recorded yet")
		}
		return doctorHistoricalThroughputWarning(throughputName, redispatchName, fmt.Sprintf("work-attempt history could not be read: %v", err))
	}

	now := deps.now().UTC().Truncate(doctorThroughputBucket)
	from := now.Add(-doctorThroughputWindow)
	report, reportErr := store.QueryConcurrencyReport(ctx, db, store.ConcurrencyQuery{
		ProjectID: projectID,
		From:      from,
		To:        now,
		Bucket:    doctorThroughputBucket,
	})
	pressure, pressureErr := doctorDispatchPressureHours(ctx, db, projectID, from)
	redispatches, redispatchErr := doctorRedispatchDiagnostics(ctx, db, projectID, from)
	closeErr := db.Close()

	for _, queryErr := range []error{reportErr, pressureErr, redispatchErr} {
		if queryErr == nil {
			continue
		}
		if strings.Contains(strings.ToLower(queryErr.Error()), "no such table") {
			return doctorHistoricalThroughputUnavailable(throughputName, redispatchName, "no work-attempt history recorded yet")
		}
		return doctorHistoricalThroughputWarning(throughputName, redispatchName, "work-attempt history query failed: "+queryErr.Error())
	}
	if closeErr != nil {
		return doctorHistoricalThroughputWarning(throughputName, redispatchName, "work-attempt history close failed: "+closeErr.Error())
	}

	concurrencyCheck := doctorConcurrencyCheck(throughputName, projectID, configuredConcurrency, report, pressure)
	redispatchCheck := doctorCheck{
		Name:            redispatchName,
		Status:          doctorOK,
		Detail:          fmt.Sprintf("no issue was dispatched %d or more times in the last 24h", doctorRedispatchThreshold),
		RedispatchLoops: redispatches,
	}
	if len(redispatches) > 0 {
		latest := redispatches[0]
		redispatchCheck.Status = doctorWarn
		redispatchCheck.Detail = fmt.Sprintf(
			"%d issue(s) were dispatched at least %d times in the last 24h; highest is %s with %d dispatches",
			len(redispatches),
			doctorRedispatchThreshold,
			latest.Issue,
			latest.Dispatches,
		)
		redispatchCheck.Hint = "Inspect the per-issue timeline and recorded recovery causes before allowing another dispatch."
	}
	return []doctorCheck{concurrencyCheck, redispatchCheck}
}

func doctorConcurrencyCheck(
	name string,
	projectID string,
	configuredConcurrency int,
	report store.ConcurrencyReport,
	pressure doctorDispatchPressure,
) doctorCheck {
	check := doctorCheck{Name: name, Status: doctorOK}
	series := doctorProjectConcurrencySeries(report.Series, projectID)
	if series == nil {
		check.Detail = "no project concurrency was recorded in the last 24h"
		return check
	}

	observedHours := 0
	observedMax := 0
	floorHours := 0
	longestFloorRun := 0
	currentFloorRun := 0
	for _, bucket := range series.Buckets {
		if bucket.ActiveSeconds > 0 {
			observedHours++
		}
		observedMax = max(observedMax, bucket.Max)
		_, backpressured := pressure.rateWindow[bucket.Start.UTC().Truncate(time.Hour)]
		floor := backpressured && bucket.Max <= doctorConcurrencyFloor
		if floor {
			floorHours++
			currentFloorRun++
			longestFloorRun = max(longestFloorRun, currentFloorRun)
		} else {
			currentFloorRun = 0
		}
	}

	windowPercent := 0.0
	if len(series.Buckets) > 0 {
		windowPercent = float64(floorHours) / float64(len(series.Buckets)) * 100
	}
	if longestFloorRun >= doctorConcurrencyMinimumHours {
		check.ConcurrencyHistory = append(check.ConcurrencyHistory, doctorConcurrencyDiagnostic{
			ProjectID:             projectID,
			Kind:                  "permit_floor",
			From:                  report.From.Format(time.RFC3339),
			To:                    report.To.Format(time.RFC3339),
			Hours:                 longestFloorRun,
			ObservedWindowPercent: windowPercent,
			ResultingCeiling:      doctorConcurrencyFloor,
			ConfiguredConcurrency: configuredConcurrency,
			ObservedMax:           observedMax,
		})
	}
	if configuredConcurrency > doctorConcurrencyFloor && len(pressure.skipped) >= doctorConcurrencyMinimumHours && observedMax < configuredConcurrency {
		check.ConcurrencyHistory = append(check.ConcurrencyHistory, doctorConcurrencyDiagnostic{
			ProjectID:             projectID,
			Kind:                  "configured_concurrency_unreachable",
			From:                  report.From.Format(time.RFC3339),
			To:                    report.To.Format(time.RFC3339),
			Hours:                 observedHours,
			ObservedWindowPercent: float64(observedHours) / float64(len(series.Buckets)) * 100,
			ConfiguredConcurrency: configuredConcurrency,
			ObservedMax:           observedMax,
		})
	}

	if len(check.ConcurrencyHistory) == 0 {
		check.Detail = fmt.Sprintf("last 24h observed %d active hourly buckets with max concurrency %d; configured concurrency is %d", observedHours, observedMax, configuredConcurrency)
		return check
	}
	check.Status = doctorWarn
	parts := make([]string, 0, len(check.ConcurrencyHistory))
	for _, finding := range check.ConcurrencyHistory {
		switch finding.Kind {
		case "permit_floor":
			parts = append(parts, fmt.Sprintf(
				"provider rate-window backpressure held the effective ceiling at %d for %d consecutive hours (%.1f%% of the observed 24h window)",
				finding.ResultingCeiling,
				finding.Hours,
				finding.ObservedWindowPercent,
			))
		case "configured_concurrency_unreachable":
			parts = append(parts, fmt.Sprintf(
				"configured concurrency %d was unreachable across %d active hours; observed max was %d",
				finding.ConfiguredConcurrency,
				finding.Hours,
				finding.ObservedMax,
			))
		}
	}
	check.Detail = strings.Join(parts, "; ")
	check.Hint = "Inspect hourly concurrency and provider rate-window history before changing configured capacity."
	return check
}

type doctorDispatchPressure struct {
	skipped    map[time.Time]struct{}
	rateWindow map[time.Time]struct{}
}

func doctorProjectConcurrencySeries(series []store.ConcurrencySeries, projectID string) *store.ConcurrencySeries {
	for index := range series {
		if strings.TrimSpace(series[index].ProjectID) == strings.TrimSpace(projectID) {
			return &series[index]
		}
	}
	return nil
}

func doctorDispatchPressureHours(ctx context.Context, db doctorTelemetryStore, projectID string, since time.Time) (doctorDispatchPressure, error) {
	rows, err := db.QueryContext(ctx, `
SELECT decision_at, COALESCE(wait_reason, '')
FROM scheduler_decisions
WHERE project_id = ?
  AND result = 'skipped'
  AND decision_at >= ?
ORDER BY decision_at`, projectID, since.Format(time.RFC3339))
	if err != nil {
		return doctorDispatchPressure{}, err
	}
	defer rows.Close()
	pressure := doctorDispatchPressure{
		skipped:    map[time.Time]struct{}{},
		rateWindow: map[time.Time]struct{}{},
	}
	for rows.Next() {
		var raw string
		var waitReason string
		if err := rows.Scan(&raw, &waitReason); err != nil {
			return doctorDispatchPressure{}, err
		}
		at, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(raw))
		if err != nil {
			return doctorDispatchPressure{}, err
		}
		hour := at.UTC().Truncate(time.Hour)
		pressure.skipped[hour] = struct{}{}
		if waitReason == doctorRateWindowBackpressureWaitName {
			pressure.rateWindow[hour] = struct{}{}
		}
	}
	return pressure, rows.Err()
}

func doctorRedispatchDiagnostics(ctx context.Context, db doctorTelemetryStore, projectID string, since time.Time) ([]doctorRedispatchDiagnostic, error) {
	rows, err := db.QueryContext(ctx, `
SELECT COALESCE(NULLIF(TRIM(identifier), ''), NULLIF(TRIM(issue_id), ''), NULLIF(TRIM(issue_url), '')) AS issue,
       COUNT(*) AS dispatches,
       MIN(started_at) AS first_at,
       MAX(started_at) AS last_at
FROM work_attempts
WHERE project_id = ?
  AND started_at >= ?
GROUP BY issue
HAVING issue IS NOT NULL AND COUNT(*) >= ?
ORDER BY dispatches DESC, last_at DESC
LIMIT ?`, projectID, since.Format(time.RFC3339), doctorRedispatchThreshold, doctorRedispatchDiagnosticLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	diagnostics := []doctorRedispatchDiagnostic{}
	for rows.Next() {
		var diagnostic doctorRedispatchDiagnostic
		diagnostic.ProjectID = projectID
		if err := rows.Scan(&diagnostic.Issue, &diagnostic.Dispatches, &diagnostic.FirstAt, &diagnostic.LastAt); err != nil {
			return nil, err
		}
		diagnostics = append(diagnostics, diagnostic)
	}
	return diagnostics, rows.Err()
}

func doctorHistoricalThroughputUnavailable(throughputName string, redispatchName string, detail string) []doctorCheck {
	return []doctorCheck{
		{Name: throughputName, Status: doctorOK, Detail: detail},
		{Name: redispatchName, Status: doctorOK, Detail: detail},
	}
}

func doctorHistoricalThroughputWarning(throughputName string, redispatchName string, detail string) []doctorCheck {
	return []doctorCheck{
		{Name: throughputName, Status: doctorWarn, Detail: detail},
		{Name: redispatchName, Status: doctorWarn, Detail: detail},
	}
}
