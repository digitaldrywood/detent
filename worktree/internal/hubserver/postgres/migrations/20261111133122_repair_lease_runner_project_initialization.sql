-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP POLICY IF EXISTS lease_binding_initialization_orgs ON organizations;
CREATE POLICY lease_binding_initialization_orgs ON organizations FOR SELECT TO detent_migration USING (true);
-- +goose StatementBegin
DO $$
DECLARE
 selected_organization text;
 after_expiry timestamptz;
 after_lease text;
 next_expiry timestamptz;
 next_lease text;
 mismatches bigint;
BEGIN
 FOR selected_organization IN SELECT organization_id FROM organizations ORDER BY organization_id LOOP
  after_expiry := '-infinity';
  after_lease := '';
  LOOP
   PERFORM set_config('detent.organization_id', selected_organization, true);
   WITH examined AS MATERIALIZED (
    SELECT l.lease_id,l.expires_at,l.issue_id FROM leases l
    WHERE l.organization_id=selected_organization AND l.released_at IS NULL
     AND l.project_id IS NULL AND (l.expires_at,l.lease_id)>(after_expiry,after_lease)
    ORDER BY l.expires_at,l.lease_id LIMIT 2000 FOR UPDATE OF l
   ), initialized AS (
    UPDATE leases l SET project_id=i.project_id FROM examined e JOIN issues i
     ON i.organization_id=selected_organization AND i.id=e.issue_id
    WHERE l.organization_id=selected_organization AND l.lease_id=e.lease_id AND l.project_id IS NULL
    RETURNING l.lease_id
   )
   SELECT e.expires_at,e.lease_id INTO next_expiry,next_lease FROM examined e
    LEFT JOIN initialized changed ON changed.lease_id=e.lease_id
    ORDER BY e.expires_at DESC,e.lease_id DESC LIMIT 1;
   IF NOT FOUND THEN
    COMMIT;
    EXIT;
   END IF;
   after_expiry := next_expiry;
   after_lease := next_lease;
   COMMIT;
  END LOOP;
  after_lease := '';
  LOOP
   PERFORM set_config('detent.organization_id', selected_organization, true);
   WITH examined AS MATERIALIZED (
    SELECT a.lease_id,a.runner_id,l.runner_id AS recorded_runner_id FROM runner_lease_assignments a
    JOIN leases l ON l.organization_id=a.organization_id AND l.lease_id=a.lease_id
    WHERE a.organization_id=selected_organization AND a.lease_id>after_lease
    ORDER BY a.lease_id LIMIT 2000 FOR UPDATE OF l
   ), initialized AS (
    UPDATE leases l SET runner_id=e.runner_id FROM examined e
    WHERE l.organization_id=selected_organization AND l.lease_id=e.lease_id AND l.runner_id IS NULL
    RETURNING l.lease_id,l.runner_id
   )
   SELECT e.lease_id,(
    SELECT count(*) FROM examined source LEFT JOIN initialized changed ON changed.lease_id=source.lease_id
    WHERE COALESCE(changed.runner_id,source.recorded_runner_id) IS DISTINCT FROM source.runner_id
   ) INTO next_lease,mismatches FROM examined e
    LEFT JOIN initialized changed ON changed.lease_id=e.lease_id
    ORDER BY e.lease_id DESC LIMIT 1;
   IF NOT FOUND THEN
    COMMIT;
    EXIT;
   END IF;
   IF mismatches<>0 THEN
    RAISE EXCEPTION 'lease runner initialization differs from assignment';
   END IF;
   after_lease := next_lease;
   COMMIT;
  END LOOP;
 END LOOP;
END;
$$;
-- +goose StatementEnd
DROP POLICY lease_binding_initialization_orgs ON organizations;
RESET lock_timeout;

-- +goose Down
SELECT 1;
