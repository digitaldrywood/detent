-- +goose Up
ALTER TABLE api_tokens ADD COLUMN operator_project_access TEXT NOT NULL DEFAULT 'selected' CHECK (operator_project_access IN ('all', 'selected'));
