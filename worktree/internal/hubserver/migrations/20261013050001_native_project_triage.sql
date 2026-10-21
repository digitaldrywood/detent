-- +goose Up
WITH lanes AS (
 SELECT p.id, CAST(s.key AS INTEGER) AS ordinal, s.value AS state
 FROM projects p, json_each(p.states_json) s WHERE p.profile = 'native'
), triage AS (
 SELECT id, ordinal, state FROM lanes WHERE json_extract(state, '$.name') = 'Triage'
 UNION ALL
 SELECT p.id, -1, json_object('name', 'Triage', 'terminal', json('false'), 'dispatchable', json('false'), 'transitions', json('[]'))
 FROM projects p WHERE p.profile = 'native'
 AND NOT EXISTS (SELECT 1 FROM lanes l WHERE l.id = p.id AND json_extract(l.state, '$.name') = 'Triage')
), destinations AS (
 SELECT id, ordinal, json_extract(state, '$.name') AS name FROM lanes
 WHERE json_extract(state, '$.name') IN ('Backlog', 'Todo', 'Cancelled')
), targets AS (
 SELECT t.id, CAST(s.key AS INTEGER) AS ordinal, s.value AS name
 FROM triage t, json_each(t.state, '$.transitions') s
 UNION ALL
 SELECT d.id, d.ordinal + 100, d.name FROM destinations d JOIN triage t ON t.id = d.id
 WHERE NOT EXISTS (SELECT 1 FROM json_each(t.state, '$.transitions') s WHERE s.value = d.name)
 UNION ALL
 SELECT t.id, 200, (
 SELECT json_extract(l.state, '$.name') FROM lanes l WHERE l.id = t.id
 AND json_extract(l.state, '$.name') <> 'Triage' AND COALESCE(json_extract(l.state, '$.operator_only'), 0) = 0
 ORDER BY COALESCE(json_extract(l.state, '$.terminal'), 0), l.ordinal LIMIT 1
 ) FROM triage t
 WHERE COALESCE(json_array_length(t.state, '$.transitions'), 0) = 0
 AND NOT EXISTS (SELECT 1 FROM destinations d WHERE d.id = t.id)
), normalized AS (
 SELECT t.id, t.ordinal, json_set(json_remove(t.state, '$.operator_only'),
 '$.terminal', json('false'), '$.dispatchable', json('false'), '$.transitions', json((
 SELECT json_group_array(name) FROM (SELECT name FROM targets WHERE id = t.id AND name IS NOT NULL ORDER BY ordinal)
 ))) AS state FROM triage t
)
UPDATE projects SET states_json = (
 SELECT json_group_array(json(value)) FROM (
 SELECT ordinal, state AS value FROM normalized WHERE id = projects.id
 UNION ALL
 SELECT ordinal, state FROM lanes WHERE id = projects.id AND json_extract(state, '$.name') <> 'Triage'
 ORDER BY ordinal
 ))
WHERE id IN (SELECT id FROM normalized);

UPDATE workflow_states SET terminal = 0, dispatchable = 0, updated_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
WHERE detent_state = 'Triage' AND (terminal <> 0 OR dispatchable <> 0)
AND project_id IN (SELECT id FROM projects WHERE profile = 'native');

INSERT INTO workflow_states(project_id, source_name, detent_state, terminal, dispatchable, created_at, updated_at)
SELECT id, 'Triage', 'Triage', 0, 0, strftime('%Y-%m-%dT%H:%M:%SZ', 'now'), strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
FROM projects p WHERE profile = 'native'
AND NOT EXISTS (SELECT 1 FROM workflow_states w WHERE w.project_id=p.id AND w.detent_state='Triage');

-- +goose Down
SELECT 1;
