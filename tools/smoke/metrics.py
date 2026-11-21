from collections import Counter, defaultdict
from datetime import datetime
import json
import re

ATTEMPTS_SQL = """
BEGIN READ ONLY;
SELECT COALESCE(json_agg(snapshot ORDER BY snapshot.started_at, snapshot.attempt_id), '[]')
FROM (
    SELECT a.id AS attempt_id, a.project_id, a.lease_id, a.fencing_token, a.status, a.started_at,
           COALESCE((SELECT json_agg(json_build_object(
               'session_id', s.id, 'role', s.role, 'thread', s.provider_thread_id,
               'provider_session', s.provider_session_id,
               'started_at', s.started_at, 'completed_at', s.completed_at)
               ORDER BY s.started_at, s.id)
               FROM runner_sessions s
               WHERE (s.organization_id, s.attempt_id) = (a.organization_id, a.id)
                 AND s.backend_kind = 'codex'), '[]') AS sessions
    FROM native_attempts a
) snapshot;
ROLLBACK;
"""

POST = re.compile(r"^SMOKE_SESSION_POST (lease_[a-zA-Z0-9_]+) (\d+)$")
RECEIPT = re.compile(r"^SMOKE_PHASE_ACK (attempt_[a-zA-Z0-9_]+) ([a-z0-9_]+) (\S+)$")


def instant(value):
    seconds = re.sub(r"\.(\d+)", lambda match: "." + match[1][:6].ljust(6, "0"), value)
    return datetime.fromisoformat(seconds.replace("Z", "+00:00"))


def completions(paths):
    by_thread = defaultdict(set)
    for path in paths:
        thread = None
        with path.open() as stream:
            for line in stream:
                try:
                    row = json.loads(line)
                except ValueError:
                    continue
                body = row.get("payload") or {}
                if row.get("type") == "session_meta":
                    thread = body.get("id")
                elif thread and row.get("type") == "event_msg" and body.get("type") == "task_complete":
                    by_thread[thread].add((row["timestamp"], body.get("turn_id")))
    return by_thread


def implementation_gaps(attempt, receipts, provider_events):
    candidates = set()
    for session in attempt["sessions"]:
        if session["role"] not in {"code", "implement", "rework"}:
            continue
        started = instant(session["started_at"])
        completed = instant(session["completed_at"]) if session["completed_at"] else None
        for thread in {session["thread"], session["provider_session"]}:
            for timestamp, turn in provider_events.get(thread, ()):
                observed = instant(timestamp)
                if started <= observed and (completed is None or observed <= completed):
                    candidates.add((observed, turn or "", session["session_id"]))
    if not receipts:
        return [], "no acknowledged implementation receipt"
    if not candidates:
        return [], "no matched implementation task_complete"
    matched = []
    previous = None
    for value in sorted(set(receipts), key=instant):
        received = instant(value)
        suitable = [candidate for candidate in candidates if candidate[0] <= received and
                    (previous is None or candidate[0] > previous)]
        previous = received
        if suitable:
            completed, turn, session = max(suitable)
            matched.append({"task_complete_at": completed.isoformat(), "receipt_ack_at": value,
                            "seconds": round((received - completed).total_seconds(), 3),
                            "turn_id": turn or None, "session_id": session})
    return matched, None if matched else "no implementation task_complete preceded the receipt"


def collect(stack):
    raw = stack.command([stack.PG / "psql", "-X", "-qAt", "-v", "ON_ERROR_STOP=1",
                         "-h", stack.SOCKETS, "-p", stack.available_ports()["postgres"],
                         "-U", "postgres", "-d", "detent"], data=ATTEMPTS_SQL)
    attempts = json.loads(raw)
    posts = Counter()
    receipts = defaultdict(list)
    runner_log = stack.LOGS / "runner.log"
    if runner_log.exists():
        with runner_log.open() as stream:
            for line in stream:
                post = POST.fullmatch(line.rstrip())
                if post:
                    posts[(post[1], int(post[2]))] += 1
                receipt = RECEIPT.fullmatch(line.rstrip())
                if receipt and receipt[2] == "implement":
                    receipts[receipt[1]].append(receipt[3])
    provider_events = completions(stack.provider_sessions(0))
    for attempt in attempts:
        key = attempt["lease_id"], attempt["fencing_token"]
        attempt["session_posts"] = posts.pop(key, 0)
        gaps, reason = implementation_gaps(attempt, receipts[attempt["attempt_id"]], provider_events)
        attempt["task_complete_to_receipt"] = gaps
        if reason:
            attempt["gap_unavailable"] = reason
        del attempt["sessions"]
    return {"attempts": attempts, "unmatched_session_posts": sum(posts.values())}
