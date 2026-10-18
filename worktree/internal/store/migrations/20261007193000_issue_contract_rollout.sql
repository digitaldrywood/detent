-- +goose Up
CREATE TABLE issue_contract_rollout (activated_at TEXT NOT NULL);
INSERT INTO issue_contract_rollout VALUES (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'));

-- +goose Down
DROP TABLE issue_contract_rollout;
