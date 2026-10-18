package hubserver

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"

	"github.com/digitaldrywood/detent/internal/operatortool"
)

type activityDurationSample struct {
	ID      string
	Status  string
	Role    string
	Seconds float64
	Partial bool
}

type activityDurationEntry struct {
	project string
	samples []activityDurationSample
	typical []operatortool.ActivityTypicalDuration
	partial bool
}

type activityDurationCache struct {
	mu      sync.Mutex
	entries []activityDurationEntry
}

const activityDurationQuery = `SELECT d.attempt_id,d.status,d.role,d.seconds,d.partial
FROM activity_attempt_durations d LEFT JOIN lease_runners lr ON lr.lease_id=(SELECT a.lease_id FROM native_attempts a WHERE a.id=d.attempt_id)
WHERE d.organization_id=? AND d.project_id=? AND d.updated_at>=julianday(?) AND d.started_at<julianday(?)
AND (?='' OR coalesce(lr.runner_id,(SELECT json_extract(a.data_json,'$.runner_id') FROM native_attempts a WHERE a.id=d.attempt_id),'')=?)
ORDER BY d.started_at,d.attempt_id LIMIT ?`

func (s *Service) readActivityTypicalDurations(ctx context.Context, q nativeQueryer, scope nativeScope, w operatortool.AnalyticsWindow, runner string) ([]operatortool.ActivityTypicalDuration, bool, error) {
	visibility, args := reportsIssueClause(ctx, "(SELECT a.work_item_id FROM native_attempts a WHERE a.id=d.attempt_id)")
	statement := strings.Replace(activityDurationQuery, "ORDER BY", visibility+" ORDER BY", 1)
	args = append([]any{scope.organization, scope.project, formatHubTime(w.From), formatHubTime(w.To), runner, runner}, args...)
	args = append(args, maxAnalyticsPopulation+1)
	rows, err := q.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	samples := []activityDurationSample{}
	partial := false
	for rows.Next() {
		var sample activityDurationSample
		if err := rows.Scan(&sample.ID, &sample.Status, &sample.Role, &sample.Seconds, &sample.Partial); err != nil {
			return nil, false, err
		}
		if len(samples) == maxAnalyticsPopulation {
			partial = true
			break
		}
		partial = partial || sample.Partial
		samples = append(samples, sample)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, false, err
	}
	return s.activityDurations.typical(string(scope.project), samples, partial), partial, nil
}

func (c *activityDurationCache) typical(project string, samples []activityDurationSample, partial bool) []operatortool.ActivityTypicalDuration {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, entry := range c.entries {
		if entry.project == project && entry.partial == partial && slices.Equal(entry.samples, samples) {
			return slices.Clone(entry.typical)
		}
	}
	typical := activityTypicalDurations(project, samples, partial)
	if len(c.entries) == 64 {
		c.entries = c.entries[1:]
	}
	c.entries = append(c.entries, activityDurationEntry{project: project, samples: samples, typical: typical, partial: partial})
	return slices.Clone(typical)
}
