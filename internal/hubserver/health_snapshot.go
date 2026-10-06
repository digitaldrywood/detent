package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var errHealthReadLimit = errors.New("health observation exceeds bounded read")

type healthRunnerState struct {
	healthRunner
	MachineID  string
	Routing    runnerauth.Routing
	Admissions map[string]tracker.NativeAdmissionObservation
}

func readHealthSnapshot(ctx context.Context, q nativeQueryer, organization tracker.OrganizationID, now time.Time) (healthSnapshot, error) {
	snapshot := healthSnapshot{}
	runners, err := readHealthRunners(ctx, q, organization, now)
	if err != nil {
		return snapshot, err
	}
	for _, r := range runners {
		snapshot.Runners = append(snapshot.Runners, r.healthRunner)
	}
	projects, err := readHealthProjects(ctx, q, organization)
	if err != nil {
		return snapshot, err
	}
	for _, p := range projects {
		scope := nativeScope{organization: organization, project: tracker.ProjectID(p.ID), credential: apiCredential{Scope: apiScopeAdmin}}
		candidates, err := readHealthCandidates(ctx, q, scope, now)
		if err != nil {
			return snapshot, err
		}
		if len(candidates) > healthReadLimit {
			return snapshot, errHealthReadLimit
		}
		p.Candidates = len(candidates)
		p.CandidateIDs = make(map[string]bool, len(candidates))
		if len(candidates) > 0 {
			raw, err := json.Marshal(candidates)
			if err != nil {
				return snapshot, err
			}
			rows, err := q.QueryContext(ctx, "SELECT native_id, native_created_at FROM issues WHERE id IN (SELECT value FROM json_each(?))", string(raw))
			if err != nil {
				return snapshot, err
			}
			for rows.Next() {
				var id, at string
				if err := rows.Scan(&id, &at); err != nil {
					return snapshot, errors.Join(err, rows.Close())
				}
				created, err := parseTimeValue(at)
				if err != nil {
					return snapshot, errors.Join(err, rows.Close())
				}
				p.CandidateIDs[id] = true
				if p.CandidateSince.IsZero() || created.Before(p.CandidateSince) {
					p.CandidateSince = created
				}
			}
			if err := errors.Join(rows.Err(), rows.Close()); err != nil {
				return snapshot, err
			}
		}
		var approved string
		err = q.QueryRowContext(ctx, "SELECT policy_id FROM project_policies WHERE scope=?", string(organization)+"/"+p.ID).Scan(&approved)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return snapshot, err
		}
		fallbackReasons := []string{}
		for _, r := range runners {
			if !slices.Contains(r.Projects, p.ID) {
				continue
			}
			if r.Routing.State != "disabled" {
				refresh := r.CreatedAt
				if observed, ok := r.Admissions[p.ID]; ok {
					if !observed.Context.LastRefreshAt.IsZero() {
						refresh = observed.Context.LastRefreshAt
					}
					p.RefreshDuration = max(p.RefreshDuration, observed.Context.RefreshDuration)
				}
				if p.RefreshAt.IsZero() || refresh.Before(p.RefreshAt) {
					p.RefreshAt = refresh
				}
			}
			reason := ""
			if r.Routing.State != "active" {
				reason = "runner_inactive"
			} else if now.Sub(r.Heartbeat) >= runnerauth.HeartbeatTimeout {
				reason = "runner_heartbeat_stale"
			} else if r.FreeSlots == 0 {
				reason = "runner_capacity_full"
			}
			observed, known := r.Admissions[p.ID]
			if reason == "" && (approved == "" || known && observed.Context.PolicyID != approved) {
				reason = "policy_mismatch"
			}
			if reason == "" {
				p.FreeSlots += r.FreeSlots
			}
			fallbackReasons = append(fallbackReasons, reason)
		}
		if len(fallbackReasons) > 0 && fallbackReasons[0] != "" {
			same := true
			for _, reason := range fallbackReasons {
				if reason != fallbackReasons[0] {
					same = false
				}
			}
			if same {
				for range p.Candidates {
					p.RefusalReasons = append(p.RefusalReasons, fallbackReasons[0])
				}
			}
		}
		snapshot.Projects = append(snapshot.Projects, p)
	}
	if err := readHealthEvents(ctx, q, organization, now, &snapshot); err != nil {
		return snapshot, err
	}
	if err := readHealthCurrentWaits(ctx, q, organization, now, &snapshot); err != nil {
		return snapshot, err
	}
	if err := readHealthRates(ctx, q, organization, now, runners, &snapshot); err != nil {
		return snapshot, err
	}
	return snapshot, nil
}

func readHealthCandidates(ctx context.Context, q nativeQueryer, scope nativeScope, now time.Time) ([]tracker.WorkItemID, error) {
	query := claimCandidateQuery{NativeScope: &scope, AvailableAt: now}
	var candidates []tracker.WorkItemID
	for len(candidates) <= healthReadLimit {
		query.Limit = min(100, healthReadLimit+1-len(candidates))
		page, err := nativeCandidateIDs(ctx, q, query, nil, nil, nil, nil, nil, nil, nil)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, page...)
		if len(page) < query.Limit {
			return candidates, nil
		}
		query.After = page[len(page)-1]
	}
	return nil, errHealthReadLimit
}

func readHealthProjects(ctx context.Context, q nativeQueryer, organization tracker.OrganizationID) ([]healthProject, error) {
	rows, err := q.QueryContext(ctx, "SELECT id FROM projects WHERE organization_id=? AND profile='native' ORDER BY id LIMIT ?", organization, healthProjectLimit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	projects := []healthProject{}
	for rows.Next() {
		var p healthProject
		if err := rows.Scan(&p.ID); err != nil {
			return nil, err
		}
		projects = append(projects, p)
		if len(projects) > healthProjectLimit {
			return nil, errHealthReadLimit
		}
	}
	return projects, rows.Err()
}

func readHealthRunners(ctx context.Context, q nativeQueryer, organization tracker.OrganizationID, now time.Time) ([]healthRunnerState, error) {
	rows, err := q.QueryContext(ctx, `SELECT r.id, r.machine_id, r.routing_settings_json, r.last_heartbeat_at, r.created_at,
 min(r.capacity_limit, r.reported_capacity), m.capacity,
 (SELECT count(*) FROM leases l INDEXED BY health_active_host JOIN lease_runners lr ON lr.lease_id=l.lease_id WHERE l.machine_id=r.machine_id AND lr.runner_id=r.id AND l.released_at IS NULL),
 (SELECT count(*) FROM leases l INDEXED BY health_active_host JOIN lease_runners lr ON lr.lease_id=l.lease_id WHERE l.machine_id=r.machine_id AND lr.runner_id=r.id AND l.released_at IS NULL AND julianday(l.expires_at)>julianday(?)),
 (SELECT count(*) FROM leases l WHERE l.machine_id=r.machine_id AND l.released_at IS NULL AND julianday(l.expires_at)>julianday(?)),
 coalesce(json_extract(m.capabilities_json, '$.native_admission.' || r.id), '{}'), r.state
 FROM runner_identities r JOIN machines m ON m.id=r.machine_id JOIN api_tokens t ON t.id=r.token_id
 WHERE r.organization_id=? AND r.removed_at IS NULL AND t.revoked_at IS NULL AND (t.expires_at IS NULL OR julianday(t.expires_at)>julianday(?)) ORDER BY r.id LIMIT ?`, formatHubTime(now), formatHubTime(now), organization, formatHubTime(now), healthRunnerLimit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	runners := []healthRunnerState{}
	for rows.Next() {
		var r healthRunnerState
		var routing, heartbeat, created, admission, state string
		var capacity, hostCapacity, hostLeases, activeLeases int
		if err := rows.Scan(&r.ID, &r.MachineID, &routing, &heartbeat, &created, &capacity, &hostCapacity, &r.Leases, &activeLeases, &hostLeases, &admission, &state); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(routing), &r.Routing); err != nil {
			return nil, err
		}
		r.Routing.State = state
		if err := json.Unmarshal([]byte(admission), &r.Admissions); err != nil {
			return nil, err
		}
		if r.Heartbeat, err = parseTimeValue(heartbeat); err != nil {
			return nil, err
		}
		if r.CreatedAt, err = parseTimeValue(created); err != nil {
			return nil, err
		}
		r.FreeSlots = max(0, min(capacity-activeLeases, hostCapacity-hostLeases))
		for project := range r.Routing.ProjectRanks {
			r.Projects = append(r.Projects, string(project))
		}
		r.Projects = append(r.Projects, projectStrings(r.Routing.ProjectIDs)...)
		slices.Sort(r.Projects)
		r.Projects = slices.Compact(r.Projects)
		runners = append(runners, r)
		if len(runners) > healthRunnerLimit {
			return nil, errHealthReadLimit
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	for i := range runners {
		rows, err := q.QueryContext(ctx, `SELECT DISTINCT i.project_id FROM leases l INDEXED BY health_active_host JOIN lease_runners lr ON lr.lease_id=l.lease_id JOIN issues i ON i.id=l.issue_id WHERE l.machine_id=? AND lr.runner_id=? AND l.released_at IS NULL LIMIT ?`, runners[i].MachineID, runners[i].ID, healthReadLimit+1)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var project string
			if err := rows.Scan(&project); err != nil {
				return nil, errors.Join(err, rows.Close())
			}
			runners[i].Projects = append(runners[i].Projects, project)
			if len(runners[i].Projects) > healthReadLimit {
				return nil, errors.Join(errHealthReadLimit, rows.Close())
			}
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return nil, err
		}
		slices.Sort(runners[i].Projects)
		runners[i].Projects = slices.Compact(runners[i].Projects)
	}
	return runners, nil
}

func projectStrings(ids []tracker.ProjectID) []string {
	result := make([]string, 0, len(ids))
	for _, id := range ids {
		result = append(result, string(id))
	}
	return result
}

type healthCycle struct {
	At      time.Time
	Reasons map[string]string
	Events  []string
	Claims  int
}

func readHealthEvents(ctx context.Context, q nativeQueryer, organization tracker.OrganizationID, now time.Time, snapshot *healthSnapshot) error {
	rows, err := q.QueryContext(ctx, `SELECT id,project_id,work_item_id,type,projection,recorded_at FROM (
 SELECT id,project_id,work_item_id,type,json_object('decision',json_extract(data_json,'$.decision')) AS projection,recorded_at,julianday(recorded_at) AS event_order
 FROM collaboration_events WHERE organization_id=? AND type='scheduler.decision'
 AND julianday(recorded_at)>=julianday(?) AND julianday(recorded_at)<=julianday(?)
 UNION ALL
 SELECT id,project_id,work_item_id,type,json_object('run',json_object('runtime',json_object('landing',json_extract(data_json,'$.run.runtime.landing')))),recorded_at,julianday(recorded_at)
 FROM collaboration_events WHERE organization_id=? AND type IN ('run.finished','run.observed') AND json_extract(data_json,'$.run.runtime.landing.landed')=1
 AND julianday(recorded_at)>=julianday(?) AND julianday(recorded_at)<=julianday(?)
 ) ORDER BY event_order,id LIMIT ?`, organization, formatHubTime(now.Add(-healthInstanceWindow)), formatHubTime(now), organization, formatHubTime(now.Add(-healthHistoryWindow)), formatHubTime(now), healthReadLimit+1)
	if err != nil {
		return err
	}
	defer rows.Close()
	cycles := map[string]healthCycle{}
	count := 0
	for rows.Next() {
		var id, project, item, kind, raw, at string
		if err := rows.Scan(&id, &project, &item, &kind, &raw, &at); err != nil {
			return err
		}
		count++
		if count > healthReadLimit {
			return errHealthReadLimit
		}
		var data tracker.CollaborationData
		if err := json.Unmarshal([]byte(raw), &data); err != nil {
			return err
		}
		observed, err := parseTimeValue(at)
		if err != nil {
			return err
		}
		for i := range snapshot.Projects {
			p := &snapshot.Projects[i]
			if p.ID != project {
				continue
			}
			if (kind == "run.finished" || kind == "run.observed") && data.Run != nil && data.Run.Runtime != nil && data.Run.Runtime.Landing != nil && data.Run.Runtime.Landing.Landed {
				landing := data.Run.Runtime.Landing.ObservedAt
				if landing.IsZero() {
					landing = observed
				}
				if !landing.After(now) && landing.After(p.LastLanding) {
					p.LastLanding = landing
				}
			}
		}
		if kind != "scheduler.decision" || data.Decision == nil || data.Decision.Source == "native_claim_eligibility" || now.Sub(observed) > healthInstanceWindow {
			continue
		}
		key := project + "\x00" + data.Decision.RunnerID
		cycle := cycles[key]
		if !cycle.At.Equal(observed) {
			cycle = healthCycle{At: observed, Reasons: map[string]string{}}
		}
		if data.Decision.Outcome == "claimed" {
			cycle.Claims++
		} else if data.Decision.Outcome == "skipped" {
			cycle.Reasons[item] = data.Decision.Reason
		}
		cycle.Events = append(cycle.Events, id)
		cycles[key] = cycle
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range snapshot.Projects {
		p := &snapshot.Projects[i]
		var latest healthCycle
		var latestKey string
		for key, cycle := range cycles {
			if strings.HasPrefix(key, p.ID+"\x00") && (cycle.At.After(latest.At) || cycle.At.Equal(latest.At) && key > latestKey) {
				latest, latestKey = cycle, key
			}
		}
		if latest.At.IsZero() {
			continue
		}
		p.Claims = latest.Claims
		p.RefusalReasons = nil
		covered := len(latest.Reasons) == p.Candidates
		for item := range latest.Reasons {
			if !p.CandidateIDs[item] {
				covered = false
			}
		}
		if !covered {
			continue
		}
		for _, reason := range latest.Reasons {
			p.RefusalReasons = append(p.RefusalReasons, reason)
		}
		p.RefusalEvents = latest.Events
	}
	return nil
}

func readHealthCurrentWaits(ctx context.Context, q nativeQueryer, organization tracker.OrganizationID, now time.Time, snapshot *healthSnapshot) error {
	rows, err := q.QueryContext(ctx, `SELECT i.native_id,i.project_id,coalesce(a.id,''),coalesce(a.updated_at,''),json_object('disposition',json_extract(a.data_json,'$.disposition')),coalesce(ws.detent_state,''),
 coalesce(e.id,''),coalesce(json_extract(e.data_json,'$.reason'),''),
 coalesce(a.status='succeeded' AND json_extract(e.actor_json,'$.kind')='runner' AND json_extract(e.data_json,'$.reason')='worker_progress' AND CAST(json_extract(e.data_json,'$.revision') AS INTEGER)=i.revision AND julianday(e.recorded_at)>=julianday(a.updated_at),0)
 FROM issues i LEFT JOIN workflow_states ws ON ws.id=i.workflow_state_id
 LEFT JOIN native_attempts a ON a.id=(SELECT latest.id FROM native_attempts latest WHERE latest.organization_id=i.organization_id AND latest.project_id=i.project_id AND latest.work_item_id=i.native_id ORDER BY latest.fencing_token DESC LIMIT 1)
 LEFT JOIN collaboration_events e ON e.id=(SELECT latest.id FROM collaboration_events latest WHERE latest.organization_id=i.organization_id AND latest.work_item_id=i.native_id AND latest.type='workflow.transitioned' ORDER BY latest.sequence DESC LIMIT 1)
 WHERE i.organization_id=? AND i.archived=0 AND ws.terminal=0 ORDER BY i.id LIMIT ?`, organization, healthReadLimit+1)
	if err != nil {
		return err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var item, project, attempt, at, raw, state, event, reason string
		var authoritative bool
		if err := rows.Scan(&item, &project, &attempt, &at, &raw, &state, &event, &reason, &authoritative); err != nil {
			return err
		}
		count++
		if count > healthReadLimit {
			return errHealthReadLimit
		}
		if reason == "lifetime_limit" {
			snapshot.Limits = append(snapshot.Limits, healthLimit{Item: item, Project: project, Event: event, Active: true})
		}
		var data tracker.NativeRunData
		if err := json.Unmarshal([]byte(raw), &data); err != nil {
			return err
		}
		if data.Disposition == nil || data.Disposition.Status != "blocked" || !authoritative || !data.Disposition.HumanAction && data.Disposition.ReasonCode != "permission_wait" {
			continue
		}
		since, err := parseTimeValue(at)
		if err != nil {
			return err
		}
		snapshot.Waits = append(snapshot.Waits, healthWait{Item: item, Project: project, Attempt: attempt, Since: since, Active: true})
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	rows, err = q.QueryContext(ctx, `SELECT coalesce(c.work_item_id,''),c.project_id,q.attempt_id,q.created_at FROM conversation_questions q JOIN conversations c ON c.id=q.conversation_id WHERE c.organization_id=? AND c.status='active' AND q.status IN ('pending','sending','sent','unknown') AND (q.expires_at IS NULL OR julianday(q.expires_at)>julianday(?)) ORDER BY q.created_at,q.id LIMIT ?`, organization, formatHubTime(now), healthReadLimit+1)
	if err != nil {
		return err
	}
	defer rows.Close()
	count = 0
	for rows.Next() {
		var w healthWait
		var at string
		if err := rows.Scan(&w.Item, &w.Project, &w.Attempt, &at); err != nil {
			return err
		}
		count++
		if count > healthReadLimit {
			return errHealthReadLimit
		}
		if w.Since, err = parseTimeValue(at); err != nil {
			return fmt.Errorf("read question time: %w", err)
		}
		w.Active = true
		snapshot.Waits = append(snapshot.Waits, w)
	}
	return rows.Err()
}
