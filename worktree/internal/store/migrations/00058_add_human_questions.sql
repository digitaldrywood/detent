-- +goose Up
CREATE TABLE human_questions (
  project_id TEXT NOT NULL,
  issue_id TEXT NOT NULL,
  question_key TEXT NOT NULL,
  issue_identifier TEXT NOT NULL,
  body TEXT NOT NULL,
  question_comment_id TEXT NOT NULL DEFAULT '',
  answer_comment_id TEXT NOT NULL DEFAULT '',
  answer_body TEXT NOT NULL DEFAULT '',
  work_fingerprint TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (project_id, issue_id, question_key)
);

CREATE UNIQUE INDEX human_questions_waiting ON human_questions(project_id, issue_id) WHERE answer_comment_id = '';

-- +goose Down
DROP TABLE human_questions;
