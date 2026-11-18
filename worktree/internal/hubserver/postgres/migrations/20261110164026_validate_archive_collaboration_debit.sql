-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE archive_objects VALIDATE CONSTRAINT archive_objects_collaboration_nonnegative;

-- +goose Down
SELECT 1;
