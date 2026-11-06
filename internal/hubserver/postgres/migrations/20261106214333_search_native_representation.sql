-- +goose Up
CREATE EXTENSION IF NOT EXISTS pg_trgm;

ALTER TABLE search_documents
 ADD COLUMN content_revision bigint NOT NULL DEFAULT 0 CHECK (content_revision >= 0),
 ADD COLUMN content_sha256 text NOT NULL DEFAULT '' CHECK (content_sha256 = '' OR content_sha256 ~ '^[0-9a-f]{64}$'),
 ADD COLUMN indexed_generation bigint NOT NULL DEFAULT 0 CHECK (indexed_generation >= 0),
 ADD COLUMN search_format smallint NOT NULL DEFAULT 0 CHECK (search_format >= 0);

-- +goose StatementBegin
CREATE FUNCTION search_segments_valid(value text, ends integer[]) RETURNS boolean
LANGUAGE sql IMMUTABLE STRICT SET search_path=pg_catalog AS $segments$
 SELECT octet_length(value) <= 65536
 AND value = translate(value, 'ABCDEFGHIJKLMNOPQRSTUVWXYZ', 'abcdefghijklmnopqrstuvwxyz')
 AND COALESCE(array_ndims(ends), 1) = 1
 AND COALESCE(array_lower(ends, 1), 1) = 1
 AND cardinality(ends) <= 65538
 AND array_position(ends, NULL) IS NULL
 AND COALESCE(ends[cardinality(ends)], 0) = octet_length(value)
 AND NOT EXISTS (SELECT 1 FROM unnest(ends) WITH ORDINALITY e(boundary, position)
   WHERE boundary < 0 OR boundary > octet_length(value)
   OR (position > 1 AND boundary < ends[position - 1]));
$segments$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION search_segments_valid(text, integer[]) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION search_segments_valid(text, integer[]) TO detent_runtime_v2;

CREATE TABLE search_document_index (
 organization_id text NOT NULL,
 document_id text NOT NULL,
 generation bigint NOT NULL CHECK (generation >= 0),
 format smallint NOT NULL CHECK (format > 0),
 content_sha256 text NOT NULL CHECK (content_sha256 ~ '^[0-9a-f]{64}$'),
 word_vector tsvector NOT NULL DEFAULT ''::tsvector,
 search_text text NOT NULL DEFAULT '',
 segment_ends integer[] NOT NULL DEFAULT '{}',
 token_count bigint NOT NULL DEFAULT 0 CHECK (token_count >= 0),
 terms text[] NOT NULL DEFAULT '{}',
 term_frequencies bigint[] NOT NULL DEFAULT '{}',
 PRIMARY KEY (organization_id, document_id),
 FOREIGN KEY (organization_id, document_id) REFERENCES search_documents(organization_id, id) ON DELETE CASCADE,
 CHECK (search_segments_valid(search_text, segment_ends)),
 CHECK (cardinality(terms) = cardinality(term_frequencies) AND array_position(terms, NULL) IS NULL
   AND array_position(term_frequencies, NULL) IS NULL AND 0 < ALL(term_frequencies))
);
ALTER TABLE search_document_index ENABLE ROW LEVEL SECURITY;
ALTER TABLE search_document_index FORCE ROW LEVEL SECURITY;
REVOKE ALL ON search_document_index FROM PUBLIC, detent_platform_v2;
GRANT SELECT, INSERT, UPDATE, DELETE ON search_document_index TO detent_runtime_v2;
CREATE POLICY tenant_isolation ON search_document_index TO detent_runtime_v2, detent_migration_v2
 USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
 WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

ALTER TABLE storage_conversion_cursors DROP CONSTRAINT storage_conversion_cursors_family_check;
ALTER TABLE storage_conversion_cursors ADD CONSTRAINT storage_conversion_cursors_family_check
 CHECK (family IN ('runs', 'versions', 'evidence', 'search'));

-- +goose Down
ALTER TABLE storage_conversion_cursors DISABLE ROW LEVEL SECURITY;
DELETE FROM storage_conversion_cursors WHERE family = 'search';
ALTER TABLE storage_conversion_cursors ENABLE ROW LEVEL SECURITY;
ALTER TABLE storage_conversion_cursors DROP CONSTRAINT storage_conversion_cursors_family_check;
ALTER TABLE storage_conversion_cursors ADD CONSTRAINT storage_conversion_cursors_family_check
 CHECK (family IN ('runs', 'versions', 'evidence'));
DROP TABLE search_document_index;
DROP FUNCTION search_segments_valid(text, integer[]);
ALTER TABLE search_documents DROP COLUMN search_format, DROP COLUMN indexed_generation,
 DROP COLUMN content_sha256, DROP COLUMN content_revision;
