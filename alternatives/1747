-- +goose Up
ALTER TABLE organizations ADD COLUMN urgent_runner_update_json TEXT NOT NULL DEFAULT '{"revision":0,"request":null}' CHECK (json_valid(urgent_runner_update_json));
