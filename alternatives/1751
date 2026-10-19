-- +goose Up
CREATE TABLE runner_host_hours (
    organization_id TEXT NOT NULL,
    runner_id TEXT NOT NULL REFERENCES runner_identities(id) ON DELETE CASCADE,
    hour TEXT NOT NULL,
    summary_json TEXT NOT NULL CHECK(json_valid(summary_json)),
    segment_ids_json TEXT NOT NULL CHECK(json_valid(segment_ids_json) AND json_array_length(segment_ids_json) <= 128),
    PRIMARY KEY (organization_id, runner_id, hour)
) STRICT;
CREATE INDEX runner_host_hours_expiry ON runner_host_hours(hour);

-- +goose Down
DROP TABLE runner_host_hours;
