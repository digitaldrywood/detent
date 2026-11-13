package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type nativePageFact struct {
	name, columns, known string
	knownArgs            []any
	values               [][]any
	override             bool
}
type nativePageQuery struct {
	hubapi.NativeQueryer
	facts        map[string]nativePageFact
	organization tracker.OrganizationID
	project      tracker.ProjectID
	item         string
}

func prepareNativePageQuery(ctx context.Context, q hubapi.NativeQueryer, scope hubscope.NativeScope, item string, families ...string) (hubapi.NativeQueryer, error) {
	if slices.Contains(families, "native_board_filter_facts") && !slices.Contains(families, "native_board_filter_values") {
		families = append(families, "native_board_filter_values")
	}
	prepared := &nativePageQuery{NativeQueryer: q, facts: map[string]nativePageFact{}, organization: scope.Organization, project: scope.Project, item: item}
	if prior, ok := q.(*nativePageQuery); ok {
		prepared.NativeQueryer = prior.NativeQueryer
		if prior.organization == scope.Organization && prior.project == scope.Project && prior.item == item {
			for key, fact := range prior.facts {
				prepared.facts[key] = fact
			}
		}
	}
	for _, family := range families {
		if _, ok := prepared.facts[family]; ok {
			continue
		}
		f, err := readLegacyPageFact(ctx, prepared.NativeQueryer, scope, item, family)
		if err != nil {
			return nil, err
		}
		prepared.facts[family] = f
	}
	return prepared, nil
}

func readLegacyPageFact(ctx context.Context, q hubapi.NativeQueryer, scope hubscope.NativeScope, item, family string) (_ nativePageFact, resultErr error) {
	if family == "report_attempts" || family == "report_events" {
		return readLegacyReportFact(ctx, q, scope, family)
	}
	if family == "native_issue_page_filter_values" || family == "native_board_filter_values" {
		return readLegacyPageFilterValues(ctx, q, scope, item, family)
	}
	f := nativePageFact{name: family}
	var query string
	switch family {
	case "native_issue_actors":
		f.columns = "id,actor_kind,actor_principal_id"
		f.known = "SELECT id,actor_kind,actor_principal_id FROM main.issues WHERE actor_principal_id IS NOT NULL"
		query = "SELECT id,actor_json FROM main.issues WHERE actor_principal_id IS NULL"
	case "native_transition_facts":
		f.columns = "id,organization_id,project_id,work_item_id,sequence,recorded_at,from_state,to_state"
		f.known = "SELECT id,organization_id,project_id,work_item_id,sequence,recorded_at,from_state,to_state FROM main.collaboration_events WHERE type='workflow.transitioned' AND page_projection_ready=1"
		query = "SELECT id,organization_id,project_id,work_item_id,sequence,recorded_at,data_json FROM main.collaboration_events WHERE type='workflow.transitioned' AND page_projection_ready IS NULL AND EXISTS(SELECT 1 FROM main.issues current JOIN main.workflow_states state ON state.id=current.workflow_state_id WHERE current.native_id=collaboration_events.work_item_id AND state.terminal=1)"
	case "native_board_filter_facts":
		f.columns = "work_item_id,actor_principal_id,issue_type,verification_state,priority"
		f.known = "SELECT work_item_id,actor_principal_id,issue_type,verification_state,priority FROM main.native_board_items WHERE actor_principal_id IS NOT NULL"
		query = "SELECT record_json FROM main.native_board_items WHERE actor_principal_id IS NULL"
	case "native_workflow_ranks":
		f.columns = "id,board_rank"
		f.known = "SELECT id,board_rank FROM main.workflow_states WHERE board_rank IS NOT NULL"
		query = "SELECT w.id,w.detent_state,p.states_json FROM main.workflow_states w JOIN main.projects p ON p.id=w.project_id WHERE w.board_rank IS NULL"
	default:
		return f, errors.New("unknown issue page projection")
	}
	var args []any
	org, project, key := "organization_id", "project_id", "native_id"
	if family == "native_transition_facts" || family == "native_board_filter_facts" {
		key = "work_item_id"
	}
	if family == "native_workflow_ranks" {
		org, project = "p.organization_id", "p.id"
	}
	if scope.Organization != "" {
		query += " AND " + org + "=?"
		args = append(args, scope.Organization)
	}
	if scope.Project != "" {
		query += " AND " + project + "=?"
		args = append(args, scope.Project)
	}
	if item != "" && family != "native_workflow_ranks" {
		query += " AND " + key + "=?"
		args = append(args, item)
	}
	order := "organization_id,project_id,id"
	switch family {
	case "native_board_filter_facts":
		order = "organization_id,project_id,work_item_id"
	case "native_transition_facts":
		order = "organization_id,project_id,work_item_id,sequence"
	case "native_workflow_ranks":
		order = "w.id"
	}
	rows, err := queryLegacyPageRows(ctx, q, query, order, args...)
	if err != nil {
		return f, err
	}
	defer func(rows *hubrecords.ChangeLegacyRows) { resultErr = errors.Join(resultErr, rows.Close()) }(rows)
	for rows.Next() {
		var raw string
		var values []any
		switch family {
		case "native_issue_actors":
			var id int64
			if err := rows.Scan(&id, &raw); err != nil {
				return f, err
			}
			var actor tracker.Actor
			if err := json.Unmarshal([]byte(raw), &actor); err != nil {
				return f, err
			}
			values = []any{id, actor.Kind, actor.PrincipalID}
		case "native_transition_facts":
			var id, org, project, item, recorded string
			var sequence int64
			if err := rows.Scan(&id, &org, &project, &item, &sequence, &recorded, &raw); err != nil {
				return f, err
			}
			var data tracker.CollaborationData
			if err := decodePageEvent(raw, &data); err != nil {
				return f, err
			}
			values = []any{id, org, project, item, sequence, recorded, data.FromState, data.ToState}
		case "native_workflow_ranks":
			var id int64
			var state string
			if err := rows.Scan(&id, &state, &raw); err != nil {
				return f, err
			}
			var states []tracker.NativeState
			if err := json.Unmarshal([]byte(raw), &states); err != nil {
				return f, err
			}
			rank := 2147483647
			for i, s := range states {
				if s.Name == state {
					rank = i
					break
				}
			}
			values = []any{id, rank}
		case "native_board_filter_facts":
			if err := rows.Scan(&raw); err != nil {
				return f, err
			}
			var card tracker.NativeBoardCard
			if err := json.Unmarshal([]byte(raw), &card); err != nil {
				return f, err
			}
			var verification any
			if card.Issue.Verification != nil {
				verification = card.Issue.Verification.State
			}
			values = []any{card.Issue.WorkItemID, card.Issue.Actor.PrincipalID, card.Issue.Type, verification, card.Issue.Priority}
		}
		f.values = append(f.values, values)
	}
	return f, rows.Err()
}

func decodePageEvent(raw string, data *tracker.CollaborationData) error {
	if !strings.HasPrefix(strings.TrimSpace(raw), "{") {
		return nil
	}
	return json.Unmarshal([]byte(raw), data)
}
