-- +goose Up
ALTER TABLE organizations ADD COLUMN error_detail TEXT NOT NULL DEFAULT '' CHECK (length(error_detail) <= 300);
