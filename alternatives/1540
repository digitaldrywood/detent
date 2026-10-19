package hubserver

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/issueorigin"
)

func readHealthScheduled(ctx context.Context, q nativeQueryer, scope nativeScope, now time.Time) (healthScheduledValidation, error) {
	out := healthScheduledValidation{}
	repository, err := policyRepository(ctx, q, string(scope.organization)+"/"+string(scope.project))
	if err != nil {
		return out, err
	}
	var first, green string
	if err := q.QueryRowContext(ctx, `SELECT
 coalesce((SELECT recorded_at FROM scheduled_validation_reports WHERE organization_id=? AND project_id=? AND julianday(recorded_at)<=julianday(?) ORDER BY julianday(recorded_at),recorded_at LIMIT 1),''),
 coalesce((SELECT recorded_at FROM scheduled_validation_reports WHERE organization_id=? AND project_id=? AND julianday(recorded_at)<=julianday(?) AND json_extract(record_json,'$.conclusion')='success' ORDER BY julianday(recorded_at) DESC,recorded_at DESC LIMIT 1),'')`, scope.organization, scope.project, formatHubTime(now), scope.organization, scope.project, formatHubTime(now)).Scan(&first, &green); err != nil {
		return out, err
	}
	if first != "" {
		if out.FirstReport, err = parseTimeValue(first); err != nil {
			return out, err
		}
	}
	if green != "" {
		if out.LastGreen, err = parseTimeValue(green); err != nil {
			return out, err
		}
	}
	rows, err := q.QueryContext(ctx, `WITH observed AS (
 SELECT record_json,recorded_at,run_id,run_attempt FROM scheduled_validation_reports WHERE organization_id=? AND project_id=? AND julianday(recorded_at)<=julianday(?)
 ), latest AS (SELECT record_json,recorded_at FROM observed ORDER BY julianday(recorded_at) DESC,run_id DESC,run_attempt DESC LIMIT 21),
 failures AS (SELECT record_json,recorded_at FROM observed WHERE json_extract(record_json,'$.conclusion')='failure' ORDER BY julianday(recorded_at) DESC,run_id DESC,run_attempt DESC LIMIT 2)
 SELECT record_json,recorded_at FROM latest UNION ALL SELECT record_json,recorded_at FROM failures`, scope.organization, scope.project, formatHubTime(now))
	if err != nil {
		return out, err
	}
	defer rows.Close()
	reports := map[string]scheduledValidationRecord{}
	persisted := map[string]bool{}
	for rows.Next() {
		var raw, at string
		if err := rows.Scan(&raw, &at); err != nil {
			rows.Close()
			return out, err
		}
		var report gate.ScheduledValidationReport
		if err := json.Unmarshal([]byte(raw), &report); err != nil {
			rows.Close()
			return out, err
		}
		recorded, err := parseTimeValue(at)
		if err != nil {
			rows.Close()
			return out, err
		}
		reports[report.RunURL] = scheduledValidationRecord{ScheduledValidationReport: report, RecordedAt: recorded}
		persisted[report.RunURL] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return out, err
	}
	if err := rows.Close(); err != nil {
		return out, err
	}
	rows, err = q.QueryContext(ctx, `WITH history AS (
 SELECT body,created_at AS at FROM issues WHERE organization_id=? AND project_id=? AND instr(body,'detent-checks')>0 AND json_extract(actor_json,'$.kind')!='runner'
 UNION ALL SELECT body,created_at FROM native_comments WHERE organization_id=? AND project_id=? AND (instr(body,'detent-checks')>0 OR body LIKE 'Scheduled full validation passed for development commit %') AND json_extract(actor_json,'$.kind')!='runner'
 ), observed AS (SELECT body,at FROM history WHERE julianday(at)<=julianday(?)),
 latest AS (SELECT body,at FROM observed ORDER BY julianday(at) DESC LIMIT 40),
 first AS (SELECT body,at FROM observed ORDER BY julianday(at) LIMIT 1),
 green AS (SELECT body,at FROM observed WHERE body LIKE 'Scheduled full validation passed for development commit %' ORDER BY julianday(at) DESC LIMIT 1)
 SELECT body,at FROM latest UNION ALL SELECT body,at FROM first UNION ALL SELECT body,at FROM green`, scope.organization, scope.project, scope.organization, scope.project, formatHubTime(now))
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var body, at string
		if err := rows.Scan(&body, &at); err != nil {
			return out, err
		}
		recorded, err := parseTimeValue(at)
		if err != nil {
			return out, err
		}
		origin, stamped := issueorigin.Parse(body)
		addLegacy := func(report gate.ScheduledValidationReport) {
			if persisted[report.RunURL] {
				return
			}
			previous, exists := reports[report.RunURL]
			if !exists || recorded.Before(previous.RecordedAt) {
				reports[report.RunURL] = scheduledValidationRecord{ScheduledValidationReport: report, RecordedAt: recorded}
			}
		}
		for _, evidence := range gate.ParseScheduledEvidence(body) {
			if !stamped || origin.Kind != "doctor" || origin.Instance != "github-actions" || !scheduledRepairOriginMatches(origin.Source, evidence) || !strings.EqualFold(repository, evidence.Repository) {
				continue
			}
			addLegacy(gate.ScheduledValidationReport{RunURL: evidence.RunURL, Conclusion: "failure"})
		}
		if strings.HasPrefix(body, "Scheduled full validation passed for development commit `") {
			sha, _, _ := strings.Cut(strings.TrimPrefix(body, "Scheduled full validation passed for development commit `"), "`")
			_, rest, found := strings.Cut(body, "\n\nRun: ")
			runURL, _, _ := strings.Cut(rest, "\n")
			identity := strings.TrimPrefix(runURL, "https://github.com/"+repository+"/actions/runs/")
			runID, attempt, _ := strings.Cut(identity, "/attempts/")
			report := gate.ScheduledValidationReport{Repository: repository, RunID: runID, RunAttempt: attempt, RunURL: runURL, HeadSHA: sha, Conclusion: "success"}
			if found && report.Valid() {
				addLegacy(report)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	ordered := make([]scheduledValidationRecord, 0, len(reports))
	for _, report := range reports {
		ordered = append(ordered, report)
		if out.FirstReport.IsZero() || report.RecordedAt.Before(out.FirstReport) {
			out.FirstReport = report.RecordedAt
		}
		if report.Conclusion == "success" && report.RecordedAt.After(out.LastGreen) {
			out.LastGreen = report.RecordedAt
		}
	}
	slices.SortFunc(ordered, func(a, b scheduledValidationRecord) int {
		if order := b.RecordedAt.Compare(a.RecordedAt); order != 0 {
			return order
		}
		return strings.Compare(b.RunURL, a.RunURL)
	})
	intervals := []time.Duration{}
	for i, report := range ordered {
		if i == 0 {
			out.LastReport, out.LastRunURL = report.RecordedAt, report.RunURL
		}
		if report.Conclusion == "failure" && report.RecordedAt.After(out.LastGreen) {
			out.Failures++
		}
		if i > 0 && len(intervals) < 20 {
			if interval := ordered[i-1].RecordedAt.Sub(report.RecordedAt); interval > 0 {
				intervals = append(intervals, interval)
			}
		}
	}
	if len(intervals) > 0 {
		slices.Sort(intervals)
		out.Cadence = intervals[len(intervals)/2]
	}
	out.LastFailed = out.Failures > 0
	return out, nil
}
