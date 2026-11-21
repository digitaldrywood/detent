package boardviewsql

func Liveness(organization, projects, now, timeout string) string {
	return liveness(`FROM leases l WHERE l.organization_id=`+organization+` AND l.project_id IN (`+projects+`)`, organization, now, timeout)
}

func ItemLiveness(organization, project, now, timeout, items string) string {
	return liveness(`FROM (SELECT DISTINCT native_id FROM unnest(`+items+`::text[]) AS ids(native_id)) requested
 JOIN LATERAL (
  SELECT i.id FROM issues i WHERE i.organization_id=`+organization+` AND i.project_id=`+project+` AND i.native_id=requested.native_id LIMIT 1
 ) i ON true
 JOIN LATERAL (
  SELECT l.* FROM leases l WHERE l.organization_id=`+organization+` AND l.project_id=`+project+` AND l.issue_id=i.id AND l.released_at IS NULL LIMIT 1
 ) l ON true WHERE true`, organization, now, timeout)
}

func liveness(source, organization, now, timeout string) string {
	return `active_leases AS MATERIALIZED (
 SELECT l.organization_id,l.project_id,l.lease_id,l.fencing_token,l.issue_id,l.runner_id,l.machine_id,l.expires_at
 ` + source + `
 AND l.released_at IS NULL AND l.renewed_at<=` + now + ` AND l.expires_at>` + now + ` AND l.runner_id IS NOT NULL
), fresh_owners AS MATERIALIZED (
 SELECT r.runner_id,r.machine_id,least(r.expires_at,r.last_heartbeat_at+` + timeout + `::bigint*interval '1 second') AS deadline
 FROM (SELECT DISTINCT runner_id,machine_id FROM active_leases) o
 JOIN LATERAL (
  SELECT r.runner_id,r.machine_id,r.expires_at,r.last_heartbeat_at FROM runner_identities r
  WHERE r.organization_id=` + organization + ` AND r.runner_id=o.runner_id AND r.machine_id=o.machine_id
  AND r.removed_at IS NULL AND r.expires_at>` + now + ` AND r.last_heartbeat_at<=` + now + `
  AND r.last_heartbeat_at>` + now + `-` + timeout + `::bigint*interval '1 second' LIMIT 1
 ) r ON true
), board_liveness AS MATERIALIZED (
 SELECT b.project_id,b.work_item_id,b.card_attempt_id,b.card_attempt_status,b.card_execution_id,b.card_execution_running,
  b.state,b.archived,b.terminal,b.page_ready,b.workspace,b.actor_principal_id,i.actor_principal_id AS current_actor,
  l.expires_at AS lease_expires_at,
  CASE WHEN a.id=b.card_attempt_id AND a.status='running' THEN least(l.expires_at,r.deadline) END AS attempt_until,
  CASE WHEN l.fencing_token=b.execution_fence
       AND (NOT coalesce(b.card_execution_running,false) OR a.id IS NULL OR a.status='running')
       THEN least(l.expires_at,r.deadline,
   CASE WHEN b.card_execution_expires_at IS DISTINCT FROM b.expires_at
         OR b.card_execution_expires_at < b.card_attempt_expires_at OR b.card_execution_id IS DISTINCT FROM a.id
         THEN b.card_execution_expires_at END) END AS execution_until
 FROM active_leases l JOIN fresh_owners r ON r.runner_id=l.runner_id AND r.machine_id=l.machine_id
 JOIN LATERAL (
  SELECT i.native_id,i.actor_principal_id FROM issues i
  WHERE i.organization_id=l.organization_id AND i.project_id=l.project_id AND i.id=l.issue_id LIMIT 1
 ) i ON true
 JOIN LATERAL (
  SELECT b.project_id,b.work_item_id,b.card_attempt_id,b.card_attempt_status,b.card_execution_id,b.card_execution_running,
   b.card_execution_expires_at,b.expires_at,b.card_attempt_expires_at,b.execution_fence,b.state,b.archived,b.terminal,b.page_ready,b.actor_principal_id,
   'detent:workspace'=ANY(coalesce(b.page_labels,'{}')) AS workspace
  FROM native_board_items b WHERE b.organization_id=l.organization_id AND b.project_id=l.project_id AND b.work_item_id=i.native_id
  AND (b.card_attempt_status='running' OR b.card_execution_running OR coalesce(b.card_execution_stage,'')<>'') LIMIT 1
 ) b ON true
 LEFT JOIN LATERAL (
  SELECT a.id,a.status FROM native_attempts a
  WHERE a.organization_id=l.organization_id AND a.lease_id=l.lease_id AND a.fencing_token=l.fencing_token LIMIT 1
 ) a ON true
)`
}

func Running(attemptStatus, executionID, executionRunning, facts, now string) string {
	return `(coalesce(` + executionRunning + `,false) AND (` + facts + `.execution_until>` + now + ` OR ` + facts + `.attempt_until>` + now + `)
 OR coalesce(` + executionID + `,'')='' AND ` + attemptStatus + `='running' AND ` + facts + `.attempt_until>` + now + `)`
}
