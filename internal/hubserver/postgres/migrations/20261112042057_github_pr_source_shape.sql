-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE github_pull_request_refresh
 ADD COLUMN source_kind text NOT NULL DEFAULT 'change',
 ADD COLUMN webhook_delivery_id text,
 ADD COLUMN merge_sha text,
 ADD COLUMN base_ref text,
 ADD COLUMN merged_at timestamptz,
 ADD COLUMN merged_by text;
ALTER TABLE github_pull_request_refresh DROP CONSTRAINT github_pull_request_refresh_pkey;
ALTER TABLE github_pull_request_refresh ADD CONSTRAINT github_pull_request_refresh_source_key UNIQUE USING INDEX github_pull_request_refresh_source_key;
ALTER TABLE github_pull_request_refresh
 ALTER COLUMN change_id DROP NOT NULL,
 ALTER COLUMN work_item_id DROP NOT NULL,
 ALTER COLUMN requested_version DROP NOT NULL,
 ALTER COLUMN requested_head_sha DROP NOT NULL,
 ALTER COLUMN request_count DROP NOT NULL,
 ALTER COLUMN requested_at DROP NOT NULL,
 ALTER COLUMN next_attempt_at DROP NOT NULL;
ALTER TABLE github_pull_request_refresh DROP CONSTRAINT github_pull_request_refresh_check;
ALTER TABLE github_pull_request_refresh ADD CONSTRAINT github_pull_request_refresh_observation_shape CHECK (source_kind='webhook' OR 
  (observed_at IS NULL AND observed_head_sha IS NULL AND source_updated_at IS NULL
   AND state IS NULL AND draft IS NULL AND mergeable IS NULL
   AND checks_status IS NULL AND checks_conclusion IS NULL AND checks_total IS NULL
   AND checks_pending IS NULL AND checks_passed IS NULL AND checks_failed IS NULL
   AND reviews_state IS NULL AND reviews_decision IS NULL AND reviews_approvals IS NULL
   AND reviews_changes_requested IS NULL AND reviews_comments IS NULL)
  OR
  (observed_at IS NOT NULL AND observed_head_sha IS NOT NULL AND observed_head_sha<>''
   AND source_updated_at IS NOT NULL AND state IS NOT NULL AND draft IS NOT NULL
   AND checks_status IS NOT NULL AND checks_conclusion IS NOT NULL AND checks_total IS NOT NULL
   AND checks_pending IS NOT NULL AND checks_passed IS NOT NULL AND checks_failed IS NOT NULL
   AND reviews_state IS NOT NULL AND reviews_decision IS NOT NULL AND reviews_approvals IS NOT NULL
   AND reviews_changes_requested IS NOT NULL AND reviews_comments IS NOT NULL)
 ) NOT VALID;
ALTER TABLE github_pull_request_refresh ADD CONSTRAINT github_pull_request_refresh_source_shape CHECK (
 (source_kind='change' AND change_id IS NOT NULL AND change_id<>'' AND work_item_id IS NOT NULL AND work_item_id<>''
  AND requested_version IS NOT NULL AND requested_head_sha IS NOT NULL AND request_count IS NOT NULL
  AND requested_at IS NOT NULL AND next_attempt_at IS NOT NULL AND webhook_delivery_id IS NULL)
 OR
 (source_kind='webhook' AND change_id IS NULL AND work_item_id IS NULL AND requested_version IS NULL AND requested_head_sha IS NULL
  AND request_count IS NULL AND observed_request_count IS NULL AND requested_at IS NULL AND next_attempt_at IS NULL
  AND NOT pending AND attempts=0 AND webhook_delivery_id IS NOT NULL AND webhook_delivery_id<>''
  AND observed_head_sha IS NOT NULL AND observed_head_sha<>'' AND source_updated_at IS NOT NULL AND observed_at IS NOT NULL AND state IS NOT NULL AND draft IS NOT NULL
  AND mergeable IS NULL AND checks_status IS NULL AND checks_conclusion IS NULL AND checks_total IS NULL AND checks_pending IS NULL AND checks_passed IS NULL AND checks_failed IS NULL
  AND reviews_state IS NULL AND reviews_decision IS NULL AND reviews_approvals IS NULL AND reviews_changes_requested IS NULL AND reviews_comments IS NULL
  AND merge_sha IS NOT NULL AND base_ref IS NOT NULL AND merged_by IS NOT NULL)
) NOT VALID;
ALTER TABLE github_pull_request_refresh ADD CONSTRAINT github_pull_request_refresh_merge_shape CHECK (
 (merge_sha IS NULL AND base_ref IS NULL AND merged_at IS NULL AND merged_by IS NULL)
 OR (merge_sha IS NOT NULL AND base_ref IS NOT NULL AND base_ref<>'' AND merged_by IS NOT NULL AND observed_at IS NOT NULL
     AND (state<>'merged' OR merged_at IS NOT NULL)
     AND (merged_at IS NULL OR merge_sha<>'' AND state IS NOT NULL AND state IN ('closed','merged')))
) NOT VALID;
ALTER TABLE github_pull_request_refresh ADD CONSTRAINT github_pull_request_refresh_webhook_source FOREIGN KEY (organization_id,webhook_delivery_id) REFERENCES github_webhook_receipts(organization_id,delivery_id) NOT VALID;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'standalone pull request provenance cannot be discarded'; END; $$;
-- +goose StatementEnd
