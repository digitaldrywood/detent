-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE conversations
    ADD COLUMN execution_attempt_id text,
    ADD COLUMN execution_run_id text,
    ADD COLUMN execution_fencing_token bigint,
    ADD COLUMN execution_runner_id text,
    ADD COLUMN execution_machine_id text,
    ADD COLUMN execution_thread_id text,
    ADD COLUMN execution_turn_id text,
    ADD COLUMN execution_resume text,
    ADD COLUMN execution_can_steer boolean,
    ADD COLUMN execution_can_interrupt boolean,
    ADD COLUMN execution_can_answer boolean,
    ADD COLUMN execution_can_continue boolean,
    ADD COLUMN execution_error text,
    ADD COLUMN execution_updated_at timestamptz,
    ADD COLUMN preference_model text,
    ADD COLUMN preference_effort text,
    ADD COLUMN preference_access text;
ALTER TABLE conversation_commands
    ADD COLUMN receipt_key text,
    ADD COLUMN receipt_kind text,
    ADD COLUMN receipt_message_id text,
    ADD COLUMN receipt_question_id text,
    ADD COLUMN receipt_error_code text,
    ADD COLUMN receipt_error_message text,
    ADD COLUMN receipt_updated_at timestamptz;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'typed conversation authority cannot be discarded'; END; $$;
-- +goose StatementEnd
