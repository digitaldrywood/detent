-- +goose Up
UPDATE work_attempts
SET worker_metadata_json = json_set(
      CASE WHEN json_valid(worker_metadata_json) THEN worker_metadata_json ELSE '{}' END,
      '$.delivery_receipt',
      json_object(
        'schema', 1,
        'kind', 'pushed_work_product',
        'recorded_at', completed_at,
        'source', 'historical_backfill',
        'pr_number', json_extract(worker_metadata_json, '$.pr_number'),
        'pr_head_sha', json_extract(worker_metadata_json, '$.pr_head_sha')
      ),
      '$.lane_revocation_receipt_backfill',
      json_object(
        'migration', 45,
        'original_terminal_state', COALESCE(terminal_state, ''),
        'original_error_class', COALESCE(error_class, ''),
        'original_error_message', COALESCE(error_message, ''),
        'original_phase', COALESCE(phase, ''),
        'original_status_message', COALESCE(status_message, ''),
        'original_worker_metadata_json', CASE WHEN json_valid(worker_metadata_json) THEN worker_metadata_json ELSE '{}' END
      )
    )
WHERE json_type(
        CASE WHEN json_valid(worker_metadata_json) THEN worker_metadata_json ELSE '{}' END,
        '$.delivery_receipt'
      ) IS NULL
  AND (
    (
      lower(trim(COALESCE(terminal_state, ''))) = 'delivered'
      AND json_extract(worker_metadata_json, '$.historical_lane_revocation.classification') = 'delivered_before_revocation'
    )
    OR (
      lower(trim(COALESCE(terminal_state, ''))) = 'lane_revoked'
      AND lower(trim(COALESCE(error_message, ''))) IN ('tracker_lane_changed', 'detent_tracker_lane_changed')
      AND json_extract(
        CASE WHEN json_valid(worker_metadata_json) THEN worker_metadata_json ELSE '{}' END,
        '$.work_product_pushed'
      ) = 1
    )
  );

UPDATE work_attempts
SET terminal_state = 'delivered',
    error_class = NULL,
    error_message = NULL,
    phase = 'completed',
    status_message = 'work was pushed but finalization was rejected',
    worker_metadata_json = json_set(
      worker_metadata_json,
      '$.lane_revocation.classification', 'delivered_before_revocation',
      '$.lane_revocation.work_discarded', json('false')
    )
WHERE json_extract(worker_metadata_json, '$.lane_revocation_receipt_backfill.migration') = 45
  AND json_extract(worker_metadata_json, '$.delivery_receipt.schema') = 1
  AND json_extract(worker_metadata_json, '$.delivery_receipt.kind') = 'pushed_work_product';

UPDATE codex_sessions
SET final_state = 'delivered'
WHERE EXISTS (
  SELECT 1
  FROM work_attempts
  WHERE work_attempts.id = codex_sessions.work_attempt_id
    AND lower(trim(COALESCE(work_attempts.terminal_state, ''))) = 'delivered'
    AND json_extract(work_attempts.worker_metadata_json, '$.lane_revocation_receipt_backfill.migration') = 45
    AND json_extract(work_attempts.worker_metadata_json, '$.delivery_receipt.schema') = 1
    AND json_extract(work_attempts.worker_metadata_json, '$.delivery_receipt.kind') = 'pushed_work_product'
);

-- +goose Down
UPDATE codex_sessions
SET final_state = COALESCE((
  SELECT NULLIF(json_extract(work_attempts.worker_metadata_json, '$.lane_revocation_receipt_backfill.original_terminal_state'), '')
  FROM work_attempts
  WHERE work_attempts.id = codex_sessions.work_attempt_id
    AND json_extract(work_attempts.worker_metadata_json, '$.lane_revocation_receipt_backfill.migration') = 45
), final_state)
WHERE EXISTS (
  SELECT 1
  FROM work_attempts
  WHERE work_attempts.id = codex_sessions.work_attempt_id
    AND json_extract(work_attempts.worker_metadata_json, '$.lane_revocation_receipt_backfill.migration') = 45
);

UPDATE work_attempts
SET terminal_state = NULLIF(json_extract(worker_metadata_json, '$.lane_revocation_receipt_backfill.original_terminal_state'), ''),
    error_class = NULLIF(json_extract(worker_metadata_json, '$.lane_revocation_receipt_backfill.original_error_class'), ''),
    error_message = NULLIF(json_extract(worker_metadata_json, '$.lane_revocation_receipt_backfill.original_error_message'), ''),
    phase = NULLIF(json_extract(worker_metadata_json, '$.lane_revocation_receipt_backfill.original_phase'), ''),
    status_message = NULLIF(json_extract(worker_metadata_json, '$.lane_revocation_receipt_backfill.original_status_message'), ''),
    worker_metadata_json = json_extract(worker_metadata_json, '$.lane_revocation_receipt_backfill.original_worker_metadata_json')
WHERE json_extract(worker_metadata_json, '$.lane_revocation_receipt_backfill.migration') = 45;
