-- +goose Up
ALTER TABLE issues ADD COLUMN machine_fingerprint TEXT GENERATED ALWAYS AS (native_machine_identity(body,'origin')) VIRTUAL;
ALTER TABLE issues ADD COLUMN machine_defect TEXT GENERATED ALWAYS AS (native_machine_identity(body,'defect')) VIRTUAL;
ALTER TABLE native_comments ADD COLUMN machine_defect TEXT GENERATED ALWAYS AS (native_machine_identity(body,'defect')) VIRTUAL;
CREATE INDEX issues_machine_fingerprint ON issues(organization_id,project_id,machine_fingerprint,id DESC);
CREATE INDEX issues_machine_defect ON issues(organization_id,project_id,machine_defect,id DESC);
CREATE INDEX native_comments_machine_defect ON native_comments(organization_id,project_id,machine_defect,work_item_id,id DESC);

-- +goose Down
DROP INDEX native_comments_machine_defect;
DROP INDEX issues_machine_defect;
DROP INDEX issues_machine_fingerprint;
ALTER TABLE native_comments DROP COLUMN machine_defect;
ALTER TABLE issues DROP COLUMN machine_defect;
ALTER TABLE issues DROP COLUMN machine_fingerprint;
