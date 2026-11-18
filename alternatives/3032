-- +goose Up
CREATE TABLE issue_notification_cursors (
 organization_id text NOT NULL,
 work_item_id text NOT NULL,
 sequence bigint NOT NULL DEFAULT 0,
 recipient_item text NOT NULL DEFAULT '',
 recipient_principal text NOT NULL DEFAULT '',
 PRIMARY KEY (organization_id,work_item_id),
 FOREIGN KEY (organization_id,work_item_id) REFERENCES issues(organization_id,native_id) ON DELETE CASCADE
);
CREATE TABLE issue_subscriptions (
 organization_id text NOT NULL,
 work_item_id text NOT NULL,
 user_id text NOT NULL,
 token text NOT NULL,
 unsubscribed boolean NOT NULL DEFAULT false,
 PRIMARY KEY (organization_id,work_item_id,user_id),
 UNIQUE (organization_id,token),
 FOREIGN KEY (organization_id,work_item_id) REFERENCES issues(organization_id,native_id) ON DELETE CASCADE,
 FOREIGN KEY (organization_id,user_id) REFERENCES hosted_members(organization_id,user_id) ON DELETE CASCADE
);
CREATE TABLE issue_notification_notices (
 organization_id text NOT NULL,
 work_item_id text NOT NULL,
 user_id text NOT NULL,
 event_id text NOT NULL,
 source_item text NOT NULL,
 due_at timestamptz NOT NULL,
 state_from text NOT NULL,
 state_to text NOT NULL,
 author text NOT NULL,
 comment_id text NOT NULL,
 related boolean NOT NULL,
 verify boolean NOT NULL,
 PRIMARY KEY (organization_id,work_item_id,user_id,source_item,event_id),
 FOREIGN KEY (organization_id,work_item_id,user_id) REFERENCES issue_subscriptions(organization_id,work_item_id,user_id) ON DELETE CASCADE
);
CREATE INDEX issue_notification_due_idx ON issue_notification_notices(organization_id,due_at,work_item_id,user_id);
ALTER TABLE issue_notification_cursors ENABLE ROW LEVEL SECURITY;
ALTER TABLE issue_notification_cursors FORCE ROW LEVEL SECURITY;
REVOKE ALL ON issue_notification_cursors FROM PUBLIC,detent_platform;
GRANT SELECT,INSERT,UPDATE,DELETE ON issue_notification_cursors TO detent_runtime;
CREATE POLICY tenant_isolation ON issue_notification_cursors TO detent_runtime,detent_migration USING (organization_id=NULLIF(current_setting('detent.organization_id',true),'')) WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
ALTER TABLE issue_subscriptions ENABLE ROW LEVEL SECURITY;
ALTER TABLE issue_subscriptions FORCE ROW LEVEL SECURITY;
REVOKE ALL ON issue_subscriptions FROM PUBLIC,detent_platform;
GRANT SELECT,INSERT,UPDATE,DELETE ON issue_subscriptions TO detent_runtime;
CREATE POLICY tenant_isolation ON issue_subscriptions TO detent_runtime,detent_migration USING (organization_id=NULLIF(current_setting('detent.organization_id',true),'')) WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
ALTER TABLE issue_notification_notices ENABLE ROW LEVEL SECURITY;
ALTER TABLE issue_notification_notices FORCE ROW LEVEL SECURITY;
REVOKE ALL ON issue_notification_notices FROM PUBLIC,detent_platform;
GRANT SELECT,INSERT,UPDATE,DELETE ON issue_notification_notices TO detent_runtime;
CREATE POLICY tenant_isolation ON issue_notification_notices TO detent_runtime,detent_migration USING (organization_id=NULLIF(current_setting('detent.organization_id',true),'')) WITH CHECK (organization_id=NULLIF(current_setting('detent.organization_id',true),''));
-- +goose Down
DROP TABLE issue_notification_notices;
DROP TABLE issue_subscriptions;
DROP TABLE issue_notification_cursors;
