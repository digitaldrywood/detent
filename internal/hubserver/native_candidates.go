package hubserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func nativeCandidateIDs(ctx context.Context, tx nativeQueryer, query claimCandidateQuery, repositoryIDs []tracker.RepositoryID, repositories, states, authors, assignees, included, excluded []string) ([]tracker.WorkItemID, error) {
	args := []any{}
	bind := func(value any) string {
		args = append(args, value)
		return "?"
	}
	var encodingErr error
	jsonList := func(value any) string {
		raw, err := marshalNative(value)
		if err != nil {
			encodingErr = err
		}
		return bind(raw)
	}
	stateRanks := normalizedQueryStrings(query.DispatchPriorityByState)
	labelRanks := normalizedQueryStrings(query.DispatchPriorityByLabel)
	merging := len(query.DispatchPriorityByState) > 0 && strings.EqualFold(strings.TrimSpace(query.DispatchPriorityByState[0]), "merging")
	statement := strings.Join([]string{`WITH candidates AS NOT MATERIALIZED (
SELECT i.id,
 CASE WHEN `,
		bind(merging),
		` AND lower(trim(ws.detent_state)) = 'merging' THEN 0 ELSE 1 END AS merging,
 CASE WHEN q.priority_override BETWEEN 0 AND 3 THEN q.priority_override + 1 ELSE 5 END AS priority,
 COALESCE((SELECT min(CAST(pref.key AS INTEGER)) FROM json_each(`,
		jsonList(labelRanks),
		`) pref
  WHERE EXISTS (SELECT 1 FROM json_each(i.labels_json) label WHERE lower(trim(label.value)) = pref.value)), `,
		bind(len(labelRanks)),
		`) AS label_rank,
 COALESCE((SELECT CAST(pref.key AS INTEGER) FROM json_each(`,
		jsonList(stateRanks),
		`) pref WHERE pref.value = lower(trim(ws.detent_state))), `,
		bind(len(stateRanks)),
		`) AS state_rank,
 CASE WHEN `,
		bind(query.PrioritizeUnblockers),
		` THEN -(SELECT count(DISTINCT d.dependent_issue_id) FROM issue_dependencies d
  JOIN issues dependent ON dependent.id = d.dependent_issue_id
  JOIN projects dp ON dp.id = dependent.project_id AND dp.require_dependencies = 1
  LEFT JOIN workflow_states ds ON ds.id = dependent.workflow_state_id
  WHERE d.blocker_issue_id = i.id AND dependent.archived = 0 AND COALESCE(ds.terminal, 0) = 0) ELSE 0 END AS unblockers,
 CASE WHEN trim(COALESCE(q.rank, '')) = '' THEN 1 ELSE 0 END AS unranked,
 trim(COALESCE(q.rank, '')) AS queue_rank,
 substr(COALESCE(i.native_created_at, i.created_at), 1, 19) || '.' || substr(CASE
  WHEN substr(COALESCE(i.native_created_at, i.created_at), 20, 1) = '.'
  THEN substr(COALESCE(i.native_created_at, i.created_at), 21, length(COALESCE(i.native_created_at, i.created_at)) - 21) || '000000000'
  ELSE '000000000' END, 1, 9) AS created,
 i.project_id || '#' || i.number AS identifier
FROM issues i
JOIN projects p ON p.id = i.project_id AND p.organization_id = i.organization_id
JOIN workflow_states ws ON ws.id = i.workflow_state_id
LEFT JOIN repositories r ON r.id = i.repository_id
LEFT JOIN queue_entries q ON q.id = (
 SELECT candidate.id FROM queue_entries candidate WHERE candidate.issue_id = i.id
 AND (`,
		bind(query.Scope),
		` = '' OR candidate.scope = `,
		bind(query.Scope),
		`)
 ORDER BY CASE WHEN candidate.scope = `,
		bind(query.Scope),
		` THEN 0 ELSE 1 END, candidate.scope, candidate.id LIMIT 1)
WHERE i.organization_id = `,
		bind(query.NativeScope.organization),
		` AND p.profile = 'native'
 AND i.archived = 0 AND ws.terminal = 0 AND ws.dispatchable = 1 AND lower(trim(ws.detent_state)) <> 'cancelled'
 AND NOT EXISTS (SELECT 1 FROM github_imports g WHERE g.work_item_id = i.native_id AND g.intake_pending = 1)
 AND (`,
		bind(query.WorkspaceLane),
		` = 1 OR `,
		notWorkspaceItemClause,
		`)
 AND `,
		notAlreadyAnsweredClause,
		`
 AND (p.require_dependencies = 0 OR NOT EXISTS (
 SELECT 1 FROM issue_dependencies dependency JOIN issues blocker ON blocker.id = dependency.blocker_issue_id
 LEFT JOIN workflow_states blocker_state ON blocker_state.id = blocker.workflow_state_id
 WHERE dependency.dependent_issue_id = i.id AND (blocker_state.id IS NULL OR blocker_state.terminal = 0)))`}, "")
	if len(query.HomeProjects) > 0 {
		statement += ` AND i.project_id IN (SELECT value FROM json_each(` + jsonList(query.HomeProjects) + `))`
	} else {
		statement += ` AND i.project_id = ` + bind(query.NativeScope.project)
	}
	if query.Scope != "" || len(query.HomeProjects) > 0 {
		statement += ` AND q.id IS NOT NULL`
	}
	if query.WorkItemID > 0 {
		statement += ` AND i.id = ` + bind(query.WorkItemID)
	}
	if len(query.OnlyIDs) > 0 {
		statement += ` AND i.id IN (SELECT value FROM json_each(` + jsonList(query.OnlyIDs) + `))`
	}
	if len(query.ProviderCandidates) > 0 {
		ids := make([]tracker.NativeWorkItemID, 0, len(query.ProviderCandidates))
		for _, candidate := range query.ProviderCandidates {
			ids = append(ids, candidate.WorkItemID)
		}
		statement += ` AND i.native_id IN (SELECT value FROM json_each(` + jsonList(ids) + `))`
	}
	var filters strings.Builder
	for _, filter := range []struct {
		expression string
		values     []string
	}{
		{"lower(trim(ws.detent_state))", states}, {"lower(trim(i.author_login))", authors},
		{"lower(trim(r.github_owner)) || '/' || lower(trim(r.github_name))", repositories},
	} {
		if len(filter.values) > 0 {
			filters.WriteString(` AND ` + filter.expression + ` IN (SELECT value FROM json_each(` + jsonList(filter.values) + `))`)
		}
	}
	statement += filters.String()
	if len(repositoryIDs) > 0 {
		statement += ` AND i.repository_id IN (SELECT value FROM json_each(` + jsonList(repositoryIDs) + `))`
	}
	if len(assignees) > 0 {
		statement += ` AND EXISTS (SELECT 1 FROM json_each(i.assignees_json) a WHERE lower(trim(a.value)) IN (SELECT value FROM json_each(` + jsonList(assignees) + `)))`
	}
	if len(included) > 0 {
		statement += ` AND NOT EXISTS (SELECT 1 FROM json_each(` + jsonList(included) + `) required WHERE NOT EXISTS (SELECT 1 FROM json_each(i.labels_json) l WHERE lower(trim(l.value)) = required.value))`
	}
	if len(excluded) > 0 {
		statement += ` AND NOT EXISTS (SELECT 1 FROM json_each(i.labels_json) l WHERE lower(trim(l.value)) IN (SELECT value FROM json_each(` + jsonList(excluded) + `)))`
	}
	statement += `), ranked AS NOT MATERIALIZED (SELECT *, CASE WHEN label_rank < ` + bind(len(labelRanks)) + ` THEN 0 ELSE unblockers END AS unblocker_rank FROM candidates)`
	order := "merging, priority, label_rank, state_rank, unblocker_rank, unranked, queue_rank, created, identifier, id"
	statement += ` SELECT id FROM ranked WHERE 1 = 1`
	if query.After > 0 {
		statement += ` AND (` + order + `) > (SELECT ` + order + ` FROM ranked WHERE id = ` + bind(query.After) + `)`
	}
	if !query.AvailableAt.IsZero() {
		statement += ` AND NOT EXISTS (SELECT 1 FROM leases l WHERE l.issue_id = ranked.id AND l.released_at IS NULL AND julianday(l.expires_at) > julianday(` + bind(formatHubTime(query.AvailableAt)) + `))`
	}
	limit := min(100, max(1, query.Limit))
	if query.Limit == 0 {
		limit = 100
	}
	statement += ` ORDER BY ` + order + ` LIMIT ` + bind(limit)
	if query.After > 0 {
		statement = strings.Replace(statement, ` SELECT id FROM ranked WHERE 1 = 1`, `, page AS (SELECT id FROM ranked WHERE 1 = 1`, 1)
		statement += `) SELECT id FROM page UNION ALL SELECT 0 WHERE NOT EXISTS (SELECT 1 FROM ranked WHERE id = ` + bind(query.After) + `)`
	}
	if encodingErr != nil {
		return nil, encodingErr
	}
	rows, err := tx.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("query native candidates: %w", err)
	}
	defer rows.Close()
	var ids []tracker.WorkItemID
	for rows.Next() {
		var id tracker.WorkItemID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if id == 0 {
			return nil, providerWait("provider_candidate_changed", "Queue changed during provider selection; refresh the candidate page")
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
