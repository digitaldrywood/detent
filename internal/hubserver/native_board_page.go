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
	for _, bound := range []struct{ name, operator string }{{"completed_from", ">="}, {"completed_to", "<"}} {
		if value := params.Get(bound.name); value != "" {
			at, err := time.Parse(time.RFC3339Nano, value)
			if err != nil {
				return "", nil, nativePublicInvalid("Completion window bounds must be RFC3339 timestamps")
			}
			where.WriteString(" AND b.closed_at<>'' AND b.closed_at" + bound.operator + "?")
			args = append(args, nativeBoardTime(at))
		}
	}
	if scope.credential.HostedRole == "reporter" {
		where.WriteString(" AND b.work_item_id IN (SELECT i.native_id FROM issues i WHERE i.organization_id=b.organization_id AND i.project_id=b.project_id AND ")
		visibility, visibilityArgs := scope.issueVisibilitySQL("i")
		where.WriteString(visibility + ")")
		args = append(args, visibilityArgs...)
	}
	createdBy, err := nativeCreatedBy(scope, params)
	if err != nil {
		return "", nil, err
	}
	if createdBy != "" {
		where.WriteString(" AND " + nativeBoardFact("actor_principal_id") + " = ?")
		args = append(args, createdBy)
	}
	switch params.Get("archived") {
	case "", "false":
		where.WriteString(" AND b.archived=0")
	case "true":
		where.WriteString(" AND b.archived=1")
	case "all":
	default:
		return "", nil, nativePublicInvalid("archived must be true, false or all")
	}
	if lanes {
		switch params.Get("open") {
		case "":
		case "true":
			where.WriteString(" AND b.terminal=0")
		case "false":
			where.WriteString(" AND b.terminal=1")
		default:
			return "", nil, nativePublicInvalid("open must be true or false")
		}
	}
	for _, filter := range []struct{ name, clause string }{
		{"type", nativeBoardFact("issue_type") + "=?"},
		{"verification", nativeBoardFact("verification_state") + "=?"},
		{"state", "b.state=?"},
		{"label", "EXISTS(SELECT 1 FROM native_board_filter_values WHERE work_item_id=b.work_item_id AND kind='label' AND value=?)"},
		{"assignee", "EXISTS(SELECT 1 FROM native_board_filter_values WHERE work_item_id=b.work_item_id AND kind='assignee' AND value=?)"},
		{"priority", "CAST(" + nativeBoardFact("priority") + " AS TEXT)=?"},
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
		selection, searchArgs, err := nativeIssueSelection(scope, url.Values{"q": {value}, "archived": {"all"}}, false)
		if err != nil {
			return "", nil, err
		}
		where.WriteString(" AND b.work_item_id IN (" + selection + ")")
		args = append(args, searchArgs...)
	}
	return where.String(), args, nil
}

func readNativeBoardSummary(ctx context.Context, q nativeQueryer, scope nativeScope, params url.Values, now time.Time) (*tracker.NativeWorkSummary, error) {
	where, args, err := nativeBoardFilter(scope, params, false)
	if err != nil {
		return nil, err
	}
	summary := &tracker.NativeWorkSummary{Items: []tracker.NativeIssue{}, Lanes: []tracker.NativeWorkLane{}, AsOf: now}
	filtered := scope.credential.HostedRole == "reporter" || params.Get("completed_from") != "" || params.Get("completed_to") != "" || strings.TrimSpace(params.Get("created_by")) != "" || strings.TrimSpace(params.Get("q")) != "" || len(params["type"])+len(params["verification"])+len(params["label"])+len(params["assignee"])+len(params["priority"]) > 0
	statement := "SELECT b.state,count(*),sum(b.running) FROM native_board_items b WHERE " + where + " GROUP BY b.state"
	if !filtered {
		statement = "SELECT state,total,running FROM native_board_counts b WHERE " + where + " AND total>0"
	}
	pageQuery, pageErr := prepareNativePageStatement(ctx, q, scope, "", statement)
	if pageErr != nil {
		return nil, pageErr
	}
	rows, err := pageQuery.QueryContext(ctx, statement, args...)
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
	selected, err := nativeShippedWindow(params.Get("completed_window"))
	if err != nil {
		return nil, err
	}
	closedParams := url.Values{}
	for name, values := range params {
		closedParams[name] = values
	}
	closedParams.Set("archived", "all")
	closedWhere, closedArgs, err := nativeBoardFilter(scope, closedParams, false)
	if err != nil {
		return nil, err
	}
	closedQuery, err := prepareNativePageStatement(ctx, pageQuery, scope, "", closedWhere)
	if err != nil {
		return nil, err
	}
	windows, err := readNativeShippedWindows(ctx, closedQuery, scope, closedWhere, closedArgs, now)
	if err != nil {
		return nil, err
	}
	applyNativeShippedWindows(summary, windows, selected)
	return summary, nil
}

func (s *Service) readBoardIssues(ctx context.Context, scope nativeScope, params url.Values) (tracker.NativeIssuePage, error) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	page := tracker.NativeIssuePage{Page: tracker.Page[tracker.NativeIssue]{Items: []tracker.NativeIssue{}}, Cards: []tracker.NativeBoardCard{}}
	if err := validateNativeIssueQuery(params); err != nil {
		return page, err
	}
	if params.Get("include") != "board" || params.Has("fingerprint") {
		return page, nativePublicInvalid("board cannot include other projections or fingerprints")
	}
	if sort := params.Get("sort"); sort != "" && sort != "closed" {
		return page, nativePublicInvalid("sort supports closed")
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
	page.ChangeSequence = page.Sequence
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
	page.GrantVersion = nativeBoardGrantVersion(grants)
	singleLane := len(params["state"]) == 1 && params.Get("state") != ""
	order := "b.lane_rank,b.state,b.priority_rank,b.activity_missing,b.activity_at DESC,b.identifier"
	if singleLane {
		order = "b.priority_rank,b.activity_missing,b.activity_at DESC,b.identifier"
	}
	if params.Get("sort") == "closed" {
		order = "b.closed_at DESC,b.identifier"
	}
	from, searchColumns := "native_board_items b", ",0,''"
	if search := strings.TrimSpace(params.Get("q")); search != "" {
		selection, searchArgs, err := nativeIssueSelection(scope, params, false)
		if err != nil {
			return page, err
		}
		selection = strings.Replace(selection, "SELECT i.native_id", "SELECT i.native_id"+nativeSearchProjection, 1)
		from += " JOIN (" + selection + ") matched ON matched.native_id=b.work_item_id"
		args = append(append([]any{search}, searchArgs...), args...)
		searchColumns = ",matched.search_score,matched.search_snippet"
		order = "matched.search_score," + order
	}
	if p := cursor.Issues; p != nil {
		whereStart, argsStart := len(where), len(args)
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
		if strings.TrimSpace(params.Get("q")) != "" {
			where = where[:whereStart] + " AND (matched.search_score>? OR (matched.search_score=?" + where[whereStart:] + "))"
			args = append(append(append([]any{}, args[:argsStart]...), p.Score, p.Score), args[argsStart:]...)
		}
	}
	args = append(args, limit+1)
	statement := strings.Join([]string{"SELECT b.record_json," + nativeActorDisplayName(nativeBoardFact("actor_principal_id"), "b.organization_id") + ",b.lane_rank,b.state,b.priority_rank,b.activity_missing,b.activity_at,b.identifier,b.closed_at" + searchColumns + " FROM " + from + " WHERE", where, "ORDER BY", order, "LIMIT ?"}, " ")
	pageQuery, err := prepareNativePageStatement(ctx, tx, scope, "", statement)
	if err != nil {
		return page, err
	}
	rows, err := pageQuery.QueryContext(ctx, statement, args...)
	if err != nil {
		return page, err
	}
	var currentPolicy *changeSummaryPolicy
	defer rows.Close()
	for rows.Next() {
		if len(page.Cards) == limit {
			page.NextCursor, err = encodeNativeCursor(cursor, key)
			if err != nil {
				return page, err
			}
			break
		}
		var raw, closed, displayName, snippet string
		position := &nativeIssuePageCursor{}
		if err := rows.Scan(&raw, &displayName, &position.Lane, &position.State, &position.Priority, &position.ActivityMissing, &position.Activity, &position.Identifier, &closed, &position.Score, &snippet); err != nil {
			return page, err
		}
		if params.Get("sort") == "closed" {
			position.Activity = closed
		}
		var card tracker.NativeBoardCard
		if err := json.Unmarshal([]byte(raw), &card); err != nil {
			return page, err
		}
		if card.Change != nil {
			if currentPolicy == nil {
				loaded, err := readChangeSummaryPolicy(ctx, tx, scope)
				if err != nil {
					return page, err
				}
				currentPolicy = &loaded
			}
			change, err := readChange(ctx, tx, scope, string(card.Issue.WorkItemID), card.Change.ID)
			if isNativeNotFound(err) && scope.credential.HostedRole == "reporter" {
				card.Change = nil
			} else if err != nil {
				return page, err
			} else {
				detail, err := readCurrentChangeDetailWithPolicy(ctx, tx, scope, change, s.config.now(), currentPolicy)
				if err != nil {
					return page, err
				}
				card.Change.Status = detail.Summary.Status
			}
		}
		card.Issue.Actor.DisplayName = displayName
		card.Issue.Snippet = snippet
		card.Issue.SearchScore = position.Score
		page.Cards = append(page.Cards, card)
		cursor.Issues = position
	}
	if err := rows.Err(); err != nil {
		return page, err
	}
	if err := rows.Close(); err != nil {
		return page, err
	}
	issues := make([]*tracker.NativeIssue, len(page.Cards))
	for index := range page.Cards {
		issues[index] = &page.Cards[index].Issue
	}
	if err := readNativeDependencyDetailsBatch(ctx, tx, scope, issues); err != nil {
		return page, err
	}
	for index := range page.Cards {
		card := &page.Cards[index]
		redactNativeBoardCard(card, grants)
		card.Issue = s.nativeIssueResponse(scope.reporterIssue(card.Issue))
		page.Items = append(page.Items, card.Issue)
	}
	return page, tx.Commit()
}
