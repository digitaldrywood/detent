-- +goose Up
-- +goose StatementBegin
CREATE FUNCTION public.bind_maintenance_scope(org text) RETURNS text
LANGUAGE plpgsql SECURITY INVOKER SET search_path = pg_catalog AS $$
DECLARE
    previous_scope text := current_setting('detent.organization_id',true);
BEGIN
    IF org IS NULL OR org='' OR (nullif(previous_scope,'') IS NOT NULL AND previous_scope<>org) THEN
        RAISE EXCEPTION 'maintenance tenant mismatch' USING ERRCODE='42501';
    END IF;
    PERFORM set_config('detent.organization_id',org,true);
    RETURN previous_scope;
END
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION public.bind_maintenance_scope(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.bind_maintenance_scope(text) TO detent_runtime_v2;
-- +goose StatementBegin
CREATE FUNCTION public.github_outbound_deadline(org text) RETURNS timestamptz
LANGUAGE plpgsql SECURITY INVOKER SET search_path = pg_catalog AS $$
DECLARE
    previous_scope text := public.bind_maintenance_scope(org);
    deadline timestamptz;
BEGIN
    SELECT o.next_attempt_at INTO deadline FROM public.github_outbound o
    WHERE o.organization_id=org AND o.status IN ('pending','retrying','processing')
      AND (o.coalesce_key='' OR NOT EXISTS (
          SELECT FROM public.github_outbound p WHERE p.organization_id=o.organization_id
          AND p.project_id=o.project_id AND p.coalesce_key=o.coalesce_key AND p.id<o.id
          AND p.status IN ('pending','retrying','processing')
      )) ORDER BY o.next_attempt_at,o.id LIMIT 1;
    PERFORM set_config('detent.organization_id',coalesce(previous_scope,''),true);
    RETURN deadline;
END
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION public.github_outbound_deadline(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.github_outbound_deadline(text) TO detent_runtime_v2;
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.github_outbound_due(org text, at_time timestamptz) RETURNS boolean
LANGUAGE sql SECURITY INVOKER SET search_path = pg_catalog AS $$
    SELECT coalesce(public.github_outbound_deadline(org)<=at_time,false)
$$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE FUNCTION public.claim_integration_page(org text) RETURNS TABLE(claimed_id text,claimed_generation bigint,claimed_at timestamptz)
LANGUAGE plpgsql SECURITY INVOKER SET search_path = pg_catalog,public,pg_temp AS $$
DECLARE
    previous_scope text := public.bind_maintenance_scope(org);
    prior text;
    candidate text;
BEGIN
    SELECT after_project_id INTO prior FROM project_integration_page_progress WHERE organization_id=org;
    SELECT id INTO candidate FROM projects WHERE organization_id=org AND id>coalesce(prior,'') ORDER BY id LIMIT 1;
    IF candidate IS NULL THEN
        SELECT id INTO candidate FROM projects WHERE organization_id=org ORDER BY id LIMIT 1;
    END IF;
    IF candidate IS NOT NULL AND candidate IS DISTINCT FROM prior THEN
        INSERT INTO project_integration_page_progress(organization_id,after_project_id) VALUES(org,candidate)
        ON CONFLICT(organization_id) DO UPDATE SET after_project_id=EXCLUDED.after_project_id
        WHERE project_integration_page_progress.after_project_id IS NOT DISTINCT FROM prior;
    END IF;
    IF candidate IS NOT NULL AND NOT EXISTS (SELECT FROM project_integration_pages WHERE organization_id=org AND project_id=candidate) THEN
        INSERT INTO project_integration_page_dirty(organization_id,project_id)
        SELECT organization_id,id FROM projects WHERE organization_id=org AND id=candidate AND deleted_at IS NULL
        ON CONFLICT(organization_id,project_id) DO NOTHING;
    END IF;
    IF EXISTS (SELECT FROM project_integration_page_dirty WHERE organization_id=org) THEN
        RETURN QUERY UPDATE project_integration_page_dirty d SET attempted_at=clock_timestamp()
        WHERE (d.organization_id,d.project_id)=(SELECT q.organization_id,q.project_id FROM project_integration_page_dirty q
            WHERE q.organization_id=org ORDER BY q.attempted_at,q.project_id LIMIT 1 FOR UPDATE SKIP LOCKED)
        RETURNING d.project_id,d.generation,d.source_at;
    END IF;
    PERFORM set_config('detent.organization_id',coalesce(previous_scope,''),true);
END
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION public.claim_integration_page(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.claim_integration_page(text) TO detent_runtime_v2;
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.maintenance_family_due(org text, family text, at_time timestamptz, linked_intake boolean) RETURNS boolean
LANGUAGE plpgsql SECURITY INVOKER SET search_path = pg_catalog, public, pg_temp AS $$
DECLARE
    previous_scope text := public.bind_maintenance_scope(org);
    due boolean;
BEGIN
    CASE family
    WHEN 'board-dirty' THEN
        SELECT EXISTS (SELECT FROM native_board_dirty WHERE organization_id=org)
            OR EXISTS (SELECT FROM native_board_items WHERE organization_id=org AND NOT card_display_ready)
            OR EXISTS (SELECT FROM native_board_items WHERE organization_id=org AND NOT page_ready)
            INTO due;
    WHEN 'work-item-pages' THEN
        SELECT EXISTS (SELECT FROM native_work_item_page_dirty WHERE organization_id=org)
            OR NOT EXISTS (SELECT FROM native_work_item_page_progress WHERE organization_id=org AND backfill_phase>=2 AND fingerprint_phase>=2)
            OR EXISTS (SELECT FROM issues WHERE organization_id=org AND source_revision<>revision)
            OR EXISTS (SELECT FROM native_comments WHERE organization_id=org AND source_revision<>revision)
            OR EXISTS (SELECT FROM collaboration_events WHERE organization_id=org AND payload_format_version=1 AND type IN ('run.started','run.checkpointed','run.observed','run.finished','scheduler.decision'))
            OR EXISTS (SELECT FROM change_versions WHERE organization_id=org AND payload_format_version=1)
            OR EXISTS (SELECT FROM change_evidence WHERE organization_id=org AND payload_format_version=1)
            OR EXISTS (SELECT FROM core_work_events WHERE organization_id=org AND payload_json IS NOT NULL) INTO due;
    WHEN 'health-pages' THEN
        SELECT NOT EXISTS (SELECT FROM health_detector_ticks WHERE organization_id=org AND last_tick_at>at_time-interval '1 minute')
            OR EXISTS (SELECT FROM health_findings WHERE organization_id=org AND page_checkpoint<page_generation)
            OR EXISTS (SELECT FROM health_slack_deliveries d JOIN slack_integrations s USING(organization_id) WHERE d.organization_id=org AND s.enabled AND d.completed_at IS NULL AND d.outcome<>'uncertain' AND d.next_attempt_at<=at_time
                AND (d.event='opened' OR NOT EXISTS (SELECT FROM health_slack_deliveries o WHERE o.organization_id=d.organization_id
                    AND o.finding_id=d.finding_id AND o.event='opened' AND o.completed_at IS NULL AND o.outcome<>'uncertain'))) INTO due;
    WHEN 'onboarding-pages' THEN
        WITH oldest AS MATERIALIZED (
            SELECT project_id,viewer_key,generation FROM project_onboarding_page_viewers WHERE organization_id=org
            ORDER BY attempted_at,project_id,viewer_key LIMIT 1
        ) SELECT EXISTS (
            SELECT FROM oldest v LEFT JOIN project_onboarding_pages p ON p.organization_id=org AND p.project_id=v.project_id AND p.viewer_key=v.viewer_key
            LEFT JOIN project_onboarding_page_access a ON a.organization_id=org
            WHERE p.source_generation IS DISTINCT FROM v.generation OR p.access_generation IS DISTINCT FROM a.generation
               OR p.projected_at<=at_time-interval '1 minute'
        ) INTO due;
    WHEN 'github-intake' THEN
        SELECT EXISTS (SELECT FROM onboarding_issue_intake i JOIN projects p ON p.organization_id=i.organization_id AND p.id=i.project_id
                       WHERE i.organization_id=org AND i.batch_status IN ('discovering','importing') AND i.next_attempt_at<=at_time AND p.deleted_at IS NULL) INTO due;
    WHEN 'github-reconciliation' THEN
        SELECT EXISTS (SELECT FROM github_pull_request_refresh WHERE organization_id=org AND source_kind='change' AND pending AND next_attempt_at<=at_time)
            OR EXISTS (SELECT FROM github_reconciliation r JOIN projects p ON p.organization_id=r.organization_id AND p.id=r.project_id
                JOIN project_settings s ON s.organization_id=p.organization_id AND s.project_id=p.id
                WHERE r.organization_id=org AND r.next_attempt_at<=at_time AND p.deleted_at IS NULL AND p.profile='native'
                  AND (s.checkout_repository<>'' OR s.repository<>'') AND (s.repository_enabled OR linked_intake))
            OR EXISTS (SELECT FROM project_settings s JOIN projects p ON p.organization_id=s.organization_id AND p.id=s.project_id
                WHERE s.organization_id=org AND NOT s.github_reconciliation_initialized AND p.deleted_at IS NULL AND p.profile='native'
                  AND (s.checkout_repository<>'' OR s.repository<>'') AND (s.repository_enabled OR linked_intake)) INTO due;
    WHEN 'github-webhooks' THEN
        SELECT EXISTS (SELECT FROM github_webhook_receipts WHERE organization_id=org AND status='pending' AND next_attempt_at<=at_time)
            OR EXISTS (SELECT FROM github_webhook_receipts WHERE organization_id=org AND status='completed' AND expires_at<=at_time AND payload_object_key<>'') INTO due;
    ELSE
        RAISE EXCEPTION 'unknown maintenance family' USING ERRCODE='22023';
    END CASE;
    PERFORM set_config('detent.organization_id',coalesce(previous_scope,''),true);
    RETURN due;
END
$$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.board_sweep_due(org text, after_id text, OUT next_id text, OUT due boolean) RETURNS record
LANGUAGE plpgsql SECURITY INVOKER SET search_path = pg_catalog, public, pg_temp AS $$
DECLARE
    previous_scope text := public.bind_maintenance_scope(org);
BEGIN
    IF EXISTS (SELECT FROM native_board_dirty WHERE organization_id=org)
       OR EXISTS (SELECT FROM native_board_items WHERE organization_id=org AND NOT card_display_ready)
       OR EXISTS (SELECT FROM native_board_items WHERE organization_id=org AND NOT page_ready) THEN
        next_id := after_id;
        due := true;
    ELSE
        SELECT candidate.id INTO next_id FROM (
            (SELECT native_id AS id,0 AS priority FROM issues WHERE organization_id=org AND native_id>after_id ORDER BY native_id LIMIT 1)
            UNION ALL (SELECT work_item_id,0 FROM native_board_items WHERE organization_id=org AND work_item_id>after_id ORDER BY work_item_id LIMIT 1)
            UNION ALL (SELECT native_id,1 FROM issues WHERE organization_id=org AND native_id<=after_id ORDER BY native_id LIMIT 1)
            UNION ALL (SELECT work_item_id,1 FROM native_board_items WHERE organization_id=org AND work_item_id<=after_id ORDER BY work_item_id LIMIT 1)
        ) candidate ORDER BY candidate.priority,candidate.id LIMIT 1;
        due := next_id IS NOT NULL AND NOT EXISTS (
            SELECT FROM issues i
            JOIN native_board_items b ON b.organization_id=i.organization_id AND b.project_id=i.project_id AND b.work_item_id=i.native_id
            JOIN projects p ON p.organization_id=i.organization_id AND p.id=i.project_id
            JOIN project_workflow_states w ON w.organization_id=p.organization_id AND w.project_id=p.id AND w.name=b.state
            WHERE i.organization_id=org AND i.native_id=next_id AND p.profile='native' AND p.deleted_at IS NULL
              AND b.source_revision=i.revision AND b.source_event_sequence=i.event_sequence AND b.source_project_revision=p.content_settings_revision
              AND b.card_display_ready AND b.page_ready AND b.list_ready AND b.page_refresh_after IS NULL AND b.expires_at IS NULL
              AND coalesce(b.card_attempt_id,'')='' AND coalesce(b.card_execution_id,'')=''
              AND (b.terminal,b.dispatchable)=(w.terminal,w.dispatchable)
              AND b.lane_rank=(SELECT count(*) FROM project_workflow_states preceding WHERE preceding.organization_id=org AND preceding.project_id=p.id AND preceding.board_rank<w.board_rank)
              AND NOT EXISTS (SELECT FROM native_board_items other WHERE other.organization_id=org AND other.work_item_id=next_id AND other.project_id<>i.project_id)
              AND NOT EXISTS (SELECT FROM issues other WHERE other.organization_id=org AND other.native_id=next_id AND other.project_id<>i.project_id)
              AND NOT EXISTS (SELECT FROM native_attempts a WHERE a.organization_id=org AND a.issue_id=i.id)
              AND NOT EXISTS (SELECT FROM leases l WHERE l.organization_id=org AND l.issue_id=i.id AND l.released_at IS NULL)
              AND NOT EXISTS (SELECT FROM change_issue_links l WHERE l.organization_id=org AND l.project_id=i.project_id
                  AND l.work_item_id=i.native_id AND l.change_id IS DISTINCT FROM b.card_change_id)
              AND (coalesce(b.card_change_id,'')='' OR EXISTS (
                  SELECT FROM change_issue_links l JOIN change_requests c ON (c.organization_id,c.project_id,c.id)=(l.organization_id,l.project_id,l.change_id)
                  WHERE l.organization_id=org AND l.project_id=i.project_id AND l.work_item_id=i.native_id AND l.change_id=b.card_change_id
                    AND c.work_item_id=i.native_id AND c.revision=1 AND c.current_version_id IS NULL AND c.evidence_sequence=0
                    AND coalesce(b.card_change_principal_id,'')=coalesce(i.actor_principal_id,'')
                    AND NOT EXISTS (SELECT FROM change_issue_links other WHERE other.organization_id=org AND other.change_id=c.id AND other.work_item_id<>i.native_id)
                    AND c.title=b.card_change_title AND b.card_change_status='draft' AND b.card_change_base_status='draft'
                    AND coalesce(b.card_change_version_id,'')='' AND coalesce(b.card_change_policy_id,'')='' AND coalesce(b.card_change_review_policy_id,'')=''
                    AND coalesce(b.card_change_external_provider,'')='' AND coalesce(b.card_change_external_id,'')='' AND coalesce(b.card_change_external_url,'')=''
              ))
        );
        next_id := coalesce(next_id,'');
    END IF;
    PERFORM set_config('detent.organization_id',coalesce(previous_scope,''),true);
END
$$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.usage_sweep_due(org text, after_source text, page_size integer, OUT next_id text, OUT due boolean) RETURNS record
LANGUAGE plpgsql SECURITY INVOKER SET search_path = pg_catalog, public, pg_temp AS $$
DECLARE
    previous_scope text := public.bind_maintenance_scope(org);
BEGIN
    IF page_size NOT BETWEEN 1 AND 100 THEN
        RAISE EXCEPTION 'invalid usage page size' USING ERRCODE='22023';
    END IF;
    IF EXISTS (SELECT FROM native_attempts WHERE organization_id=org AND usage_pending_sequence>0)
       OR EXISTS (SELECT FROM attempt_usage WHERE organization_id=org AND usage_origin='import' AND usage_input_total IS NULL) THEN
        next_id := after_source;
        due := true;
    ELSE
        WITH page AS MATERIALIZED (
            SELECT source_key,resource_id,project_id,revision,payload_hash FROM cost_sources
            WHERE organization_id=org AND bucket='runner_api' AND source_key>after_source ORDER BY source_key LIMIT page_size
        )
        SELECT coalesce(max(c.source_key),''),coalesce(bool_or(
            receipt.sha256 IS NULL OR receipt.sha256 IS DISTINCT FROM c.payload_hash OR
            (i.id IS NOT NULL AND (u.currency IS NULL OR u.usage_kind IS NULL OR (u.revision,u.attempt_id,u.project_id) IS DISTINCT FROM (c.revision,c.resource_id,c.project_id)))
        ),false) INTO next_id,due
        FROM page c
        LEFT JOIN cost_observation_revisions receipt ON receipt.organization_id=org AND receipt.source_key=c.source_key AND receipt.revision=c.revision
        LEFT JOIN native_attempts a ON a.organization_id=org AND a.id=c.resource_id
        LEFT JOIN issues i ON i.organization_id=org AND i.id=a.issue_id AND i.project_id=c.project_id
        LEFT JOIN report_usage_current u ON u.organization_id=org AND u.source_key=c.source_key;
    END IF;
    PERFORM set_config('detent.organization_id',coalesce(previous_scope,''),true);
END
$$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.project_sweep_due(org text, family text, after_id text, OUT next_id text, OUT due boolean) RETURNS record
LANGUAGE plpgsql SECURITY INVOKER SET search_path = pg_catalog, public, pg_temp AS $$
DECLARE
    previous_scope text := public.bind_maintenance_scope(org);
BEGIN
    SELECT id INTO next_id FROM projects WHERE organization_id=org AND id>after_id AND deleted_at IS NULL ORDER BY id LIMIT 1;
    CASE family
    WHEN 'integration-pages' THEN
        SELECT EXISTS (SELECT FROM project_integration_page_dirty WHERE organization_id=org)
            OR (next_id IS NOT NULL AND NOT EXISTS (
                SELECT FROM project_integration_pages WHERE organization_id=org AND project_id=next_id
            )) INTO due;
    WHEN 'project-pages' THEN
        SELECT EXISTS (
            SELECT FROM project_settings s JOIN organizations o ON o.organization_id=s.organization_id
            LEFT JOIN project_page_read_model v ON v.organization_id=s.organization_id AND v.project_id=s.project_id
            LEFT JOIN project_policies c ON c.organization_id=s.organization_id AND c.project_id=s.project_id
            WHERE s.organization_id=org AND s.project_id=next_id AND (v.project_id IS NULL OR
                (v.revision,v.source_generation,v.workflow_sha256,v.model_sha256,v.organization_revision,v.policy_id)
                IS DISTINCT FROM (s.revision,s.model_revision,s.workflow_sha256,s.model_sha256,o.model_selection_revision,COALESCE(c.policy_id,'')))
        ) INTO due;
    ELSE
        RAISE EXCEPTION 'unknown project maintenance family' USING ERRCODE='22023';
    END CASE;
    next_id := coalesce(next_id,'');
    PERFORM set_config('detent.organization_id',coalesce(previous_scope,''),true);
END
$$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.github_outbound_due(org text, at_time timestamptz) RETURNS boolean
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
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.maintenance_family_due(org text, family text, at_time timestamptz, linked_intake boolean) RETURNS boolean
LANGUAGE plpgsql SECURITY INVOKER SET search_path = pg_catalog, public, pg_temp AS $$
DECLARE
    previous_scope text := current_setting('detent.organization_id',true);
    due boolean;
BEGIN
    PERFORM set_config('detent.organization_id',org,true);
    CASE family
    WHEN 'board-dirty' THEN
        SELECT EXISTS (SELECT FROM native_board_dirty WHERE organization_id=org)
            OR EXISTS (SELECT FROM native_board_items WHERE organization_id=org AND NOT card_display_ready)
            OR EXISTS (SELECT FROM native_board_items WHERE organization_id=org AND NOT page_ready)
            INTO due;
    WHEN 'work-item-pages' THEN
        SELECT EXISTS (SELECT FROM native_work_item_page_dirty WHERE organization_id=org)
            OR NOT EXISTS (SELECT FROM native_work_item_page_progress WHERE organization_id=org AND backfill_phase>=2 AND fingerprint_phase>=2)
            OR EXISTS (SELECT FROM issues WHERE organization_id=org AND source_revision<>revision)
            OR EXISTS (SELECT FROM native_comments WHERE organization_id=org AND source_revision<>revision)
            OR EXISTS (SELECT FROM collaboration_events WHERE organization_id=org AND payload_format_version=1 AND type IN ('run.started','run.checkpointed','run.observed','run.finished','scheduler.decision'))
            OR EXISTS (SELECT FROM change_versions WHERE organization_id=org AND payload_format_version=1)
            OR EXISTS (SELECT FROM change_evidence WHERE organization_id=org AND payload_format_version=1)
            OR EXISTS (SELECT FROM core_work_events WHERE organization_id=org AND payload_json IS NOT NULL) INTO due;
    WHEN 'health-pages' THEN
        SELECT NOT EXISTS (SELECT FROM health_detector_ticks WHERE organization_id=org AND last_tick_at>at_time-interval '1 minute')
            OR EXISTS (SELECT FROM health_findings WHERE organization_id=org AND page_checkpoint<page_generation)
            OR EXISTS (SELECT FROM health_slack_deliveries d JOIN slack_integrations s USING(organization_id) WHERE d.organization_id=org AND s.enabled AND d.completed_at IS NULL AND d.outcome<>'uncertain' AND d.next_attempt_at<=at_time
                AND (d.event='opened' OR NOT EXISTS (SELECT FROM health_slack_deliveries o WHERE o.organization_id=d.organization_id
                    AND o.finding_id=d.finding_id AND o.event='opened' AND o.completed_at IS NULL AND o.outcome<>'uncertain'))) INTO due;
    WHEN 'onboarding-pages' THEN
        WITH oldest AS MATERIALIZED (
            SELECT project_id,viewer_key,generation FROM project_onboarding_page_viewers WHERE organization_id=org
            ORDER BY attempted_at,project_id,viewer_key LIMIT 1
        ) SELECT EXISTS (
            SELECT FROM oldest v LEFT JOIN project_onboarding_pages p ON p.organization_id=org AND p.project_id=v.project_id AND p.viewer_key=v.viewer_key
            LEFT JOIN project_onboarding_page_access a ON a.organization_id=org
            WHERE p.source_generation IS DISTINCT FROM v.generation OR p.access_generation IS DISTINCT FROM a.generation
               OR p.projected_at<=at_time-interval '1 minute'
        ) INTO due;
    WHEN 'github-intake' THEN
        SELECT EXISTS (SELECT FROM onboarding_issue_intake i JOIN projects p ON p.organization_id=i.organization_id AND p.id=i.project_id
                       WHERE i.organization_id=org AND i.batch_status IN ('discovering','importing') AND i.next_attempt_at<=at_time AND p.deleted_at IS NULL) INTO due;
    WHEN 'github-reconciliation' THEN
        SELECT EXISTS (SELECT FROM github_pull_request_refresh WHERE organization_id=org AND source_kind='change' AND pending AND next_attempt_at<=at_time)
            OR EXISTS (SELECT FROM github_reconciliation r JOIN projects p ON p.organization_id=r.organization_id AND p.id=r.project_id
                JOIN project_settings s ON s.organization_id=p.organization_id AND s.project_id=p.id
                WHERE r.organization_id=org AND r.next_attempt_at<=at_time AND p.deleted_at IS NULL AND p.profile='native'
                  AND (s.checkout_repository<>'' OR s.repository<>'') AND (s.repository_enabled OR linked_intake))
            OR EXISTS (SELECT FROM project_settings s JOIN projects p ON p.organization_id=s.organization_id AND p.id=s.project_id
                WHERE s.organization_id=org AND NOT s.github_reconciliation_initialized AND p.deleted_at IS NULL AND p.profile='native'
                  AND (s.checkout_repository<>'' OR s.repository<>'') AND (s.repository_enabled OR linked_intake)) INTO due;
    WHEN 'github-webhooks' THEN
        SELECT EXISTS (SELECT FROM github_webhook_receipts WHERE organization_id=org AND status='pending' AND next_attempt_at<=at_time)
            OR EXISTS (SELECT FROM github_webhook_receipts WHERE organization_id=org AND status='completed' AND expires_at<=at_time AND payload_object_key<>'') INTO due;
    ELSE
        RAISE EXCEPTION 'unknown maintenance family' USING ERRCODE='22023';
    END CASE;
    PERFORM set_config('detent.organization_id',coalesce(previous_scope,''),true);
    RETURN due;
END
$$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.board_sweep_due(org text, after_id text, OUT next_id text, OUT due boolean) RETURNS record
LANGUAGE plpgsql SECURITY INVOKER SET search_path = pg_catalog, public, pg_temp AS $$
DECLARE
    previous_scope text := current_setting('detent.organization_id',true);
BEGIN
    PERFORM set_config('detent.organization_id',org,true);
    IF EXISTS (SELECT FROM native_board_dirty WHERE organization_id=org)
       OR EXISTS (SELECT FROM native_board_items WHERE organization_id=org AND NOT card_display_ready)
       OR EXISTS (SELECT FROM native_board_items WHERE organization_id=org AND NOT page_ready) THEN
        next_id := after_id;
        due := true;
    ELSE
        SELECT candidate.id INTO next_id FROM (
            (SELECT native_id AS id,0 AS priority FROM issues WHERE organization_id=org AND native_id>after_id ORDER BY native_id LIMIT 1)
            UNION ALL (SELECT work_item_id,0 FROM native_board_items WHERE organization_id=org AND work_item_id>after_id ORDER BY work_item_id LIMIT 1)
            UNION ALL (SELECT native_id,1 FROM issues WHERE organization_id=org AND native_id<=after_id ORDER BY native_id LIMIT 1)
            UNION ALL (SELECT work_item_id,1 FROM native_board_items WHERE organization_id=org AND work_item_id<=after_id ORDER BY work_item_id LIMIT 1)
        ) candidate ORDER BY candidate.priority,candidate.id LIMIT 1;
        due := next_id IS NOT NULL AND NOT EXISTS (
            SELECT FROM issues i
            JOIN native_board_items b ON b.organization_id=i.organization_id AND b.project_id=i.project_id AND b.work_item_id=i.native_id
            JOIN projects p ON p.organization_id=i.organization_id AND p.id=i.project_id
            JOIN project_workflow_states w ON w.organization_id=p.organization_id AND w.project_id=p.id AND w.name=b.state
            WHERE i.organization_id=org AND i.native_id=next_id AND p.profile='native' AND p.deleted_at IS NULL
              AND b.source_revision=i.revision AND b.source_event_sequence=i.event_sequence AND b.source_project_revision=p.content_settings_revision
              AND b.card_display_ready AND b.page_ready AND b.list_ready AND b.page_refresh_after IS NULL AND b.expires_at IS NULL
              AND coalesce(b.card_change_id,'')='' AND coalesce(b.card_attempt_id,'')='' AND coalesce(b.card_execution_id,'')=''
              AND (b.terminal,b.dispatchable)=(w.terminal,w.dispatchable)
              AND b.lane_rank=(SELECT count(*) FROM project_workflow_states preceding WHERE preceding.organization_id=org AND preceding.project_id=p.id AND preceding.board_rank<w.board_rank)
              AND NOT EXISTS (SELECT FROM native_board_items other WHERE other.organization_id=org AND other.work_item_id=next_id AND other.project_id<>i.project_id)
              AND NOT EXISTS (SELECT FROM issues other WHERE other.organization_id=org AND other.native_id=next_id AND other.project_id<>i.project_id)
              AND NOT EXISTS (SELECT FROM native_attempts a WHERE a.organization_id=org AND a.issue_id=i.id)
              AND NOT EXISTS (SELECT FROM leases l WHERE l.organization_id=org AND l.issue_id=i.id AND l.released_at IS NULL)
              AND NOT EXISTS (SELECT FROM change_issue_links l WHERE l.organization_id=org AND l.project_id=i.project_id AND l.work_item_id=i.native_id)
        );
        next_id := coalesce(next_id,'');
    END IF;
    PERFORM set_config('detent.organization_id',coalesce(previous_scope,''),true);
END
$$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.usage_sweep_due(org text, after_source text, page_size integer, OUT next_id text, OUT due boolean) RETURNS record
LANGUAGE plpgsql SECURITY INVOKER SET search_path = pg_catalog, public, pg_temp AS $$
DECLARE
    previous_scope text := current_setting('detent.organization_id',true);
BEGIN
    IF page_size NOT BETWEEN 1 AND 100 THEN
        RAISE EXCEPTION 'invalid usage page size' USING ERRCODE='22023';
    END IF;
    PERFORM set_config('detent.organization_id',org,true);
    IF EXISTS (SELECT FROM native_attempts WHERE organization_id=org AND usage_pending_sequence>0)
       OR EXISTS (SELECT FROM attempt_usage WHERE organization_id=org AND usage_origin='import' AND usage_input_total IS NULL) THEN
        next_id := after_source;
        due := true;
    ELSE
        WITH page AS MATERIALIZED (
            SELECT source_key,resource_id,project_id,revision,payload_hash FROM cost_sources
            WHERE organization_id=org AND bucket='runner_api' AND source_key>after_source ORDER BY source_key LIMIT page_size
        )
        SELECT coalesce(max(c.source_key),''),coalesce(bool_or(
            receipt.sha256 IS DISTINCT FROM c.payload_hash OR
            (i.id IS NOT NULL AND (u.revision,u.attempt_id,u.project_id) IS DISTINCT FROM (c.revision,c.resource_id,c.project_id))
        ),false) INTO next_id,due
        FROM page c
        LEFT JOIN cost_observation_revisions receipt ON receipt.organization_id=org AND receipt.source_key=c.source_key AND receipt.revision=c.revision
        LEFT JOIN native_attempts a ON a.organization_id=org AND a.id=c.resource_id
        LEFT JOIN issues i ON i.organization_id=org AND i.id=a.issue_id AND i.project_id=c.project_id
        LEFT JOIN report_usage_current u ON u.organization_id=org AND u.source_key=c.source_key;
    END IF;
    PERFORM set_config('detent.organization_id',coalesce(previous_scope,''),true);
END
$$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.project_sweep_due(org text, family text, after_id text, OUT next_id text, OUT due boolean) RETURNS record
LANGUAGE plpgsql SECURITY INVOKER SET search_path = pg_catalog, public, pg_temp AS $$
DECLARE
    previous_scope text := current_setting('detent.organization_id',true);
BEGIN
    PERFORM set_config('detent.organization_id',org,true);
    SELECT id INTO next_id FROM projects WHERE organization_id=org AND id>after_id AND deleted_at IS NULL ORDER BY id LIMIT 1;
    CASE family
    WHEN 'integration-pages' THEN
        IF next_id IS NOT NULL AND NOT EXISTS (
            SELECT FROM project_integration_pages WHERE organization_id=org AND project_id=next_id
        ) THEN
            INSERT INTO project_integration_page_dirty(organization_id,project_id)
                SELECT org,id FROM projects WHERE organization_id=org AND id=next_id AND deleted_at IS NULL
                ON CONFLICT(organization_id,project_id) DO NOTHING;
        END IF;
        SELECT EXISTS (SELECT FROM project_integration_page_dirty WHERE organization_id=org) INTO due;
    WHEN 'project-pages' THEN
        SELECT EXISTS (
            SELECT FROM project_settings s JOIN organizations o ON o.organization_id=s.organization_id
            LEFT JOIN project_page_read_model v ON v.organization_id=s.organization_id AND v.project_id=s.project_id
            LEFT JOIN project_policies c ON c.organization_id=s.organization_id AND c.project_id=s.project_id
            WHERE s.organization_id=org AND s.project_id=next_id AND (v.project_id IS NULL OR
                (v.revision,v.source_generation,v.workflow_sha256,v.model_sha256,v.organization_revision,v.policy_id)
                IS DISTINCT FROM (s.revision,s.model_revision,s.workflow_sha256,s.model_sha256,o.model_selection_revision,COALESCE(c.policy_id,'')))
        ) INTO due;
    ELSE
        RAISE EXCEPTION 'unknown project maintenance family' USING ERRCODE='22023';
    END CASE;
    next_id := coalesce(next_id,'');
    PERFORM set_config('detent.organization_id',coalesce(previous_scope,''),true);
END
$$;
-- +goose StatementEnd
DROP FUNCTION public.claim_integration_page(text);
DROP FUNCTION public.github_outbound_deadline(text);
DROP FUNCTION public.bind_maintenance_scope(text);
