-- Inputs: validation_events(payload TEXT), validation_report_params(host,
-- worker_host, day), and the host's read-only work_attempts history.
-- All derived objects are TEMP; this never changes the history database.
CREATE TEMP VIEW validation_bounds AS
SELECT host, worker_host, unixepoch(day) AS lo,
       min(unixepoch(day, '+1 day'), unixepoch('now')) AS hi
FROM validation_report_params;

CREATE TEMP VIEW validation_parsed AS
SELECT json_extract(payload, '$.run_id') AS run_id,
       json_extract(payload, '$.host') AS host,
       json_extract(payload, '$.identifier') AS identifier,
       json_extract(payload, '$.phase') AS phase,
       unixepoch(json_extract(payload, '$.started_at'), 'subsec') AS started,
       unixepoch(json_extract(payload, '$.at'), 'subsec') AS observed,
       json_extract(payload, '$.wait_seconds') AS wait_seconds,
       json_extract(payload, '$.hold_seconds') AS hold_seconds,
       json_extract(payload, '$.queue_size') AS queue_size
FROM validation_events WHERE json_valid(payload);

CREATE TEMP VIEW validation_runs AS
SELECT host, run_id, identifier, started,
       max(observed) AS observed,
       max(wait_seconds) AS wait_seconds,
       max(hold_seconds) AS hold_seconds,
       max(queue_size) AS max_observed_queue,
       max(phase IN ('running', 'passed', 'failed', 'canceled', 'wait_failed')) AS wait_complete,
       max(phase IN ('passed', 'failed', 'canceled', 'wait_failed')) AS terminal
FROM validation_parsed
WHERE run_id IS NOT NULL AND host = (SELECT host FROM validation_bounds)
GROUP BY host, run_id;

-- Wait durations of interrupted records are observed lower bounds. Never
-- extend an orphaned waiting record through the rest of the report day.
CREATE TEMP VIEW validation_waits AS
SELECT r.*, max(started, b.lo) AS lo,
       min(started + wait_seconds, b.hi) AS hi
FROM validation_runs r CROSS JOIN validation_bounds b
WHERE started < b.hi AND started + wait_seconds >= b.lo;

CREATE TEMP VIEW validation_attempts AS
SELECT a.id, a.identifier, unixepoch(a.started_at, 'subsec') AS started,
       unixepoch(coalesce(a.completed_at, a.heartbeat_at, a.started_at), 'subsec') AS ended,
       max(unixepoch(a.started_at, 'subsec'), b.lo) AS lo,
       min(unixepoch(coalesce(a.completed_at, a.heartbeat_at, a.started_at), 'subsec'), b.hi) AS hi
FROM work_attempts a CROSS JOIN validation_bounds b
WHERE coalesce(a.worker_host, '') = b.worker_host
  AND a.worker_type IN ('agent', 'code', 'rework', 'implementation')
  AND unixepoch(a.started_at, 'subsec') < b.hi
  AND unixepoch(coalesce(a.completed_at, a.heartbeat_at, a.started_at), 'subsec') > b.lo;

-- Only uniquely matched issue+host+time intervals are attributable. Multiple
-- attempts for the same issue must not silently multiply the numerator.
CREATE TEMP VIEW validation_matches AS
SELECT w.run_id, min(a.id) AS attempt_id, count(a.id) AS matches
FROM validation_waits w LEFT JOIN validation_attempts a
 ON a.identifier = w.identifier AND a.started <= w.started AND a.ended >= w.started
GROUP BY w.run_id;

CREATE TEMP VIEW validation_attributed_waits AS
SELECT a.id, max(w.lo, a.lo) AS lo, min(w.hi, a.hi) AS hi
FROM validation_waits w JOIN validation_matches m USING (run_id)
JOIN validation_attempts a ON a.id = m.attempt_id
WHERE m.matches = 1 AND min(w.hi, a.hi) > max(w.lo, a.lo);

-- Union concurrent/retried gate waits within each attempt before summing.
CREATE TEMP VIEW validation_wait_union AS
WITH ordered AS (
 SELECT *, max(hi) OVER (PARTITION BY id ORDER BY lo, hi
   ROWS BETWEEN UNBOUNDED PRECEDING AND 1 PRECEDING) AS prior_hi
 FROM validation_attributed_waits
)
SELECT id, sum(max(0, hi - max(lo, coalesce(prior_hi, lo)))) AS seconds
FROM ordered GROUP BY id;

CREATE TEMP VIEW validation_queue_intervals AS
WITH events AS (
 SELECT lo AS at, 1 AS delta FROM validation_waits WHERE hi > lo
 UNION ALL SELECT hi, -1 FROM validation_waits WHERE hi > lo
), grouped AS (
 SELECT at, sum(delta) AS delta FROM events GROUP BY at
), points AS (
 SELECT at, lead(at) OVER (ORDER BY at) AS next_at,
        sum(delta) OVER (ORDER BY at) AS depth FROM grouped
)
SELECT depth, sum(next_at - at) AS seconds
FROM points WHERE depth > 0 AND next_at > at GROUP BY depth;

-- Output 1: coverage and dispatched-session share, using observed attempt
-- intervals. A missing fraction means no denominator, not zero cost.
SELECT (SELECT host FROM validation_bounds) AS host,
       (SELECT day FROM validation_report_params) AS utc_day,
       (SELECT count(*) FROM validation_events WHERE NOT json_valid(payload)) AS malformed_records,
       (SELECT count(*) FROM validation_parsed WHERE run_id IS NULL) AS legacy_records,
       (SELECT count(*) FROM validation_waits) AS observed_runs,
       (SELECT count(*) FROM validation_waits WHERE terminal = 0) AS incomplete_runs,
       (SELECT count(*) FROM validation_matches WHERE matches != 1) AS unattributed_runs,
       (SELECT sum(hi - lo) FROM validation_attempts) AS dispatched_seconds,
       (SELECT coalesce(sum(seconds), 0) FROM validation_wait_union) AS attributed_wait_seconds,
       CASE WHEN (SELECT count(*) FROM validation_matches WHERE matches = 1) > 0 THEN
       (SELECT coalesce(sum(seconds), 0) FROM validation_wait_union) * 1.0 /
         nullif((SELECT sum(hi - lo) FROM validation_attempts), 0) END AS observed_wait_fraction;

-- Output 2: time-weighted queue depth while somebody is waiting (registration
-- included), scoped to this event file/lock, with simultaneous edges coalesced.
WITH ranked AS (
 SELECT *, sum(seconds) OVER (ORDER BY depth) AS cumulative,
        sum(seconds) OVER () AS total FROM validation_queue_intervals
)
SELECT sum(seconds) AS seconds_with_waiters,
       sum(depth * seconds) / nullif(sum(seconds), 0) AS mean_busy_queue_depth,
       min(CASE WHEN cumulative >= total * 0.5 THEN depth END) AS median_busy_queue_depth,
       min(CASE WHEN cumulative >= total * 0.9 THEN depth END) AS p90_busy_queue_depth,
       max(depth) AS max_instrumented_queue_depth,
       (SELECT max(p.queue_size) FROM validation_parsed p
        CROSS JOIN validation_bounds b
        WHERE p.host = b.host AND p.observed >= b.lo AND p.observed < b.hi)
         AS max_observed_queue_depth
FROM ranked;

-- Output 3: durable per-run detail, including holds for runs that entered the
-- day before it. Durations here are whole-run values, not daily totals.
SELECT r.run_id, r.identifier, datetime(r.started, 'unixepoch') AS started_utc,
       r.wait_seconds, r.hold_seconds, r.wait_complete, r.terminal,
       r.max_observed_queue
FROM validation_runs r CROSS JOIN validation_bounds b
WHERE r.started < b.hi AND r.observed >= b.lo
ORDER BY r.started, r.run_id;
