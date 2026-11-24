-- +goose Up
-- +goose StatementBegin
CREATE FUNCTION public.hosted_write_authorized(expected_generation bigint) RETURNS boolean
LANGUAGE sql VOLATILE SECURITY DEFINER SET search_path = pg_catalog AS $$
    SELECT EXISTS (
        SELECT 1 FROM platform.organizations
        WHERE id = current_setting('detent.organization_id', true)
          AND generation = expected_generation
          AND (state = 'ready' OR (managed AND state = 'allocating'))
        FOR SHARE
    )
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION public.hosted_write_authorized(bigint) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.hosted_write_authorized(bigint) TO detent_runtime_v2;

-- +goose Down
DROP FUNCTION public.hosted_write_authorized(bigint);
