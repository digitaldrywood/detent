-- +goose Up
CREATE INDEX organizations_allocation_candidates ON platform.organizations(
 (CASE WHEN state='deleting' THEN timestamptz '0001-01-01 00:00:00+00' ELSE coalesce(next_attempt_at,timestamptz '0001-01-01 00:00:00+00') END),
 created_at,id)
WHERE managed AND state IN ('deleting','requested','allocating');

-- +goose Down
DROP INDEX platform.organizations_allocation_candidates;
