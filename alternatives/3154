-- +goose Up
CREATE POLICY designated_local_organization_repair ON public.organizations TO detent_migration USING (true) WITH CHECK (true);
UPDATE public.organizations SET local = false WHERE local AND (SELECT count(*) FROM public.organizations WHERE local) > 1;
DROP POLICY designated_local_organization_repair ON public.organizations;
CREATE UNIQUE INDEX organizations_single_local ON public.organizations (local) WHERE local;
CREATE POLICY designated_local_organization_read ON public.organizations
    FOR SELECT TO detent_migration USING (true);

-- +goose StatementBegin
CREATE FUNCTION public.designated_local_organization() RETURNS text
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    designated text;
BEGIN
    SELECT organization_id INTO STRICT designated FROM public.organizations WHERE local;
    RETURN designated;
EXCEPTION
    WHEN no_data_found THEN
        IF EXISTS (SELECT 1 FROM public.organizations) THEN
            RAISE EXCEPTION USING ERRCODE = '55000', MESSAGE = 'populated PostgreSQL database has no designated local organization';
        END IF;
        RETURN NULL;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION public.designated_local_organization() OWNER TO detent_migration;
REVOKE ALL ON FUNCTION public.designated_local_organization() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.designated_local_organization() TO detent_runtime;

-- +goose Down
DROP FUNCTION public.designated_local_organization();
DROP POLICY designated_local_organization_read ON public.organizations;
DROP INDEX public.organizations_single_local;
