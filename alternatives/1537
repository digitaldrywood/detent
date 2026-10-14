package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func readHealthRates(ctx context.Context, q nativeQueryer, organization tracker.OrganizationID, now time.Time, runners []healthRunnerState, snapshot *healthSnapshot) error {
	for _, project := range snapshot.Projects {
		p := healthRateProject{ID: project.ID, Candidates: project.Candidates}
		scope := nativeScope{organization: organization, project: tracker.ProjectID(p.ID)}
		window := operatortool.AnalyticsWindow{From: now.Add(-healthBaselineWindow - healthLongWindow), To: now}
		states, issues, partial, err := readAnalyticsResidenceHistory(ctx, q, scope, window)
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
	rows, err := q.QueryContext(ctx, `SELECT a.id,a.work_item_id,a.status,a.updated_at,
 json_object('terminal_failure',json_extract(a.data_json,'$.terminal_failure'),'finalization',json_extract(a.data_json,'$.finalization'),'runtime',json_object('heartbeat_at',json_extract(a.data_json,'$.runtime.heartbeat_at'),'landing',json_extract(a.data_json,'$.runtime.landing'))),
 (SELECT coalesce(sum(u.cost_estimate),0) FROM attempt_usage u WHERE u.organization_id=a.organization_id AND u.project_id=a.project_id AND u.attempt_id=a.id AND u.currency='USD' AND julianday(u.updated_at)<=julianday(?))
 FROM native_attempts a JOIN projects p ON p.id=a.project_id
 WHERE a.organization_id=? AND a.project_id=? AND p.profile='native'
 AND (julianday(a.updated_at)>=julianday(?) OR julianday(json_extract(a.data_json,'$.runtime.landing.observed_at'))>=julianday(?))
 AND julianday(a.started_at)<julianday(?) ORDER BY a.fencing_token LIMIT ?`, formatHubTime(window.To), scope.organization, scope.project, formatHubTime(window.From), formatHubTime(window.From), formatHubTime(window.To), healthReadLimit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []healthRateAttempt{}
	landed := map[string]bool{}
	for rows.Next() {
		if len(out) == healthReadLimit {
			return nil, errHealthReadLimit
		}
		var a healthRateAttempt
		var at, raw string
		if err := rows.Scan(&a.ID, &a.Item, &a.Status, &at, &raw, &a.Cost); err != nil {
			return nil, err
		}
		if a.At, err = parseTimeValue(at); err != nil {
			return nil, err
		}
		var data tracker.NativeRunData
		if err := json.Unmarshal([]byte(raw), &data); err != nil {
			return nil, err
		}
		text, _ := failureFromAttempt(data, a.Status)
		a.Recovery = healthRecoveryReason(text)
		if text != "" && a.Recovery == "" {
			a.Signature = normalizeFailureSignature(text)
		}
		if data.Runtime != nil {
			a.Heartbeat = data.Runtime.HeartbeatAt
			if l := data.Runtime.Landing; l != nil && (l.Landed || l.RefusalKind != "") {
				a.Landing, a.Landed, a.Conflict = true, l.Landed, !l.Landed && l.RefusalKind == "conflict"
				a.LandingAt = l.ObservedAt
				if a.LandingAt.IsZero() {
					a.LandingAt = a.At
				}
				if l.Landed && l.ChangeID != "" {
					key := l.ChangeID + "\x00" + l.VersionID + "\x00" + l.MergeSHA
					a.LandingKey = key
					if landed[key] {
						a.Landing, a.Landed = false, false
					}
					landed[key] = true
				}
			}
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
