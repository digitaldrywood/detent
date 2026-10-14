-- +goose Up
-- Hosted projects gain a review lane.
--
-- A completed native run's Change Request waits in the project's review lane,
-- and the hosted project template had none: Todo and In Progress dispatch,
-- Done ends the work, so a run that committed a change had nowhere to go and
-- its completion waited forever. New hosted projects start with Human Review.
-- This moves the projects created from the old template onto the same
-- workflow. Only a workflow that is exactly the old template is rewritten; a
-- workflow somebody customized is left as it is, and needs its own review
-- lane for native changes.
INSERT INTO workflow_states (project_id, source_name, detent_state, terminal, dispatchable, created_at, updated_at)
SELECT p.id, 'Human Review', 'Human Review', 0, 0, strftime('%Y-%m-%dT%H:%M:%SZ', 'now'), strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
FROM projects p
WHERE p.profile = 'native'
  AND json_valid(p.states_json)
  AND json(p.states_json) = json('[{"name":"Todo","terminal":false,"dispatchable":true,"transitions":["In Progress","Done"]},{"name":"In Progress","terminal":false,"dispatchable":true,"transitions":["Todo","Done"]},{"name":"Done","terminal":true,"dispatchable":false,"transitions":["Todo"]}]')
  AND NOT EXISTS (SELECT 1 FROM workflow_states w WHERE w.project_id = p.id AND w.source_name = 'Human Review');

UPDATE projects
SET states_json = '[{"name":"Todo","terminal":false,"dispatchable":true,"transitions":["In Progress","Done"]},{"name":"In Progress","terminal":false,"dispatchable":true,"transitions":["Todo","Human Review","Done"]},{"name":"Human Review","terminal":false,"dispatchable":false,"transitions":["Done","In Progress"]},{"name":"Done","terminal":true,"dispatchable":false,"transitions":["Todo"]}]'
WHERE profile = 'native'
  AND json_valid(states_json)
  AND json(states_json) = json('[{"name":"Todo","terminal":false,"dispatchable":true,"transitions":["In Progress","Done"]},{"name":"In Progress","terminal":false,"dispatchable":true,"transitions":["Todo","Done"]},{"name":"Done","terminal":true,"dispatchable":false,"transitions":["Todo"]}]');

-- +goose Down
-- A project may already hold items in Human Review, so the lane is kept.
SELECT 1;
