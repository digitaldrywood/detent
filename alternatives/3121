-- +goose Up
ALTER TABLE collaboration_events
    ADD COLUMN actor_envelope_known boolean NOT NULL DEFAULT false,
    ADD COLUMN actor_via_key text NOT NULL DEFAULT '',
    ADD COLUMN actor_display_name text NOT NULL DEFAULT '',
    ADD COLUMN actor_agent text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE collaboration_events
    DROP COLUMN actor_agent,
    DROP COLUMN actor_display_name,
    DROP COLUMN actor_via_key,
    DROP COLUMN actor_envelope_known;
