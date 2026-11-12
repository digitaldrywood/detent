-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
-- +goose StatementBegin
DO $$
DECLARE
 selected_organization text;
 after_change text;
 after_item text;
 next_change text;
 next_item text;
 mismatches bigint;
BEGIN
 FOR selected_organization IN SELECT organization_id FROM organizations ORDER BY organization_id LOOP
  after_change := '';
  after_item := '';
  LOOP
   PERFORM set_config('detent.organization_id', selected_organization, true);
   WITH examined AS MATERIALIZED (
    SELECT l.change_id,l.work_item_id,l.retained_pr_version_id
    FROM change_issue_links l
    WHERE l.organization_id=selected_organization AND (l.change_id,l.work_item_id)>(after_change,after_item)
    ORDER BY l.change_id,l.work_item_id LIMIT 2000 FOR UPDATE OF l
   ), expected AS MATERIALIZED (
    SELECT e.change_id,e.work_item_id,e.retained_pr_version_id,v.id AS expected_version_id
    FROM examined e LEFT JOIN LATERAL (
     SELECT v.id FROM change_versions v
     WHERE v.organization_id=selected_organization AND v.change_id=e.change_id AND v.external_provider='github' AND v.external_url<>''
     ORDER BY v.number DESC LIMIT 1
    ) v ON true
   ), initialized AS (
    UPDATE change_issue_links l SET retained_pr_version_id=e.expected_version_id
    FROM expected e
    WHERE l.organization_id=selected_organization AND l.change_id=e.change_id AND l.work_item_id=e.work_item_id
     AND l.retained_pr_version_id IS NULL AND e.expected_version_id IS NOT NULL
    RETURNING l.change_id,l.work_item_id,l.retained_pr_version_id
   )
   SELECT e.change_id,e.work_item_id,(
    SELECT count(*) FROM expected source LEFT JOIN initialized changed
     ON changed.change_id=source.change_id AND changed.work_item_id=source.work_item_id
    WHERE COALESCE(changed.retained_pr_version_id,source.retained_pr_version_id) IS DISTINCT FROM source.expected_version_id
   ) INTO next_change,next_item,mismatches
   FROM examined e LEFT JOIN initialized changed ON changed.change_id=e.change_id AND changed.work_item_id=e.work_item_id
   ORDER BY e.change_id DESC,e.work_item_id DESC LIMIT 1;
   IF NOT FOUND THEN
    COMMIT;
    EXIT;
   END IF;
   IF mismatches<>0 THEN
    RAISE EXCEPTION 'retained PR initialization differs from immutable source';
   END IF;
   after_change := next_change;
   after_item := next_item;
   COMMIT;
  END LOOP;
 END LOOP;
END;
$$;
-- +goose StatementEnd
RESET lock_timeout;

-- +goose Down
SELECT 1;
