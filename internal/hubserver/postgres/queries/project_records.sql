-- name: BoundProjectRecord :many
SELECT sqlc.embed(p),sqlc.embed(s),w.name AS state_name,COALESCE(w.terminal,false)::boolean AS state_terminal,COALESCE(w.dispatchable,false)::boolean AS state_dispatchable,COALESCE(w.operator_only,false)::boolean AS state_operator_only,t.target_name
FROM projects p JOIN project_settings s ON s.organization_id=p.organization_id AND s.project_id=p.id
LEFT JOIN project_workflow_states w ON w.organization_id=p.organization_id AND w.project_id=p.id
LEFT JOIN project_workflow_transitions t ON t.organization_id=w.organization_id AND t.project_id=w.project_id AND t.source_name=w.name
WHERE p.organization_id=sqlc.arg(organization_id) AND p.id=sqlc.arg(id) AND p.deleted_at IS NULL
ORDER BY w.board_rank,t.transition_rank;

-- name: ResolveProjectSlug :one
SELECT project_id FROM project_slugs WHERE organization_id=$1 AND slug=$2;

-- name: ReserveProjectSlug :execrows
INSERT INTO project_slugs(organization_id,slug,project_id) VALUES($1,$2,$3)
ON CONFLICT(organization_id,slug) DO UPDATE SET project_id=EXCLUDED.project_id WHERE project_slugs.project_id=EXCLUDED.project_id;

-- name: CreateProjectSettings :exec
INSERT INTO project_settings(organization_id,project_id) VALUES($1,$2);

-- name: UpdateProjectSettings :one
UPDATE project_settings SET revision=revision+1,intake=sqlc.arg(intake),projection=sqlc.arg(projection),repository_enabled=sqlc.arg(repository_enabled),repository=sqlc.arg(repository),checkout_repository=sqlc.arg(checkout_repository),aging_enabled=sqlc.arg(aging_enabled),duplicate_challenge=sqlc.arg(duplicate_challenge),archive_completed_after_days=sqlc.narg(archive_completed_after_days),archive_cancelled_after_days=sqlc.narg(archive_cancelled_after_days),workflow_key=sqlc.arg(workflow_key),workflow_sha256=sqlc.arg(workflow_sha256),workflow_bytes=sqlc.arg(workflow_bytes),workflow_content_bytes=sqlc.arg(workflow_content_bytes)
WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id) AND revision=sqlc.arg(expected_revision) RETURNING *;

-- name: DeleteProjectStates :exec
DELETE FROM project_workflow_states WHERE organization_id=$1 AND project_id=$2 AND name<>ALL(sqlc.arg(names)::text[]);

-- name: CreateProjectState :exec
INSERT INTO project_workflow_states(organization_id,project_id,name,terminal,dispatchable,operator_only,board_rank) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(organization_id,project_id,name) DO UPDATE SET terminal=EXCLUDED.terminal,dispatchable=EXCLUDED.dispatchable,operator_only=EXCLUDED.operator_only,board_rank=EXCLUDED.board_rank;

-- name: LockActiveProject :one
SELECT id FROM projects WHERE organization_id=$1 AND id=$2 AND deleted_at IS NULL FOR UPDATE;

-- name: ProjectHasActiveWork :one
SELECT EXISTS(SELECT 1 FROM issues i JOIN leases l ON l.organization_id=i.organization_id AND l.issue_id=i.id WHERE i.organization_id=sqlc.arg(organization_id) AND i.project_id=sqlc.arg(project_id) AND l.released_at IS NULL AND l.expires_at>sqlc.arg(now)::timestamptz)
OR EXISTS(SELECT 1 FROM native_attempts a JOIN issues i ON i.organization_id=a.organization_id AND i.id=a.issue_id WHERE i.organization_id=sqlc.arg(organization_id) AND i.project_id=sqlc.arg(project_id) AND a.status='running')
OR EXISTS(SELECT 1 FROM workspace_sessions w WHERE w.organization_id=sqlc.arg(organization_id) AND w.project_id=sqlc.arg(project_id) AND w.state NOT IN ('closed','failed'))
OR EXISTS(SELECT 1 FROM project_action_runs a WHERE a.organization_id=sqlc.arg(organization_id) AND a.project_id=sqlc.arg(project_id) AND a.status IN ('queued','running'))
OR EXISTS(SELECT 1 FROM conversations c WHERE c.organization_id=sqlc.arg(organization_id) AND c.project_id=sqlc.arg(project_id) AND (c.execution_status IS NULL OR c.execution_lease_id IS NULL OR c.execution_status IN ('waiting_for_runner','starting','running','waiting_input','interrupting','unknown')))
OR EXISTS(SELECT 1 FROM conversations c JOIN conversation_messages m ON m.organization_id=c.organization_id AND m.conversation_id=c.id WHERE c.organization_id=sqlc.arg(organization_id) AND c.project_id=sqlc.arg(project_id) AND m.delivery IN ('queued','sending','sent','delivered','responding','unknown'))
OR EXISTS(SELECT 1 FROM github_imports g WHERE g.organization_id=sqlc.arg(organization_id) AND g.project_id=sqlc.arg(project_id) AND (g.status IN ('pending','running','partial') OR g.intake_pending))
OR EXISTS(SELECT 1 FROM github_outbound o WHERE o.organization_id=sqlc.arg(organization_id) AND o.project_id=sqlc.arg(project_id) AND o.status IN ('pending','processing','retrying'))
OR EXISTS(SELECT 1 FROM runner_sprite_memberships m WHERE m.organization_id=sqlc.arg(organization_id) AND (m.token_project_id=sqlc.arg(project_id) OR m.allocation_project_id=sqlc.arg(project_id)) AND m.state<>'deleted')
OR EXISTS(SELECT 1 FROM project_sprite_allocations a WHERE a.organization_id=sqlc.arg(organization_id) AND a.project_id=sqlc.arg(project_id) AND a.desired_count>0)
OR EXISTS(SELECT 1 FROM onboarding_issue_intake b WHERE b.organization_id=sqlc.arg(organization_id) AND b.project_id=sqlc.arg(project_id) AND b.hub_owned AND b.batch_status IN ('discovering','importing'))
OR EXISTS(SELECT 1 FROM artifact_upload_authorizations a WHERE a.organization_id=sqlc.arg(organization_id) AND a.project_id=sqlc.arg(project_id) AND a.expires_at>sqlc.arg(now))
OR EXISTS(SELECT 1 FROM health_finding_projects p JOIN health_slack_deliveries d ON d.organization_id=p.organization_id AND d.finding_id=p.finding_id WHERE p.organization_id=sqlc.arg(organization_id) AND p.project_id=sqlc.arg(project_id) AND d.completed_at IS NULL);

-- name: RetireProject :execrows
UPDATE projects SET deleted_at=$3 WHERE organization_id=$1 AND id=$2 AND deleted_at IS NULL;

-- name: DeleteProjectTransitions :exec
DELETE FROM project_workflow_transitions WHERE organization_id=$1 AND project_id=$2;

-- name: CreateProjectTransition :exec
INSERT INTO project_workflow_transitions(organization_id,project_id,source_name,target_name,transition_rank) VALUES($1,$2,$3,$4,$5);

-- name: UpdateProjectMetadata :execrows
UPDATE projects SET name=sqlc.arg(name),slug=sqlc.arg(slug),require_dependencies=sqlc.arg(require_dependencies),workflow_source=sqlc.arg(workflow_source),workflow_source_revision=sqlc.arg(workflow_source_revision)
WHERE organization_id=sqlc.arg(organization_id) AND id=sqlc.arg(id) AND deleted_at IS NULL AND content_settings_revision=sqlc.arg(expected_revision);

-- name: UpdateProjectContent :execrows
UPDATE projects SET content_settings_revision=content_settings_revision+1 WHERE organization_id=$1 AND id=$2 AND content_settings_revision=$3 AND deleted_at IS NULL;

-- name: SetProjectVisualAttachments :exec
UPDATE project_settings SET require_visual_attachments=$3 WHERE organization_id=$1 AND project_id=$2;

-- name: UpdateProjectModel :execrows
UPDATE project_settings SET model_revision=model_revision+1,model_key=sqlc.arg(model_key),model_sha256=sqlc.arg(model_sha256),model_bytes=sqlc.arg(model_bytes),model_content_bytes=sqlc.arg(model_content_bytes)
WHERE organization_id=sqlc.arg(organization_id) AND project_id=sqlc.arg(project_id) AND model_revision=sqlc.arg(expected_revision);

-- name: SetProjectSlug :execrows
UPDATE projects SET slug=$3 WHERE organization_id=$1 AND id=$2 AND deleted_at IS NULL;

-- name: ProjectUsedRemovedStates :one
SELECT EXISTS(SELECT 1 FROM project_workflow_states w WHERE w.organization_id=sqlc.arg(organization_id) AND w.project_id=sqlc.arg(project_id) AND w.name<>ALL(sqlc.arg(names)::text[]) AND EXISTS(SELECT 1 FROM issues i WHERE i.organization_id=w.organization_id AND i.project_id=w.project_id AND i.workflow_state_name=w.name));

-- name: ShareActiveProject :one
SELECT id FROM projects WHERE organization_id=$1 AND id=$2 AND deleted_at IS NULL FOR SHARE;
