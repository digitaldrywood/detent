-- +goose Up
SET LOCAL lock_timeout = '5s';
GRANT SELECT (name) ON public.organizations TO detent_local_bootstrap_reader_v2;
GRANT SELECT (organization_id,id,token_hash,scope,native_only,created_at,revoked_at,expires_at,hosted_user_id,operator_key_scope) ON public.api_tokens TO detent_local_bootstrap_reader_v2;
GRANT SELECT (organization_id,token_id) ON public.runner_identities TO detent_local_bootstrap_reader_v2;
CREATE POLICY operator_directory_credential_read ON public.api_tokens FOR SELECT TO detent_local_bootstrap_reader_v2
 USING (current_user='detent_local_bootstrap_reader_v2' AND organization_id=NULLIF(current_setting('detent.organization_id',true),''));
CREATE POLICY operator_directory_runner_read ON public.runner_identities FOR SELECT TO detent_local_bootstrap_reader_v2
 USING (current_user='detent_local_bootstrap_reader_v2' AND organization_id=NULLIF(current_setting('detent.organization_id',true),''));

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
    RAISE EXCEPTION 'operator organization directory authority cannot be rolled back';
END;
$$;
-- +goose StatementEnd
