-- +goose Up
WITH triage AS (
 SELECT p.id, json_object('name', 'Triage', 'terminal', json('false'), 'dispatchable', json('false'),
 'transitions', json((SELECT json_group_array(json_extract(s.value, '$.name')) FROM json_each(p.states_json) s
 WHERE json_extract(s.value, '$.name') IN ('Backlog', 'Todo', 'Cancelled')))) AS state
 FROM projects p WHERE p.profile = 'native' AND (p.repository_id IS NOT NULL OR p.checkout_repository <> '')
)
UPDATE projects SET states_json = (
 SELECT json_group_array(json(value)) FROM (
 SELECT -1 AS ordinal, triage.state AS value FROM triage WHERE triage.id = projects.id
 UNION ALL
 SELECT CAST(s.key AS INTEGER), s.value FROM json_each(projects.states_json) s WHERE json_extract(s.value, '$.name') <> 'Triage'
 ORDER BY ordinal
 )), github_intake = 'manual', integration_revision = integration_revision + 1
WHERE id IN (SELECT id FROM triage);

INSERT INTO workflow_states(project_id, source_name, detent_state, terminal, dispatchable, created_at, updated_at)
SELECT id, 'Triage', 'Triage', 0, 0, strftime('%Y-%m-%dT%H:%M:%SZ', 'now'), strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
FROM projects WHERE profile = 'native' AND (repository_id IS NOT NULL OR checkout_repository <> '')
ON CONFLICT(project_id, source_name) DO UPDATE SET terminal = 0, dispatchable = 0;

-- +goose Down
SELECT 1;
