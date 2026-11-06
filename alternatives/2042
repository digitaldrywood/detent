-- +goose Up
ALTER TABLE issues ADD COLUMN issue_type TEXT CHECK (issue_type IN ('bug', 'change_request'));

-- +goose Down
ALTER TABLE issues DROP COLUMN issue_type;
