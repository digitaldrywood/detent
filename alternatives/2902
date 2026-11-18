-- +goose Up
CREATE FUNCTION request_histogram_add(a bigint[], b bigint[]) RETURNS bigint[]
LANGUAGE SQL IMMUTABLE STRICT PARALLEL SAFE
RETURN ARRAY(SELECT a[i] + b[i] FROM generate_series(1,64) AS i ORDER BY i);

CREATE TABLE request_metric_windows (
 organization_id text NOT NULL CHECK (btrim(organization_id) <> ''),
 minute timestamptz NOT NULL,
 caller_count bigint NOT NULL DEFAULT 0 CHECK (caller_count BETWEEN 0 AND 200),
 PRIMARY KEY (organization_id,minute)
);
CREATE TABLE request_metric_callers (
 organization_id text NOT NULL,
 minute timestamptz NOT NULL,
 caller_kind text NOT NULL CHECK (caller_kind IN ('runner','browser','mcp','ci','anonymous','other')),
 caller_id text NOT NULL,
 PRIMARY KEY (organization_id,minute,caller_kind,caller_id),
 FOREIGN KEY (organization_id,minute) REFERENCES request_metric_windows(organization_id,minute) ON DELETE CASCADE
);
CREATE TABLE request_minutes (
 organization_id text NOT NULL CHECK (btrim(organization_id) <> ''),
 minute timestamptz NOT NULL,
 route text NOT NULL CHECK (route <> ''),
 caller_kind text NOT NULL CHECK (caller_kind IN ('runner','browser','mcp','ci','anonymous','other')),
 caller_id text NOT NULL,
 status_class bigint NOT NULL CHECK (status_class BETWEEN 0 AND 9),
 count bigint NOT NULL CHECK (count > 0),
 sum_ms double precision NOT NULL CHECK (sum_ms >= 0 AND sum_ms < 'Infinity'::float8),
 max_ms double precision NOT NULL CHECK (max_ms >= 0 AND max_ms < 'Infinity'::float8),
 histogram bigint[] NOT NULL CHECK (coalesce(array_ndims(histogram)=1 AND array_lower(histogram,1)=1 AND array_length(histogram,1)=64 AND array_position(histogram,NULL) IS NULL AND 0 <= ALL(histogram),false)),
 PRIMARY KEY (organization_id,minute,route,caller_kind,caller_id,status_class)
);
CREATE TABLE request_hours (
 organization_id text NOT NULL CHECK (btrim(organization_id) <> ''),
 hour timestamptz NOT NULL,
 route text NOT NULL CHECK (route <> ''),
 status_class bigint NOT NULL CHECK (status_class BETWEEN 0 AND 9),
 count bigint NOT NULL CHECK (count > 0),
 sum_ms double precision NOT NULL CHECK (sum_ms >= 0 AND sum_ms < 'Infinity'::float8),
 max_ms double precision NOT NULL CHECK (max_ms >= 0 AND max_ms < 'Infinity'::float8),
 histogram bigint[] NOT NULL CHECK (coalesce(array_ndims(histogram)=1 AND array_lower(histogram,1)=1 AND array_length(histogram,1)=64 AND array_position(histogram,NULL) IS NULL AND 0 <= ALL(histogram),false)),
 PRIMARY KEY (organization_id,hour,route,status_class)
);
CREATE TABLE request_days (
 organization_id text NOT NULL CHECK (btrim(organization_id) <> ''),
 day timestamptz NOT NULL,
 route text NOT NULL CHECK (route <> ''),
 caller_kind text NOT NULL CHECK (caller_kind IN ('runner','browser','mcp','ci','anonymous','other')),
 status_class bigint NOT NULL CHECK (status_class BETWEEN 0 AND 9),
 count bigint NOT NULL CHECK (count > 0),
 sum_ms double precision NOT NULL CHECK (sum_ms >= 0 AND sum_ms < 'Infinity'::float8),
 max_ms double precision NOT NULL CHECK (max_ms >= 0 AND max_ms < 'Infinity'::float8),
 PRIMARY KEY (organization_id,day,route,caller_kind,status_class)
);
ALTER TABLE request_metric_windows ENABLE ROW LEVEL SECURITY;
ALTER TABLE request_metric_windows FORCE ROW LEVEL SECURITY;
REVOKE ALL ON request_metric_windows FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON request_metric_windows TO detent_runtime,detent_platform;
CREATE POLICY tenant_isolation ON request_metric_windows TO detent_runtime,detent_migration
 USING (organization_id = NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id',true),''));
CREATE POLICY platform_access ON request_metric_windows TO detent_platform USING (true) WITH CHECK (true);
ALTER TABLE request_metric_callers ENABLE ROW LEVEL SECURITY;
ALTER TABLE request_metric_callers FORCE ROW LEVEL SECURITY;
REVOKE ALL ON request_metric_callers FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON request_metric_callers TO detent_runtime,detent_platform;
CREATE POLICY tenant_isolation ON request_metric_callers TO detent_runtime,detent_migration
 USING (organization_id = NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id',true),''));
CREATE POLICY platform_access ON request_metric_callers TO detent_platform USING (true) WITH CHECK (true);
ALTER TABLE request_minutes ENABLE ROW LEVEL SECURITY;
ALTER TABLE request_minutes FORCE ROW LEVEL SECURITY;
REVOKE ALL ON request_minutes FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON request_minutes TO detent_runtime,detent_platform;
CREATE POLICY tenant_isolation ON request_minutes TO detent_runtime,detent_migration
 USING (organization_id = NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id',true),''));
CREATE POLICY platform_access ON request_minutes TO detent_platform USING (true) WITH CHECK (true);
ALTER TABLE request_hours ENABLE ROW LEVEL SECURITY;
ALTER TABLE request_hours FORCE ROW LEVEL SECURITY;
REVOKE ALL ON request_hours FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON request_hours TO detent_runtime,detent_platform;
CREATE POLICY tenant_isolation ON request_hours TO detent_runtime,detent_migration
 USING (organization_id = NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id',true),''));
CREATE POLICY platform_access ON request_hours TO detent_platform USING (true) WITH CHECK (true);
ALTER TABLE request_days ENABLE ROW LEVEL SECURITY;
ALTER TABLE request_days FORCE ROW LEVEL SECURITY;
REVOKE ALL ON request_days FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON request_days TO detent_runtime,detent_platform;
CREATE POLICY tenant_isolation ON request_days TO detent_runtime,detent_migration
 USING (organization_id = NULLIF(current_setting('detent.organization_id',true),''))
 WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id',true),''));
CREATE POLICY platform_access ON request_days TO detent_platform USING (true) WITH CHECK (true);

-- +goose Down
DROP TABLE request_days;
DROP TABLE request_hours;
DROP TABLE request_minutes;
DROP TABLE request_metric_callers;
DROP TABLE request_metric_windows;
DROP FUNCTION request_histogram_add(bigint[],bigint[]);
