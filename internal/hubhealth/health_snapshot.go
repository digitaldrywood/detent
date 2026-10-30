package hubhealth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubsql"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const (
	permissionWaitTransitionReason = "permission_wait"
)

type RunnerState struct {
	Runner
	MachineID  string
	Routing    runnerauth.Routing
	Admissions map[string]tracker.NativeAdmissionObservation
}

func (reader *SnapshotReader) ReadSnapshot(ctx context.Context, q hubsql.Queryer, organization tracker.OrganizationID, now time.Time) (Snapshot, error) {
	return reader.ReadSnapshotWithContracts(ctx, q, organization, now, nil)
}

func (reader *SnapshotReader) ReadSnapshotWithContracts(ctx context.Context, q hubsql.Queryer, organization tracker.OrganizationID, now time.Time, contracts *ContractCache) (Snapshot, error) {
	snapshot := Snapshot{}
	runners, err := reader.ReadRunners(ctx, q, organization, now)
	if err != nil {
		return snapshot, err
	}
	for _, r := range runners {
		snapshot.Runners = append(snapshot.Runners, r.Runner)
	}
	projects, err := reader.ReadProjects(ctx, q, organization)
	if err != nil {
		return snapshot, err
	}
	for _, p := range projects {
		scope := Scope{Organization: organization, Project: tracker.ProjectID(p.ID)}
		p.Scheduled, err = reader.ReadScheduled(ctx, q, scope, now)
		if err != nil {
			return snapshot, err
		}
		candidates, err := reader.hooks.Candidates(ctx, q, scope, now)
		if err != nil {
			return snapshot, err
		}
		if len(candidates) > ReadLimit {
			return snapshot, ErrReadLimit
		}
		p.Candidates = len(candidates)
		if p.DependencyGated, p.HeldCount, p.HeldBlockers, p.HoldSince, err = reader.ReadDependencyHolds(ctx, q, scope); err != nil {
			return snapshot, err
		}
		p.CandidateIDs = make(map[string]bool, len(candidates))
		var revisions map[string]tracker.Revision
		if len(candidates) > 0 {
			if p.CandidateSince, revisions, err = reader.ReadCandidateRevisions(ctx, q, scope, candidates, p.CandidateIDs); err != nil {
				return snapshot, err
			}
		}
		policyID := ""
		if contracts != nil && len(revisions) > 0 {
			if policyID, err = ReadContractPolicyID(ctx, q, scope); err != nil {
				return snapshot, err
			}
		}
		for item, revision := range revisions {
			wait, err := contracts.Read(ctx, q, scope, item, revision, policyID, reader.hooks.Contract)
			if err != nil {
				return snapshot, err
			}
			if wait != nil {
				snapshot.Waits = append(snapshot.Waits, *wait)
			}
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
	if err := reader.ReadEvents(ctx, q, organization, now, &snapshot); err != nil {
		return snapshot, err
	}
	if err := reader.ReadCurrentWaits(ctx, q, organization, now, &snapshot); err != nil {
		return snapshot, err
	}
	if err := reader.ReadRates(ctx, q, organization, now, runners, &snapshot); err != nil {
		return snapshot, err
	}
	return snapshot, nil
}

func (reader *SnapshotReader) ReadProjects(ctx context.Context, q hubsql.Queryer, organization tracker.OrganizationID) ([]Project, error) {
	rows, err := q.QueryContext(ctx, "SELECT id FROM projects WHERE deleted_at IS NULL AND organization_id=? AND profile='native' ORDER BY id LIMIT ?", organization, ProjectLimit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	projects := []Project{}
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID); err != nil {
			return nil, err
		}
		projects = append(projects, p)
		if len(projects) > ProjectLimit {
			return nil, ErrReadLimit
		}
	}
	return projects, rows.Err()
}

func (reader *SnapshotReader) ReadRunners(ctx context.Context, q hubsql.Queryer, organization tracker.OrganizationID, now time.Time) ([]RunnerState, error) {
	rows, err := q.QueryContext(ctx, `SELECT r.id, r.machine_id, r.routing_settings_json, r.last_heartbeat_at, r.created_at,
 min(r.capacity_limit, r.reported_capacity), m.capacity,
 (SELECT count(*) FROM leases l INDEXED BY health_active_host JOIN lease_runners lr ON lr.lease_id=l.lease_id WHERE l.machine_id=r.machine_id AND lr.runner_id=r.id AND l.released_at IS NULL),
 (SELECT count(*) FROM leases l INDEXED BY health_active_host JOIN lease_runners lr ON lr.lease_id=l.lease_id WHERE l.machine_id=r.machine_id AND lr.runner_id=r.id AND l.released_at IS NULL AND julianday(l.expires_at)>julianday(?)),
 (SELECT count(*) FROM leases l WHERE l.machine_id=r.machine_id AND l.released_at IS NULL AND julianday(l.expires_at)>julianday(?)),
 r.state
 FROM runner_identities r JOIN machines m ON m.id=r.machine_id JOIN api_tokens t ON t.id=r.token_id
 WHERE r.organization_id=? AND r.removed_at IS NULL AND t.revoked_at IS NULL AND (t.expires_at IS NULL OR julianday(t.expires_at)>julianday(?)) ORDER BY r.id LIMIT ?`, formatHubTime(now), formatHubTime(now), organization, formatHubTime(now), RunnerLimit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	runners := []RunnerState{}
	for rows.Next() {
		var r RunnerState
		var routing, heartbeat, created, state string
		var capacity, hostCapacity, hostLeases, activeLeases int
		if err := rows.Scan(&r.ID, &r.MachineID, &routing, &heartbeat, &created, &capacity, &hostCapacity, &r.Leases, &activeLeases, &hostLeases, &state); err != nil {
			return nil, err
		}
		if err := reader.hooks.DecodeStorageJSON(ctx, []byte(routing), &r.Routing); err != nil {
			return nil, err
		}
		r.Routing.State = state
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
		if len(runners) > RunnerLimit {
			return nil, ErrReadLimit
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	for i := range runners {
		admissions, err := reader.hooks.Admissions(ctx, q, organization, runners[i].ID, tracker.MachineID(runners[i].MachineID))
		if err != nil {
			return nil, err
		}
		runners[i].Admissions = admissions
	}
	for i := range runners {
		leased, err := reader.ReadRunnerLeasedProjects(ctx, q, runners[i].MachineID, runners[i].ID)
		if err != nil {
			return nil, err
		}
		runners[i].Projects = append(runners[i].Projects, leased...)
		if len(runners[i].Projects) > ReadLimit {
			return nil, ErrReadLimit
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

type TimeEvent struct {
	Id, Project, item, Kind, Raw string
	At                           time.Time
}

func (reader *SnapshotReader) ReadTimeEvents(ctx context.Context, q hubsql.Queryer, organization tracker.OrganizationID, now time.Time) ([]TimeEvent, error) {
	var complete bool
	if err := q.QueryRowContext(ctx, `SELECT coalesce((SELECT complete FROM time_index_progress WHERE owner='health'),0)`).Scan(&complete); err != nil {
		return nil, err
	}
	query := `SELECT id,project_id,work_item_id,type,
 CASE WHEN health_projected IS NULL THEN data_json ELSE '{}' END,recorded_at,
 scheduler_outcome,scheduler_reason,scheduler_source,scheduler_runner_id,event_landed,event_landing_at,health_projected,scheduler_present,scheduler_at,scheduler_revision
 FROM collaboration_events WHERE organization_id=? AND
 ((type='scheduler.decision' AND julianday(recorded_at)>=julianday(?)-1.0/86400) OR
 (type IN ('run.finished','run.observed') AND (event_landed=1 OR health_projected IS NULL) AND julianday(recorded_at)>=julianday(?)-1.0/86400))
 AND julianday(recorded_at)<=julianday(?)+1.0/86400 ORDER BY julianday(recorded_at),id LIMIT ?`
	args := []any{organization, formatHubTime(now.Add(-InstanceWindow)), formatHubTime(now.Add(-HistoryWindow)), formatHubTime(now), ReadLimit + 1}
	if complete {
		query = `SELECT e.id,e.project_id,e.work_item_id,t.type,
 CASE WHEN e.health_projected IS NULL THEN e.data_json ELSE '{}' END,e.recorded_at,
 e.scheduler_outcome,e.scheduler_reason,e.scheduler_source,e.scheduler_runner_id,e.event_landed,e.event_landing_at,e.health_projected,e.scheduler_present,e.scheduler_at,e.scheduler_revision
 FROM (
 SELECT event_id,type,recorded_at_ns FROM health_event_times WHERE organization_id=? AND type='scheduler.decision' AND recorded_at_ns>=? AND recorded_at_ns<=?
 UNION ALL
 SELECT event_id,type,recorded_at_ns FROM health_event_times WHERE organization_id=? AND type IN ('run.finished','run.observed') AND landed=1 AND recorded_at_ns>=? AND recorded_at_ns<=?
 ) t JOIN collaboration_events e ON e.id=t.event_id ORDER BY t.recorded_at_ns,e.id LIMIT ?`
		args = []any{organization, now.Add(-InstanceWindow).UnixNano(), now.UnixNano(), organization, now.Add(-HistoryWindow).UnixNano(), now.UnixNano(), ReadLimit + 1}
	}
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []TimeEvent
	count := 0
	for rows.Next() {
		count++
		if count > ReadLimit {
			return nil, ErrReadLimit
		}
		var e TimeEvent
		var at, raw string
		var outcome, reason, source, runner, landingAt sql.NullString
		var landed, present sql.NullBool
		var decisionAt sql.NullString
		var decisionRevision sql.NullInt64
		var projected sql.NullInt64
		if err := rows.Scan(&e.Id, &e.Project, &e.item, &e.Kind, &raw, &at, &outcome, &reason, &source, &runner, &landed, &landingAt, &projected, &present, &decisionAt, &decisionRevision); err != nil {
			return nil, err
		}
		var data tracker.CollaborationData
		if !projected.Valid {
			if err := reader.hooks.DecodeStorageJSON(ctx, []byte(raw), &data); err != nil {
				return nil, err
			}
		} else if e.Kind == "scheduler.decision" && present.Bool {
			data.Decision = &tracker.NativeSchedulerDecision{Outcome: outcome.String, Reason: reason.String, Source: source.String, RunnerID: runner.String, WorkItemRevision: tracker.Revision(decisionRevision.Int64)}
			if decisionAt.String != "" {
				data.Decision.At, err = parseTimeValue(decisionAt.String)
				if err != nil {
					return nil, err
				}
			}

		} else if landed.Bool {
			landing := &tracker.NativeLandingReceipt{Landed: true}
			if landingAt.String != "" {
				landing.ObservedAt, err = parseTimeValue(landingAt.String)
				if err != nil {
					return nil, err
				}
			}
			data.Run = &tracker.NativeRunData{Runtime: &tracker.NativeRuntimeObservation{Landing: landing}}
		}
		if e.Kind != "scheduler.decision" && (data.Run == nil || data.Run.Runtime == nil || data.Run.Runtime.Landing == nil || !data.Run.Runtime.Landing.Landed) {
			continue
		}
		projection := tracker.CollaborationData{}
		if e.Kind == "scheduler.decision" {
			projection.Decision = data.Decision
		}
		if e.Kind != "scheduler.decision" && data.Run != nil && data.Run.Runtime != nil {
			projection.Run = &tracker.NativeRunData{Runtime: &tracker.NativeRuntimeObservation{Landing: data.Run.Runtime.Landing}}
		}
		encoded, err := json.Marshal(projection)
		if err != nil {
			return nil, err
		}
		e.Raw = string(encoded)
		e.At, err = parseTimeValue(at)
		if err != nil {
			return nil, err
		}
		window := HistoryWindow
		if e.Kind == "scheduler.decision" {
			window = InstanceWindow
		}
		if e.At.Before(now.Add(-window)) || e.At.After(now) {
			continue
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	slices.SortFunc(events, func(a, b TimeEvent) int {
		if order := a.At.Compare(b.At); order != 0 {
			return order
		}
		return strings.Compare(a.Id, b.Id)
	})
	return events, nil
}

func (reader *SnapshotReader) ReadEvents(ctx context.Context, q hubsql.Queryer, organization tracker.OrganizationID, now time.Time, snapshot *Snapshot) error {
	events, err := reader.ReadTimeEvents(ctx, q, organization, now)
	if err != nil {
		return err
	}
	cycles := map[string]healthCycle{}
	for _, event := range events {
		id, project, item, kind, raw, observed := event.Id, event.Project, event.item, event.Kind, event.Raw, event.At
		var data tracker.CollaborationData
		if err := reader.hooks.DecodeStorageJSON(ctx, []byte(raw), &data); err != nil {
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
		if kind != "scheduler.decision" || data.Decision == nil || data.Decision.Source == "native_claim_eligibility" || now.Sub(observed) > InstanceWindow {
			continue
		}
		key := project + "\x00" + data.Decision.RunnerID
		cycle := cycles[key]
		if !cycle.At.Equal(observed) {
			cycle = healthCycle{At: observed, Reasons: map[string]string{}}
		}
		switch data.Decision.Outcome {
		case "claimed":
			cycle.Claims++
		case "skipped":
			cycle.Reasons[item] = data.Decision.Reason
		}
		cycle.Events = append(cycle.Events, id)
		cycles[key] = cycle
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

func (reader *SnapshotReader) ReadCurrentWaits(ctx context.Context, q hubsql.Queryer, organization tracker.OrganizationID, now time.Time, snapshot *Snapshot) error {
	rows, err := q.QueryContext(ctx, `SELECT i.native_id,i.project_id,coalesce(a.id,''),coalesce(a.updated_at,''),CASE WHEN a.health_disposition_reason IS NULL THEN coalesce(COALESCE(NULLIF(a.payload_object_key,''),a.data_json),'{}') ELSE '{}' END,coalesce(ws.detent_state,''),
 coalesce(e.id,''),coalesce(e.transition_reason,''),coalesce(e.transition_detail,''),coalesce(e.actor_kind,''),coalesce(e.recorded_at,''),
 coalesce(a.status='succeeded' AND e.actor_kind='runner' AND e.transition_reason='worker_progress' AND e.transition_revision=i.revision AND julianday(e.recorded_at)>=julianday(a.updated_at),0),
 coalesce(a.disposition_status,''),coalesce(a.disposition_human_action,0),a.health_disposition_reason,
 CASE WHEN e.health_projected IS NULL THEN coalesce(e.data_json,'{}') ELSE '{}' END, CASE WHEN e.health_projected IS NULL THEN coalesce(e.actor_json,'{}') ELSE '{}' END,e.health_projected,coalesce(a.status,''),i.revision
 FROM issues i LEFT JOIN workflow_states ws ON ws.id=i.workflow_state_id
 LEFT JOIN native_attempts a ON a.id=(SELECT latest.id FROM native_attempts latest WHERE latest.organization_id=i.organization_id AND latest.project_id=i.project_id AND latest.work_item_id=i.native_id ORDER BY latest.fencing_token DESC LIMIT 1)
 LEFT JOIN collaboration_events e ON e.id=(SELECT latest.id FROM collaboration_events latest WHERE latest.organization_id=i.organization_id AND latest.work_item_id=i.native_id AND latest.type='workflow.transitioned' ORDER BY latest.sequence DESC LIMIT 1)
 WHERE i.organization_id=? AND i.archived=0 AND ws.terminal=0 ORDER BY i.id LIMIT ?`, organization, ReadLimit+1)
	if err != nil {
		return err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var item, project, attempt, at, raw, state, event, reason, detail, actor, recorded string
		var authoritative, humanAction bool
		var dispositionStatus, eventRaw, actorRaw, status string
		var dispositionReason sql.NullString
		var eventProjected sql.NullInt64
		var revision tracker.Revision
		if err := rows.Scan(&item, &project, &attempt, &at, &raw, &state, &event, &reason, &detail, &actor, &recorded, &authoritative, &dispositionStatus, &humanAction, &dispositionReason, &eventRaw, &actorRaw, &eventProjected, &status, &revision); err != nil {
			return err
		}
		count++
		if count > ReadLimit {
			return ErrReadLimit
		}
		if !eventProjected.Valid {
			var data tracker.CollaborationData
			var eventActor tracker.Actor
			if err := reader.hooks.DecodeStorageJSON(ctx, []byte(eventRaw), &data); err != nil {
				return err
			}
			if err := reader.hooks.DecodeStorageJSON(ctx, []byte(actorRaw), &eventActor); err != nil {
				return err
			}
			reason, detail, actor = data.Reason, data.ReasonDetail, eventActor.Kind
			authoritative = status == "succeeded" && actor == "runner" && reason == "worker_progress" && data.Revision == revision
			if authoritative {
				eventAt, err := parseTimeValue(recorded)
				if err != nil {
					return err
				}
				attemptAt, err := parseTimeValue(at)
				if err != nil {
					return err
				}
				authoritative = !eventAt.Before(attemptAt)
			}
		}
		detail = strings.ReplaceAll(strings.TrimSpace(detail), " ", "_")
		if actor == "runner" && (detail == permissionWaitTransitionReason || detail == "human_action") {
			since, err := parseTimeValue(recorded)
			if err != nil {
				return err
			}
			snapshot.Waits = append(snapshot.Waits, Wait{Item: item, Project: project, Attempt: attempt, Event: event, Since: since, Active: true})
			continue
		}
		var data tracker.NativeRunData
		if err := reader.hooks.DecodeStorageJSON(ctx, []byte(raw), &data); err != nil {
			return err
		}
		if dispositionReason.Valid {
			data.Disposition = &tracker.NativeDisposition{Status: dispositionStatus, HumanAction: humanAction, ReasonCode: dispositionReason.String}
		}
		if data.Disposition == nil || data.Disposition.Status != "blocked" || !authoritative || !data.Disposition.HumanAction && data.Disposition.ReasonCode != "permission_wait" {
			continue
		}
		since, err := parseTimeValue(at)
		if err != nil {
			return err
		}
		snapshot.Waits = append(snapshot.Waits, Wait{Item: item, Project: project, Attempt: attempt, Since: since, Active: true})
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	rows, err = q.QueryContext(ctx, `SELECT coalesce(c.work_item_id,''),c.project_id,q.attempt_id,q.created_at FROM conversation_questions q JOIN conversations c ON c.id=q.conversation_id WHERE c.organization_id=? AND c.status='active' AND q.status IN ('pending','sending','sent','unknown') AND (q.expires_at IS NULL OR julianday(q.expires_at)>julianday(?)) ORDER BY q.created_at,q.id LIMIT ?`, organization, formatHubTime(now), ReadLimit+1)
	if err != nil {
		return err
	}
	defer rows.Close()
	count = 0
	for rows.Next() {
		var w Wait
		var at string
		if err := rows.Scan(&w.Item, &w.Project, &w.Attempt, &at); err != nil {
			return err
		}
		count++
		if count > ReadLimit {
			return ErrReadLimit
		}
		if w.Since, err = parseTimeValue(at); err != nil {
			return fmt.Errorf("read question time: %w", err)
		}
		w.Active = true
		snapshot.Waits = append(snapshot.Waits, w)
	}
	return rows.Err()
}

func (reader *SnapshotReader) ReadCandidateRevisions(ctx context.Context, q hubsql.Queryer, scope Scope, ids []tracker.WorkItemID, seen map[string]bool) (time.Time, map[string]tracker.Revision, error) {
	var since time.Time
	revisions := make(map[string]tracker.Revision)
	args := make([]any, 0, len(ids)+2)
	args = append(args, scope.Organization, scope.Project)
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := q.QueryContext(ctx, "SELECT native_id, native_created_at, revision FROM issues WHERE organization_id=? AND project_id=? AND id IN ("+placeholders(len(ids))+")", args...)
	if err != nil {
		return since, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, at string
		var revision tracker.Revision
		if err := rows.Scan(&id, &at, &revision); err != nil {
			return since, nil, err
		}
		created, err := parseTimeValue(at)
		if err != nil {
			return since, nil, err
		}
		seen[id] = true
		revisions[id] = revision
		if since.IsZero() || created.Before(since) {
			since = created
		}
	}
	return since, revisions, errors.Join(rows.Err(), rows.Close())
}

func (reader *SnapshotReader) ReadRunnerLeasedProjects(ctx context.Context, q hubsql.Queryer, machineID, runnerID any) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT DISTINCT i.project_id FROM leases l INDEXED BY health_active_host JOIN lease_runners lr ON lr.lease_id=l.lease_id JOIN issues i ON i.id=l.issue_id WHERE l.machine_id=? AND lr.runner_id=? AND l.released_at IS NULL LIMIT ?`, machineID, runnerID, ReadLimit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	projects := []string{}
	for rows.Next() {
		var project string
		if err := rows.Scan(&project); err != nil {
			return nil, err
		}
		projects = append(projects, project)
		if len(projects) > ReadLimit {
			return nil, ErrReadLimit
		}
	}
	return projects, errors.Join(rows.Err(), rows.Close())
}

func (reader *SnapshotReader) ReadDependencyHolds(ctx context.Context, q hubsql.Queryer, scope Scope) (int, int, []string, time.Time, error) {
	var since time.Time
	rows, err := q.QueryContext(ctx, `SELECT d.native_id,b.native_id,b.project_id=d.project_id,bws.dispatchable,
 max(dep.created_at,
  coalesce((SELECT e.recorded_at FROM collaboration_events e WHERE e.organization_id=d.organization_id AND e.work_item_id=d.native_id AND e.type='workflow.transitioned' ORDER BY e.sequence DESC LIMIT 1),d.created_at),
  coalesce((SELECT e.recorded_at FROM collaboration_events e WHERE e.organization_id=b.organization_id AND e.work_item_id=b.native_id AND e.type='workflow.transitioned' ORDER BY e.sequence DESC LIMIT 1),b.created_at))
 FROM issues d JOIN projects p ON p.id=d.project_id AND p.organization_id=d.organization_id AND p.require_dependencies=1
 JOIN workflow_states dws ON dws.id=d.workflow_state_id
 JOIN issue_dependencies dep ON dep.dependent_issue_id=d.id
 JOIN issues b ON b.id=dep.blocker_issue_id JOIN workflow_states bws ON bws.id=b.workflow_state_id
 WHERE d.organization_id=? AND d.project_id=? AND d.archived=0 AND dws.dispatchable=1 AND dws.terminal=0 AND bws.terminal=0
 ORDER BY d.native_id,b.native_id LIMIT ?`, scope.Organization, scope.Project, ReadLimit+1)
	if err != nil {
		return 0, 0, nil, since, err
	}
	defer rows.Close()
	gated, held, listed := map[string]bool{}, map[string]bool{}, map[string]bool{}
	count := 0
	for rows.Next() {
		var dependent, blocker, at string
		var sameProject, dispatchable bool
		if err := rows.Scan(&dependent, &blocker, &sameProject, &dispatchable, &at); err != nil {
			return 0, 0, nil, since, err
		}
		count++
		if count > ReadLimit {
			return 0, 0, nil, since, ErrReadLimit
		}
		gated[dependent] = true
		if dispatchable {
			continue
		}
		held[blocker] = true
		if sameProject {
			listed[blocker] = true
		}
		gatedAt, err := parseTimeValue(at)
		if err != nil {
			return 0, 0, nil, since, err
		}
		if since.IsZero() || gatedAt.Before(since) {
			since = gatedAt
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return 0, 0, nil, since, err
	}
	return len(gated), len(held), slices.Sorted(maps.Keys(listed)), since, nil
}

var ErrReadLimit = errors.New("health observation exceeds bounded read")
