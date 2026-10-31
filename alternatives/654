-- +goose Up
CREATE TABLE platform_members (
  email TEXT PRIMARY KEY,
  role TEXT NOT NULL CHECK (role IN ('admin','support','billing','viewer')),
  added_by TEXT NOT NULL,
  added_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE platform_member_changes (
  id INTEGER PRIMARY KEY,
  email TEXT NOT NULL,
  action TEXT NOT NULL CHECK (action IN ('added','role_changed','removed','seeded')),
  previous_role TEXT NOT NULL DEFAULT '',
  role TEXT NOT NULL DEFAULT '',
  actor_email TEXT NOT NULL,
  actor_subject TEXT NOT NULL,
  reason TEXT NOT NULL,
  recorded_at TEXT NOT NULL
);
CREATE INDEX platform_member_changes_recorded_at ON platform_member_changes(recorded_at);
-- +goose StatementBegin
CREATE TRIGGER platform_member_change_immutable BEFORE UPDATE ON platform_member_changes
BEGIN
  SELECT RAISE(ABORT, 'platform member changes are immutable');
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER platform_member_change_retained BEFORE DELETE ON platform_member_changes
BEGIN
  SELECT RAISE(ABORT, 'platform member changes are retained');
END;
-- +goose StatementEnd
