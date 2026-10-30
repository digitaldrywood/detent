package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"sync"

	"github.com/digitaldrywood/detent/internal/hubanalytics"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
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

const activityDurationQuery = `WITH samples AS (
 SELECT d.attempt_id,d.status,d.role,d.seconds,d.partial,d.started_at,a.work_item_id,a.activity_projection_version,'' AS data_json,
 coalesce(lr.runner_id,a.runner_id,'') AS runner_id,i.activity_actor_principal_id,CASE WHEN i.activity_actor_principal_id IS NULL THEN i.actor_json ELSE '' END AS actor_json
 FROM activity_attempt_durations d LEFT JOIN lease_runners lr ON lr.lease_id=(SELECT a.lease_id FROM native_attempts a WHERE a.id=d.attempt_id)
 JOIN native_attempts a ON a.id=d.attempt_id
 JOIN issues i ON i.organization_id=a.organization_id AND i.project_id=a.project_id AND i.native_id=a.work_item_id
 WHERE d.organization_id=?1 AND d.project_id=?2 AND d.updated_at>=julianday(?3) AND d.started_at<julianday(?4)
 AND a.activity_projection_version IS NOT NULL AND (?5='' OR coalesce(lr.runner_id,a.runner_id,'')=?6)
 UNION ALL
 SELECT a.id,a.status,'',0,1,a.activity_started_at,a.work_item_id,a.activity_projection_version,COALESCE(NULLIF(a.payload_object_key,''),COALESCE(NULLIF(a.payload_object_key,''),COALESCE(NULLIF(a.payload_object_key,''),a.data_json))),
 coalesce(lr.runner_id,''),i.activity_actor_principal_id,CASE WHEN i.activity_actor_principal_id IS NULL THEN i.actor_json ELSE '' END
 FROM native_attempts a LEFT JOIN lease_runners lr ON lr.lease_id=a.lease_id
 JOIN issues i ON i.organization_id=a.organization_id AND i.project_id=a.project_id AND i.native_id=a.work_item_id
 WHERE a.organization_id=?1 AND a.project_id=?2 AND a.activity_updated_at>=julianday(?3) AND a.activity_started_at<julianday(?4)
 AND a.activity_projection_version IS NULL
)
SELECT attempt_id,status,role,seconds,partial,activity_projection_version,data_json,runner_id,activity_actor_principal_id,actor_json FROM samples
WHERE 1 ORDER BY started_at,attempt_id LIMIT ?`

func (s *Service) readActivityTypicalDurations(ctx context.Context, q nativeQueryer, scope nativeScope, w operatortool.AnalyticsWindow, runner string) ([]operatortool.ActivityTypicalDuration, bool, error) {
	visibility, args := reportsIssueClause(ctx, "work_item_id")
	statement := strings.Replace(activityDurationQuery, "WHERE 1 ORDER BY", "WHERE 1 "+visibility+" ORDER BY", 1)
	args = append([]any{scope.organization, scope.project, formatHubTime(w.From), formatHubTime(w.To), runner, runner}, args...)
	args = append(args, hubanalytics.MaxPopulation+1)
	samples := []activityDurationSample{}
	partial := false
	offset := 0
	for {
		candidates, err := readActivityDurationCandidates(ctx, q, statement+" OFFSET ?", append(slices.Clone(args), offset))
		if err != nil {
			return nil, false, err
		}
		for _, c := range candidates {
			if visibility, ok := reportsScopeFrom(ctx); ok && visibility.reporterPrincipal != "" {
				principal := c.actor.String
				if !c.actor.Valid {
					var actor tracker.Actor
					if err := decodeStorageJSON(ctx, []byte(c.actorRaw), &actor); err != nil {
						return nil, false, err
					}
					principal = actor.PrincipalID
				}
				if principal == "" || principal != visibility.reporterPrincipal {
					continue
				}
			}
			if !c.ready.Valid {
				var data tracker.NativeRunData
				if err := decodeActivityRun(ctx, q, c.raw, &data); err != nil {
					return nil, false, err
				}
				if c.runner == "" {
					c.runner = data.RunnerID
				}
				phases, dropped := activityRunPhases(data)
				if data.Runtime != nil {
					c.sample.Role = data.Runtime.Identity.Role
				}
				c.sample.Partial = len(phases) == 0 || dropped > 0
				if c.sample.Status == "succeeded" || c.sample.Status == "failed" {
					c.sample.Seconds = closedAttemptSeconds(phases)
				}
			}
			if runner != "" && c.runner != runner {
				continue
			}
			if len(samples) == hubanalytics.MaxPopulation {
				partial = true
				break
			}
			partial = partial || c.sample.Partial
			samples = append(samples, c.sample)
		}
		if len(samples) == hubanalytics.MaxPopulation && partial || len(candidates) < hubanalytics.MaxPopulation+1 {
			break
		}
		offset += len(candidates)
		if offset >= hubanalytics.MaxPopulation {
			return nil, false, errHubOperatorUnavailable
		}
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

type activityDurationCandidate struct {
	sample                activityDurationSample
	ready                 sql.NullInt64
	raw, runner, actorRaw string
	actor                 sql.NullString
}

func readActivityDurationCandidates(ctx context.Context, q nativeQueryer, statement string, args []any) ([]activityDurationCandidate, error) {
	rows, err := q.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, err
	}

	defer rows.Close()
	var candidates []activityDurationCandidate
	for rows.Next() {
		var c activityDurationCandidate
		if err := rows.Scan(&c.sample.ID, &c.sample.Status, &c.sample.Role, &c.sample.Seconds, &c.sample.Partial, &c.ready, &c.raw, &c.runner, &c.actor, &c.actorRaw); err != nil {
			rows.Close()
			return nil, err
		}
		candidates = append(candidates, c)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	return candidates, nil
}
