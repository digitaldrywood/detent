-- +goose Up
SET LOCAL lock_timeout = '5s';
CREATE POLICY audit_prune_read ON hosted_audit FOR SELECT TO detent_migration
 USING (current_user='detent_migration' AND (recorded_at_ns<(extract(epoch FROM (transaction_timestamp()-interval '720 hours'))*1000000000)::bigint OR (event='action' AND status<400 AND actual_actor=effective_user AND reason='' AND route LIKE 'GET %' AND mutation_object_key IS NULL)));
CREATE POLICY audit_prune_lock ON hosted_audit FOR UPDATE TO detent_migration
 USING (current_user='detent_migration' AND (recorded_at_ns<(extract(epoch FROM (transaction_timestamp()-interval '720 hours'))*1000000000)::bigint OR (event='action' AND status<400 AND actual_actor=effective_user AND reason='' AND route LIKE 'GET %' AND mutation_object_key IS NULL))) WITH CHECK (false);
CREATE POLICY audit_prune_delete ON hosted_audit FOR DELETE TO detent_migration
 USING (current_user='detent_migration' AND (recorded_at_ns<(extract(epoch FROM (transaction_timestamp()-interval '720 hours'))*1000000000)::bigint OR (event='action' AND status<400 AND actual_actor=effective_user AND reason='' AND route LIKE 'GET %' AND mutation_object_key IS NULL)));
CREATE POLICY audit_prune_read ON hosted_audit_times FOR SELECT TO detent_migration
 USING (current_user='detent_migration' AND EXISTS (SELECT 1 FROM public.hosted_audit audit
 WHERE audit.organization_id=hosted_audit_times.organization_id AND audit.id=hosted_audit_times.audit_id
 AND (audit.recorded_at_ns<(extract(epoch FROM (transaction_timestamp()-interval '720 hours'))*1000000000)::bigint OR (audit.event='action' AND audit.status<400 AND audit.actual_actor=audit.effective_user AND audit.reason='' AND audit.route LIKE 'GET %' AND audit.mutation_object_key IS NULL))));
CREATE POLICY audit_prune_delete ON hosted_audit_times FOR DELETE TO detent_migration
 USING (current_user='detent_migration' AND EXISTS (SELECT 1 FROM public.hosted_audit audit
 WHERE audit.organization_id=hosted_audit_times.organization_id AND audit.id=hosted_audit_times.audit_id
 AND (audit.recorded_at_ns<(extract(epoch FROM (transaction_timestamp()-interval '720 hours'))*1000000000)::bigint OR (audit.event='action' AND audit.status<400 AND audit.actual_actor=audit.effective_user AND audit.reason='' AND audit.route LIKE 'GET %' AND audit.mutation_object_key IS NULL))));
-- +goose StatementBegin
CREATE FUNCTION public.prune_legacy_access_audits(batch_limit integer) RETURNS integer
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    victim_organizations text[];
    victim_ids bigint[];
    victim_tids tid[];
    pruned integer;
BEGIN
    IF batch_limit IS NULL OR batch_limit < 1 OR batch_limit > 5000 THEN
        RAISE EXCEPTION 'invalid audit prune batch' USING ERRCODE = '22023';
    END IF;
    SELECT array_agg(victims.organization_id),array_agg(victims.id),array_agg(victims.ctid)
    INTO victim_organizations,victim_ids,victim_tids
    FROM (
        WITH selected AS MATERIALIZED (
            SELECT organization_id,id FROM public.hosted_audit
            WHERE event='action' AND status<400 AND actual_actor=effective_user AND reason='' AND route LIKE 'GET %' AND mutation_object_key IS NULL
            ORDER BY organization_id,id LIMIT batch_limit
        ), ordered AS MATERIALIZED (
            SELECT organization_id,id FROM selected ORDER BY organization_id,id
        )
        SELECT locked.organization_id,locked.id,locked.ctid FROM ordered candidate
        CROSS JOIN LATERAL (
            SELECT audit.organization_id,audit.id,audit.ctid FROM public.hosted_audit audit
            WHERE audit.organization_id=candidate.organization_id AND audit.id=candidate.id
              AND audit.event='action' AND audit.status<400 AND audit.actual_actor=audit.effective_user AND audit.reason='' AND audit.route LIKE 'GET %' AND audit.mutation_object_key IS NULL
            OFFSET 0 FOR UPDATE OF audit
        ) locked
        ORDER BY candidate.organization_id,candidate.id
    ) victims;
    IF coalesce(cardinality(victim_ids),0)=0 THEN
        RETURN 0;
    END IF;
    DELETE FROM public.hosted_audit_times WHERE ctid=ANY(ARRAY(
        SELECT target.ctid FROM unnest(victim_organizations,victim_ids) victim(organization_id,id)
        CROSS JOIN LATERAL (
            SELECT paired.ctid FROM public.hosted_audit_times paired
            WHERE paired.organization_id=victim.organization_id AND paired.audit_id=victim.id OFFSET 0
        ) target
    ));
    DELETE FROM public.hosted_audit WHERE ctid=ANY(victim_tids);
    GET DIAGNOSTICS pruned = ROW_COUNT;
    IF pruned<>cardinality(victim_ids) THEN
        RAISE EXCEPTION 'audit prune changed after selection' USING ERRCODE = '40001';
    END IF;
    RETURN pruned;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION public.prune_legacy_access_audits(integer) OWNER TO detent_migration;
REVOKE ALL ON FUNCTION public.prune_legacy_access_audits(integer) FROM PUBLIC,detent_platform;
GRANT EXECUTE ON FUNCTION public.prune_legacy_access_audits(integer) TO detent_runtime;
-- +goose StatementBegin
CREATE FUNCTION public.prune_expired_access_audits(cutoff_ns bigint, batch_limit integer) RETURNS integer
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    victim_organizations text[];
    victim_ids bigint[];
    victim_tids tid[];
    pruned integer;
    effective_cutoff_ns bigint;
BEGIN
    IF batch_limit IS NULL OR batch_limit < 1 OR batch_limit > 5000 THEN
        RAISE EXCEPTION 'invalid audit prune batch' USING ERRCODE = '22023';
    END IF;
    IF cutoff_ns IS NULL OR cutoff_ns <= 0 THEN
        RAISE EXCEPTION 'invalid audit retention cutoff' USING ERRCODE = '22023';
    END IF;
    effective_cutoff_ns := least(cutoff_ns,(extract(epoch FROM (transaction_timestamp()-interval '720 hours'))*1000000000)::bigint);
    SELECT array_agg(victims.organization_id),array_agg(victims.id),array_agg(victims.ctid)
    INTO victim_organizations,victim_ids,victim_tids
    FROM (
        WITH selected AS MATERIALIZED (
            SELECT organization_id,id FROM public.hosted_audit
            WHERE recorded_at_ns<effective_cutoff_ns
            ORDER BY recorded_at_ns,organization_id,id LIMIT batch_limit
        ), ordered AS MATERIALIZED (
            SELECT organization_id,id FROM selected ORDER BY organization_id,id
        )
        SELECT locked.organization_id,locked.id,locked.ctid FROM ordered candidate
        CROSS JOIN LATERAL (
            SELECT audit.organization_id,audit.id,audit.ctid FROM public.hosted_audit audit
            WHERE audit.organization_id=candidate.organization_id AND audit.id=candidate.id
              AND audit.recorded_at_ns<effective_cutoff_ns
            OFFSET 0 FOR UPDATE OF audit
        ) locked
        ORDER BY candidate.organization_id,candidate.id
    ) victims;
    IF coalesce(cardinality(victim_ids),0)=0 THEN
        RETURN 0;
    END IF;
    DELETE FROM public.hosted_audit_times WHERE ctid=ANY(ARRAY(
        SELECT target.ctid FROM unnest(victim_organizations,victim_ids) victim(organization_id,id)
        CROSS JOIN LATERAL (
            SELECT paired.ctid FROM public.hosted_audit_times paired
            WHERE paired.organization_id=victim.organization_id AND paired.audit_id=victim.id OFFSET 0
        ) target
    ));
    DELETE FROM public.hosted_audit WHERE ctid=ANY(victim_tids);
    GET DIAGNOSTICS pruned = ROW_COUNT;
    IF pruned<>cardinality(victim_ids) THEN
        RAISE EXCEPTION 'audit prune changed after selection' USING ERRCODE = '40001';
    END IF;
    RETURN pruned;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION public.prune_expired_access_audits(bigint,integer) OWNER TO detent_migration;
REVOKE ALL ON FUNCTION public.prune_expired_access_audits(bigint,integer) FROM PUBLIC,detent_platform;
GRANT EXECUTE ON FUNCTION public.prune_expired_access_audits(bigint,integer) TO detent_runtime;

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP FUNCTION public.prune_expired_access_audits(bigint,integer);
DROP FUNCTION public.prune_legacy_access_audits(integer);
DROP POLICY audit_prune_read ON hosted_audit_times;
DROP POLICY audit_prune_delete ON hosted_audit_times;
DROP POLICY audit_prune_read ON hosted_audit;
DROP POLICY audit_prune_lock ON hosted_audit;
DROP POLICY audit_prune_delete ON hosted_audit;
