-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
ALTER TABLE change_evidence DROP CONSTRAINT IF EXISTS change_evidence_project_present;
DROP POLICY IF EXISTS evidence_project_initialization_enum ON change_evidence;
CREATE POLICY evidence_project_initialization_enum ON change_evidence FOR SELECT TO detent_migration USING (true);
-- +goose StatementBegin
DO $$
DECLARE
 after_organization text := '';
 selected_organization text;
 after_change text;
 after_sequence bigint;
 next_change text;
 next_sequence bigint;
 mismatches bigint;
BEGIN
 LOOP
  SELECT organization_id INTO selected_organization FROM change_evidence WHERE organization_id>after_organization ORDER BY organization_id,change_id,sequence LIMIT 1;
  EXIT WHEN NOT FOUND;
  after_change := ''; after_sequence := 0;
  LOOP
   PERFORM set_config('detent.organization_id',selected_organization,true);
   WITH examined AS MATERIALIZED (
    SELECT e.change_id,e.sequence,e.project_id FROM change_evidence e WHERE e.organization_id=selected_organization AND (e.change_id,e.sequence)>(after_change,after_sequence) ORDER BY e.change_id,e.sequence LIMIT 2000 FOR UPDATE OF e
   ), expected AS MATERIALIZED (
    SELECT e.*,c.project_id AS expected_project FROM examined e LEFT JOIN change_requests c ON c.organization_id=selected_organization AND c.id=e.change_id
   ), initialized AS (
    UPDATE change_evidence e SET project_id=p.expected_project FROM expected p WHERE e.organization_id=selected_organization AND e.change_id=p.change_id AND e.sequence=p.sequence AND e.project_id IS NULL AND p.expected_project IS NOT NULL RETURNING e.change_id,e.sequence
   )
   SELECT e.change_id,e.sequence,(SELECT count(*) FROM expected p WHERE p.expected_project IS NULL OR p.project_id IS NOT NULL AND p.project_id IS DISTINCT FROM p.expected_project)
   INTO next_change,next_sequence,mismatches FROM expected e LEFT JOIN initialized changed ON changed.change_id=e.change_id AND changed.sequence=e.sequence ORDER BY e.change_id DESC,e.sequence DESC LIMIT 1;
   IF NOT FOUND THEN COMMIT; EXIT; END IF;
   IF mismatches<>0 THEN RAISE EXCEPTION 'evidence project initialization differs from retained change'; END IF;
   after_change := next_change; after_sequence := next_sequence;
   COMMIT;
  END LOOP;
  after_organization := selected_organization;
 END LOOP;
END;
$$;
-- +goose StatementEnd
ALTER TABLE change_evidence ADD CONSTRAINT change_evidence_project_present CHECK (project_id IS NOT NULL AND btrim(project_id)<>'') NOT VALID;
ALTER TABLE change_evidence VALIDATE CONSTRAINT change_evidence_project_present;
DROP POLICY evidence_project_initialization_enum ON change_evidence;
RESET lock_timeout;

-- +goose Down
SELECT 1;
