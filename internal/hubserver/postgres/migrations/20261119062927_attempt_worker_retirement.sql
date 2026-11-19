-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
ALTER TABLE native_attempts ADD COLUMN IF NOT EXISTS worker_retired_at timestamptz,
    DROP CONSTRAINT IF EXISTS native_attempts_worker_retirement_check,
    ADD CONSTRAINT native_attempts_worker_retirement_check CHECK (worker_retired_at IS NULL OR worker_pid IS NOT NULL) NOT VALID;
ALTER TABLE native_attempts VALIDATE CONSTRAINT native_attempts_worker_retirement_check;
-- +goose StatementBegin
DO $$
DECLARE
    tenants text[];
    tenant text;
    after_tenant text := '';
    after_attempt text;
BEGIN
    LOOP
        CREATE POLICY worker_receipt_organizations ON organizations FOR SELECT TO detent_migration_v2 USING (true);
        tenants := ARRAY(SELECT organization_id FROM organizations WHERE organization_id > after_tenant ORDER BY organization_id LIMIT 256);
        DROP POLICY worker_receipt_organizations ON organizations;
        COMMIT;
        EXIT WHEN cardinality(tenants) = 0;
        FOREACH tenant IN ARRAY tenants LOOP
            after_attempt := '';
            LOOP
                PERFORM set_config('detent.organization_id', tenant, true);
                WITH batch AS MATERIALIZED (
                    SELECT id FROM native_attempts WHERE organization_id = tenant AND id > after_attempt
                    AND worker_pid IS NOT NULL AND cleanup_acknowledged_at IS NOT NULL
                    ORDER BY id LIMIT 1000
                ), cleared AS (
                    UPDATE native_attempts a SET cleanup_acknowledged_at = NULL FROM batch
                    WHERE a.organization_id = tenant AND a.id = batch.id RETURNING a.id
                )
                SELECT max(id) INTO after_attempt FROM cleared;
                COMMIT;
                EXIT WHEN after_attempt IS NULL;
            END LOOP;
            after_tenant := tenant;
        END LOOP;
    END LOOP;
END $$;
-- +goose StatementEnd
DROP INDEX CONCURRENTLY IF EXISTS native_attempts_worker_inventory_idx;
CREATE INDEX CONCURRENTLY native_attempts_worker_inventory_idx ON native_attempts(organization_id,project_id,worker_runner_id,id)
WHERE worker_pid IS NOT NULL AND (worker_retired_at IS NULL OR (cleanup_acknowledged_at IS NULL AND status IN ('running','interrupted','cancelled')));
DROP INDEX CONCURRENTLY IF EXISTS native_attempts_pending_cleanup_idx;
CREATE INDEX CONCURRENTLY native_attempts_pending_cleanup_idx ON native_attempts(organization_id,project_id,id)
WHERE worker_pid IS NULL AND cleanup_acknowledged_at IS NULL AND status IN ('running','interrupted','cancelled') AND (worker_cleanup_required IS DISTINCT FROM FALSE OR runtime_summary_sequence IS DISTINCT FROM sequence);
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS native_attempts_pending_cleanup_idx;
CREATE INDEX CONCURRENTLY native_attempts_pending_cleanup_idx ON native_attempts(organization_id,project_id,id)
WHERE cleanup_acknowledged_at IS NULL AND status IN ('running','interrupted','cancelled') AND (worker_cleanup_required IS DISTINCT FROM FALSE OR runtime_summary_sequence IS DISTINCT FROM sequence);
DROP INDEX CONCURRENTLY IF EXISTS native_attempts_worker_inventory_idx;
CREATE INDEX CONCURRENTLY native_attempts_worker_inventory_idx ON native_attempts(organization_id,project_id,worker_runner_id,id)
WHERE worker_pid IS NOT NULL AND cleanup_acknowledged_at IS NULL;
-- +goose StatementBegin
DO $$
DECLARE
    tenants text[];
    tenant text;
    after_tenant text := '';
    after_attempt text;
BEGIN
    IF NOT EXISTS (SELECT FROM pg_attribute WHERE attrelid='native_attempts'::regclass AND attname='worker_retired_at' AND NOT attisdropped) THEN
        RETURN;
    END IF;
    LOOP
        CREATE POLICY worker_receipt_organizations ON organizations FOR SELECT TO detent_migration_v2 USING (true);
        tenants := ARRAY(SELECT organization_id FROM organizations WHERE organization_id > after_tenant ORDER BY organization_id LIMIT 256);
        DROP POLICY worker_receipt_organizations ON organizations;
        COMMIT;
        EXIT WHEN cardinality(tenants) = 0;
        FOREACH tenant IN ARRAY tenants LOOP
            after_attempt := '';
            LOOP
                PERFORM set_config('detent.organization_id', tenant, true);
                WITH batch AS MATERIALIZED (
                    SELECT id FROM native_attempts WHERE organization_id = tenant AND id > after_attempt
                    AND worker_pid IS NOT NULL AND cleanup_acknowledged_at IS NOT NULL AND worker_retired_at IS NULL
                    ORDER BY id LIMIT 1000
                ), cleared AS (
                    UPDATE native_attempts a SET cleanup_acknowledged_at = NULL FROM batch
                    WHERE a.organization_id = tenant AND a.id = batch.id RETURNING a.id
                )
                SELECT max(id) INTO after_attempt FROM cleared;
                COMMIT;
                EXIT WHEN after_attempt IS NULL;
            END LOOP;
            after_tenant := tenant;
        END LOOP;
    END LOOP;
END $$;
-- +goose StatementEnd
ALTER TABLE native_attempts DROP COLUMN IF EXISTS worker_retired_at;
RESET lock_timeout;
