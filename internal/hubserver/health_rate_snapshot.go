package hubserver

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func readHealthRates(ctx context.Context, q nativeQueryer, organization tracker.OrganizationID, now time.Time, runners []healthRunnerState, snapshot *healthSnapshot) error {
	for _, project := range snapshot.Projects {
		p := healthRateProject{ID: project.ID, Candidates: project.Candidates}
		scope := nativeScope{organization: organization, project: tracker.ProjectID(p.ID)}
		window := operatortool.AnalyticsWindow{From: now.Add(-healthBaselineWindow - healthLongWindow), To: now}
		states, issues, partial, err := readAnalyticsResidenceHistory(withAnalyticsLiveIssues(ctx), q, scope, window)
		if err != nil {
			return err
		}
		p.ResidencePartial = partial
		for _, issue := range issues {
			p.Timelines = append(p.Timelines, buildAnalyticsResidenceTimeline(issue, states, now))
		}
		p.Attempts, err = readHealthRateAttempts(ctx, q, scope, window)
		if err != nil {
			return err
		}
		if err := readHealthProductivityAttempts(ctx, q, scope, p.Attempts); err != nil {
			return err
		}
		landings, err := nativeAnalyticsLandings(ctx, q, scope, window)
		if err != nil {
			return err
		}
		if len(landings) > maxAnalyticsPopulation {
			return errHealthReadLimit
		}
		canonical := map[string]bool{}
		for _, l := range landings {
			key := l.ChangeID + "\x00" + l.Landing.VersionID + "\x00" + l.Landing.MergeSHA
			canonical[key] = true
			p.Attempts = append(p.Attempts, healthRateAttempt{ID: l.ChangeID, Item: string(l.WorkItemID), Landing: true, Landed: true, LandingAt: l.Landing.LandedAt})
		}
		for i := range p.Attempts {
			if canonical[p.Attempts[i].LandingKey] {
				p.Attempts[i].Landing, p.Attempts[i].Landed = false, false
			}
		}
		p.Costs, err = readHealthItemCosts(ctx, q, scope, now)
		if err != nil {
			return err
		}
		p.BaselineCosts, err = readHealthItemCosts(ctx, q, scope, now.Add(-healthLongWindow))
		if err != nil {
			return err
		}
		for _, r := range runners {
			if r.Routing.State == "active" {
				for _, id := range r.Projects {
					if id == p.ID {
						p.Runners = append(p.Runners, r.healthRunner)
						break
					}
				}
			}
		}
		snapshot.BaselineUnavailable = append(snapshot.BaselineUnavailable, healthBaselineGaps(p, buildHealthRateBaseline(now, p))...)
		snapshot.Rates = append(snapshot.Rates, p)
	}
	return nil
}

func readHealthRateAttempts(ctx context.Context, q nativeQueryer, scope nativeScope, window operatortool.AnalyticsWindow) ([]healthRateAttempt, error) {
	var legacy bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM native_attempts WHERE organization_id=? AND project_id=? AND hot_projection_version=0)`, scope.organization, scope.project).Scan(&legacy); err != nil {
		return nil, err
	}
	if legacy {
		return readLegacyHealthRateAttempts(ctx, q, scope, window)
	}
	rows, err := q.QueryContext(ctx, `WITH candidates AS (
 SELECT id FROM (SELECT id FROM native_attempts WHERE organization_id=? AND project_id=? AND hot_projection_version=1 AND health_updated_day>=julianday(?) AND julianday(started_at)<julianday(?) LIMIT ?)
 UNION ALL SELECT id FROM (SELECT id FROM native_attempts WHERE organization_id=? AND project_id=? AND hot_projection_version=1 AND health_landing_day>=julianday(?) AND health_updated_day<julianday(?) AND julianday(started_at)<julianday(?) LIMIT ?)
)
SELECT a.id,a.work_item_id,a.status,a.updated_at,
 COALESCE((SELECT lr.runner_id FROM lease_runners lr WHERE lr.lease_id=a.lease_id),''),
 a.heartbeat_at,a.health_failure_signature,a.health_recovery_reason,a.health_instance_failure,
 a.health_landing,a.health_landed,a.health_conflict,a.health_landing_at,a.health_landing_key,a.fencing_token,
 (SELECT coalesce(sum(u.cost_estimate),0) FROM attempt_usage u WHERE u.organization_id=a.organization_id AND u.project_id=a.project_id AND u.attempt_id=a.id AND u.currency='USD' AND julianday(u.updated_at)<=julianday(?))
 FROM candidates c JOIN native_attempts a ON a.id=c.id JOIN projects p ON p.id=a.project_id
 WHERE p.profile='native'`,
		scope.organization, scope.project, formatHubTime(window.From), formatHubTime(window.To), healthReadLimit+1,
		scope.organization, scope.project, formatHubTime(window.From), formatHubTime(window.From), formatHubTime(window.To), healthReadLimit+1,
		formatHubTime(window.To))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []healthRateAttempt{}
	fences := map[string]int64{}
	for rows.Next() {
		var a healthRateAttempt
		var at, heartbeat, landingAt string
		var fence int64
		if err := rows.Scan(&a.ID, &a.Item, &a.Status, &at, &a.Runner, &heartbeat, &a.Signature, &a.Recovery, &a.InstanceSetupFailure, &a.Landing, &a.Landed, &a.Conflict, &landingAt, &a.LandingKey, &fence, &a.Cost); err != nil {
			return nil, err
		}
		if a.At, err = parseTimeValue(at); err != nil {
			return nil, err
		}
		if heartbeat != "" {
			a.Heartbeat, err = parseTimeValue(heartbeat)
			if err != nil {
				return nil, err
			}
		}
		if landingAt != "" && a.Landing {
			a.LandingAt, err = parseTimeValue(landingAt)
			if err != nil {
				return nil, err
			}
		}
		if a.Landing && a.LandingAt.IsZero() {
			a.LandingAt = a.At
		}
		if len(out) == healthReadLimit {
			return nil, errHealthReadLimit
		}
		fences[a.ID] = fence
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return fences[out[i].ID] < fences[out[j].ID] })
	landed := map[string]bool{}
	for i := range out {
		a := &out[i]
		if a.Landed && a.LandingKey != "" {
			if landed[a.LandingKey] {
				a.Landing, a.Landed = false, false
			}
			landed[a.LandingKey] = true
		}
	}
	return out, rows.Err()
}

func readLegacyHealthRateAttempts(ctx context.Context, q nativeQueryer, scope nativeScope, window operatortool.AnalyticsWindow) ([]healthRateAttempt, error) {
	rows, err := q.QueryContext(ctx, `SELECT a.id,a.work_item_id,a.status,a.updated_at,
 COALESCE((SELECT lr.runner_id FROM lease_runners lr WHERE lr.lease_id=a.lease_id),''),
 CASE WHEN a.hot_projection_version=0 THEN COALESCE(NULLIF(a.payload_object_key,''),a.data_json) ELSE '{}' END,
 a.hot_projection_version,a.heartbeat_at,a.health_failure_signature,a.health_recovery_reason,a.health_instance_failure,a.health_landing,a.health_landed,a.health_conflict,a.health_landing_at,a.health_landing_key,
 (SELECT coalesce(sum(u.cost_estimate),0) FROM attempt_usage u WHERE u.organization_id=a.organization_id AND u.project_id=a.project_id AND u.attempt_id=a.id AND u.currency='USD' AND julianday(u.updated_at)<=julianday(?))
 FROM native_attempts a JOIN projects p ON p.id=a.project_id
 WHERE a.organization_id=? AND a.project_id=? AND p.profile='native'
 AND (a.hot_projection_version=0 OR julianday(a.updated_at)>=julianday(?) OR a.health_landing_day>=julianday(?))
 AND julianday(a.started_at)<julianday(?) ORDER BY a.fencing_token LIMIT ?`, formatHubTime(window.To), scope.organization, scope.project, formatHubTime(window.From), formatHubTime(window.From), formatHubTime(window.To), 2*healthReadLimit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []healthRateAttempt{}
	landed := map[string]bool{}
	scanned := 0
	for rows.Next() {
		scanned++
		if scanned > 2*healthReadLimit {
			return nil, errHealthReadLimit
		}
		if len(out) == healthReadLimit {
			return nil, errHealthReadLimit
		}
		var a healthRateAttempt
		var at, raw, heartbeat, landingAt string
		var version int
		if err := rows.Scan(&a.ID, &a.Item, &a.Status, &at, &a.Runner, &raw, &version, &heartbeat, &a.Signature, &a.Recovery, &a.InstanceSetupFailure, &a.Landing, &a.Landed, &a.Conflict, &landingAt, &a.LandingKey, &a.Cost); err != nil {
			return nil, err
		}
		if a.At, err = parseTimeValue(at); err != nil {
			return nil, err
		}
		if version == 0 {
			var data tracker.NativeRunData
			if err := decodeStorageJSON(ctx, []byte(raw), &data); err != nil {
				return nil, err
			}
			applyHealthAttemptData(&a, data)
			a.Session, a.Stage, a.SourceVersion = "", "", ""
			landingObservation := time.Time{}
			if data.Runtime != nil && data.Runtime.Landing != nil {
				landingObservation = data.Runtime.Landing.ObservedAt
			}
			if a.At.Round(time.Millisecond).Before(window.From.Round(time.Millisecond)) && landingObservation.Round(time.Millisecond).Before(window.From.Round(time.Millisecond)) {
				continue
			}
		} else {
			if heartbeat != "" {
				a.Heartbeat, err = parseTimeValue(heartbeat)
				if err != nil {
					return nil, err
				}
			}
			if a.Landing && landingAt != "" {
				a.LandingAt, err = parseTimeValue(landingAt)
				if err != nil {
					return nil, err
				}
			}
		}
		if a.Landing && a.LandingAt.IsZero() {
			a.LandingAt = a.At
		}
		if a.Landed && a.LandingKey != "" {
			if landed[a.LandingKey] {
				a.Landing, a.Landed = false, false
			}
			landed[a.LandingKey] = true
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func readHealthItemCosts(ctx context.Context, q nativeQueryer, scope nativeScope, now time.Time) (map[string]healthItemCost, error) {
	rows, err := q.QueryContext(ctx, `SELECT a.work_item_id,sum(u.cost_estimate)
 FROM attempt_usage u JOIN native_attempts a ON a.id=u.attempt_id AND a.organization_id=u.organization_id AND a.project_id=u.project_id
 WHERE a.organization_id=? AND a.project_id=? AND u.currency='USD' AND julianday(u.updated_at)<=julianday(?)
 GROUP BY a.work_item_id ORDER BY a.work_item_id LIMIT ?`, scope.organization, scope.project, formatHubTime(now), healthReadLimit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]healthItemCost{}
	for rows.Next() {
		if len(out) == healthReadLimit {
			return nil, errHealthReadLimit
		}
		var id string
		var cost float64
		if err := rows.Scan(&id, &cost); err != nil {
			return nil, err
		}
		out[id] = healthItemCost{Cost: cost, Known: true}
	}
	return out, errors.Join(rows.Err(), rows.Close())
}
