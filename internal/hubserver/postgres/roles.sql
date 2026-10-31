DO $roles$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'detent_migration') THEN
        CREATE ROLE detent_migration NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'detent_runtime') THEN
        CREATE ROLE detent_runtime NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'detent_platform') THEN
        CREATE ROLE detent_platform NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
    END IF;
    IF EXISTS (
        SELECT 1 FROM pg_roles
        WHERE rolname IN ('detent_migration', 'detent_runtime', 'detent_platform')
        AND (rolcanlogin OR rolsuper OR rolcreatedb OR rolcreaterole OR rolreplication OR rolbypassrls)
    ) THEN
        RAISE EXCEPTION 'Detent group roles must be unprivileged and unable to log in';
    END IF;
    IF pg_has_role('detent_runtime', 'detent_platform', 'MEMBER')
        OR pg_has_role('detent_runtime', 'detent_migration', 'MEMBER')
        OR pg_has_role('detent_platform', 'detent_migration', 'MEMBER')
        OR pg_has_role('detent_platform', 'detent_runtime', 'MEMBER')
        OR pg_has_role('detent_migration', 'detent_runtime', 'MEMBER')
        OR pg_has_role('detent_migration', 'detent_platform', 'MEMBER') THEN
        RAISE EXCEPTION 'Detent database roles must have separate authority';
    END IF;
END
$roles$;

REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT USAGE, CREATE ON SCHEMA public TO detent_migration;
GRANT USAGE ON SCHEMA public TO detent_runtime, detent_platform;
