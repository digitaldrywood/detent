-- +goose Up
UPDATE work_attempts
SET terminal_state = 'delivered',
    error_class = NULL,
    error_message = NULL,
    phase = 'completed',
    status_message = 'delivery completed before lane revocation',
    worker_metadata_json = json_set(
      CASE WHEN json_valid(worker_metadata_json) THEN worker_metadata_json ELSE '{}' END,
      '$.historical_lane_revocation',
      json_object(
        'classification', 'delivered_before_revocation',
        'original_terminal_state', terminal_state,
        'original_error_class', COALESCE(error_class, ''),
        'original_error_message', COALESCE(error_message, ''),
        'original_phase', COALESCE(phase, ''),
        'original_status_message', COALESCE(status_message, '')
      )
    )
WHERE lower(trim(COALESCE(terminal_state, ''))) = 'lane_revoked'
  AND lower(trim(COALESCE(error_message, ''))) IN ('tracker_lane_changed', 'detent_tracker_lane_changed')
  AND json_extract(
    CASE WHEN json_valid(worker_metadata_json) THEN worker_metadata_json ELSE '{}' END,
    '$.work_product_pushed'
  ) = 1;

UPDATE codex_sessions
SET final_state = 'delivered'
WHERE lower(trim(COALESCE(final_state, ''))) = 'lane_revoked'
  AND EXISTS (
    SELECT 1
    FROM work_attempts
    WHERE work_attempts.id = codex_sessions.work_attempt_id
      AND lower(trim(COALESCE(work_attempts.terminal_state, ''))) = 'delivered'
      AND json_extract(work_attempts.worker_metadata_json, '$.historical_lane_revocation.classification') = 'delivered_before_revocation'
  );

-- +goose Down
UPDATE codex_sessions
SET final_state = 'lane_revoked'
WHERE lower(trim(COALESCE(final_state, ''))) = 'delivered'
  AND EXISTS (
    SELECT 1
    FROM work_attempts
    WHERE work_attempts.id = codex_sessions.work_attempt_id
      AND lower(trim(COALESCE(work_attempts.terminal_state, ''))) = 'delivered'
      AND json_extract(work_attempts.worker_metadata_json, '$.historical_lane_revocation.classification') = 'delivered_before_revocation'
  );

UPDATE work_attempts
SET terminal_state = json_extract(worker_metadata_json, '$.historical_lane_revocation.original_terminal_state'),
    error_class = NULLIF(json_extract(worker_metadata_json, '$.historical_lane_revocation.original_error_class'), ''),
    error_message = NULLIF(json_extract(worker_metadata_json, '$.historical_lane_revocation.original_error_message'), ''),
    phase = NULLIF(json_extract(worker_metadata_json, '$.historical_lane_revocation.original_phase'), ''),
    status_message = NULLIF(json_extract(worker_metadata_json, '$.historical_lane_revocation.original_status_message'), ''),
    worker_metadata_json = json_remove(worker_metadata_json, '$.historical_lane_revocation')
WHERE lower(trim(COALESCE(terminal_state, ''))) = 'delivered'
  AND json_extract(worker_metadata_json, '$.historical_lane_revocation.classification') = 'delivered_before_revocation'
  AND json_extract(worker_metadata_json, '$.historical_lane_revocation.original_terminal_state') = 'lane_revoked';
