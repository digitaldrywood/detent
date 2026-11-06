-- name: WorkViewPreference :one
SELECT view_query FROM hosted_work_view_preferences
WHERE organization_id=$1 AND user_id=$2 AND scope=$3;

-- name: PutWorkViewPreference :exec
INSERT INTO hosted_work_view_preferences (organization_id,user_id,scope,view_query)
VALUES ($1,$2,$3,$4)
ON CONFLICT (organization_id,user_id,scope) DO UPDATE SET view_query=EXCLUDED.view_query;

-- name: DeleteWorkViewPreference :exec
DELETE FROM hosted_work_view_preferences WHERE organization_id=$1 AND user_id=$2 AND scope=$3;

-- name: SavedWorkView :one
SELECT v.*, EXISTS(SELECT 1 FROM hosted_work_view_favorites f WHERE f.organization_id=v.organization_id AND f.view_id=v.id AND f.user_id=sqlc.arg(user_id)) AS favorite,
 ARRAY(SELECT p.project_id FROM hosted_work_view_projects p WHERE p.organization_id=v.organization_id AND p.view_id=v.id ORDER BY p.position)::text[] AS project_ids,
 ARRAY(SELECT state_filter.value FROM hosted_work_view_values state_filter WHERE state_filter.organization_id=v.organization_id AND state_filter.view_id=v.id AND state_filter.kind='state' ORDER BY state_filter.position)::text[] AS state_values,
 ARRAY(SELECT type_filter.value FROM hosted_work_view_values type_filter WHERE type_filter.organization_id=v.organization_id AND type_filter.view_id=v.id AND type_filter.kind='type' ORDER BY type_filter.position)::text[] AS type_values,
 ARRAY(SELECT label_filter.value FROM hosted_work_view_values label_filter WHERE label_filter.organization_id=v.organization_id AND label_filter.view_id=v.id AND label_filter.kind='label' ORDER BY label_filter.position)::text[] AS label_values,
 ARRAY(SELECT assignee_filter.value FROM hosted_work_view_values assignee_filter WHERE assignee_filter.organization_id=v.organization_id AND assignee_filter.view_id=v.id AND assignee_filter.kind='assignee' ORDER BY assignee_filter.position)::text[] AS assignee_values,
 ARRAY(SELECT priority_filter.value FROM hosted_work_view_values priority_filter WHERE priority_filter.organization_id=v.organization_id AND priority_filter.view_id=v.id AND priority_filter.kind='priority' ORDER BY priority_filter.position)::text[] AS priority_values,
 ARRAY(SELECT lanes_filter.value FROM hosted_work_view_values lanes_filter WHERE lanes_filter.organization_id=v.organization_id AND lanes_filter.view_id=v.id AND lanes_filter.kind='lanes' ORDER BY lanes_filter.position)::text[] AS lanes_values,
 ARRAY(SELECT collapsed_filter.value FROM hosted_work_view_values collapsed_filter WHERE collapsed_filter.organization_id=v.organization_id AND collapsed_filter.view_id=v.id AND collapsed_filter.kind='collapsed' ORDER BY collapsed_filter.position)::text[] AS collapsed_values
FROM hosted_work_views v
WHERE v.organization_id=$1 AND v.id=$2 AND (v.owner_id=sqlc.arg(user_id) OR v.scope='organization')
AND (NOT sqlc.arg(enforce_grants)::boolean OR NOT EXISTS (
 SELECT 1 FROM hosted_work_view_projects p WHERE p.organization_id=v.organization_id AND p.view_id=v.id
 AND NOT EXISTS (SELECT 1 FROM hosted_project_grants g WHERE g.organization_id=p.organization_id AND g.project_id=p.project_id AND g.user_id=sqlc.arg(user_id))
));

-- name: SavedWorkViewIDs :many
SELECT v.id FROM hosted_work_views v
WHERE v.organization_id=$1 AND v.id>sqlc.arg(cursor) AND (v.owner_id=sqlc.arg(user_id) OR v.scope='organization')
AND (NOT sqlc.arg(enforce_grants)::boolean OR NOT EXISTS (
 SELECT 1 FROM hosted_work_view_projects p WHERE p.organization_id=v.organization_id AND p.view_id=v.id
 AND NOT EXISTS (SELECT 1 FROM hosted_project_grants g WHERE g.organization_id=p.organization_id AND g.project_id=p.project_id AND g.user_id=sqlc.arg(user_id))
)) ORDER BY v.id LIMIT sqlc.arg(page_limit);

-- name: SavedWorkViewOwnerSlot :one
SELECT slot::integer FROM generate_series(0,49) slot
WHERE NOT EXISTS (SELECT 1 FROM hosted_work_views WHERE organization_id=$1 AND owner_id=$2 AND owner_slot=slot)
ORDER BY slot LIMIT 1;

-- name: SavedWorkViewOrganizationSlot :one
SELECT slot::integer FROM generate_series(0,199) slot
WHERE NOT EXISTS (SELECT 1 FROM hosted_work_views WHERE organization_id=$1 AND organization_slot=slot)
ORDER BY slot LIMIT 1;

-- name: PutSavedWorkView :execrows
INSERT INTO hosted_work_views (organization_id,id,owner_id,name,scope,project_id,layout,tab,search,sort,completed_window,completed_from,completed_to,archived,created_by,all_lanes,owner_slot,organization_slot)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)
ON CONFLICT (organization_id,id) DO UPDATE SET
 name=EXCLUDED.name,scope=EXCLUDED.scope,project_id=EXCLUDED.project_id,layout=EXCLUDED.layout,tab=EXCLUDED.tab,search=EXCLUDED.search,sort=EXCLUDED.sort,completed_window=EXCLUDED.completed_window,completed_from=EXCLUDED.completed_from,completed_to=EXCLUDED.completed_to,archived=EXCLUDED.archived,created_by=EXCLUDED.created_by,all_lanes=EXCLUDED.all_lanes
WHERE hosted_work_views.owner_id=EXCLUDED.owner_id AND NOT sqlc.arg(creating)::boolean;

-- name: DeleteSavedWorkView :execrows
DELETE FROM hosted_work_views WHERE organization_id=$1 AND id=$2 AND owner_id=$3;

-- name: DeleteSavedWorkViewValues :exec
DELETE FROM hosted_work_view_values WHERE organization_id=$1 AND view_id=$2;

-- name: PutSavedWorkViewValue :exec
INSERT INTO hosted_work_view_values (organization_id,view_id,kind,position,value) VALUES ($1,$2,$3,$4,$5);

-- name: DeleteSavedWorkViewProjects :exec
DELETE FROM hosted_work_view_projects WHERE organization_id=$1 AND view_id=$2;

-- name: PutSavedWorkViewProject :exec
INSERT INTO hosted_work_view_projects (organization_id,view_id,project_id,position) VALUES ($1,$2,$3,$4);

-- name: PutSavedWorkViewFavorite :exec
INSERT INTO hosted_work_view_favorites (organization_id,view_id,user_id) VALUES ($1,$2,$3) ON CONFLICT DO NOTHING;

-- name: DeleteSavedWorkViewFavorite :exec
DELETE FROM hosted_work_view_favorites WHERE organization_id=$1 AND view_id=$2 AND user_id=$3;

-- name: LockSavedWorkView :one
SELECT * FROM hosted_work_views WHERE organization_id=$1 AND id=$2 FOR UPDATE;
