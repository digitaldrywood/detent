-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
ALTER TABLE native_attempts DROP CONSTRAINT IF EXISTS native_attempts_project_present;
DROP POLICY IF EXISTS attempt_project_initialization_enum ON native_attempts;
CREATE POLICY attempt_project_initialization_enum ON native_attempts FOR SELECT TO detent_migration USING (true);
-- +goose StatementBegin
DO $$
DECLARE
 after_organization text := '';
 selected_organization text;
 after_attempt text;
 next_attempt text;
 mismatches bigint;
BEGIN
 LOOP
  SELECT organization_id INTO selected_organization FROM native_attempts
  WHERE organization_id>after_organization ORDER BY organization_id,id LIMIT 1;
  EXIT WHEN NOT FOUND;
  after_attempt := '';
  LOOP
   PERFORM set_config('detent.organization_id', selected_organization, true);
   WITH examined AS MATERIALIZED (
    SELECT a.id,a.issue_id,a.project_id FROM native_attempts a
    WHERE a.organization_id=selected_organization AND a.id>after_attempt
    ORDER BY a.id LIMIT 2000 FOR UPDATE OF a
   ), expected AS MATERIALIZED (
    SELECT e.*,i.project_id AS expected_project FROM examined e
    LEFT JOIN work_item_identities i ON i.organization_id=selected_organization AND i.id=e.issue_id
   ), initialized AS (
    UPDATE native_attempts a SET project_id=e.expected_project FROM expected e
    WHERE a.organization_id=selected_organization AND a.id=e.id AND a.project_id IS NULL AND e.expected_project IS NOT NULL
    RETURNING a.id
   )
   SELECT e.id,(SELECT count(*) FROM expected p WHERE p.expected_project IS NULL OR p.project_id IS NOT NULL AND p.project_id IS DISTINCT FROM p.expected_project)
   INTO next_attempt,mismatches FROM expected e LEFT JOIN initialized changed ON changed.id=e.id
   ORDER BY e.id DESC LIMIT 1;
   IF NOT FOUND THEN
    COMMIT;
    EXIT;
   END IF;
   IF mismatches<>0 THEN
    RAISE EXCEPTION 'attempt project initialization differs from retained identity';
   END IF;
   after_attempt := next_attempt;
   COMMIT;
  END LOOP;
  after_organization := selected_organization;
 END LOOP;
END;
$$;
-- +goose StatementEnd
ALTER TABLE native_attempts ADD CONSTRAINT native_attempts_project_present CHECK (project_id IS NOT NULL AND btrim(project_id)<>'') NOT VALID;
ALTER TABLE native_attempts VALIDATE CONSTRAINT native_attempts_project_present;
DROP POLICY attempt_project_initialization_enum ON native_attempts;
RESET lock_timeout;

-- +goose Down
SELECT 1;
