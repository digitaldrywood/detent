-- name: DoctorProposalIdentityRows :many
SELECT kind,identity_hash,identity_value,source_revision,version_sha256
FROM doctor_proposal_identities
WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3
ORDER BY kind,identity_hash;

-- name: NextDoctorProposalIdentityHead :one
SELECT p.id AS project_id,head.native_id FROM projects p
CROSS JOIN LATERAL (
 SELECT i.native_id FROM issues i WHERE i.organization_id=p.organization_id AND i.project_id=p.id
  AND NOT i.archived AND i.doctor_identity_revision IS DISTINCT FROM i.revision
 ORDER BY i.native_id LIMIT 1
) head
WHERE p.organization_id=$1 AND p.deleted_at IS NULL AND p.profile='native'
ORDER BY p.id,head.native_id LIMIT 1;

-- name: ClearDoctorProposalIdentities :exec
DELETE FROM doctor_proposal_identities WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3;

-- name: PutDoctorProposalIdentity :exec
INSERT INTO doctor_proposal_identities(organization_id,project_id,work_item_id,kind,identity_hash,identity_value,source_revision,version_sha256,has_state,state_name,archived,item_number)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12);

-- name: BindDoctorProposalIdentityHead :execrows
UPDATE issues SET doctor_identity_revision=revision WHERE organization_id=$1 AND project_id=$2 AND native_id=$3 AND revision=$4;

-- name: DoctorProposalIdentitySource :one
SELECT (duplicate_source_sequence+content_settings_revision)::bigint AS source_sequence
FROM projects WHERE organization_id=$1 AND id=$2 AND deleted_at IS NULL AND profile='native';

-- name: LookupDoctorProposalIdentity :one
WITH scope AS MATERIALIZED (
 SELECT p.organization_id,p.id,(p.duplicate_source_sequence+p.content_settings_revision)::bigint AS source_sequence,
 NOT EXISTS(SELECT 1 FROM issues i WHERE i.organization_id=p.organization_id AND i.project_id=p.id AND NOT i.archived AND i.doctor_identity_revision IS DISTINCT FROM i.revision) AS complete
 FROM projects p WHERE p.organization_id=sqlc.arg(organization_id) AND p.id=sqlc.arg(project_id) AND p.deleted_at IS NULL AND p.profile='native'
), states AS MATERIALIZED (
 SELECT true AS has_state,w.name AS state_name,w.terminal FROM project_workflow_states w JOIN scope s ON s.organization_id=w.organization_id AND s.id=w.project_id
 UNION ALL SELECT false,'',false FROM scope
), candidates AS MATERIALIZED (
 SELECT hit.*,w.terminal FROM scope s CROSS JOIN states w CROSS JOIN LATERAL (
  SELECT d.work_item_id,d.item_number,d.source_revision,d.version_sha256 FROM doctor_proposal_identities d
  WHERE s.complete AND d.organization_id=s.organization_id AND d.project_id=s.id AND d.kind=sqlc.arg(kind) AND d.kind<>'complete'
   AND d.identity_hash=sqlc.arg(identity_hash) AND d.identity_value=sqlc.arg(identity_value)
   AND d.has_state=w.has_state AND d.state_name=w.state_name AND NOT d.archived
  ORDER BY d.item_number DESC,d.work_item_id LIMIT 1
 ) hit WHERE NOT sqlc.arg(open_only)::boolean OR NOT w.terminal
)
SELECT s.source_sequence,s.complete,coalesce(hit.work_item_id,'')::text AS work_item_id,coalesce(hit.item_number,0)::bigint AS item_number,
 coalesce(hit.source_revision,0)::bigint AS source_revision,coalesce(hit.version_sha256,'')::text AS version_sha256,coalesce(hit.terminal,false)::boolean AS terminal
FROM scope s LEFT JOIN LATERAL (SELECT * FROM candidates ORDER BY terminal,item_number DESC,work_item_id LIMIT 1) hit ON true;
