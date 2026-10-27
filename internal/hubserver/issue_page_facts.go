package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func updateIssueActor(ctx context.Context, tx *sql.Tx, id int64, actor tracker.Actor) error {
	_, err := tx.ExecContext(ctx, "UPDATE issues SET actor_kind=?,actor_principal_id=? WHERE id=?", actor.Kind, actor.PrincipalID, id)
	return err
}

func updateEventPageFacts(ctx context.Context, tx *sql.Tx, id string, actor tracker.Actor, data tracker.CollaborationData) error {
	var revision any
	if data.Revision != 0 {
		revision = data.Revision
	}
	var attempt string
	var fence tracker.FencingToken
	if data.Run != nil {
		attempt, fence = data.Run.AttemptID, data.Run.FencingToken
	}
	_, err := tx.ExecContext(ctx, `UPDATE collaboration_events SET page_projection_ready=1,actor_kind=?,actor_principal_id=?,event_revision=?,from_state=?,to_state=?,reason=?,run_present=?,run_attempt_id=?,run_fencing_token=?,edited_title=?,edited_body=? WHERE id=?`, actor.Kind, actor.PrincipalID, revision, data.FromState, data.ToState, data.Reason, data.Run != nil, attempt, fence, slices.Contains(data.Fields, "title"), slices.Contains(data.Fields, "body"), id)
	return err
}

func updateBoardFilterFacts(ctx context.Context, tx nativeExecer, card tracker.NativeBoardCard) error {
	issue := card.Issue
	var verification any
	if issue.Verification != nil {
		verification = issue.Verification.State
	}
	if _, err := tx.ExecContext(ctx, `UPDATE native_board_items SET actor_principal_id=?,issue_type=?,verification_state=?,priority=? WHERE work_item_id=?`, issue.Actor.PrincipalID, issue.Type, verification, issue.Priority, issue.WorkItemID); err != nil {
		return err
	}
	return replaceBoardPageFilterValues(ctx, tx, card)
}

type nativePageFact struct {
	name, columns, known string
	values               [][]any
	override             bool
}
type nativePageQuery struct {
	nativeQueryer
	facts        map[string]nativePageFact
	organization tracker.OrganizationID
	project      tracker.ProjectID
	item         string
}

func (q *nativePageQuery) statement(query string, args []any) (string, []any) {
	var clauses []string
	var bindings []any
	for _, name := range []string{"native_issue_actors", "native_transition_facts", "native_workflow_ranks", "native_board_filter_facts", "native_board_filter_values", "native_issue_page_filter_values"} {
		f, ok := q.facts[name]
		if !ok || len(f.values) == 0 && !f.override || !strings.Contains(query, name) {
			continue
		}
		var values []string
		for _, row := range f.values {
			values = append(values, "("+strings.TrimSuffix(strings.Repeat("?,", len(row)), ",")+")")
			bindings = append(bindings, row...)
		}
		selection := f.known
		if len(values) > 0 {
			selection += " UNION ALL SELECT * FROM (VALUES " + strings.Join(values, ",") + ")"
		}
		clauses = append(clauses, name+"("+f.columns+") AS ("+selection+")")
	}
	if len(clauses) == 0 {
		return query, args
	}
	prefix := "WITH "
	trimmed := strings.TrimSpace(query)
	if strings.HasPrefix(strings.ToUpper(trimmed), "WITH RECURSIVE ") {
		prefix = "WITH RECURSIVE "
		query = strings.TrimSpace(trimmed[len("WITH RECURSIVE "):])
		query = strings.Join(clauses, ",") + "," + query
	} else if strings.HasPrefix(strings.ToUpper(trimmed), "WITH ") {
		query = strings.Join(clauses, ",") + "," + strings.TrimSpace(trimmed[len("WITH "):])
	} else {
		query = strings.Join(clauses, ",") + " " + query
	}
	return prefix + query, append(bindings, args...)
}
func (q *nativePageQuery) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	query, args = q.statement(query, args)
	return q.nativeQueryer.QueryContext(ctx, query, args...)
}
func (q *nativePageQuery) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	query, args = q.statement(query, args)
	return q.nativeQueryer.QueryRowContext(ctx, query, args...)
}

func prepareNativePageQuery(ctx context.Context, q nativeQueryer, scope nativeScope, item string, families ...string) (nativeQueryer, error) {
	if slices.Contains(families, "native_board_filter_facts") && !slices.Contains(families, "native_board_filter_values") {
		families = append(families, "native_board_filter_values")
	}
	prepared := &nativePageQuery{nativeQueryer: q, facts: map[string]nativePageFact{}, organization: scope.organization, project: scope.project, item: item}
	if prior, ok := q.(*nativePageQuery); ok {
		prepared.nativeQueryer = prior.nativeQueryer
		if prior.organization == scope.organization && prior.project == scope.project && prior.item == item {
			for key, fact := range prior.facts {
				prepared.facts[key] = fact
			}
		}
	}
	for _, family := range families {
		if _, ok := prepared.facts[family]; ok {
			continue
		}
		f, err := readLegacyPageFact(ctx, prepared.nativeQueryer, scope, item, family)
		if err != nil {
			return nil, err
		}
		prepared.facts[family] = f
	}
	return prepared, nil
}

func readLegacyPageFact(ctx context.Context, q nativeQueryer, scope nativeScope, item, family string) (nativePageFact, error) {
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
	if scope.organization != "" {
		query += " AND " + org + "=?"
		args = append(args, scope.organization)
	}
	if scope.project != "" {
		query += " AND " + project + "=?"
		args = append(args, scope.project)
	}
	if item != "" && family != "native_workflow_ranks" {
		query += " AND " + key + "=?"
		args = append(args, item)
	}
	query += " LIMIT ?"
	args = append(args, storageConversionRows+1)
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return f, err
	}
	defer rows.Close()
	bytes := 0
	for rows.Next() {
		if len(f.values) >= storageConversionRows {
			return f, operatortool.ErrReadUnavailable
		}
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
		bytes += len(raw)
		if bytes > storageConversionBytes {
			return f, operatortool.ErrReadUnavailable
		}
		f.values = append(f.values, values)
	}
	return f, rows.Err()
}

func nativeIssuePrincipal(alias string) string {
	return "(SELECT actor_principal_id FROM native_issue_actors WHERE id=" + alias + ".id)"
}
func nativeBoardFact(column string) string {
	return "(SELECT " + column + " FROM native_board_filter_facts WHERE work_item_id=b.work_item_id)"
}

func decodePageEvent(raw string, data *tracker.CollaborationData) error {
	if !strings.HasPrefix(strings.TrimSpace(raw), "{") {
		return nil
	}
	return json.Unmarshal([]byte(raw), data)
}

func prepareNativePageStatement(ctx context.Context, q nativeQueryer, scope nativeScope, item, statement string) (nativeQueryer, error) {
	var families []string
	for _, name := range []string{"native_issue_actors", "native_transition_facts", "native_workflow_ranks", "native_board_filter_facts", "native_board_filter_values", "native_issue_page_filter_values"} {
		if strings.Contains(statement, name) {
			families = append(families, name)
		}
	}
	if len(families) == 0 {
		return q, nil
	}
	return prepareNativePageQuery(ctx, q, scope, item, families...)
}
