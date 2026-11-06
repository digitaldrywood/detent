package hubserver

import (
	"context"
	"net/url"
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
	currentLease := `EXISTS(SELECT 1 FROM issues i JOIN leases l ON l.issue_id=i.id WHERE i.organization_id=c.organization_id AND i.project_id=c.project_id AND i.native_id=c.work_item_id AND l.fencing_token=c.execution_fence AND l.released_at IS NULL AND julianday(c.expires_at)>julianday(?) AND julianday(l.renewed_at)<=julianday(?) AND julianday(l.expires_at)>julianday(?))`
	statement := "SELECT b.state,count(*),sum(b.running AND " + strings.ReplaceAll(currentLease, "c.", "b.") + ") FROM native_board_items b WHERE " + where + " GROUP BY b.state"
	if !filtered {
		statement = "SELECT b.state,b.total,(SELECT count(*) FROM native_board_items c WHERE c.organization_id=b.organization_id AND c.project_id=b.project_id AND c.archived=b.archived AND c.state=b.state AND c.running=1 AND " + currentLease + ") FROM native_board_counts b WHERE " + where + " AND b.total>0"
	}
	args = append([]any{formatHubTime(now), formatHubTime(now), formatHubTime(now)}, args...)
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
	return s.readBoardRecords(ctx, scope, params)
}
