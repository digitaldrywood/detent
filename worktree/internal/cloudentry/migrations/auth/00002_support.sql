-- +goose Up
ALTER TABLE authorizations ADD COLUMN support INTEGER NOT NULL DEFAULT 0 CHECK (support IN (0, 1));
ALTER TABLE authorizations ADD COLUMN effective_email TEXT NOT NULL DEFAULT '';
ALTER TABLE transactions ADD COLUMN support_actor TEXT NOT NULL DEFAULT '';
ALTER TABLE transactions ADD COLUMN support_session TEXT NOT NULL DEFAULT '';
