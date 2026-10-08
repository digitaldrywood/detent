package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func nativeBoardFilter(scope nativeScope, params url.Values, lanes bool) (string, []any, error) {
	var where strings.Builder
	where.WriteString("b.organization_id=? AND b.project_id=?")
	args := []any{scope.organization, scope.project}
	switch params.Get("archived") {
	case "", "false":
		where.WriteString(" AND b.archived=0")
	case "true":
		where.WriteString(" AND b.archived=1")
	case "all":
	default:
		return "", nil, nativeInvalid("archived must be true, false or all")
	}
	if lanes {
		switch params.Get("open") {
		case "":
		case "true":
			where.WriteString(" AND b.terminal=0")
		case "false":
			where.WriteString(" AND b.terminal=1")
		default:
			return "", nil, nativeInvalid("open must be true or false")
		}
	}
	for _, filter := range []struct{ name, clause string }{
		{"state", "b.state=?"},
		{"label", "EXISTS(SELECT 1 FROM json_each(b.record_json,'$.issue.labels') WHERE value=?)"},
		{"assignee", "EXISTS(SELECT 1 FROM json_each(b.record_json,'$.issue.assignees') WHERE value=?)"},
		{"priority", "CAST(json_extract(b.record_json,'$.issue.priority') AS TEXT)=?"},
	} {
		if filter.name == "state" && !lanes {
			continue
		}
		var alternatives []string
		for _, value := range params[filter.name] {
			if value == "" {
				continue
			}
			alternatives = append(alternatives, filter.clause)
			args = append(args, value)
		}
		if len(alternatives) > 0 {
			where.WriteString(" AND (" + strings.Join(alternatives, " OR ") + ")")
		}
	}
	if value := strings.TrimSpace(params.Get("q")); value != "" {
		where.WriteString(` AND (instr(lower(json_extract(b.record_json,'$.issue.title')),lower(?))>0
 OR instr(lower((SELECT name FROM projects WHERE id=b.project_id)||'#'||json_extract(b.record_json,'$.issue.number')),lower(?))>0
 OR instr(lower(b.identifier),lower(?))>0
 OR EXISTS(SELECT 1 FROM json_each(b.record_json,'$.issue.labels') WHERE instr(lower(value),lower(?))>0))`)
		args = append(args, value, value, value, value)
	}
	return where.String(), args, nil
}

func readNativeBoardSummary(ctx context.Context, q nativeQueryer, scope nativeScope, params url.Values, now time.Time) (*tracker.NativeWorkSummary, error) {
	where, args, err := nativeBoardFilter(scope, params, false)
	if err != nil {
		return nil, err
	}
	summary := &tracker.NativeWorkSummary{Items: []tracker.NativeIssue{}, Lanes: []tracker.NativeWorkLane{}, AsOf: now}
	filtered := strings.TrimSpace(params.Get("q")) != "" || len(params["label"])+len(params["assignee"])+len(params["priority"]) > 0
	statement := "SELECT b.state,count(*),sum(b.running) FROM native_board_items b WHERE " + where + " GROUP BY b.state"
	if !filtered {
		statement = "SELECT state,total,running FROM native_board_counts b WHERE " + where + " AND total>0"
	}
	rows, err := q.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var lane tracker.NativeWorkLane
		if err := rows.Scan(&lane.State, &lane.Total, &lane.Running); err != nil {
			return nil, err
		}
		summary.Lanes = append(summary.Lanes, lane)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	window, err := nativeCompletedWindow(params.Get("completed_window"))
	if err != nil {
		return nil, err
	}
	statement = "SELECT count(*) FROM native_board_items b WHERE " + where + " AND b.terminal=1"
	if params.Get("completed_window") != "all" {
		statement += " AND b.closed_at<>'' AND b.closed_at BETWEEN ? AND ?"
		args = append(args, now.Add(-window).UTC().Format("2006-01-02T15:04:05.000000000Z"), now.UTC().Format("2006-01-02T15:04:05.000000000Z"))
	}
	err = q.QueryRowContext(ctx, statement, args...).Scan(&summary.Completed)
	return summary, err
}

func (s *Service) readBoardIssues(ctx context.Context, scope nativeScope, params url.Values) (tracker.NativeIssuePage, error) {
	page := tracker.NativeIssuePage{Page: tracker.Page[tracker.NativeIssue]{Items: []tracker.NativeIssue{}}, Cards: []tracker.NativeBoardCard{}}
	if err := validateNativeIssueQuery(params); err != nil {
		return page, err
	}
	if params.Get("include") != "board" || params.Has("fingerprint") {
		return page, nativeInvalid("board cannot include other projections or fingerprints")
	}
	if sort := params.Get("sort"); sort != "" && sort != "closed" {
		return page, nativeInvalid("sort supports closed")
	}
	pageParams := params
	requestedLimit, limitErr := strconv.Atoi(params.Get("limit"))
	if limitErr == nil && requestedLimit > 200 && requestedLimit <= 2000 {
		pageParams = url.Values{}
		for name, values := range params {
			pageParams[name] = values
		}
		pageParams.Set("limit", "200")
	}
	limit, cursor, key, err := s.readNativePage(ctx, scope, "/board/"+string(scope.project), pageParams)
	if limitErr == nil && requestedLimit > 200 && requestedLimit <= 2000 {
		limit = requestedLimit
	}
	if err != nil {
		return page, err
	}
	tx, err := s.database.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return page, err
	}
	defer tx.Rollback()
	err = tx.QueryRowContext(ctx, "SELECT sequence FROM native_board_sequences WHERE organization_id=? AND project_id=?", scope.organization, scope.project).Scan(&page.Sequence)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return page, err
	}
	page.Work, err = readNativeBoardSummary(ctx, tx, scope, params, s.config.now())
	if err != nil {
		return page, err
	}
	where, args, err := nativeBoardFilter(scope, params, true)
	if err != nil {
		return page, err
	}
	states := map[string]bool{}
	if params.Get("open") != "" {
		rows, err := tx.QueryContext(ctx, "SELECT detent_state,terminal FROM workflow_states WHERE project_id=?", scope.project)
		if err != nil {
			return page, err
		}
		defer rows.Close()
		for rows.Next() {
			var state string
			var terminal bool
			if err := rows.Scan(&state, &terminal); err != nil {
				return page, err
			}
			states[state] = terminal
		}
		if err := rows.Err(); err != nil {
			return page, err
		}
		if err := rows.Close(); err != nil {
			return page, err
		}
	}
	for _, lane := range page.Work.Lanes {
		if len(params["state"]) > 0 && !slices.Contains(params["state"], lane.State) {
			continue
		}
		if params.Get("open") != "" && states[lane.State] != (params.Get("open") == "false") {
			continue
		}
		page.Total += lane.Total
	}
	grants, err := nativeBoardGrants(ctx, tx, scope)
	if err != nil {
		return page, err
	}
	singleLane := len(params["state"]) == 1 && params.Get("state") != ""
	order := "b.lane_rank,b.state,b.priority_rank,b.activity_missing,b.activity_at DESC,b.identifier"
	if singleLane {
		order = "b.priority_rank,b.activity_missing,b.activity_at DESC,b.identifier"
	}
	if params.Get("sort") == "closed" {
		order = "b.closed_at DESC,b.identifier"
	}
	if p := cursor.Issues; p != nil {
		if params.Get("sort") == "closed" {
			where += " AND (b.closed_at<? OR (b.closed_at=? AND b.identifier>?))"
			args = append(args, p.Activity, p.Activity, p.Identifier)
		} else if singleLane {
			where += ` AND ((b.priority_rank,b.activity_missing)>(?,?) OR ((b.priority_rank,b.activity_missing)=(?,?) AND (b.activity_at<? OR (b.activity_at=? AND b.identifier>?))))`
			args = append(args, p.Priority, p.ActivityMissing, p.Priority, p.ActivityMissing, p.Activity, p.Activity, p.Identifier)
		} else {
			prefix := []any{p.Lane, p.State, p.Priority, p.ActivityMissing}
			where += ` AND ((b.lane_rank,b.state,b.priority_rank,b.activity_missing)>(?,?,?,?) OR
 ((b.lane_rank,b.state,b.priority_rank,b.activity_missing)=(?,?,?,?) AND (b.activity_at<? OR (b.activity_at=? AND b.identifier>?))))`
			args = append(args, prefix...)
			args = append(args, prefix...)
			args = append(args, p.Activity, p.Activity, p.Identifier)
		}
	}
	args = append(args, limit+1)
	statement := strings.Join([]string{"SELECT b.record_json,b.lane_rank,b.state,b.priority_rank,b.activity_missing,b.activity_at,b.identifier,b.closed_at FROM native_board_items b WHERE", where, "ORDER BY", order, "LIMIT ?"}, " ")
	rows, err := tx.QueryContext(ctx, statement, args...)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		if len(page.Cards) == limit {
			page.NextCursor, err = encodeNativeCursor(cursor, key)
			if err != nil {
				return page, err
			}
			break
		}
		var raw, closed string
		position := &nativeIssuePageCursor{}
		if err := rows.Scan(&raw, &position.Lane, &position.State, &position.Priority, &position.ActivityMissing, &position.Activity, &position.Identifier, &closed); err != nil {
			return page, err
		}
		if params.Get("sort") == "closed" {
			position.Activity = closed
		}
		var card tracker.NativeBoardCard
		if err := json.Unmarshal([]byte(raw), &card); err != nil {
			return page, err
		}
		redactNativeBoardCard(&card, grants)
		card.Issue = s.nativeIssueResponse(card.Issue)
		page.Cards = append(page.Cards, card)
		page.Items = append(page.Items, card.Issue)
		cursor.Issues = position
	}
	if err := rows.Err(); err != nil {
		return page, err
	}
	if err := rows.Close(); err != nil {
		return page, err
	}
	return page, tx.Commit()
}
