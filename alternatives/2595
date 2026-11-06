-- +goose Up
ALTER TABLE search_documents
    ADD COLUMN progress_generation bigint,
    ADD COLUMN progress_after_id text,
    ADD COLUMN progress_object_key text,
    ADD COLUMN progress_sha256 text,
    ADD COLUMN progress_bytes bigint,
    ADD COLUMN progress_content_bytes bigint,
    ADD CONSTRAINT search_progress_complete CHECK (
        (progress_generation IS NULL AND progress_after_id IS NULL AND progress_object_key IS NULL
            AND progress_sha256 IS NULL AND progress_bytes IS NULL AND progress_content_bytes IS NULL)
        OR (progress_generation IS NOT NULL AND progress_generation >= 0
            AND progress_after_id IS NOT NULL AND progress_after_id <> ''
            AND progress_object_key IS NOT NULL AND progress_object_key <> ''
            AND progress_sha256 IS NOT NULL AND progress_sha256 ~ '^[0-9a-f]{64}$'
            AND progress_bytes IS NOT NULL AND progress_bytes > 0
            AND progress_content_bytes IS NOT NULL AND progress_content_bytes > 0)
    );

-- +goose Down
ALTER TABLE search_documents
    DROP CONSTRAINT search_progress_complete,
    DROP COLUMN progress_generation,
    DROP COLUMN progress_after_id,
    DROP COLUMN progress_object_key,
    DROP COLUMN progress_sha256,
    DROP COLUMN progress_bytes,
    DROP COLUMN progress_content_bytes;
