-- +goose Up
WITH triage AS (
 SELECT p.id, json_object('name', 'Triage', 'terminal', json('false'), 'dispatchable', json('false'),
 'transitions', json((SELECT json_group_array(json_extract(s.value, '$.name')) FROM json_each(p.states_json) s
 WHERE json_extract(s.value, '$.name') IN ('Backlog', 'Todo', 'Cancelled')))) AS state
 FROM projects p WHERE p.profile = 'native'
 AND NOT EXISTS (SELECT 1 FROM json_each(p.states_json) s WHERE json_extract(s.value, '$.name') = 'Triage')
)
UPDATE projects SET states_json = (
 SELECT json_group_array(json(value)) FROM (
 SELECT -1 AS ordinal, triage.state AS value FROM triage WHERE triage.id = projects.id
 UNION ALL
 SELECT CAST(s.key AS INTEGER), s.value FROM json_each(projects.states_json) s
 ORDER BY ordinal
 ))
WHERE id IN (SELECT id FROM triage);

INSERT INTO workflow_states(project_id, source_name, detent_state, terminal, dispatchable, created_at, updated_at)
SELECT id, 'Triage', 'Triage', 0, 0, strftime('%Y-%m-%dT%H:%M:%SZ', 'now'), strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
FROM projects p WHERE profile = 'native'
AND NOT EXISTS (SELECT 1 FROM workflow_states w WHERE w.project_id=p.id AND w.detent_state='Triage');

-- +goose Down
SELECT 1;
