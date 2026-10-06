-- +goose Up
ALTER TABLE project_sprite_pools ADD COLUMN placement_json TEXT NOT NULL DEFAULT '{"mode":"blended"}' CHECK (json_valid(placement_json));

-- +goose Down
ALTER TABLE project_sprite_pools DROP COLUMN placement_json;
