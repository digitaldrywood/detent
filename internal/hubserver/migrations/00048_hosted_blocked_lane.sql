-- +goose Up
-- Add Blocked to untouched hosted workflows so projects with review.human=false
-- can retain the existing reason without entering Human Review. Customized
-- workflows are not rewritten.
INSERT INTO workflow_states (project_id, source_name, detent_state, terminal, dispatchable, created_at, updated_at)
SELECT p.id, 'Blocked', 'Blocked', 0, 0, strftime('%Y-%m-%dT%H:%M:%SZ', 'now'), strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
FROM projects p
WHERE p.profile = 'native'
  AND json_valid(p.states_json)
  AND json(p.states_json) = json('[{"name":"Todo","terminal":false,"dispatchable":true,"transitions":["In Progress","Done"]},{"name":"In Progress","terminal":false,"dispatchable":true,"transitions":["Todo","Human Review","Merging","Done"]},{"name":"Human Review","terminal":false,"dispatchable":false,"transitions":["Done","In Progress","Merging"]},{"name":"Merging","terminal":false,"dispatchable":true,"transitions":["Done","Human Review","In Progress"]},{"name":"Done","terminal":true,"dispatchable":false,"transitions":["Todo"]}]')
  AND NOT EXISTS (SELECT 1 FROM workflow_states w WHERE w.project_id = p.id AND w.source_name = 'Blocked');

UPDATE projects
SET states_json = '[{"name":"Todo","terminal":false,"dispatchable":true,"transitions":["In Progress","Done"]},{"name":"In Progress","terminal":false,"dispatchable":true,"transitions":["Todo","Blocked","Human Review","Merging","Done"]},{"name":"Blocked","terminal":false,"dispatchable":false,"transitions":["Todo","In Progress","Done"]},{"name":"Human Review","terminal":false,"dispatchable":false,"transitions":["Done","In Progress","Blocked","Merging"]},{"name":"Merging","terminal":false,"dispatchable":true,"transitions":["Done","Blocked","Human Review","In Progress"]},{"name":"Done","terminal":true,"dispatchable":false,"transitions":["Todo"]}]'
WHERE profile = 'native'
  AND json_valid(states_json)
  AND json(states_json) = json('[{"name":"Todo","terminal":false,"dispatchable":true,"transitions":["In Progress","Done"]},{"name":"In Progress","terminal":false,"dispatchable":true,"transitions":["Todo","Human Review","Merging","Done"]},{"name":"Human Review","terminal":false,"dispatchable":false,"transitions":["Done","In Progress","Merging"]},{"name":"Merging","terminal":false,"dispatchable":true,"transitions":["Done","Human Review","In Progress"]},{"name":"Done","terminal":true,"dispatchable":false,"transitions":["Todo"]}]');

-- +goose Down
-- Existing work may occupy Blocked, so the lane is retained.
SELECT 1;
