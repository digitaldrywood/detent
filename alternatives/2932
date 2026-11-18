-- +goose Up
ALTER TABLE organizations
 ADD COLUMN model_selection_revision bigint NOT NULL DEFAULT 0 CHECK (model_selection_revision >= 0),
 ADD COLUMN model_selection_object_key text,
 ADD COLUMN model_selection_sha256 text,
 ADD COLUMN model_selection_bytes bigint,
 ADD COLUMN model_selection_content_bytes bigint,
 ADD CONSTRAINT organization_model_selection_payload CHECK (
  (model_selection_revision = 0 AND model_selection_object_key IS NULL AND model_selection_sha256 IS NULL AND model_selection_bytes IS NULL AND model_selection_content_bytes IS NULL)
  OR (model_selection_revision > 0 AND model_selection_object_key IS NOT NULL AND model_selection_sha256 IS NOT NULL AND model_selection_bytes IS NOT NULL AND model_selection_content_bytes IS NOT NULL AND model_selection_bytes > 0 AND model_selection_content_bytes > 0)
 );

-- +goose Down
ALTER TABLE organizations
 DROP CONSTRAINT organization_model_selection_payload,
 DROP COLUMN model_selection_content_bytes,
 DROP COLUMN model_selection_bytes,
 DROP COLUMN model_selection_sha256,
 DROP COLUMN model_selection_object_key,
 DROP COLUMN model_selection_revision;
