import datetime as dt
import json
import re
import subprocess
import sys
from pathlib import Path


ACTIVE = ("queued", "in_progress", "waiting", "pending", "requested")


def cron_field(field, minimum, maximum):
    values = set()
    for part in field.split(","):
        span, _, step = part.partition("/")
        stride = int(step) if step else 1
        if span == "*":
            first, last = minimum, maximum
        elif "-" in span:
            first, last = map(int, span.split("-"))
        else:
            first = int(span)
            last = maximum if step else first
        if not minimum <= first <= last <= maximum or stride < 1:
            raise ValueError("invalid release cadence cron field")
        values.update(range(first, last + 1, stride))
    return values


def cadence(now):
    workflow = Path(".github/workflows/release-cut.yml").read_text()
    schedules = re.findall(r"^\s*-\s*cron:\s*['\"]([^'\"]+)['\"]\s*$", workflow, re.M)
    if len(schedules) != 1:
        raise ValueError("release-cut must have exactly one cadence cron")
    fields = schedules[0].split()
    if len(fields) != 5:
        raise ValueError("release cadence requires five cron fields")
    minute, hour, day, month, weekday = (
        cron_field(field, low, high)
        for field, low, high in zip(fields, (0, 0, 1, 1, 0), (59, 23, 31, 12, 7))
    )
    if 7 in weekday:
        weekday.add(0)
    occurrences = []
    current = now.replace(second=0, microsecond=0)
    for _ in range(5 * 366 * 24 * 60):
        day_match = current.day in day
        weekday_match = (current.weekday() + 1) % 7 in weekday
        calendar_match = day_match and weekday_match if fields[2] == "*" or fields[4] == "*" else day_match or weekday_match
        if current.minute in minute and current.hour in hour and current.month in month and calendar_match:
            occurrences.append(current)
            if len(occurrences) == 2:
                return occurrences[0] - occurrences[1], schedules[0]
        current -= dt.timedelta(minutes=1)
    raise ValueError("release cadence has fewer than two occurrences in five years")


def timestamp(value):
    result = dt.datetime.fromisoformat(value.replace("Z", "+00:00"))
    if result.tzinfo is None:
        raise ValueError("release history timestamp must include a UTC offset")
    return result.astimezone(dt.timezone.utc)


def utc(value):
    return value.isoformat().replace("+00:00", "Z")


def api(repository, path, collection):
    result = subprocess.run(
        ["gh", "api", "--paginate", "--slurp", f"repos/{repository}/{path}"],
        check=True, capture_output=True, text=True,
    )
    pages = json.loads(result.stdout)
    return [item for page in pages for item in page[collection]]


def reference(run):
    return f"{run['html_url']} (run {run['id']}, source {run['head_branch']}, SHA {run['head_sha']})"


def stable_version(value):
    match = re.fullmatch(r"v(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)", value or "")
    return tuple(map(int, match.groups())) if match else None


def admit(repository, current_run, now):
    interval, cron = cadence(now)
    cutoff = now - interval
    print(f"Release admission UTC window: ({utc(cutoff)}, {utc(now)}]; cadence {cron!r} from release-cut.yml.")
    for status in ACTIVE:
        runs = api(repository, f"actions/runs?status={status}&per_page=100", "workflow_runs")
        for run in runs:
            workflow = run["path"].split("@", 1)[0].rsplit("/", 1)[-1]
            if str(run["id"]) == current_run or run["status"] not in ACTIVE:
                continue
            if workflow in ("release-cut.yml", "release.yml") or (workflow == "release-finalize.yml" and re.fullmatch(r"release/v\d+\.\d+", run["head_branch"] or "")):
                print(f"Release admission: no-op; active {workflow} ({run['status']}): {reference(run)}")
                return False
    runs = api(repository, "actions/workflows/release.yml/runs?status=success&per_page=100", "workflow_runs")
    completed_release = None
    for run in sorted(runs, key=lambda run: stable_version(run["head_branch"]) or (-1, -1, -1), reverse=True):
        if run["status"] != "completed" or run["conclusion"] != "success":
            continue
        version = stable_version(run["head_branch"])
        if timestamp(run["updated_at"]) <= cutoff and (version is None or completed_release is not None):
            continue
        jobs = api(repository, f"actions/runs/{run['id']}/attempts/{run['run_attempt']}/jobs?per_page=100", "jobs")
        for job in jobs:
            if job["name"] != "production / Deploy and smoke production" or job["conclusion"] != "success" or job["status"] != "completed":
                continue
            steps = {step["name"]: step for step in job["steps"]}
            if not all(steps.get(name, {}).get("conclusion") == "success" for name in ("Deploy signed release", "Smoke deployed release", "Exercise production MCP tools")):
                continue
            completed = timestamp(job["completed_at"])
            if completed > now:
                continue
            if version is not None and completed_release is None:
                completed_release = (version, run)
            if cutoff < completed <= now:
                print(f"Release admission: no-op; production deployment and smoke completed at {utc(completed)}: {reference(run)}")
                return False
    refs = subprocess.run(
        ["git", "ls-remote", "--heads", "origin", "refs/heads/release/v*"],
        check=True, capture_output=True, text=True,
    )
    branches = []
    for line in refs.stdout.splitlines():
        sha, ref = line.split()
        match = re.fullmatch(r"refs/heads/release/v(0|[1-9]\d*)\.(0|[1-9]\d*)", ref)
        if match:
            branches.append((tuple(map(int, match.groups())), ref.removeprefix("refs/heads/"), sha))
    if branches:
        series, branch, sha = max(branches)
        if completed_release is None or series > completed_release[0][:2] or (series == completed_release[0][:2] and sha != completed_release[1]["head_sha"]):
            previous = f"; last production completion: {reference(completed_release[1])}" if completed_release else "; no successful stable production release"
            print(f"Release admission: no-op; unfinished frozen series: https://github.com/{repository}/tree/{branch} (source {branch}, SHA {sha}){previous}")
            return False
    print("Release admission: proceed; no active, unfinished current series or recently completed production release.")
    return True


if __name__ == "__main__":
    try:
        permitted = admit(sys.argv[1], sys.argv[2], timestamp(sys.argv[3]))
    except (OSError, ValueError, KeyError, TypeError, subprocess.CalledProcessError) as error:
        print(f"Release admission read failed: {error}", file=sys.stderr)
        if isinstance(error, subprocess.CalledProcessError) and error.stderr:
            print(error.stderr.strip(), file=sys.stderr)
        sys.exit(1)
    sys.exit(0 if permitted else 10)
