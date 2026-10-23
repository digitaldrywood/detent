-- +goose Up
UPDATE hosted_plans
SET record_json = json_set(record_json, '$.allowances.collaboration_bytes', 26843545600)
WHERE id = 'starter' AND version = 1
  AND coalesce(json_extract(record_json, '$.allowances.collaboration_bytes'), 0) < 26843545600;

UPDATE hosted_plans
SET record_json = json_set(record_json, '$.allowances.collaboration_bytes', 107374182400)
WHERE id = 'team' AND version = 2
  AND coalesce(json_extract(record_json, '$.allowances.collaboration_bytes'), 0) < 107374182400;

UPDATE hosted_plans
SET record_json = json_set(record_json, '$.allowances.collaboration_bytes', 268435456000)
WHERE id = 'growth' AND version = 2
  AND coalesce(json_extract(record_json, '$.allowances.collaboration_bytes'), 0) < 268435456000;

-- +goose Down
SELECT 1;
