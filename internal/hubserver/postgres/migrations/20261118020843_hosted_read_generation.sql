-- +goose Up
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.hosted_write_authorized(expected_generation bigint) RETURNS boolean
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog AS $$
BEGIN
    IF current_setting('transaction_read_only')::boolean THEN
        RETURN EXISTS (
            SELECT 1 FROM platform.organizations o
            JOIN public.hosted_tenant h ON h.organization_id=o.id AND h.provider_id=o.provider_id
                AND h.deployment='shared' AND h.allocation_generation=o.generation
            WHERE o.id = current_setting('detent.organization_id', true)
              AND o.generation = expected_generation
              AND (o.state = 'ready' OR (o.managed AND o.state = 'allocating'))
        );
    END IF;
    RETURN EXISTS (
        SELECT 1 FROM platform.organizations
        WHERE id = current_setting('detent.organization_id', true)
          AND generation = expected_generation
          AND (state = 'ready' OR (managed AND state = 'allocating'))
        FOR SHARE
    );
END;
$$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.hosted_write_authorized(expected_generation bigint) RETURNS boolean
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
