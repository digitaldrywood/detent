-- +goose Up
CREATE TABLE worker_generations (
  gen INTEGER PRIMARY KEY AUTOINCREMENT,
  pid INTEGER NOT NULL,
  process_start TEXT NOT NULL,
  version TEXT NOT NULL,
  state TEXT NOT NULL,
  started_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  exited_at TEXT
);
CREATE INDEX idx_worker_generations_unexited ON worker_generations(gen) WHERE exited_at IS NULL;
ALTER TABLE codex_sessions ADD COLUMN owner_generation INTEGER;
ALTER TABLE work_attempts ADD COLUMN owner_generation INTEGER;

-- +goose Down
ALTER TABLE work_attempts DROP COLUMN owner_generation;
ALTER TABLE codex_sessions DROP COLUMN owner_generation;
DROP TABLE worker_generations;
