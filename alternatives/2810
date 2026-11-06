-- name: StartRequestMetricImport :exec
INSERT INTO request_metric_imports(organization_id,source_schema,snapshot_id,catalog_digest,retain_at)
SELECT sqlc.arg(organization_id),sqlc.arg(source_schema),sqlc.arg(snapshot_id),sqlc.arg(catalog_digest),sqlc.arg(retain_at)
WHERE NOT EXISTS (SELECT 1 FROM request_minutes WHERE organization_id=sqlc.arg(organization_id))
AND NOT EXISTS (SELECT 1 FROM request_hours WHERE organization_id=sqlc.arg(organization_id))
AND NOT EXISTS (SELECT 1 FROM request_days WHERE organization_id=sqlc.arg(organization_id))
AND NOT EXISTS (SELECT 1 FROM request_metric_windows WHERE organization_id=sqlc.arg(organization_id))
ON CONFLICT (organization_id) DO NOTHING;

-- name: ReadRequestMetricImport :one
SELECT * FROM request_metric_imports WHERE organization_id=sqlc.arg(organization_id);

-- name: LockRequestMetricImport :one
SELECT * FROM request_metric_imports WHERE organization_id=sqlc.arg(organization_id) FOR UPDATE;

-- name: AdvanceRequestMetricImport :exec
UPDATE request_metric_imports SET stage=sqlc.arg(stage),after_time=sqlc.arg(after_time),
after_route=sqlc.arg(after_route),after_kind=sqlc.arg(after_kind),after_caller=sqlc.arg(after_caller),
after_status=sqlc.arg(after_status),processed=processed+sqlc.arg(processed)::bigint,revision=revision+1
WHERE organization_id=sqlc.arg(organization_id);

-- name: ImportRequestMinute :exec
INSERT INTO request_minutes(organization_id,minute,route,caller_kind,caller_id,status_class,count,sum_ms,max_ms,histogram)
VALUES(sqlc.arg(organization_id),sqlc.arg(minute),sqlc.arg(route),sqlc.arg(caller_kind),sqlc.arg(caller_id),sqlc.arg(status_class),sqlc.arg(count),sqlc.arg(sum_ms),sqlc.arg(max_ms),sqlc.arg(histogram));

-- name: ImportRequestHour :exec
INSERT INTO request_hours(organization_id,hour,route,status_class,count,sum_ms,max_ms,histogram)
VALUES(sqlc.arg(organization_id),sqlc.arg(hour),sqlc.arg(route),sqlc.arg(status_class),sqlc.arg(count),sqlc.arg(sum_ms),sqlc.arg(max_ms),sqlc.arg(histogram));

-- name: ImportRequestDay :exec
INSERT INTO request_days(organization_id,day,route,caller_kind,status_class,count,sum_ms,max_ms)
VALUES(sqlc.arg(organization_id),sqlc.arg(day),sqlc.arg(route),sqlc.arg(caller_kind),sqlc.arg(status_class),sqlc.arg(count),sqlc.arg(sum_ms),sqlc.arg(max_ms));

-- name: ImportRequestMetricCaller :execrows
INSERT INTO request_metric_callers(organization_id,minute,caller_kind,caller_id)
VALUES(sqlc.arg(organization_id),sqlc.arg(minute),sqlc.arg(caller_kind),sqlc.arg(caller_id))
ON CONFLICT DO NOTHING;

-- name: CountImportedRequestMetricCaller :exec
UPDATE request_metric_windows SET caller_count=least(caller_count+1,200)
WHERE organization_id=sqlc.arg(organization_id) AND minute=sqlc.arg(minute);
