-- +goose Up
-- Hosted projects land work that needs no review without a stop in Human
-- Review. A run whose version its project's review policy already accepts
-- moves from In Progress straight to Merging, where the runner lands it.
-- Human Review stays for projects that ask for a person and for landings a
-- person has to resolve. Only a workflow that is exactly the migration-38
-- template is rewritten; a customized workflow is left as it is.
UPDATE projects
SET states_json = '[{"name":"Todo","terminal":false,"dispatchable":true,"transitions":["In Progress","Done"]},{"name":"In Progress","terminal":false,"dispatchable":true,"transitions":["Todo","Human Review","Merging","Done"]},{"name":"Human Review","terminal":false,"dispatchable":false,"transitions":["Done","In Progress","Merging"]},{"name":"Merging","terminal":false,"dispatchable":true,"transitions":["Done","Human Review","In Progress"]},{"name":"Done","terminal":true,"dispatchable":false,"transitions":["Todo"]}]'
WHERE profile = 'native'
  AND json_valid(states_json)
  AND json(states_json) = json('[{"name":"Todo","terminal":false,"dispatchable":true,"transitions":["In Progress","Done"]},{"name":"In Progress","terminal":false,"dispatchable":true,"transitions":["Todo","Human Review","Done"]},{"name":"Human Review","terminal":false,"dispatchable":false,"transitions":["Done","In Progress","Merging"]},{"name":"Merging","terminal":false,"dispatchable":true,"transitions":["Done","Human Review","In Progress"]},{"name":"Done","terminal":true,"dispatchable":false,"transitions":["Todo"]}]');

-- +goose Down
-- Items may already have moved along the new transition, so it is kept.
SELECT 1;
