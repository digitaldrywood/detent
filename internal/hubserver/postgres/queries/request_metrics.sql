-- name: LockRequestMetricWindow :exec
INSERT INTO request_metric_windows(organization_id,minute) VALUES(sqlc.arg(organization_id),sqlc.arg(minute))
ON CONFLICT(organization_id,minute) DO UPDATE SET caller_count=request_metric_windows.caller_count;

-- name: RequestMetricCallerExists :one
SELECT EXISTS(SELECT 1 FROM request_metric_callers WHERE organization_id=sqlc.arg(organization_id) AND minute=sqlc.arg(minute) AND caller_kind=sqlc.arg(caller_kind) AND caller_id=sqlc.arg(caller_id));

-- name: ReserveRequestMetricCaller :execrows
UPDATE request_metric_windows SET caller_count=caller_count+1
WHERE organization_id=sqlc.arg(organization_id) AND minute=sqlc.arg(minute) AND caller_count<200;

-- name: InsertRequestMetricCaller :exec
INSERT INTO request_metric_callers(organization_id,minute,caller_kind,caller_id)
VALUES(sqlc.arg(organization_id),sqlc.arg(minute),sqlc.arg(caller_kind),sqlc.arg(caller_id));

-- name: MergeRequestMinute :exec
INSERT INTO request_minutes(organization_id,minute,route,caller_kind,caller_id,status_class,count,sum_ms,max_ms,histogram)
VALUES(sqlc.arg(organization_id),sqlc.arg(minute),sqlc.arg(route),sqlc.arg(caller_kind),sqlc.arg(caller_id),sqlc.arg(status_class),sqlc.arg(count),sqlc.arg(sum_ms),sqlc.arg(max_ms),sqlc.arg(histogram))
ON CONFLICT(organization_id,minute,route,caller_kind,caller_id,status_class) DO UPDATE SET
 count=request_minutes.count+excluded.count,
 sum_ms=request_minutes.sum_ms+excluded.sum_ms,
 max_ms=greatest(request_minutes.max_ms,excluded.max_ms),
 histogram=request_histogram_add(request_minutes.histogram,excluded.histogram);

-- name: ListRequestMinutes :many
SELECT * FROM request_minutes WHERE organization_id=sqlc.arg(organization_id)
AND minute>=sqlc.arg(from_time) AND minute<sqlc.arg(to_time)
AND (minute,route,caller_kind,caller_id,status_class) > (sqlc.arg(after_minute),sqlc.arg(after_route)::text,sqlc.arg(after_caller_kind)::text,sqlc.arg(after_caller_id)::text,sqlc.arg(after_status_class)::bigint)
ORDER BY minute,route,caller_kind,caller_id,status_class LIMIT sqlc.arg(page_limit);

-- name: MergeRequestHour :exec
INSERT INTO request_hours(organization_id,hour,route,status_class,count,sum_ms,max_ms,histogram)
VALUES(sqlc.arg(organization_id),sqlc.arg(hour),sqlc.arg(route),sqlc.arg(status_class),sqlc.arg(count),sqlc.arg(sum_ms),sqlc.arg(max_ms),sqlc.arg(histogram))
ON CONFLICT(organization_id,hour,route,status_class) DO UPDATE SET
 count=request_hours.count+excluded.count,
 sum_ms=request_hours.sum_ms+excluded.sum_ms,
 max_ms=greatest(request_hours.max_ms,excluded.max_ms),
 histogram=request_histogram_add(request_hours.histogram,excluded.histogram);

-- name: ListRequestHours :many
SELECT * FROM request_hours WHERE organization_id=sqlc.arg(organization_id)
AND hour>=sqlc.arg(from_time) AND hour<sqlc.arg(to_time)
AND (hour,route,status_class) > (sqlc.arg(after_hour),sqlc.arg(after_route)::text,sqlc.arg(after_status_class)::bigint)
ORDER BY hour,route,status_class LIMIT sqlc.arg(page_limit);

-- name: ExpireRequestHours :execrows
WITH expired AS (SELECT x.organization_id,x.hour,x.route,x.status_class FROM request_hours x
WHERE x.organization_id=sqlc.arg(organization_id) AND x.hour<sqlc.arg(cutoff)
ORDER BY hour,route,status_class LIMIT 200 FOR UPDATE SKIP LOCKED)
DELETE FROM request_hours t USING expired e WHERE t.organization_id=e.organization_id AND t.hour=e.hour AND t.route=e.route AND t.status_class=e.status_class;

-- name: MergeRequestDay :exec
INSERT INTO request_days(organization_id,day,route,caller_kind,status_class,count,sum_ms,max_ms)
VALUES(sqlc.arg(organization_id),sqlc.arg(day),sqlc.arg(route),sqlc.arg(caller_kind),sqlc.arg(status_class),sqlc.arg(count),sqlc.arg(sum_ms),sqlc.arg(max_ms))
ON CONFLICT(organization_id,day,route,caller_kind,status_class) DO UPDATE SET
 count=request_days.count+excluded.count,
 sum_ms=request_days.sum_ms+excluded.sum_ms,
 max_ms=greatest(request_days.max_ms,excluded.max_ms);

-- name: ListRequestDays :many
SELECT * FROM request_days WHERE organization_id=sqlc.arg(organization_id)
AND day>=sqlc.arg(from_time) AND day<sqlc.arg(to_time)
AND (day,route,caller_kind,status_class) > (sqlc.arg(after_day),sqlc.arg(after_route)::text,sqlc.arg(after_caller_kind)::text,sqlc.arg(after_status_class)::bigint)
ORDER BY day,route,caller_kind,status_class LIMIT sqlc.arg(page_limit);

-- name: ExpireRequestDays :execrows
WITH expired AS (SELECT x.organization_id,x.day,x.route,x.caller_kind,x.status_class FROM request_days x
WHERE x.organization_id=sqlc.arg(organization_id) AND x.day<sqlc.arg(cutoff)
ORDER BY day,route,caller_kind,status_class LIMIT 200 FOR UPDATE SKIP LOCKED)
DELETE FROM request_days t USING expired e WHERE t.organization_id=e.organization_id AND t.day=e.day AND t.route=e.route AND t.caller_kind=e.caller_kind AND t.status_class=e.status_class;

-- name: TakeExpiredRequestMinute :one
WITH expired AS (SELECT x.organization_id,x.minute,x.route,x.caller_kind,x.caller_id,x.status_class FROM request_minutes x
WHERE x.organization_id=sqlc.arg(organization_id) AND x.minute<sqlc.arg(cutoff)
ORDER BY minute,route,caller_kind,caller_id,status_class LIMIT 1 FOR UPDATE SKIP LOCKED)
DELETE FROM request_minutes t USING expired e
WHERE t.organization_id=e.organization_id AND t.minute=e.minute AND t.route=e.route AND t.caller_kind=e.caller_kind AND t.caller_id=e.caller_id AND t.status_class=e.status_class
RETURNING t.*;

-- name: ExpireRequestMetricWindows :execrows
WITH expired AS (SELECT w.organization_id,w.minute FROM request_metric_windows w
WHERE w.organization_id=sqlc.arg(organization_id) AND w.minute<sqlc.arg(cutoff)
AND NOT EXISTS(SELECT 1 FROM request_minutes m WHERE m.organization_id=w.organization_id AND m.minute=w.minute)
ORDER BY w.minute LIMIT 1 FOR UPDATE SKIP LOCKED)
DELETE FROM request_metric_windows t USING expired e WHERE t.organization_id=e.organization_id AND t.minute=e.minute;
