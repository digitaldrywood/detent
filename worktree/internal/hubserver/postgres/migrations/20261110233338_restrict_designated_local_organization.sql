-- +goose Up
DROP POLICY designated_local_organization_read ON public.organizations;
GRANT SELECT (organization_id, local) ON public.organizations TO detent_local_bootstrap_reader_v2;
CREATE POLICY designated_local_organization_read ON public.organizations
    FOR SELECT TO detent_local_bootstrap_reader_v2
    USING (current_user = 'detent_local_bootstrap_reader_v2');
DROP FUNCTION public.designated_local_organization();

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
    RAISE EXCEPTION 'designated local organization isolation cannot be rolled back';
END
$$;
-- +goose StatementEnd
