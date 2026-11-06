-- +goose Up
ALTER TABLE codex_sessions
ADD COLUMN cached_input_tokens INTEGER;

ALTER TABLE codex_sessions
ADD COLUMN reasoning_output_tokens INTEGER;

ALTER TABLE codex_sessions
ADD COLUMN model_context_window INTEGER;

ALTER TABLE usage_events
ADD COLUMN cached_input_tokens INTEGER;

ALTER TABLE usage_events
ADD COLUMN reasoning_output_tokens INTEGER;

ALTER TABLE usage_events
ADD COLUMN model_context_window INTEGER;

ALTER TABLE workflow_phase_events
ADD COLUMN cached_input_tokens INTEGER;

ALTER TABLE workflow_phase_events
ADD COLUMN reasoning_output_tokens INTEGER;

ALTER TABLE workflow_phase_events
ADD COLUMN model_context_window INTEGER;

-- +goose Down
ALTER TABLE workflow_phase_events
DROP COLUMN model_context_window;

ALTER TABLE workflow_phase_events
DROP COLUMN reasoning_output_tokens;

ALTER TABLE workflow_phase_events
DROP COLUMN cached_input_tokens;

ALTER TABLE usage_events
DROP COLUMN model_context_window;

ALTER TABLE usage_events
DROP COLUMN reasoning_output_tokens;

ALTER TABLE usage_events
DROP COLUMN cached_input_tokens;

ALTER TABLE codex_sessions
DROP COLUMN model_context_window;

ALTER TABLE codex_sessions
DROP COLUMN reasoning_output_tokens;

ALTER TABLE codex_sessions
DROP COLUMN cached_input_tokens;
