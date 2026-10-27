package hubserver

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const activityIssueHistoryLimit = 1000

const activityIssueSamplesQuery = `SELECT data_json FROM analytics_hourly_rollups
WHERE organization_id=? AND project_id=? AND metric='residence_edge'
AND substr(dimension,1,instr(dimension,':')-1)=? ORDER BY hour DESC,value DESC LIMIT ?`

const activityIssueCandidatesQuery = `WITH live AS MATERIALIZED (
 SELECT a.work_item_id FROM native_attempts a JOIN leases l ON l.lease_id=a.lease_id
 WHERE a.organization_id=:org AND a.project_id=:project AND a.status='running'
 AND l.released_at IS NULL AND julianday(l.renewed_at)<=julianday(:now) AND julianday(l.expires_at)>julianday(:now)
 GROUP BY a.work_item_id LIMIT :limit
), finished AS MATERIALIZED (
 SELECT native_id FROM issues WHERE organization_id=:org AND project_id=:project
 AND terminal_entered_at>=:from ORDER BY terminal_entered_at DESC,native_id LIMIT :limit
), selected AS MATERIALIZED (SELECT work_item_id AS id FROM live UNION ALL SELECT native_id FROM finished)
SELECT DISTINCT i.native_id,i.number,i.title,i.actor_json,ws.detent_state,i.updated_at
FROM selected CROSS JOIN issues i ON i.organization_id=:org AND i.project_id=:project AND i.native_id=selected.id
JOIN workflow_states ws ON ws.id=i.workflow_state_id`

type activityIssueCandidate struct {
	issue             operatortool.ActivityIssue
	actorRaw, updated string
}

func readActivityIssueCandidates(ctx context.Context, q nativeQueryer, organization, project string, from, now time.Time, limit int) ([]activityIssueCandidate, error) {
	rows, err := q.QueryContext(ctx, activityIssueCandidatesQuery,
		sql.Named("org", organization), sql.Named("project", project), sql.Named("from", formatHubTime(from.Truncate(time.Second))), sql.Named("now", formatHubTime(now)), sql.Named("limit", limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	candidates := []activityIssueCandidate{}
	for rows.Next() {
		var candidate activityIssueCandidate
		issue := &candidate.issue
		if err := rows.Scan(&issue.WorkItemID, &issue.Number, &issue.Title, &candidate.actorRaw, &issue.CurrentLane, &candidate.updated); err != nil {
			return nil, err
		}
		candidates = append(candidates, candidate)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	slices.SortFunc(candidates, func(a, b activityIssueCandidate) int {
		return cmp.Or(cmp.Compare(b.updated, a.updated), cmp.Compare(a.issue.WorkItemID, b.issue.WorkItemID))
	})
	return candidates[:min(len(candidates), limit)], nil
}

type activityWorkInterval struct{ from, to time.Time }
type activityIssueAttempt struct {
	run                   tracker.NativeRunData
	started, ended        time.Time
	role, runner, session string
	live                  bool
}

func activityLaneRole(lane tracker.NativeState) string {
	if lane.Terminal {
		return "terminal"
	}
	if lane.Dispatchable {
		return "active"
	}
	return "holding"
}

func activityIssueResidence(issue operatortool.ActivityIssue, lanes []tracker.NativeState, samples []analyticsResidenceSample, now time.Time) operatortool.ActivityIssue {
	issue.Lanes = []operatortool.ActivityLane{}
	issue.RunnerIDs = []string{}
	issue.WorkedByRole = []operatortool.ActivityRoleWork{}
	if len(samples) == 0 {
		issue.Partial = true
		for _, lane := range lanes {
			if activityLaneRole(lane) == "active" {
				issue.Lanes = append(issue.Lanes, operatortool.ActivityLane{Name: lane.Name, Role: "active"})
			}
		}
		return issue
	}
	slices.SortFunc(samples, func(a, b analyticsResidenceSample) int {
		return cmp.Or(a.At.Compare(b.At), cmp.Compare(a.Sequence, b.Sequence))
	})
	firstActive := slices.IndexFunc(lanes, func(l tracker.NativeState) bool { return l.Dispatchable && !l.Terminal })
	durations := map[string]float64{}
	entries := map[string]int{}
	state := ""
	entered := time.Time{}
	known := samples[0].Sequence == 0
	visit := func(name string, at time.Time) {
		if name == state {
			return
		}
		if !issue.ClockStartedAt.IsZero() && !entered.IsZero() {
			durations[state] += max(0, at.Sub(entered).Seconds())
		}
		state, entered = name, at
		index := slices.IndexFunc(lanes, func(l tracker.NativeState) bool { return l.Name == name })
		if index < 0 {
			issue.Partial = true
			return
		}
		lane := lanes[index]
		if !lane.Dispatchable && !lane.Terminal && index < firstActive {
			known = true
			issue.ClockStartedAt = time.Time{}
			issue.ClockLane = ""
			issue.FinishedAt = nil
			issue.TerminalLane = ""
			durations = map[string]float64{}
			entries = map[string]int{}
			return
		}
		if known && issue.ClockStartedAt.IsZero() && lane.Dispatchable && !lane.Terminal {
			issue.ClockStartedAt = at
			issue.ClockLane = name
		}
		if !issue.ClockStartedAt.IsZero() {
			entries[name]++
			if lane.Terminal {
				end := at
				issue.FinishedAt = &end
				issue.TerminalLane = name
			}
			if !lane.Terminal {
				issue.FinishedAt = nil
				issue.TerminalLane = ""
			}
		}
	}
	first := samples[0]
	if first.Sequence == 0 {
		visit(first.Initial, first.Created)
	} else {
		issue.Partial = true
		visit(first.State, first.Entered)
	}
	for _, sample := range samples {
		if sample.At.After(now) {
			break
		}
		issue.Partial = issue.Partial || sample.Partial
		visit(sample.State, sample.Entered)
	}
	issue.CurrentLane = state
	end := now
	if issue.FinishedAt != nil {
		end = *issue.FinishedAt
	}
	if !issue.ClockStartedAt.IsZero() {
		durations[state] += max(0, end.Sub(entered).Seconds())
		issue.TotalSeconds = max(0, end.Sub(issue.ClockStartedAt).Seconds())
	}
	for _, lane := range lanes {
		role := activityLaneRole(lane)
		if role == "terminal" || role == "holding" && entries[lane.Name] == 0 {
			continue
		}
		issue.Lanes = append(issue.Lanes, operatortool.ActivityLane{Name: lane.Name, Role: role, Seconds: durations[lane.Name], Entries: entries[lane.Name], Current: lane.Name == state && issue.FinishedAt == nil})
	}
	return issue
}

func activityIntervalSeconds(intervals []activityWorkInterval) float64 {
	slices.SortFunc(intervals, func(a, b activityWorkInterval) int { return a.from.Compare(b.from) })
	var total float64
	var end time.Time
	for _, interval := range intervals {
		from := maxTime(interval.from, end)
		if interval.to.After(from) {
			total += interval.to.Sub(from).Seconds()
		}
		end = maxTime(end, interval.to)
	}
	return total
}

func activityIssueWork(issue *operatortool.ActivityIssue, attempts []activityIssueAttempt, now time.Time) bool {
	end := now
	if issue.FinishedAt != nil {
		end = *issue.FinishedAt
	}
	byRole := map[string][]activityWorkInterval{}
	roles := map[string]operatortool.ActivityRoleWork{}
	all := []activityWorkInterval{}
	live := false
	slices.SortFunc(attempts, func(a, b activityIssueAttempt) int { return a.started.Compare(b.started) })
	for _, attempt := range attempts {
		live = live || attempt.live
		if attempt.ended.Before(issue.ClockStartedAt) || !attempt.started.Before(end) {
			continue
		}
		if attempt.runner != "" && !slices.Contains(issue.RunnerIDs, attempt.runner) {
			issue.RunnerIDs = append(issue.RunnerIDs, attempt.runner)
		}
		if attempt.live || issue.RunnerID == "" || !live {
			issue.RunnerID = attempt.runner
			issue.SessionID = attempt.session
		}
		if attempt.run.Finalization != nil {
			if attempt.run.Finalization.ChangeID != "" {
				issue.ChangeID = attempt.run.Finalization.ChangeID
			}
			if publication := attempt.run.Finalization.Publication; publication != nil && publication.External.URL != "" {
				issue.PullRequestURL = publication.External.URL
			}
		}
		if issue.ClockStartedAt.IsZero() {
			continue
		}
		role := roles[attempt.role]
		role.Role = attempt.role
		role.Attempts++
		role.Running = role.Running || attempt.live
		roles[attempt.role] = role
		runtime := attempt.run.Runtime
		if runtime == nil || len(runtime.Phases) == 0 {
			issue.Partial = true
			continue
		}
		issue.Partial = issue.Partial || runtime.PhasesDropped > 0
		for _, phase := range runtime.Phases {
			to := phase.FinishedAt
			if to.IsZero() {
				issue.WorkedRunning = issue.WorkedRunning || attempt.live
				to = attempt.ended
			}
			from := maxTime(phase.StartedAt, issue.ClockStartedAt)
			to = minTime(to, minTime(end, attempt.ended))
			if !phase.StartedAt.IsZero() && to.After(from) {
				interval := activityWorkInterval{from, to}
				all = append(all, interval)
				byRole[attempt.role] = append(byRole[attempt.role], interval)
			}
		}
	}
	issue.WorkedSeconds = activityIntervalSeconds(all)
	for key, role := range roles {
		role.Seconds = activityIntervalSeconds(byRole[key])
		issue.WorkedByRole = append(issue.WorkedByRole, role)
	}
	slices.SortFunc(issue.WorkedByRole, func(a, b operatortool.ActivityRoleWork) int { return cmp.Compare(a.Role, b.Role) })
	return live
}

func readActivityIssueSamples(ctx context.Context, q nativeQueryer, organization, project, id string) ([]analyticsResidenceSample, bool, error) {
	rows, err := q.QueryContext(ctx, activityIssueSamplesQuery, organization, project, id, activityIssueHistoryLimit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	samples := []analyticsResidenceSample{}
	for rows.Next() {
		var raw string
		var sample analyticsResidenceSample
		if err := rows.Scan(&raw); err != nil {
			return nil, false, err
		}
		if err := json.Unmarshal([]byte(raw), &sample); err != nil {
			return nil, false, err
		}
		samples = append(samples, sample)
	}
	partial := len(samples) > activityIssueHistoryLimit
	if partial {
		samples = samples[:activityIssueHistoryLimit]
	}
	return samples, partial, rows.Err()
}

func readActivityIssueAttempts(ctx context.Context, q nativeQueryer, organization, project, id string, start, now time.Time) ([]activityIssueAttempt, bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT a.data_json,a.started_at,a.updated_at,a.status,coalesce(a.role,''),coalesce(lr.runner_id,''),coalesce(l.session_id,''),coalesce(l.renewed_at,''),coalesce(l.expires_at,''),coalesce(l.released_at,'') FROM native_attempts a LEFT JOIN leases l ON l.lease_id=a.lease_id LEFT JOIN lease_runners lr ON lr.lease_id=a.lease_id WHERE a.organization_id=? AND a.project_id=? AND a.work_item_id=? AND (julianday(a.updated_at)>=julianday(?) OR (a.status='running' AND julianday(l.expires_at)>=julianday(?))) ORDER BY a.started_at DESC,a.id DESC LIMIT ?`, organization, project, id, formatHubTime(start), formatHubTime(start), activityIssueHistoryLimit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	attempts := []activityIssueAttempt{}
	for rows.Next() {
		var a activityIssueAttempt
		var raw, started, updated, status, renewed, expires, released string
		if err := rows.Scan(&raw, &started, &updated, &status, &a.role, &a.runner, &a.session, &renewed, &expires, &released); err != nil {
			return nil, false, err
		}
		if err := decodeRunDetail(ctx, q, raw, &a.run); err != nil {
			return nil, false, err
		}
		a.started, err = parseTimeValue(started)
		if err != nil {
			return nil, false, err
		}
		a.ended, err = parseTimeValue(updated)
		if err != nil {
			return nil, false, err
		}
		if a.role == "" {
			a.role = activityRunRole(a.run)
		}
		if a.runner == "" {
			a.runner = a.run.RunnerID
		}
		if a.session == "" {
			a.session = a.run.SessionID
		}
		if status == "running" {
			renewal, err := parseTimeValue(renewed)
			if err != nil {
				return nil, false, err
			}
			expiry, err := parseTimeValue(expires)
			if err != nil {
				return nil, false, err
			}
			a.live = released == "" && !renewal.After(now) && expiry.After(now)
			a.ended = minTime(expiry, now)
			if released != "" {
				at, err := parseTimeValue(released)
				if err != nil {
					return nil, false, err
				}
				a.ended = minTime(a.ended, at)
			}
		}
		attempts = append(attempts, a)
	}
	partial := len(attempts) > activityIssueHistoryLimit
	if partial {
		attempts = attempts[:activityIssueHistoryLimit]
	}
	return attempts, partial, rows.Err()
}

func readActivityIssues(ctx context.Context, q nativeQueryer, projects []string, r operatortool.ActivityRequest, out *operatortool.ActivityReport) error {
	out.RunningIssues = []operatortool.ActivityIssue{}
	out.FinishedIssues = []operatortool.ActivityIssue{}
	out.IssueTypicalDurations = []operatortool.ActivityIssueTypical{}
	remaining := maxAnalyticsPopulation
	for _, project := range projects {
		var name, raw string
		if err := q.QueryRowContext(ctx, `SELECT name,states_json FROM projects WHERE organization_id=? AND id=?`, out.OrganizationID, project).Scan(&name, &raw); err != nil {
			return err
		}
		var lanes []tracker.NativeState
		if err := json.Unmarshal([]byte(raw), &lanes); err != nil {
			return err
		}
		selected, err := readActivityIssueCandidates(ctx, q, out.OrganizationID, project, out.Window.From, out.ObservedAt, remaining+1)
		if err != nil {
			return err
		}
		candidates := []operatortool.ActivityIssue{}
		scanned := len(selected)
		for _, candidate := range selected {
			issue := candidate.issue
			issue.ProjectID, issue.ProjectName = project, name
			if visibility, ok := reportsScopeFrom(ctx); ok && visibility.reporterPrincipal != "" {
				var actor struct {
					PrincipalID string `json:"principal_id"`
				}
				if err := json.Unmarshal([]byte(candidate.actorRaw), &actor); err != nil {
					return err
				}
				if actor.PrincipalID != visibility.reporterPrincipal {
					continue
				}
			}
			candidates = append(candidates, issue)
		}
		partial := scanned > remaining
		if len(candidates) > remaining {
			candidates = candidates[:remaining]
		}
		remaining = max(0, remaining-scanned)
		typical := operatortool.ActivityIssueTypical{ProjectID: project, Lanes: map[string]operatortool.AnalyticsDuration{}}
		totals := []float64{}
		laneTimes := map[string][]float64{}
		for _, candidate := range candidates {
			samples, clipped, err := readActivityIssueSamples(ctx, q, out.OrganizationID, project, candidate.WorkItemID)
			if err != nil {
				return err
			}
			issue := activityIssueResidence(candidate, lanes, samples, out.ObservedAt)
			issue.Partial = issue.Partial || clipped
			if issue.ClockStartedAt.IsZero() {
				out.IssuesPartial = true
				typical.Partial = true
			}
			finished := issue.FinishedAt != nil && !issue.FinishedAt.Before(out.Window.From) && issue.FinishedAt.Before(out.Window.To)
			if finished {
				typical.Partial = typical.Partial || issue.Partial
				totals = append(totals, issue.TotalSeconds)
				for _, lane := range issue.Lanes {
					if lane.Entries > 0 {
						laneTimes[lane.Name] = append(laneTimes[lane.Name], lane.Seconds)
					}
				}
			}
			attempts, clipped, err := readActivityIssueAttempts(ctx, q, out.OrganizationID, project, issue.WorkItemID, issue.ClockStartedAt, out.ObservedAt)
			if err != nil {
				return err
			}
			live := activityIssueWork(&issue, attempts, out.ObservedAt)
			issue.Partial = issue.Partial || clipped
			out.IssuesPartial = out.IssuesPartial || issue.Partial
			if r.RunnerID != "" && !slices.Contains(issue.RunnerIDs, r.RunnerID) {
				continue
			}
			if !live && !finished {
				continue
			}
			if err := readActivityIssueChange(ctx, q, out.OrganizationID, &issue); err != nil {
				return err
			}
			if live {
				out.RunningIssues = append(out.RunningIssues, issue)
			} else {
				out.FinishedIssues = append(out.FinishedIssues, issue)
			}
		}
		typical.Total = analyticsDuration(totals)
		typical.Partial = typical.Partial || partial
		for lane, values := range laneTimes {
			typical.Lanes[lane] = analyticsDuration(values)
		}
		out.IssueTypicalDurations = append(out.IssueTypicalDurations, typical)
		out.IssuesPartial = out.IssuesPartial || partial
	}
	slices.SortFunc(out.FinishedIssues, func(a, b operatortool.ActivityIssue) int { return b.FinishedAt.Compare(*a.FinishedAt) })
	if len(out.FinishedIssues) > r.Limit {
		out.FinishedIssues = out.FinishedIssues[:r.Limit]
	}
	return nil
}

func readActivityIssueChange(ctx context.Context, q nativeQueryer, organization string, issue *operatortool.ActivityIssue) error {
	var raw string
	err := q.QueryRowContext(ctx, `SELECT record_json FROM change_requests WHERE organization_id=? AND project_id=? AND work_item_id=? AND (?='' OR id=?) ORDER BY rowid DESC LIMIT 1`, organization, issue.ProjectID, issue.WorkItemID, issue.ChangeID, issue.ChangeID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var request tracker.ChangeRequest
	if err := json.Unmarshal([]byte(raw), &request); err != nil {
		return err
	}
	issue.ChangeID = request.ID
	if request.CurrentVersion == "" {
		return nil
	}
	if err := q.QueryRowContext(ctx, `SELECT record_json FROM change_versions WHERE change_id=? AND id=?`, request.ID, request.CurrentVersion).Scan(&raw); errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	var version tracker.ChangeVersion
	if err := json.Unmarshal([]byte(raw), &version); err != nil {
		return err
	}
	if version.External != nil && issue.PullRequestURL == "" {
		issue.PullRequestURL = version.External.URL
	}
	return nil
}
