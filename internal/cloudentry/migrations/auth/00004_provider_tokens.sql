-- +goose Up
ALTER TABLE authorizations ADD COLUMN access_token TEXT NOT NULL DEFAULT '';
ALTER TABLE authorizations ADD COLUMN refresh_token TEXT NOT NULL DEFAULT '';
