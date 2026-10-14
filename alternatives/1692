-- +goose Up
CREATE INDEX change_evidence_current_idx ON change_evidence(change_id, version_id, kind, sequence);
CREATE INDEX change_evidence_approval_idx ON change_evidence(change_id, kind, json_extract(record_json, '$.decision'), version_id);

-- +goose Down
DROP INDEX change_evidence_approval_idx;
DROP INDEX change_evidence_current_idx;
