-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout='5s';
DROP POLICY IF EXISTS landing_receipt_initialization_enum ON change_landing_receipts;
CREATE POLICY landing_receipt_initialization_enum ON change_landing_receipts FOR SELECT TO detent_migration USING(true);
-- +goose StatementBegin
DO $$
DECLARE after_organization text:=''; selected_organization text; after_attempt text; next_attempt text; mismatches bigint;
BEGIN
 LOOP
  SELECT organization_id INTO selected_organization FROM change_landing_receipts WHERE organization_id>after_organization ORDER BY organization_id,attempt_id LIMIT 1;
  EXIT WHEN NOT FOUND;
  after_attempt:='';
  LOOP
   PERFORM set_config('detent.organization_id',selected_organization,true);
   WITH examined AS MATERIALIZED (
    SELECT * FROM change_landing_receipts WHERE organization_id=selected_organization AND attempt_id>after_attempt ORDER BY attempt_id LIMIT 2000 FOR UPDATE
   ), expected AS MATERIALIZED (
    SELECT e.*,a.issue_id AS expected_issue,a.fencing_token AS expected_fence,v.change_id AS expected_change,c.project_id AS expected_project,regexp_replace(regexp_replace(regexp_replace(lower(coalesce(nullif(e.repository,''),v.repository)),'^https://github[.]com/',''),'^git@github[.]com:',''),'[.]git$','') AS expected_repository,
     a.id IS NULL OR v.id IS NULL OR c.id IS NULL OR i.id IS NULL OR i.project_id IS DISTINCT FROM c.project_id OR i.native_id IS DISTINCT FROM c.work_item_id
     OR e.repository<>'' AND v.repository<>'' AND regexp_replace(regexp_replace(regexp_replace(lower(e.repository),'^https://github[.]com/',''),'^git@github[.]com:',''),'[.]git$','')<>regexp_replace(regexp_replace(regexp_replace(lower(v.repository),'^https://github[.]com/',''),'^git@github[.]com:',''),'[.]git$','')
     OR e.issue_id IS NOT NULL AND e.issue_id IS DISTINCT FROM a.issue_id
     OR e.change_id IS NOT NULL AND e.change_id IS DISTINCT FROM v.change_id
     OR e.fencing_token IS NOT NULL AND e.fencing_token IS DISTINCT FROM a.fencing_token
     OR e.repository_identity IS NOT NULL AND e.repository_identity IS DISTINCT FROM regexp_replace(regexp_replace(regexp_replace(lower(coalesce(nullif(e.repository,''),v.repository)),'^https://github[.]com/',''),'^git@github[.]com:',''),'[.]git$','')
     OR e.project_id IS NOT NULL AND e.project_id IS DISTINCT FROM c.project_id AS differs
    FROM examined e
    LEFT JOIN native_attempts a ON a.organization_id=selected_organization AND a.id=e.attempt_id
    LEFT JOIN change_versions v ON v.organization_id=selected_organization AND v.id=e.version_id
    LEFT JOIN change_requests c ON c.organization_id=selected_organization AND c.id=v.change_id
    LEFT JOIN work_item_identities i ON i.organization_id=selected_organization AND i.id=a.issue_id
   ), initialized AS (
    UPDATE change_landing_receipts r SET issue_id=e.expected_issue,change_id=e.expected_change,fencing_token=e.expected_fence,repository_identity=e.expected_repository,project_id=e.expected_project
    FROM expected e WHERE r.organization_id=selected_organization AND r.attempt_id=e.attempt_id AND r.issue_id IS NULL AND NOT e.differs
    RETURNING r.attempt_id
   )
   SELECT e.attempt_id,(SELECT count(*) FROM expected x WHERE x.differs) INTO next_attempt,mismatches
   FROM expected e LEFT JOIN initialized changed ON changed.attempt_id=e.attempt_id ORDER BY e.attempt_id DESC LIMIT 1;
   IF NOT FOUND THEN COMMIT;EXIT;END IF;
   IF mismatches<>0 THEN RAISE EXCEPTION 'landing receipt identity differs from retained authority';END IF;
   after_attempt:=next_attempt;
   COMMIT;
  END LOOP;
  after_organization:=selected_organization;
 END LOOP;
END;
$$;
-- +goose StatementEnd
DROP POLICY landing_receipt_initialization_enum ON change_landing_receipts;
RESET lock_timeout;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'landing receipt identity cannot be discarded'; END; $$;
-- +goose StatementEnd
