-- name: LocalBoardImportAllowanceUsage :one
SELECT a.value AS active_issues,h.value AS history_records
FROM hosted_usage_counters a JOIN hosted_usage_counters h ON h.organization_id=a.organization_id
WHERE a.organization_id=sqlc.arg(organization_id) AND a.name='unarchived_issues' AND h.name='history_records'
 AND a.initialized AND h.initialized AND a.value>=0 AND h.value>=0;
