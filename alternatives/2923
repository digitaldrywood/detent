-- +goose Up
CREATE TABLE provenance_attribution_boundaries (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  trustworthy_since TEXT NOT NULL
);

INSERT INTO provenance_attribution_boundaries (id, trustworthy_since)
VALUES (1, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'));

-- +goose Down
DROP TABLE IF EXISTS provenance_attribution_boundaries;
