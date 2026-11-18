-- +goose Up
-- +goose StatementBegin
CREATE FUNCTION public.hosted_administrator_access(organization text, member_user text, member_principal text, effective_role text) RETURNS boolean
LANGUAGE sql STABLE SET search_path=pg_catalog,public AS $access$
SELECT coalesce(effective_role IN ('owner','admin'),false) AND public.hosted_administrator_access(organization, member_user, member_principal);
$access$;
-- +goose StatementEnd
ALTER FUNCTION public.hosted_administrator_access(text,text,text,text) OWNER TO detent_migration;
REVOKE ALL ON FUNCTION public.hosted_administrator_access(text,text,text,text) FROM PUBLIC,detent_platform;
GRANT EXECUTE ON FUNCTION public.hosted_administrator_access(text,text,text,text) TO detent_runtime;

-- +goose Down
DROP FUNCTION public.hosted_administrator_access(text,text,text,text);
