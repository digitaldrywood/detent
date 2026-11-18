package reportcheckpoints

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const TailRows = 512

var excludedWorkStates = []string{"Backlog", "Triage", "Blocked", "Human Review"}

type TailEvent struct {
	WorkItem   string
	Type       string
	ToState    string
	RecordedAt time.Time
}

type Lag struct {
	Unbounded bool
	Items     map[string]bool
	Events    []TailEvent
}

func (l *Lag) Stale(item string) bool {
	return l != nil && (l.Unbounded || l.Items[item])
}

func ReadLag(ctx context.Context, db pgquery.DBTX, organization tracker.OrganizationID, projects []string) (map[string]*Lag, error) {
	type position struct {
		project              string
		head, through        int64
		legacyItem           string
		legacySequence       int64
		legacyComplete, seen bool
	}
	rows, err := db.Query(ctx, `SELECT p.project_id,coalesce(h.sequence,0),coalesce(c.through_project_sequence,0),coalesce(c.legacy_work_item_id,''),coalesce(c.legacy_sequence,0),coalesce(c.legacy_complete,false),c.project_id IS NOT NULL
FROM unnest($2::text[]) p(project_id)
LEFT JOIN project_event_cursors h ON h.organization_id=$1 AND h.project_id=p.project_id
LEFT JOIN report_checkpoint_cursors c ON c.organization_id=$1 AND c.project_id=p.project_id`, organization, projects)
	if err != nil {
		return nil, fmt.Errorf("read report checkpoint cursors: %w", err)
	}
	positions := []position{}
	for rows.Next() {
		var p position
		if err := rows.Scan(&p.project, &p.head, &p.through, &p.legacyItem, &p.legacySequence, &p.legacyComplete, &p.seen); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan report checkpoint cursor: %w", err)
		}
		positions = append(positions, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read report checkpoint cursors: %w", err)
	}
	out := map[string]*Lag{}
	for _, p := range positions {
		lag := &Lag{Items: map[string]bool{}}
		out[p.project] = lag
		if p.head > p.through {
			if err := readTail(ctx, db, lag, `SELECT e.work_item_id,e.type,e.to_state,e.recorded_at FROM collaboration_events e WHERE e.organization_id=$1 AND e.project_id=$2 AND e.project_sequence>$3 ORDER BY e.project_sequence LIMIT $4`, organization, p.project, p.through, TailRows+1); err != nil {
				return nil, err
			}
		}
		if !p.legacyComplete && !lag.Unbounded {
			if err := readTail(ctx, db, lag, `SELECT e.work_item_id,e.type,e.to_state,e.recorded_at FROM collaboration_events e WHERE e.organization_id=$1 AND e.project_id=$2 AND e.project_sequence=0 AND e.work_item_id>=$3 AND (e.work_item_id,e.sequence)>($3::text,$4::bigint) ORDER BY e.project_sequence,e.work_item_id,e.sequence LIMIT $5`, organization, p.project, p.legacyItem, p.legacySequence, TailRows+1); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

func readTail(ctx context.Context, db pgquery.DBTX, lag *Lag, query string, args ...any) error {
	rows, err := db.Query(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("read report checkpoint tail: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var event TailEvent
		if err := rows.Scan(&event.WorkItem, &event.Type, &event.ToState, &event.RecordedAt); err != nil {
			return fmt.Errorf("scan report checkpoint tail: %w", err)
		}
		lag.Events = append(lag.Events, event)
		lag.Items[event.WorkItem] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read report checkpoint tail: %w", err)
	}
	if len(lag.Events) > TailRows {
		lag.Unbounded, lag.Events, lag.Items = true, nil, map[string]bool{}
	}
	return nil
}

type Item struct {
	Project  string
	WorkItem string
}

func FirstWorked(ctx context.Context, db pgquery.DBTX, organization tracker.OrganizationID, at time.Time, items []Item, lags map[string]*Lag) (map[Item]time.Time, error) {
	projects := []string{}
	byProject := map[string][]string{}
	wanted := map[Item]bool{}
	seenProject := map[string]bool{}
	for _, item := range items {
		if lag := lags[item.Project]; lag != nil && lag.Unbounded {
			continue
		}
		if !seenProject[item.Project] {
			seenProject[item.Project] = true
			projects = append(projects, item.Project)
		}
		wanted[item] = true
		byProject[item.Project] = append(byProject[item.Project], item.WorkItem)
	}
	out := map[Item]time.Time{}
	if len(wanted) == 0 {
		return out, nil
	}
	qualifying := map[Item]bool{}
	names := map[string][]string{}
	rows, err := db.Query(ctx, `SELECT project_id,name FROM project_workflow_states WHERE organization_id=$1 AND project_id=ANY($2::text[]) AND dispatchable AND NOT terminal AND NOT name=ANY($3::text[]) ORDER BY project_id,name LIMIT 5001`, organization, projects, excludedWorkStates)
	if err != nil {
		return nil, fmt.Errorf("read report work states: %w", err)
	}
	for rows.Next() {
		var state Item
		if err := rows.Scan(&state.Project, &state.WorkItem); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan report work state: %w", err)
		}
		qualifying[state] = true
		names[state.Project] = append(names[state.Project], state.WorkItem)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read report work states: %w", err)
	}
	keep := func(item Item, state string, recorded time.Time) {
		if !wanted[item] || !qualifying[Item{Project: item.Project, WorkItem: state}] || recorded.After(at) {
			return
		}
		if first, ok := out[item]; !ok || recorded.Before(first) {
			out[item] = recorded.UTC()
		}
	}
	for _, project := range projects {
		if err := readEntries(ctx, db, organization, project, byProject[project], names[project], keep); err != nil {
			return nil, err
		}
	}
	for _, project := range projects {
		for _, event := range lags[project].events() {
			if event.Type == "workflow.transitioned" {
				keep(Item{Project: project, WorkItem: event.WorkItem}, event.ToState, event.RecordedAt)
			}
		}
	}
	return out, nil
}

func (l *Lag) events() []TailEvent {
	if l == nil {
		return nil
	}
	return l.Events
}

type Landing struct {
	Present         bool
	LandedAt        time.Time
	Observed        int
	SuccessPosition int
	SuccessAttempts int
	Complete        bool
	LastStartedAt   *time.Time
	LastID          string
}

func (l Landing) Tries(limit int, landedAt, from time.Time) (attempts int, complete bool, observed int) {
	if landedAt.Before(from) {
		return 0, false, 0
	}
	window := limit + 1
	observed = min(l.Observed, window)
	if l.SuccessPosition == 0 || l.SuccessPosition > window {
		return 0, false, observed
	}
	return l.SuccessAttempts, l.Complete, observed
}

func (l Landing) Current(landedAt time.Time, revision, landingRevision int64, lastStartedAt *time.Time, lastID string) bool {
	if !l.Present || !l.LandedAt.Equal(landedAt) || revision == 0 || landingRevision != revision || l.LastID != lastID {
		return false
	}
	if l.LastStartedAt == nil || lastStartedAt == nil {
		return l.LastStartedAt == nil && lastStartedAt == nil
	}
	return l.LastStartedAt.Equal(*lastStartedAt)
}

func readEntries(ctx context.Context, db pgquery.DBTX, organization tracker.OrganizationID, project string, items, states []string, keep func(Item, string, time.Time)) error {
	if len(items) == 0 || len(states) == 0 {
		return nil
	}
	rows, err := db.Query(ctx, `SELECT f.work_item_id,f.state,f.first_entered_at FROM report_issue_state_entries f WHERE f.organization_id=$1 AND f.project_id=$2 AND f.work_item_id=ANY($3::text[]) AND f.state=ANY($4::text[])`, organization, project, items, states)
	if err != nil {
		return fmt.Errorf("read report state entries: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		item := Item{Project: project}
		var state string
		var entered pgtype.Timestamptz
		if err := rows.Scan(&item.WorkItem, &state, &entered); err != nil {
			return fmt.Errorf("scan report state entry: %w", err)
		}
		keep(item, state, entered.Time)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read report state entries: %w", err)
	}
	return nil
}
