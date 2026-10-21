-- +goose Up
CREATE TABLE decision_requests (
  attempt_id TEXT PRIMARY KEY REFERENCES native_attempts(id),
  summary TEXT NOT NULL,
  question TEXT NOT NULL,
  human_action TEXT NOT NULL,
  answered_comment_id TEXT REFERENCES native_comments(id),
  custom_allowed INTEGER NOT NULL CHECK (custom_allowed = 1),
  custom_return_state TEXT NOT NULL CHECK (custom_return_state IN ('Todo', 'Rework'))
);
CREATE TABLE decision_options (
  attempt_id TEXT NOT NULL REFERENCES decision_requests(attempt_id),
  position INTEGER NOT NULL CHECK (position BETWEEN 0 AND 3),
  label TEXT NOT NULL,
  consequence TEXT NOT NULL,
  recommended INTEGER NOT NULL CHECK (recommended IN (0, 1)),
  return_state TEXT NOT NULL CHECK (return_state IN ('Todo', 'Rework')),
  PRIMARY KEY (attempt_id, position)
);

-- +goose Down
DROP TABLE decision_options;
DROP TABLE decision_requests;
