-- +goose Up
ALTER TABLE runner_enrollments ADD COLUMN scope TEXT NOT NULL DEFAULT 'projects' CHECK (scope IN ('projects', 'organization'));
ALTER TABLE runner_identities ADD COLUMN scope TEXT NOT NULL DEFAULT 'projects' CHECK (scope IN ('projects', 'organization'));
