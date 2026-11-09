-- +goose Up
CREATE TABLE issue_notification_receipts (
 organization_id text NOT NULL,
 id text NOT NULL,
 work_item_id text NOT NULL,
 user_id text NOT NULL,
 status text NOT NULL CHECK(status IN ('uncertain','rejected','sent','suppressed')),
 attempts bigint NOT NULL CHECK(attempts>0),
 notice_count integer NOT NULL CHECK(notice_count BETWEEN 1 AND 1000),
 claimed_at timestamptz NOT NULL,
 next_attempt_at timestamptz NOT NULL,
 finished_at timestamptz,
 PRIMARY KEY(organization_id,id),
 FOREIGN KEY(organization_id,work_item_id,user_id) REFERENCES issue_subscriptions(organization_id,work_item_id,user_id) ON DELETE CASCADE
);
CREATE INDEX issue_notification_retry ON issue_notification_receipts(organization_id,next_attempt_at,id) WHERE status='rejected';
CREATE TABLE issue_notification_receipt_notices (
 organization_id text NOT NULL,
 receipt_id text NOT NULL,
 source_item text NOT NULL,
 event_id text NOT NULL,
 due_at timestamptz NOT NULL,
 state_from text NOT NULL,
 state_to text NOT NULL,
 author text NOT NULL,
 comment_id text NOT NULL,
 related boolean NOT NULL,
 verify boolean NOT NULL,
 PRIMARY KEY(organization_id,receipt_id,source_item,event_id),
 FOREIGN KEY(organization_id,receipt_id) REFERENCES issue_notification_receipts(organization_id,id) ON DELETE CASCADE
);
ALTER TABLE issue_notification_receipts ENABLE ROW LEVEL SECURITY;
ALTER TABLE issue_notification_receipts FORCE ROW LEVEL SECURITY;
REVOKE ALL ON issue_notification_receipts FROM PUBLIC,detent_platform;
GRANT SELECT,INSERT,UPDATE,DELETE ON issue_notification_receipts TO detent_runtime;
CREATE POLICY tenant_isolation ON issue_notification_receipts TO detent_runtime,detent_migration
 USING(organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK(organization_id=NULLIF(current_setting('detent.organization_id',true),''));
ALTER TABLE issue_notification_receipt_notices ENABLE ROW LEVEL SECURITY;
ALTER TABLE issue_notification_receipt_notices FORCE ROW LEVEL SECURITY;
REVOKE ALL ON issue_notification_receipt_notices FROM PUBLIC,detent_platform;
GRANT SELECT,INSERT,UPDATE,DELETE ON issue_notification_receipt_notices TO detent_runtime;
CREATE POLICY tenant_isolation ON issue_notification_receipt_notices TO detent_runtime,detent_migration
 USING(organization_id=NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK(organization_id=NULLIF(current_setting('detent.organization_id',true),''));

-- +goose Down
DROP TABLE issue_notification_receipt_notices;
DROP TABLE issue_notification_receipts;
