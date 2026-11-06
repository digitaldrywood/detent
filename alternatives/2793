-- name: ReadGitHubImport :one
SELECT * FROM github_imports WHERE organization_id=$1 AND project_id=$2 AND id=$3;

-- name: CreateGitHubImport :one
INSERT INTO github_imports (organization_id,project_id,id,issue_number,stage,status,observed_at)
VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING *;

-- name: UpdateGitHubImport :one
UPDATE github_imports SET work_item_id=$5,stage=$6,cursor=$7,pages=$8,status=$9,gaps=$10,last_error=$11,retry_after=$12,source_updated_at=$13,observed_at=$14,intake_pending=$15,edit_sequence=$16,revision=revision+1
WHERE organization_id=$1 AND project_id=$2 AND id=$3 AND revision=$4 RETURNING *;

-- name: ReadGitHubImportForIssue :one
SELECT * FROM github_imports WHERE organization_id=$1 AND project_id=$2 AND issue_number=$3;
