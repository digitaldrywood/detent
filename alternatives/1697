-- +goose Up
CREATE INDEX collaboration_events_publication_push ON collaboration_events(
 organization_id, project_id, work_item_id,
 json_extract(data_json,'$.run.handoff.change.change_id'),
 json_extract(data_json,'$.run.handoff.change.version_id'),
 json_extract(data_json,'$.run.handoff.change.head_sha'), sequence DESC
)
WHERE type='run.checkpointed'
 AND json_extract(data_json,'$.run.handoff.external_effect')='git_push'
 AND json_extract(data_json,'$.run.handoff.effect_state')='confirmed';

-- +goose Down
DROP INDEX collaboration_events_publication_push;
