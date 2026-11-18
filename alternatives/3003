-- +goose Up
CREATE TABLE native_duplicate_challenges (
    organization_id text NOT NULL,
    project_id text NOT NULL,
    token text NOT NULL CHECK (token ~ '^dup_[0-9a-f]{32}$'),
    principal_id text NOT NULL CHECK (length(principal_id) BETWEEN 1 AND 256),
    request_id text NOT NULL CHECK (length(request_id) BETWEEN 1 AND 128),
    content_hash text NOT NULL CHECK (content_hash ~ '^[0-9a-f]{64}$'),
    policy_hash text NOT NULL CHECK (policy_hash ~ '^[0-9a-f]{64}$'),
    source_sequence bigint NOT NULL CHECK (source_sequence >= 0),
    candidate_ids text[] NOT NULL,
    candidate_numbers bigint[] NOT NULL,
    candidate_revisions bigint[] NOT NULL,
    created_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    PRIMARY KEY (organization_id, project_id, token),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id) ON DELETE CASCADE,
    CHECK (expires_at = created_at + INTERVAL '1 hour'),
    CHECK (cardinality(candidate_ids) BETWEEN 1 AND 5),
    CHECK (cardinality(candidate_ids) = cardinality(candidate_numbers) AND cardinality(candidate_ids) = cardinality(candidate_revisions)),
    CHECK (array_ndims(candidate_ids) = 1 AND array_ndims(candidate_numbers) = 1 AND array_ndims(candidate_revisions) = 1),
    CHECK (array_lower(candidate_ids, 1) = 1 AND array_lower(candidate_numbers, 1) = 1 AND array_lower(candidate_revisions, 1) = 1),
    CHECK (array_position(candidate_ids, NULL) IS NULL AND array_position(candidate_numbers, NULL) IS NULL AND array_position(candidate_revisions, NULL) IS NULL),
    CHECK ('' <> ALL(candidate_ids) AND 0 < ALL(candidate_numbers) AND 0 < ALL(candidate_revisions))
);

CREATE INDEX native_duplicate_challenges_expiry_idx
    ON native_duplicate_challenges (organization_id, expires_at, project_id, token);

-- +goose StatementBegin
CREATE FUNCTION prune_native_duplicate_challenges(tenant text, cutoff timestamptz, batch_limit integer)
RETURNS bigint LANGUAGE SQL VOLATILE SECURITY INVOKER SET search_path = pg_catalog AS $$
    WITH candidates AS MATERIALIZED (
        SELECT project_id,token FROM public.native_duplicate_challenges
        WHERE organization_id=tenant AND expires_at<=cutoff
        ORDER BY expires_at,project_id,token LIMIT GREATEST(0,LEAST(batch_limit,5000))
    ), unlocked AS MATERIALIZED (
        SELECT locked.ctid FROM candidates
        CROSS JOIN LATERAL (
            SELECT ctid FROM public.native_duplicate_challenges
            WHERE organization_id=tenant AND project_id=candidates.project_id AND token=candidates.token AND expires_at<=cutoff
            FOR UPDATE SKIP LOCKED OFFSET 0
        ) AS locked
    ), deleted AS (
        DELETE FROM public.native_duplicate_challenges
        WHERE organization_id=tenant AND ctid=ANY(ARRAY(SELECT ctid FROM unlocked))
        RETURNING 1
    )
    SELECT count(*) FROM deleted;
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION prune_native_duplicate_challenges(text, timestamptz, integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION prune_native_duplicate_challenges(text, timestamptz, integer) TO detent_runtime, detent_platform, detent_migration;
ALTER TABLE native_duplicate_challenges ENABLE ROW LEVEL SECURITY;
ALTER TABLE native_duplicate_challenges FORCE ROW LEVEL SECURITY;

REVOKE ALL ON native_duplicate_challenges FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON native_duplicate_challenges TO detent_runtime, detent_platform;

CREATE POLICY tenant_isolation ON native_duplicate_challenges TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY platform_access ON native_duplicate_challenges TO detent_platform USING (true) WITH CHECK (true);

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
 RAISE EXCEPTION 'duplicate challenge records require an explicit retained-data migration';
END;
$$;
-- +goose StatementEnd
