package store

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/operations"
)

// OperationsReport uses durable history, not the bounded live snapshot, for totals.
func (s *sqliteStore) OperationsReport(ctx context.Context, now, since time.Time) (operations.Report, error) {
	report := operations.Report{DataTime: now, Stats: []operations.Window{}, Actions: []operations.Action{}, Decisions: []operations.Decision{}}
	for _, days := range []int{1, 7} {
		w, err := s.operationsWindow(ctx, now.Add(-time.Duration(days)*24*time.Hour), now)
		if err != nil {
			return operations.Report{}, fmt.Errorf("operations history: %w", err)
		}
		w.Label = fmt.Sprintf("%dd", days)
		if days == 1 {
			w.Label = "24h"
		}
		report.Stats = append(report.Stats, w)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, project_id, issue_id, reason, written_at FROM lane_ledger WHERE result = 'applied' AND reason GLOB 'operator_routine:*' AND julianday(written_at) > julianday(?) AND julianday(written_at) <= julianday(?) ORDER BY id`, since.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	if err != nil {
		return operations.Report{}, fmt.Errorf("operations actions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var a operations.Action
		var at string
		if err := rows.Scan(&a.ID, &a.ProjectID, &a.Issue, &a.Reason, &at); err != nil {
			return operations.Report{}, err
		}
		a.At, err = parseStoredTime(at)
		if err != nil {
			return operations.Report{}, err
		}
		a.Kind = strings.TrimPrefix(a.Reason, "operator_routine:")
		report.Actions = append(report.Actions, a)
	}
	if err := rows.Err(); err != nil {
		return operations.Report{}, err
	}
	rows.Close()
	rows, err = s.db.QueryContext(ctx, `SELECT project_id, issue_identifier, body, question_comment_id FROM human_questions WHERE answer_comment_id = '' AND question_comment_id <> '' ORDER BY project_id, issue_identifier`)
	if err != nil {
		return operations.Report{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var d operations.Decision
		var comment string
		if err := rows.Scan(&d.ProjectID, &d.Issue, &d.Question, &comment); err != nil {
			return operations.Report{}, err
		}
		if repo, number, ok := strings.Cut(d.Issue, "#"); ok && strings.Count(repo, "/") == 1 {
			d.URL = "https://github.com/" + repo + "/issues/" + number
			if _, err := strconv.ParseUint(comment, 10, 64); err == nil {
				d.URL += "#issuecomment-" + comment
			}
		}
		report.Decisions = append(report.Decisions, d)
	}
	return report, rows.Err()
}

func (s *sqliteStore) operationsWindow(ctx context.Context, from, to time.Time) (operations.Window, error) {
	w := operations.Window{From: from, To: to, BlockedNights: []operations.Night{}, Projects: []operations.Project{}}
	bounds := []any{from.Format(time.RFC3339Nano), to.Format(time.RFC3339Nano)}
	rows, err := s.db.QueryContext(ctx, `SELECT pr_number, attempts, total_tokens FROM efficiency_receipts WHERE in_progress = 0 AND julianday(completed_at) >= julianday(?) AND julianday(completed_at) < julianday(?)`, bounds...)
	if err != nil {
		return w, err
	}
	defer rows.Close()
	var clean, tokens int64
	for rows.Next() {
		var pr sql.NullInt64
		var attempts, total int64
		if err := rows.Scan(&pr, &attempts, &total); err != nil {
			return w, err
		}
		w.Closes++
		if pr.Valid && pr.Int64 > 0 {
			w.Merges++
		}
		if attempts == 1 {
			clean++
		}
		tokens += total
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return w, err
	}
	days := to.Sub(from).Hours() / 24
	w.MergesPerDay = float64(w.Merges) / days
	w.ClosesPerDay = float64(w.Closes) / days
	if w.Closes > 0 {
		rate, average := float64(clean)/float64(w.Closes), float64(tokens)/float64(w.Closes)
		w.CleanAttemptRate, w.TokensPerCompletedIssue = &rate, &average
	}
	rows, err = s.db.QueryContext(ctx, `SELECT (MIN(julianday(done.started_at)) - (SELECT MIN(julianday(first.started_at)) FROM workflow_phase_events first WHERE first.project_id = done.project_id AND first.issue_id = done.issue_id AND first.phase_type = 'lane' AND lower(first.phase_name) = 'in progress' AND first.status = 'entered')) * 86400 FROM workflow_phase_events done WHERE done.phase_type = 'lane' AND lower(done.phase_name) = 'done' AND done.status = 'entered' GROUP BY done.project_id, done.issue_id HAVING MIN(julianday(done.started_at)) >= julianday(?) AND MIN(julianday(done.started_at)) < julianday(?)`, bounds...)
	if err != nil {
		return w, err
	}
	defer rows.Close()
	var cycles []float64
	for rows.Next() {
		var duration sql.NullFloat64
		if err := rows.Scan(&duration); err != nil {
			return w, err
		}
		if duration.Valid && duration.Float64 >= 0 {
			cycles = append(cycles, duration.Float64)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return w, err
	}
	sort.Float64s(cycles)
	w.CycleSamples = len(cycles)
	if len(cycles) > 0 {
		median, p75 := operationsPercentile(cycles, .5), operationsPercentile(cycles, .75)
		w.CycleMedianSeconds, w.CycleP75Seconds = &median, &p75
	}
	rows, err = s.db.QueryContext(ctx, `SELECT date(started_at), COUNT(*) FROM (SELECT started_at FROM workflow_phase_events WHERE phase_type = 'lane' AND lower(phase_name) = 'blocked' AND status = 'entered' AND julianday(started_at) >= julianday(?) AND julianday(started_at) < julianday(?) GROUP BY date(started_at), project_id, issue_id) GROUP BY date(started_at) ORDER BY date(started_at)`, bounds...)
	if err != nil {
		return w, err
	}
	defer rows.Close()
	for rows.Next() {
		var night operations.Night
		if err := rows.Scan(&night.Date, &night.Issues); err != nil {
			return w, err
		}
		w.BlockedNights = append(w.BlockedNights, night)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return w, err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT project_id, CASE WHEN selected = 1 OR result = 'selected' THEN 1 ELSE 0 END AS was_selected, COALESCE(reason, ''), COUNT(*) FROM scheduler_decisions WHERE julianday(decision_at) >= julianday(?) AND julianday(decision_at) < julianday(?) GROUP BY project_id, was_selected, reason ORDER BY project_id, COUNT(*) DESC, reason`, bounds...)
	if err != nil {
		return w, err
	}
	defer rows.Close()
	projects := map[string]*operations.Project{}
	for rows.Next() {
		var id, reason string
		var selected, count int64
		if err := rows.Scan(&id, &selected, &reason, &count); err != nil {
			return w, err
		}
		p := projects[id]
		if p == nil {
			p = &operations.Project{ID: id, SkipReasons: []operations.Reason{}}
			projects[id] = p
		}
		if selected != 0 {
			p.Dispatches += count
		} else if len(p.SkipReasons) < 5 {
			p.SkipReasons = append(p.SkipReasons, operations.Reason{Reason: reason, Count: count})
		}
	}
	for _, p := range projects {
		w.Projects = append(w.Projects, *p)
	}
	sort.Slice(w.Projects, func(i, j int) bool { return w.Projects[i].ID < w.Projects[j].ID })
	return w, rows.Err()
}

func operationsPercentile(values []float64, fraction float64) float64 {
	position := float64(len(values)-1) * fraction
	lo, hi := int(math.Floor(position)), int(math.Ceil(position))
	return values[lo] + (values[hi]-values[lo])*(position-float64(lo))
}
