-- +goose Up
CREATE UNIQUE INDEX native_comments_search_parent_idx ON native_comments (organization_id, project_id, work_item_id, id);
CREATE UNIQUE INDEX collaboration_events_search_parent_idx ON collaboration_events (organization_id, project_id, work_item_id, id);
CREATE UNIQUE INDEX change_requests_search_parent_idx ON change_requests (organization_id, project_id, work_item_id, id);

CREATE TABLE search_documents (
    organization_id text NOT NULL,
    id text NOT NULL,
    project_id text,
    work_item_id text,
    kind text NOT NULL CHECK (kind IN ('issue', 'comment', 'event', 'change', 'thread')),
    source_id text NOT NULL,
    comment_id text GENERATED ALWAYS AS (CASE WHEN kind = 'comment' THEN source_id END) STORED,
    event_id text GENERATED ALWAYS AS (CASE WHEN kind = 'event' THEN source_id END) STORED,
    change_id text GENERATED ALWAYS AS (CASE WHEN kind = 'change' THEN source_id END) STORED,
    revision bigint NOT NULL CHECK (revision > 0),
    indexed_revision bigint NOT NULL DEFAULT 0 CHECK (indexed_revision >= 0 AND indexed_revision <= revision),
    title text NOT NULL,
    scope text NOT NULL CHECK (scope IN ('general', 'project', 'issue')),
    status text NOT NULL DEFAULT '' CHECK (status IN ('', 'open', 'resolved')),
    updated_at timestamptz NOT NULL,
    updated_at_submicrosecond bigint NOT NULL DEFAULT 0 CHECK (updated_at_submicrosecond BETWEEN 0 AND 999),
    payload_object_key text NOT NULL,
    payload_sha256 text NOT NULL,
    payload_bytes bigint NOT NULL CHECK (payload_bytes > 0),
    payload_content_bytes bigint NOT NULL CHECK (payload_content_bytes > 0),
    vector tsvector NOT NULL DEFAULT ''::tsvector,
    token_count bigint NOT NULL DEFAULT 0 CHECK (token_count >= 0),
    terms text[] NOT NULL DEFAULT '{}',
    term_frequencies bigint[] NOT NULL DEFAULT '{}',
    PRIMARY KEY (organization_id, id),
    UNIQUE (organization_id, kind, source_id),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id) ON DELETE CASCADE,
    FOREIGN KEY (organization_id, project_id, work_item_id) REFERENCES issues (organization_id, project_id, native_id) ON DELETE CASCADE,
    FOREIGN KEY (organization_id, project_id, work_item_id, comment_id) REFERENCES native_comments (organization_id, project_id, work_item_id, id) ON DELETE CASCADE,
    FOREIGN KEY (organization_id, project_id, work_item_id, event_id) REFERENCES collaboration_events (organization_id, project_id, work_item_id, id),
    FOREIGN KEY (organization_id, project_id, work_item_id, change_id) REFERENCES change_requests (organization_id, project_id, work_item_id, id),
    CHECK ((scope = 'general' AND project_id IS NULL AND work_item_id IS NULL)
        OR (scope = 'project' AND project_id IS NOT NULL AND work_item_id IS NULL)
        OR (scope = 'issue' AND project_id IS NOT NULL AND work_item_id IS NOT NULL)),
    CHECK (kind = 'thread' OR scope = 'issue'),
    CHECK (cardinality(terms) = cardinality(term_frequencies) AND array_position(terms, NULL) IS NULL
        AND array_position(term_frequencies, NULL) IS NULL AND 0 < ALL(term_frequencies)),
    CHECK (kind <> 'issue' OR source_id = work_item_id)
);

CREATE INDEX search_documents_vector_idx ON search_documents USING GIN (vector) WITH (fastupdate = off);
CREATE INDEX search_documents_scope_idx ON search_documents (organization_id, project_id, kind, updated_at DESC, updated_at_submicrosecond DESC, id COLLATE "C" DESC);
CREATE INDEX search_documents_threads_idx ON search_documents (organization_id, kind, updated_at DESC, updated_at_submicrosecond DESC, id COLLATE "C" DESC);
CREATE INDEX search_documents_issue_idx ON search_documents (organization_id, project_id, work_item_id, id);
CREATE INDEX search_documents_pending_idx ON search_documents (organization_id, id) WHERE indexed_revision <> revision;
CREATE INDEX search_documents_pending_threads_idx ON search_documents (organization_id, kind, updated_at DESC, updated_at_submicrosecond DESC, id COLLATE "C" DESC) WHERE indexed_revision <> revision;

ALTER TABLE search_documents ENABLE ROW LEVEL SECURITY;
ALTER TABLE search_documents FORCE ROW LEVEL SECURITY;
REVOKE ALL ON search_documents FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON search_documents TO detent_runtime;
GRANT SELECT ON search_documents TO detent_platform;
CREATE POLICY tenant_isolation ON search_documents TO detent_runtime, detent_migration
    USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
    WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY platform_access ON search_documents FOR SELECT TO detent_platform USING (true);

-- +goose Down
DROP TABLE search_documents;
DROP INDEX change_requests_search_parent_idx;
DROP INDEX collaboration_events_search_parent_idx;
DROP INDEX native_comments_search_parent_idx;
