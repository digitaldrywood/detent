-- +goose Up
-- Hosted projects gain a landing lane. (Numbered 38: 37 is the observed-policies
-- migration that landed on develop first.)
--
-- An approved Change Request moves to Merging, where the runner that holds
-- the project lands it on the base branch with its own git credentials and
-- the Hub finishes the issue in Done. The lane dispatches, so the runner
-- claims the item the way it claims work. Only a workflow that is exactly the
-- migration-36 template is rewritten; a customized workflow is left as it is,
-- and an approval on such a project leaves the item in review for a person
-- to land by hand.
INSERT INTO workflow_states (project_id, source_name, detent_state, terminal, dispatchable, created_at, updated_at)
SELECT p.id, 'Merging', 'Merging', 0, 1, strftime('%Y-%m-%dT%H:%M:%SZ', 'now'), strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
FROM projects p
WHERE p.profile = 'native'
  AND json_valid(p.states_json)
  AND json(p.states_json) = json('[{"name":"Todo","terminal":false,"dispatchable":true,"transitions":["In Progress","Done"]},{"name":"In Progress","terminal":false,"dispatchable":true,"transitions":["Todo","Human Review","Done"]},{"name":"Human Review","terminal":false,"dispatchable":false,"transitions":["Done","In Progress"]},{"name":"Done","terminal":true,"dispatchable":false,"transitions":["Todo"]}]')
  AND NOT EXISTS (SELECT 1 FROM workflow_states w WHERE w.project_id = p.id AND w.source_name = 'Merging');

UPDATE projects
SET states_json = '[{"name":"Todo","terminal":false,"dispatchable":true,"transitions":["In Progress","Done"]},{"name":"In Progress","terminal":false,"dispatchable":true,"transitions":["Todo","Human Review","Done"]},{"name":"Human Review","terminal":false,"dispatchable":false,"transitions":["Done","In Progress","Merging"]},{"name":"Merging","terminal":false,"dispatchable":true,"transitions":["Done","Human Review","In Progress"]},{"name":"Done","terminal":true,"dispatchable":false,"transitions":["Todo"]}]'
WHERE profile = 'native'
  AND json_valid(states_json)
  AND json(states_json) = json('[{"name":"Todo","terminal":false,"dispatchable":true,"transitions":["In Progress","Done"]},{"name":"In Progress","terminal":false,"dispatchable":true,"transitions":["Todo","Human Review","Done"]},{"name":"Human Review","terminal":false,"dispatchable":false,"transitions":["Done","In Progress"]},{"name":"Done","terminal":true,"dispatchable":false,"transitions":["Todo"]}]');

-- +goose Down
-- A project may already hold items in Merging, so the lane is kept.
SELECT 1;
