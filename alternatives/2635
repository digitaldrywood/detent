-- +goose Up
ALTER TABLE access_imports DROP CONSTRAINT access_imports_stage_check;
ALTER TABLE access_imports ADD CONSTRAINT access_imports_stage_check CHECK (stage BETWEEN 0 AND 20);

CREATE TABLE hosted_release_preferences (
    organization_id text NOT NULL,
    user_id text NOT NULL,
    version text NOT NULL,
    build_time timestamptz NOT NULL,
    PRIMARY KEY (organization_id, user_id),
    FOREIGN KEY (organization_id, user_id) REFERENCES hosted_members (organization_id, user_id) ON DELETE CASCADE
);

CREATE TABLE daily_summary_preferences (
    organization_id text NOT NULL,
    user_id text NOT NULL,
    enabled boolean NOT NULL DEFAULT false,
    send_minute integer NOT NULL DEFAULT 360 CHECK (send_minute >= 0 AND send_minute < 1440 AND send_minute % 30 = 0),
    time_zone text NOT NULL DEFAULT 'UTC',
    weekdays boolean NOT NULL DEFAULT false,
    token text NOT NULL UNIQUE,
    window_end bigint NOT NULL,
    due_at bigint NOT NULL,
    PRIMARY KEY (organization_id, user_id),
    FOREIGN KEY (organization_id, user_id) REFERENCES hosted_members (organization_id, user_id) ON DELETE CASCADE
);

CREATE INDEX daily_summary_preferences_due ON daily_summary_preferences (organization_id, due_at, user_id) WHERE enabled;

CREATE TABLE daily_summary_receipts (
    organization_id text NOT NULL,
    user_id text NOT NULL,
    window_end bigint NOT NULL,
    status text NOT NULL CHECK (status IN ('uncertain', 'quiet', 'sent', 'rejected')),
    attempts integer NOT NULL DEFAULT 1 CHECK (attempts > 0),
    recorded_at bigint NOT NULL,
    PRIMARY KEY (organization_id, user_id, window_end),
    FOREIGN KEY (organization_id, user_id) REFERENCES daily_summary_preferences (organization_id, user_id) ON DELETE CASCADE
);

ALTER TABLE hosted_release_preferences ENABLE ROW LEVEL SECURITY;
ALTER TABLE hosted_release_preferences FORCE ROW LEVEL SECURITY;
REVOKE ALL ON hosted_release_preferences FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT, UPDATE, DELETE ON hosted_release_preferences TO detent_runtime;
CREATE POLICY tenant_isolation ON hosted_release_preferences TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

ALTER TABLE daily_summary_receipts ENABLE ROW LEVEL SECURITY;
ALTER TABLE daily_summary_receipts FORCE ROW LEVEL SECURITY;
REVOKE ALL ON daily_summary_receipts FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT, UPDATE, DELETE ON daily_summary_receipts TO detent_runtime;
CREATE POLICY tenant_isolation ON daily_summary_receipts TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

ALTER TABLE daily_summary_preferences ENABLE ROW LEVEL SECURITY;
ALTER TABLE daily_summary_preferences FORCE ROW LEVEL SECURITY;
REVOKE ALL ON daily_summary_preferences FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT, UPDATE, DELETE ON daily_summary_preferences TO detent_runtime;
CREATE POLICY tenant_isolation ON daily_summary_preferences TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

-- +goose Down
ALTER TABLE access_imports DROP CONSTRAINT access_imports_stage_check;
ALTER TABLE access_imports ADD CONSTRAINT access_imports_stage_check CHECK (stage BETWEEN 0 AND 17);

DROP TABLE daily_summary_receipts;
DROP TABLE daily_summary_preferences;
DROP TABLE hosted_release_preferences;
