-- +goose Up
ALTER TABLE hosted_invitations ADD COLUMN expires_at TEXT NOT NULL DEFAULT '';
