-- name: CutoverReceipt :one
SELECT * FROM github_cutovers WHERE organization_id=$1 AND project_id=$2 AND checkpoint=$3;

-- name: StoreCutoverReceipt :one
INSERT INTO github_cutovers(organization_id,project_id,checkpoint,source_profile,expected_revision,receipt_object_key,receipt_sha256,receipt_bytes,receipt_content_bytes,actor_id,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING *;

-- name: ApplyCutoverProfile :execrows
UPDATE projects SET profile='native',content_settings_revision=content_settings_revision+1 WHERE organization_id=$1 AND id=$2 AND profile=$3 AND content_settings_revision=$4 AND deleted_at IS NULL;

-- name: ApplyCutoverSettings :execrows
UPDATE project_settings SET intake='manual',revision=revision+1 WHERE organization_id=$1 AND project_id=$2 AND revision=$3;

-- name: LocalImportMapping :one
SELECT * FROM local_board_imports WHERE organization_id=$1 AND project_id=$2 AND source_key=$3;

-- name: PutLocalImportMapping :one
INSERT INTO local_board_imports(organization_id,project_id,source_key,source_kind,source_digest,work_item_id,imported_at) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT DO NOTHING RETURNING *;

-- name: NeutralImportSource :one
SELECT * FROM neutral_import_sources WHERE organization_id=$1 AND source_id=$2;

-- name: BindNeutralImportSource :one
INSERT INTO neutral_import_sources(organization_id,source_id,schema_version,checksum,schema_digest,verified_at) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING RETURNING *;

-- name: NeutralImportRecord :one
SELECT * FROM neutral_import_records WHERE organization_id=$1 AND source_id=$2 AND family=$3 AND source_key=$4;

-- name: PutNeutralImportRecord :one
INSERT INTO neutral_import_records(organization_id,source_id,family,source_key,record_digest,destination_id,source_revision,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,imported_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) ON CONFLICT DO NOTHING RETURNING *;
