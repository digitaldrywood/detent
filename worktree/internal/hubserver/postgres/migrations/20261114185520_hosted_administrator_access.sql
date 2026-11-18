-- +goose Up
-- +goose StatementBegin
CREATE FUNCTION public.hosted_administrator_access(organization text, member_user text, member_principal text) RETURNS boolean
LANGUAGE sql STABLE SET search_path=pg_catalog,public AS $access$
SELECT member_user <> '' AND EXISTS (
 SELECT 1 FROM public.hosted_members administrator
 JOIN public.api_tokens administrator_principal ON administrator_principal.organization_id=administrator.organization_id AND administrator_principal.id=administrator.principal_id
 WHERE administrator.organization_id=organization AND administrator.user_id=member_user
 AND administrator.active AND administrator.role IN ('owner','admin')
 AND administrator_principal.revoked_at IS NULL
 AND (member_principal='' OR administrator.principal_id=member_principal)
);
$access$;
-- +goose StatementEnd
ALTER FUNCTION public.hosted_administrator_access(text,text,text) OWNER TO detent_migration;
REVOKE ALL ON FUNCTION public.hosted_administrator_access(text,text,text) FROM PUBLIC,detent_platform;
GRANT EXECUTE ON FUNCTION public.hosted_administrator_access(text,text,text) TO detent_runtime;

ALTER TABLE hosted_project_grants ADD COLUMN explicit boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE hosted_project_grants DROP COLUMN explicit;
DROP FUNCTION public.hosted_administrator_access(text,text,text);
