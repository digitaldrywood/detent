-- name: NextWorkItemNumber :one
SELECT (COALESCE((SELECT number FROM issues WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id) ORDER BY number DESC LIMIT 1),0) + 1)::bigint AS number;

-- name: LockWorkItemProject :one
SELECT id FROM projects WHERE organization_id=sqlc.arg(organization_id) AND id=sqlc.arg(project_id) AND deleted_at IS NULL FOR UPDATE;

-- name: LockWorkItem :one
SELECT * FROM issues WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id) AND native_id=sqlc.arg(native_id) FOR UPDATE;

-- name: WorkItemBySource :one
SELECT * FROM issues WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id) AND native_source_key=sqlc.arg(source_key);

-- name: CommentBySource :one
SELECT * FROM native_comments WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id) AND work_item_id=sqlc.arg(work_item_id) AND source_key=sqlc.arg(source_key);

-- name: SelectCommentsOldest :many
SELECT * FROM native_comments WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id) AND work_item_id=sqlc.arg(work_item_id) AND sequence>sqlc.arg(after_sequence)::bigint
ORDER BY sequence LIMIT sqlc.arg(page_limit);

-- name: SelectCommentsNewest :many
SELECT * FROM native_comments WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id) AND work_item_id=sqlc.arg(work_item_id) AND sequence<sqlc.arg(after_sequence)::bigint
ORDER BY sequence DESC LIMIT sqlc.arg(page_limit);

-- name: WorkItemHasActiveLease :one
SELECT EXISTS(SELECT 1 FROM leases l JOIN issues i ON i.organization_id=l.organization_id AND i.id=l.issue_id WHERE i.organization_id=sqlc.arg(organization_id) AND i.project_id=sqlc.arg(project_id) AND i.native_id=sqlc.arg(native_id) AND l.released_at IS NULL AND l.expires_at>sqlc.arg(now)::timestamptz);
