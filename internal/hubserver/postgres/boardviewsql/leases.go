package boardviewsql

const unexpiredLease = "l.released_at IS NULL AND l.expires_at>transaction_timestamp()"

const CurrentCardLease = `CASE WHEN b.running OR b.card_attempt_status='running' OR b.card_execution_stage<>'' THEN EXISTS(
 SELECT 1 FROM issues i JOIN leases l ON l.organization_id=i.organization_id AND l.issue_id=i.id
 WHERE i.organization_id=b.organization_id AND i.project_id=b.project_id AND i.native_id=b.work_item_id
 AND l.fencing_token=b.execution_fence AND ` + unexpiredLease + `) ELSE false END`

func LiveItems(organization, projects string) string {
	return `live_items AS MATERIALIZED (
 SELECT b.project_id,b.work_item_id,b.state,b.archived,b.terminal,b.running,
  b.actor_principal_id,i.actor_principal_id AS current_actor,
  'detent:workspace'=ANY(coalesce(b.page_labels,'{}')) AS workspace
 FROM leases l JOIN issues i ON i.organization_id=l.organization_id AND i.id=l.issue_id
 JOIN native_board_items b ON b.organization_id=i.organization_id AND b.project_id=i.project_id AND b.work_item_id=i.native_id
 WHERE l.organization_id=` + organization + ` AND i.project_id IN (` + projects + `) AND ` + unexpiredLease + `
 AND l.fencing_token=b.execution_fence AND b.page_ready AND (b.running OR b.card_attempt_status='running' OR b.card_execution_stage<>'')
)`
}

func RunningCount(project, kind, state string) string {
	return `CASE WHEN ` + kind + `='closed' THEN 0 ELSE coalesce((SELECT sum(r.running) FROM running_counts r
 WHERE r.project_id=` + project + ` AND (` + kind + `<>'lane' OR r.state=` + state + `)),0) END`
}
