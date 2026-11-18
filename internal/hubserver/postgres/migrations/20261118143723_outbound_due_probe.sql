-- +goose Up
-- +goose StatementBegin
CREATE FUNCTION public.github_outbound_due(org text, at_time timestamptz) RETURNS boolean
LANGUAGE plpgsql SECURITY INVOKER SET search_path = pg_catalog AS $$
DECLARE
    previous_scope text := current_setting('detent.organization_id',true);
    due boolean;
BEGIN
    PERFORM set_config('detent.organization_id',org,true);
    SELECT EXISTS (
        SELECT 1 FROM public.github_outbound o
        WHERE o.organization_id=org AND o.status IN ('pending','retrying','processing') AND o.next_attempt_at<=at_time
          AND (o.coalesce_key='' OR NOT EXISTS (
              SELECT 1 FROM public.github_outbound p WHERE p.organization_id=o.organization_id
              AND p.project_id=o.project_id AND p.coalesce_key=o.coalesce_key AND p.id<o.id
              AND p.status IN ('pending','retrying','processing')
          ))
    ) INTO due;
    PERFORM set_config('detent.organization_id',coalesce(previous_scope,''),true);
    RETURN due;
END
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION public.github_outbound_due(text,timestamptz) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.github_outbound_due(text,timestamptz) TO detent_runtime_v2;

-- +goose Down
DROP FUNCTION public.github_outbound_due(text,timestamptz);
