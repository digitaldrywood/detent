-- name: WorkItemConversationReferences :many
SELECT r.conversation_id,r.message_id,r.created_at,c.visibility,c.owner_principal_id,c.read_audience
FROM message_references r
JOIN conversations c ON c.organization_id=r.organization_id AND c.id=r.conversation_id
WHERE c.deleted_at IS NULL AND r.organization_id=$1 AND c.project_id=$2 AND r.target_kind='issue' AND r.target_id=$3
ORDER BY r.created_at DESC,r.message_id DESC
LIMIT $4;

-- name: DeleteConversationRecord :execrows
UPDATE conversations SET deleted_at=$4,archived=true,creation_key='',creation_hash='',work_item_id=NULL,subject_work_item_id=NULL,linked_at=NULL,revision=revision+1,updated_at=$4
WHERE organization_id=$1 AND project_id=$2 AND id=$3 AND deleted_at IS NULL;
