-- +goose Up
ALTER TABLE issues ADD COLUMN issue_contract_json TEXT NOT NULL DEFAULT '{}';
UPDATE issues SET issue_contract_json = '{"exempt":true}'
WHERE native_id IS NOT NULL AND workflow_state_id IN (
  SELECT id FROM workflow_states WHERE lower(trim(detent_state)) <> 'backlog'
);

UPDATE issues SET issue_contract_json = json_object('confirmed_body', body)
WHERE native_id IS NOT NULL AND json_extract(issue_contract_json, '$.exempt') IS NULL
AND COALESCE((SELECT json_extract(e.actor_json, '$.kind') FROM collaboration_events e
 WHERE e.work_item_id = issues.native_id AND e.organization_id = issues.organization_id AND e.project_id = issues.project_id
 AND (e.type = 'issue.created' OR (e.type = 'issue.edited' AND EXISTS (SELECT 1 FROM json_each(e.data_json, '$.fields') WHERE value = 'body')))
 ORDER BY e.sequence DESC LIMIT 1), json_extract(actor_json, '$.kind')) = 'human'
AND instr(body, 'detent-origin') = 0 AND instr(body, 'detent-audit-fp') = 0;

-- +goose Down
ALTER TABLE issues DROP COLUMN issue_contract_json;
