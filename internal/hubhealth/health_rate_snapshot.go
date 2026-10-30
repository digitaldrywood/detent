package hubhealth

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"time"

	"github.com/digitaldrywood/detent/internal/hubanalytics"
	"github.com/digitaldrywood/detent/internal/hubsql"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (reader *SnapshotReader) ReadRates(ctx context.Context, q hubsql.Queryer, organization tracker.OrganizationID, now time.Time, runners []RunnerState, snapshot *Snapshot) error {
	for _, project := range snapshot.Projects {
		p := RateProject{ID: project.ID, Candidates: project.Candidates}
		scope := Scope{Organization: organization, Project: tracker.ProjectID(p.ID)}
		window := operatortool.AnalyticsWindow{From: now.Add(-BaselineWindow - LongWindow), To: now}
		states, issues, partial, err := reader.hooks.ResidenceHistory(ctx, q, scope, window)
		if err != nil {
			return err
		}
		p.ResidencePartial = partial
		for _, issue := range issues {
			p.Timelines = append(p.Timelines, hubanalytics.BuildResidenceTimeline(issue, states, now))
		}
		p.Attempts, err = reader.ReadRateAttempts(ctx, q, scope, window)
		if err != nil {
			return err
		}
		if err := reader.ReadProductivityAttempts(ctx, q, scope, p.Attempts); err != nil {
			return err
		}
		landings, err := reader.ReadLandings(ctx, q, scope, window)
		if err != nil {
			return err
		}
		if len(landings) > hubanalytics.MaxPopulation {
			return ErrReadLimit
		}
		canonical := map[string]bool{}
		for _, l := range landings {
			key := l.ChangeID + "\x00" + l.Landing.VersionID + "\x00" + l.Landing.MergeSHA
			canonical[key] = true
			p.Attempts = append(p.Attempts, RateAttempt{ID: l.ChangeID, Item: string(l.WorkItemID), Landing: true, Landed: true, LandingAt: l.Landing.LandedAt})
		}
		for i := range p.Attempts {
			if canonical[p.Attempts[i].LandingKey] {
				p.Attempts[i].Landing, p.Attempts[i].Landed = false, false
			}
		}
		p.Costs, err = reader.ReadItemCosts(ctx, q, scope, now)
		if err != nil {
			return err
		}
		p.BaselineCosts, err = reader.ReadItemCosts(ctx, q, scope, now.Add(-LongWindow))
		if err != nil {
			return err
		}
		for _, r := range runners {
			if r.Routing.State == "active" {
				for _, id := range r.Projects {
					if id == p.ID {
						p.Runners = append(p.Runners, r.Runner)
						break
					}
				}
			}
		}
		snapshot.BaselineUnavailable = append(snapshot.BaselineUnavailable, BaselineGaps(p, BuildRateBaseline(now, p))...)
		snapshot.Rates = append(snapshot.Rates, p)
	}
	return nil
}

func (reader *SnapshotReader) ReadLandings(ctx context.Context, q hubsql.Queryer, scope Scope, window operatortool.AnalyticsWindow) ([]hubanalytics.Landing, error) {
	rows, err := q.QueryContext(ctx, `SELECT id,work_item_id,record_json,legacy FROM (
	 SELECT id,work_item_id,record_json,0 AS legacy FROM (SELECT id,work_item_id,record_json FROM change_requests INDEXED BY change_requests_analytics_landed WHERE organization_id=? AND project_id=? AND landed_at>=? AND landed_at<? ORDER BY landed_at,id LIMIT ?)
	 UNION ALL SELECT id,work_item_id,record_json,1 FROM (SELECT id,work_item_id,record_json FROM change_requests INDEXED BY change_requests_policy_unknown WHERE organization_id=? AND project_id=? AND created_at IS NULL LIMIT ?)
)`, scope.Organization, scope.Project, reader.hooks.ColumnTime(window.From), reader.hooks.ColumnTime(window.To), ReadLimit+1, scope.Organization, scope.Project, reader.hooks.LegacyReadLimit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var candidates []hubanalytics.Landing
	legacy, converted := 0, 0
	for rows.Next() {
		var candidate hubanalytics.Landing
		var raw string
		var pending bool
		if err := rows.Scan(&candidate.ChangeID, &candidate.WorkItemID, &raw, &pending); err != nil {
			return nil, err
		}
		if pending {
			legacy++
		} else {
			converted++
		}
		if legacy > reader.hooks.LegacyReadLimit || converted > ReadLimit {
			return nil, ErrReadLimit
		}
		var change tracker.ChangeRequest
		if err := reader.hooks.DecodeChangeJSON(ctx, []byte(raw), &change); err != nil {
			return nil, err
		}
		if change.Landed == nil || change.Landed.LandedAt.Before(window.From) || !change.Landed.LandedAt.Before(window.To) {
			continue
		}
		candidate.Landing = *change.Landed
		candidates = append(candidates, candidate)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	landings := []hubanalytics.Landing{}
	for _, candidate := range candidates {
		var head sql.NullString
		var raw string
		err := q.QueryRowContext(ctx, `SELECT head_sha,CASE WHEN head_sha IS NULL THEN COALESCE(NULLIF(payload_object_key,''),record_json) ELSE '' END FROM change_versions WHERE id=? AND change_id=?`, candidate.Landing.VersionID, candidate.ChangeID).Scan(&head, &raw)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !head.Valid {
			var version tracker.ChangeVersion
			if err := reader.hooks.DecodeChangeJSON(ctx, []byte(raw), &version); err != nil {
				return nil, err
			}
			head.String = version.HeadSHA
		}
		if candidate.Landing.HeadSHA == head.String && (len(candidate.Landing.MergeSHA) == 40 || len(candidate.Landing.MergeSHA) == 64) {
			candidate.Landing.Quality = nil
			landings = append(landings, candidate)
		}
	}
	return landings, nil
}

func (reader *SnapshotReader) ReadRateAttempts(ctx context.Context, q hubsql.Queryer, scope Scope, window operatortool.AnalyticsWindow) ([]RateAttempt, error) {
	var legacy bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM native_attempts WHERE organization_id=? AND project_id=? AND hot_projection_version=0)`, scope.Organization, scope.Project).Scan(&legacy); err != nil {
		return nil, err
	}
	if legacy {
		return reader.ReadLegacyHealthRateAttempts(ctx, q, scope, window)
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
		scope.Organization, scope.Project, formatHubTime(window.From), formatHubTime(window.To), ReadLimit+1,
		scope.Organization, scope.Project, formatHubTime(window.From), formatHubTime(window.From), formatHubTime(window.To), ReadLimit+1,
		formatHubTime(window.To))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RateAttempt{}
	fences := map[string]int64{}
	for rows.Next() {
		var a RateAttempt
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
		if len(out) == ReadLimit {
			return nil, ErrReadLimit
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

func (reader *SnapshotReader) ReadLegacyHealthRateAttempts(ctx context.Context, q hubsql.Queryer, scope Scope, window operatortool.AnalyticsWindow) ([]RateAttempt, error) {
	rows, err := q.QueryContext(ctx, `SELECT a.id,a.work_item_id,a.status,a.updated_at,
 COALESCE((SELECT lr.runner_id FROM lease_runners lr WHERE lr.lease_id=a.lease_id),''),
 CASE WHEN a.hot_projection_version=0 THEN COALESCE(NULLIF(a.payload_object_key,''),a.data_json) ELSE '{}' END,
 a.hot_projection_version,a.heartbeat_at,a.health_failure_signature,a.health_recovery_reason,a.health_instance_failure,a.health_landing,a.health_landed,a.health_conflict,a.health_landing_at,a.health_landing_key,
 (SELECT coalesce(sum(u.cost_estimate),0) FROM attempt_usage u WHERE u.organization_id=a.organization_id AND u.project_id=a.project_id AND u.attempt_id=a.id AND u.currency='USD' AND julianday(u.updated_at)<=julianday(?))
 FROM native_attempts a JOIN projects p ON p.id=a.project_id
 WHERE a.organization_id=? AND a.project_id=? AND p.profile='native'
 AND (a.hot_projection_version=0 OR julianday(a.updated_at)>=julianday(?) OR a.health_landing_day>=julianday(?))
 AND julianday(a.started_at)<julianday(?) ORDER BY a.fencing_token LIMIT ?`, formatHubTime(window.To), scope.Organization, scope.Project, formatHubTime(window.From), formatHubTime(window.From), formatHubTime(window.To), 2*ReadLimit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RateAttempt{}
	landed := map[string]bool{}
	scanned := 0
	for rows.Next() {
		scanned++
		if scanned > 2*ReadLimit {
			return nil, ErrReadLimit
		}
		if len(out) == ReadLimit {
			return nil, ErrReadLimit
		}
		var a RateAttempt
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
			if err := reader.hooks.DecodeStorageJSON(ctx, []byte(raw), &data); err != nil {
				return nil, err
			}
			reader.hooks.ApplyAttemptData(&a, data)
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

func (reader *SnapshotReader) ReadItemCosts(ctx context.Context, q hubsql.Queryer, scope Scope, now time.Time) (map[string]ItemCost, error) {
	rows, err := q.QueryContext(ctx, `SELECT a.work_item_id,sum(u.cost_estimate)
 FROM attempt_usage u JOIN native_attempts a ON a.id=u.attempt_id AND a.organization_id=u.organization_id AND a.project_id=u.project_id
 WHERE a.organization_id=? AND a.project_id=? AND u.currency='USD' AND julianday(u.updated_at)<=julianday(?)
 GROUP BY a.work_item_id ORDER BY a.work_item_id LIMIT ?`, scope.Organization, scope.Project, formatHubTime(now), ReadLimit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]ItemCost{}
	for rows.Next() {
		if len(out) == ReadLimit {
			return nil, ErrReadLimit
		}
		var id string
		var cost float64
		if err := rows.Scan(&id, &cost); err != nil {
			return nil, err
		}
		out[id] = ItemCost{Cost: cost, Known: true}
	}
	return out, errors.Join(rows.Err(), rows.Close())
}
