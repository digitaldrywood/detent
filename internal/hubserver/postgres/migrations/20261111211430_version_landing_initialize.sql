-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
ALTER TABLE change_versions DROP CONSTRAINT IF EXISTS change_versions_project_present;
DROP POLICY IF EXISTS version_landing_initialization_enum ON change_versions;
CREATE POLICY version_landing_initialization_enum ON change_versions FOR SELECT TO detent_migration USING (true);
-- +goose StatementBegin
DO $$
DECLARE
 after_organization text := '';
 selected_organization text;
 after_version text;
 next_version text;
 mismatches bigint;
BEGIN
 LOOP
  SELECT organization_id INTO selected_organization FROM change_versions WHERE organization_id>after_organization ORDER BY organization_id,id LIMIT 1;
  EXIT WHEN NOT FOUND;
  after_version := '';
  LOOP
   PERFORM set_config('detent.organization_id',selected_organization,true);
   WITH examined AS MATERIALIZED (
    SELECT v.id,v.change_id,v.project_id FROM change_versions v WHERE v.organization_id=selected_organization AND v.id>after_version ORDER BY v.id LIMIT 2000 FOR UPDATE OF v
   ), expected AS MATERIALIZED (
    SELECT e.*,c.project_id AS expected_project,c.landed_version_id,c.landed_at,c.landed_head_sha,c.landed_merge_sha,c.landed_base_ref,c.payload_object_key,c.payload_sha256,c.payload_bytes,c.payload_content_bytes
    FROM examined e LEFT JOIN change_requests c ON c.organization_id=selected_organization AND c.id=e.change_id
   ), initialized AS (
    UPDATE change_versions v SET project_id=coalesce(v.project_id,e.expected_project),
     landing_history_known=v.landing_history_known OR coalesce(e.landed_version_id=v.id,false),
     landed_at=CASE WHEN e.landed_version_id=v.id THEN e.landed_at ELSE v.landed_at END,
     landed_head_sha=CASE WHEN e.landed_version_id=v.id THEN e.landed_head_sha ELSE v.landed_head_sha END,
     landed_merge_sha=CASE WHEN e.landed_version_id=v.id THEN e.landed_merge_sha ELSE v.landed_merge_sha END,
     landed_base_ref=CASE WHEN e.landed_version_id=v.id THEN e.landed_base_ref ELSE v.landed_base_ref END,
     landed_object_key=CASE WHEN e.landed_version_id=v.id THEN e.payload_object_key ELSE v.landed_object_key END,
     landed_sha256=CASE WHEN e.landed_version_id=v.id THEN e.payload_sha256 ELSE v.landed_sha256 END,
     landed_bytes=CASE WHEN e.landed_version_id=v.id THEN e.payload_bytes ELSE v.landed_bytes END,
     landed_content_bytes=CASE WHEN e.landed_version_id=v.id THEN e.payload_content_bytes ELSE v.landed_content_bytes END,
     landed_payload_format_version=CASE WHEN e.landed_version_id=v.id THEN 1 ELSE v.landed_payload_format_version END
    FROM expected e WHERE v.organization_id=selected_organization AND v.id=e.id AND e.expected_project IS NOT NULL AND (v.project_id IS NULL OR v.landed_at IS NULL AND e.landed_version_id=v.id)
    RETURNING v.id
   )
   SELECT e.id,(SELECT count(*) FROM expected p WHERE p.expected_project IS NULL OR p.project_id IS NOT NULL AND p.project_id IS DISTINCT FROM p.expected_project)
   INTO next_version,mismatches FROM expected e LEFT JOIN initialized changed ON changed.id=e.id ORDER BY e.id DESC LIMIT 1;
   IF NOT FOUND THEN COMMIT; EXIT; END IF;
   IF mismatches<>0 THEN RAISE EXCEPTION 'version project initialization differs from retained change'; END IF;
   after_version := next_version;
   COMMIT;
  END LOOP;
  after_organization := selected_organization;
 END LOOP;
END;
$$;
-- +goose StatementEnd
ALTER TABLE change_versions ADD CONSTRAINT change_versions_project_present CHECK (project_id IS NOT NULL AND btrim(project_id)<>'') NOT VALID;
ALTER TABLE change_versions VALIDATE CONSTRAINT change_versions_project_present;
DROP POLICY version_landing_initialization_enum ON change_versions;
RESET lock_timeout;

-- +goose Down
SELECT 1;
