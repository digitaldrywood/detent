-- name: ReleaseDeliveryFacts :one
SELECT EXISTS (
 SELECT 1 FROM issue_dependencies d JOIN issues source ON source.organization_id=d.organization_id AND source.project_id=d.blocker_project_id AND source.id=d.blocker_issue_id
 WHERE d.organization_id=i.organization_id AND d.dependent_project_id=i.project_id AND d.dependent_issue_id=i.id
 AND source.project_id=i.project_id AND source.native_id=sqlc.arg(source_item)::text
) AS depends_on_source,
NOT EXISTS (
 SELECT 1 FROM issue_dependencies d JOIN issues blocker ON blocker.organization_id=d.organization_id AND blocker.project_id=d.blocker_project_id AND blocker.id=d.blocker_issue_id
 LEFT JOIN project_workflow_states state ON state.organization_id=blocker.organization_id AND state.project_id=blocker.project_id AND state.name=blocker.workflow_state_name
 WHERE d.organization_id=i.organization_id AND d.dependent_project_id=i.project_id AND d.dependent_issue_id=i.id AND NOT COALESCE(state.terminal,false)
) AS dependency_ready,
(sqlc.arg(pr_number)::bigint>0 AND (EXISTS (
 SELECT 1 FROM release_deliveries d WHERE d.organization_id=i.organization_id AND lower(d.repository)=lower(sqlc.arg(repository)::text) AND d.pr_number=sqlc.arg(pr_number) AND (d.project_id<>i.project_id OR d.work_item_id<>i.native_id)
) OR EXISTS (
 SELECT 1 FROM change_versions v JOIN change_requests c ON c.organization_id=v.organization_id AND c.id=v.change_id
 WHERE v.organization_id=i.organization_id AND v.repository=sqlc.arg(repository) AND v.external_provider='github' AND v.external_id=sqlc.arg(pr_number)::text AND (c.project_id<>i.project_id OR c.work_item_id<>i.native_id)
))) AS conflicting_pr
FROM issues i WHERE i.organization_id=sqlc.arg(organization_id) AND i.project_id=sqlc.arg(project_id) AND i.native_id=sqlc.arg(item)::text;

-- name: ReleaseSourceReviews :many
SELECT * FROM change_evidence WHERE organization_id=sqlc.arg(organization_id) AND change_id=sqlc.arg(change_id) AND version_id=sqlc.arg(version_id) AND kind='review' ORDER BY sequence DESC LIMIT 32;
