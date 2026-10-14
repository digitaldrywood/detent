package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

type concurrencyQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

type concurrencyInterval struct {
	projectID string
	start     time.Time
	end       time.Time
}

type concurrencyEvent struct {
	at    time.Time
	delta int
}

func (s *sqliteStore) ConcurrencyReport(ctx context.Context, query ConcurrencyQuery) (ConcurrencyReport, error) {
	return QueryConcurrencyReport(ctx, s.db, query)
}

func QueryConcurrencyReport(ctx context.Context, db concurrencyQueryer, query ConcurrencyQuery) (ConcurrencyReport, error) {
	query.ProjectID = strings.TrimSpace(query.ProjectID)
	query.From = query.From.UTC()
	query.To = query.To.UTC()
	if query.From.IsZero() || query.To.IsZero() {
		return ConcurrencyReport{}, errors.New("concurrency window is required")
	}
	if !query.From.Before(query.To) {
		return ConcurrencyReport{}, errors.New("concurrency window start must be before end")
	}
	if query.Bucket <= 0 {
		return ConcurrencyReport{}, errors.New("concurrency bucket must be positive")
	}
	if db == nil {
		return ConcurrencyReport{}, errors.New("concurrency database is required")
	}

	rows, err := db.QueryContext(ctx, `
SELECT project_id, started_at,
       COALESCE(completed_at, lease_expires_at, heartbeat_at, started_at)
FROM work_attempts
WHERE started_at < ?
  AND COALESCE(completed_at, lease_expires_at, heartbeat_at, started_at) > ?
  AND (? = '' OR project_id = ?)
ORDER BY project_id, started_at, id`,
		query.To.Format(time.RFC3339Nano),
		query.From.Format(time.RFC3339Nano),
		query.ProjectID,
		query.ProjectID,
	)
	if err != nil {
		return ConcurrencyReport{}, fmt.Errorf("querying concurrency intervals: %w", err)
	}
	defer rows.Close()

	intervals := make([]concurrencyInterval, 0)
	for rows.Next() {
		var projectID string
		var startedAt string
		var endedAt string
		if err := rows.Scan(&projectID, &startedAt, &endedAt); err != nil {
			return ConcurrencyReport{}, fmt.Errorf("scanning concurrency interval: %w", err)
		}
		start, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(startedAt))
		if err != nil {
			return ConcurrencyReport{}, fmt.Errorf("parse concurrency start for project %s: %w", projectID, err)
		}
		end, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(endedAt))
		if err != nil {
			return ConcurrencyReport{}, fmt.Errorf("parse concurrency end for project %s: %w", projectID, err)
		}
		start = maxTime(start.UTC(), query.From)
		end = minTime(end.UTC(), query.To)
		if !start.Before(end) {
			continue
		}
		intervals = append(intervals, concurrencyInterval{
			projectID: strings.TrimSpace(projectID),
			start:     start,
			end:       end,
		})
	}
	if err := rows.Err(); err != nil {
		return ConcurrencyReport{}, fmt.Errorf("iterating concurrency intervals: %w", err)
	}

	return concurrencyReport(intervals, query), nil
}

func concurrencyReport(intervals []concurrencyInterval, query ConcurrencyQuery) ConcurrencyReport {
	report := ConcurrencyReport{
		From:         query.From,
		To:           query.To,
		Bucket:       query.Bucket,
		AttemptCount: len(intervals),
	}
	byProject := make(map[string][]concurrencyInterval)
	for _, interval := range intervals {
		byProject[interval.projectID] = append(byProject[interval.projectID], interval)
	}

	if query.ProjectID == "" {
		report.Series = append(report.Series, concurrencySeries("", intervals, query))
	}
	projectIDs := make([]string, 0, len(byProject))
	for projectID := range byProject {
		projectIDs = append(projectIDs, projectID)
	}
	sort.Strings(projectIDs)
	for _, projectID := range projectIDs {
		report.Series = append(report.Series, concurrencySeries(projectID, byProject[projectID], query))
	}
	return report
}

func concurrencySeries(projectID string, intervals []concurrencyInterval, query ConcurrencyQuery) ConcurrencySeries {
	series := ConcurrencySeries{ProjectID: projectID}
	for start := query.From; start.Before(query.To); start = start.Add(query.Bucket) {
		end := minTime(start.Add(query.Bucket), query.To)
		series.Buckets = append(series.Buckets, concurrencyBucket(intervals, start, end))
	}
	return series
}

func concurrencyBucket(intervals []concurrencyInterval, start time.Time, end time.Time) ConcurrencyBucket {
	events := make([]concurrencyEvent, 0, len(intervals)*2)
	for _, interval := range intervals {
		overlapStart := maxTime(interval.start, start)
		overlapEnd := minTime(interval.end, end)
		if !overlapStart.Before(overlapEnd) {
			continue
		}
		events = append(events,
			concurrencyEvent{at: overlapStart, delta: 1},
			concurrencyEvent{at: overlapEnd, delta: -1},
		)
	}
	sort.Slice(events, func(i, j int) bool {
		if events[i].at.Equal(events[j].at) {
			return events[i].delta < events[j].delta
		}
		return events[i].at.Before(events[j].at)
	})

	durations := map[int]time.Duration{}
	current := 0
	cursor := start
	maximum := 0
	for index := 0; index < len(events); {
		at := events[index].at
		if at.After(cursor) {
			durations[current] += at.Sub(cursor)
			cursor = at
		}
		for index < len(events) && events[index].at.Equal(at) {
			current += events[index].delta
			index++
		}
		maximum = max(maximum, current)
	}
	if cursor.Before(end) {
		durations[current] += end.Sub(cursor)
	}

	active := time.Duration(0)
	for count, duration := range durations {
		if count > 0 {
			active += duration
		}
	}
	return ConcurrencyBucket{
		Start:         start,
		End:           end,
		Median:        weightedConcurrencyQuantile(durations, 0.5),
		P90:           weightedConcurrencyQuantile(durations, 0.9),
		Max:           maximum,
		ActiveSeconds: int64(active / time.Second),
	}
}

func weightedConcurrencyQuantile(durations map[int]time.Duration, quantile float64) int {
	total := time.Duration(0)
	counts := make([]int, 0, len(durations))
	for count, duration := range durations {
		if duration <= 0 {
			continue
		}
		total += duration
		counts = append(counts, count)
	}
	if total <= 0 {
		return 0
	}
	sort.Ints(counts)
	target := time.Duration(math.Ceil(float64(total) * quantile))
	cumulative := time.Duration(0)
	for _, count := range counts {
		cumulative += durations[count]
		if cumulative >= target {
			return count
		}
	}
	return counts[len(counts)-1]
}

func minTime(a time.Time, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func maxTime(a time.Time, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
