-- +goose Up
-- +goose StatementBegin
CREATE FUNCTION allocate_core_record_id(target regclass, imported_id bigint) RETURNS bigint
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    IF target NOT IN ('public.issues_id_seq'::regclass, 'public.native_comments_record_id_seq'::regclass) THEN
        RAISE EXCEPTION 'unsupported record sequence' USING ERRCODE = '42501';
    END IF;
    IF imported_id = 0 THEN
        PERFORM pg_advisory_xact_lock_shared(target::oid::bigint);
        RETURN nextval(target);
    END IF;
    PERFORM pg_advisory_xact_lock(target::oid::bigint);
    PERFORM setval(target, GREATEST(imported_id, nextval(target)));
    RETURN imported_id;
END;
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION allocate_core_record_id(regclass, bigint) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION allocate_core_record_id(regclass, bigint) TO detent_runtime, detent_platform;

-- +goose Down
DROP FUNCTION allocate_core_record_id(regclass, bigint);
