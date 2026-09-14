-- +goose Up
-- provider_thread_origin records which kind of turn produced the
-- conversation's provider thread: a coordinator turn, which runs read-only
-- with the coordinator instructions and no checkout, or a worker attempt on
-- the linked issue. A thread carries the instructions and the permission set
-- of the turn that created it, so resuming a coordinator thread from a worker
-- attempt hands the worker the coordinator's restrictions (decisions section
-- 9.3). The binding rule is origin equality, so the column is empty only for
-- conversations whose thread predates it; an empty origin never resumes.
ALTER TABLE conversations ADD COLUMN provider_thread_origin TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE conversations DROP COLUMN provider_thread_origin;
