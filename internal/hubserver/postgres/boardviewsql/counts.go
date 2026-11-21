package boardviewsql

import "strconv"

func DirtyItems(projects string) string {
	return `dirty AS MATERIALIZED (
 SELECT d.project_id,d.work_item_id,i.workflow_state_name AS state,i.archived,w.terminal,w.dispatchable,w.board_rank,i.actor_principal_id
 FROM (` + projects + `) v
 CROSS JOIN LATERAL (
  SELECT d.project_id,d.work_item_id FROM native_board_dirty d
  WHERE d.organization_id=$1 AND d.project_id=v.id ORDER BY d.work_item_id LIMIT ` + strconv.Itoa(dirtyOverlayLimit) + `
 ) d
 JOIN issues i ON i.organization_id=$1 AND i.project_id=d.project_id AND i.native_id=d.work_item_id
 LEFT JOIN project_workflow_states w ON w.organization_id=$1 AND w.project_id=d.project_id AND w.name=i.workflow_state_name
)`
}

func ScopedCounts(projects, audience, archive, excludeWorkspace string) string {
	return `count_adjustments AS MATERIALIZED (
 SELECT b.project_id,k.kind,k.state,sum(x.delta) AS total,max(b.page_projected_at) AS projected_at
 FROM dirty d JOIN native_board_items b ON b.organization_id=$1 AND b.project_id=d.project_id AND b.work_item_id=d.work_item_id
 CROSS JOIN LATERAL (VALUES (b.state,b.archived,b.terminal,-1),
  (coalesce(d.state,b.state),coalesce(d.archived,b.archived),coalesce(d.terminal,b.terminal),1)) x(lane,archived,terminal,delta)
 CROSS JOIN LATERAL (VALUES ('total',''),('lane',x.lane),(CASE WHEN x.terminal THEN 'closed' ELSE 'open' END,'')) k(kind,state)
 WHERE b.page_ready AND (` + archive + `='all' OR x.archived::text=` + archive + `)
 AND (NOT ` + excludeWorkspace + `::boolean OR NOT 'detent:workspace'=ANY(coalesce(b.page_labels,'{}')))
 AND (` + audience + `::text='' OR b.actor_principal_id=` + audience + ` AND d.actor_principal_id=` + audience + `)
 GROUP BY b.project_id,k.kind,k.state
 HAVING sum(x.delta)<>0
),
scoped_counts AS MATERIALIZED (
 SELECT project_id,kind,state,sum(total)::bigint AS total,max(projected_at) AS projected_at
 FROM (
  SELECT project_id,kind,state,total,projected_at FROM native_board_page_summaries
  WHERE organization_id=$1 AND project_id IN (` + projects + `) AND audience=` + audience + `
  AND archive_scope=` + archive + ` AND exclude_workspace=` + excludeWorkspace + `
  UNION ALL SELECT project_id,kind,state,total,projected_at FROM count_adjustments
 ) counts
 GROUP BY project_id,kind,state HAVING sum(total)>0
),
running_counts AS MATERIALIZED (
 SELECT l.project_id,coalesce(d.state,l.state) AS state,count(*) AS running
 FROM live_items l LEFT JOIN dirty d ON d.project_id=l.project_id AND d.work_item_id=l.work_item_id
 WHERE l.running AND NOT coalesce(d.terminal,l.terminal) AND (NOT ` + excludeWorkspace + `::boolean OR NOT l.workspace)
 AND (` + archive + `='all' OR coalesce(d.archived,l.archived)::text=` + archive + `)
 AND (` + audience + `::text='' OR l.actor_principal_id=` + audience + ` AND l.current_actor=` + audience + `)
 GROUP BY l.project_id,coalesce(d.state,l.state)
)`
}
