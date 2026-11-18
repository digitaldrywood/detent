-- +goose Up
ALTER TABLE leases DROP CONSTRAINT leases_work_item_revision_check;
ALTER TABLE leases ADD CONSTRAINT leases_work_item_revision_check CHECK (work_item_revision >= 0);
ALTER TABLE native_attempts DROP CONSTRAINT native_attempts_work_item_revision_check;
ALTER TABLE native_attempts ADD CONSTRAINT native_attempts_work_item_revision_check CHECK (work_item_revision >= 0);

-- +goose StatementBegin
CREATE FUNCTION allocate_imported_lease_fencing_token(imported_token bigint) RETURNS bigint
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    target regclass := 'public.leases_fencing_token_seq'::regclass;
BEGIN
    IF imported_token < 1 THEN
        RAISE EXCEPTION 'invalid imported fencing token' USING ERRCODE = '22023';
    END IF;
    PERFORM pg_advisory_xact_lock(target::oid::bigint);
    PERFORM setval(target, GREATEST(imported_token, nextval(target)));
    RETURN imported_token;
END;
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION allocate_imported_lease_fencing_token(bigint) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION allocate_imported_lease_fencing_token(bigint) TO detent_runtime, detent_platform;

-- +goose Down
CREATE TEMP TABLE imported_execution_rollback_guard(value INTEGER CHECK(value = 0));
INSERT INTO imported_execution_rollback_guard
SELECT CASE WHEN EXISTS (SELECT 1 FROM leases WHERE work_item_revision = 0)
    OR EXISTS (SELECT 1 FROM native_attempts WHERE work_item_revision = 0) THEN 1 ELSE 0 END;
DROP FUNCTION allocate_imported_lease_fencing_token(bigint);
ALTER TABLE native_attempts DROP CONSTRAINT native_attempts_work_item_revision_check;
ALTER TABLE native_attempts ADD CONSTRAINT native_attempts_work_item_revision_check CHECK (work_item_revision > 0);
ALTER TABLE leases DROP CONSTRAINT leases_work_item_revision_check;
ALTER TABLE leases ADD CONSTRAINT leases_work_item_revision_check CHECK (work_item_revision > 0);
