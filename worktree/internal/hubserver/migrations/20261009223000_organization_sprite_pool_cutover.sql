-- +goose Up
INSERT INTO organization_sprite_pools
SELECT a.organization_id,min(a.floor,100),min(a.ceiling,100),a.idle,p.bootstrap,p.configured_by,p.revision+1,p.isolation_tier,p.placement_json
FROM (SELECT organization_id,sum(min_runners) AS floor,sum(max_runners) AS ceiling,min(idle_seconds) AS idle FROM project_sprite_pools GROUP BY organization_id) a
JOIN project_sprite_pools p ON p.organization_id=a.organization_id AND p.project_id=(
 SELECT x.project_id FROM project_sprite_pools x JOIN projects y ON y.id=x.project_id AND y.organization_id=x.organization_id
 WHERE x.organization_id=a.organization_id ORDER BY y.scheduling_rank,x.project_id LIMIT 1);

INSERT INTO organization_sprite_members(organization_id,name,provider_organization,token_project_id,enrollment_id,state,bootstrap_log,idle_since,created_at)
SELECT organization_id,name,provider_organization,project_id,enrollment_id,state,bootstrap_log,idle_since,created_at FROM project_sprite_members;
UPDATE runner_enrollments SET scope='organization' WHERE id IN (SELECT enrollment_id FROM organization_sprite_members);
UPDATE runner_identities SET scope='organization',revision=revision+1 WHERE enrollment_id IN (SELECT enrollment_id FROM organization_sprite_members);
DELETE FROM token_grants WHERE token_id IN (SELECT token_id FROM runner_identities WHERE enrollment_id IN (SELECT enrollment_id FROM organization_sprite_members));
DELETE FROM runner_enrollment_projects WHERE enrollment_id IN (SELECT enrollment_id FROM organization_sprite_members);
DROP TABLE project_sprite_members;
DROP TABLE project_sprite_pools;
