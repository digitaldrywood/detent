import concurrent.futures
import json
import os
from pathlib import Path
import re
import shlex
import shutil
import subprocess
import sys
import tempfile
import time


STEPS = (
    "app", "generated", "migrations", "invariants", "build",
    "windows-build-amd64", "windows-vet", "windows-build-arm64", "vet", "lint",
    "scripts", "nilaway-changed", "race", "subprocess-lifecycle", "full",
    "security", "browser",
)
PRIORITY = (
    "app", "generated", "full", "race", "browser", "invariants", "lint",
    "nilaway-changed", "scripts", "migrations", "build", "windows-build-amd64",
    "windows-vet", "windows-build-arm64", "vet", "subprocess-lifecycle", "security",
)
SMOKE_SPECS = (
    "tests/visual/magic-link-auth.spec.js",
    "tests/visual/hub-login.spec.js",
    "tests/visual/board-navigation.spec.js",
    "tests/visual/change-review.spec.js",
    "tests/visual/conversation-actions.spec.js",
    "tests/visual/mobile.spec.js",
)


def dependencies(step):
    if step in ("app", "generated", "scripts", "migrations"):
        return set()
    if step == "browser":
        return {"app", "generated"}
    return {"app", "generated", "browser-assets"}


def process_budget(step, total):
    if step == "full":
        return max(1, total // 2)
    if step in ("race", "browser", "app"):
        return max(1, total // 4)
    return 1


def step_environment(budget):
    env = dict(os.environ, TEST_PROCS=str(budget), GOMAXPROCS="1")
    env["GOFLAGS"] = shlex.join([*shlex.split(env.get("GOFLAGS", "")), f"-p={budget}"])
    return env


def run_step(step, budget, directory, browser_ready):
    started = time.monotonic()
    result = 1
    with (directory / f"{step}.log").open("w") as log:
        try:
            command = ["bash", "scripts/check-release.sh", str(budget), step]
            env = step_environment(budget)
            if step == "browser":
                read_fd, write_fd = os.pipe()
                try:
                    env["DETENT_RELEASE_ASSETS_READY_FD"] = str(write_fd)
                    with subprocess.Popen(command, env=env, stdout=log, stderr=subprocess.STDOUT,
                                          pass_fds=(write_fd,)) as process:
                        os.close(write_fd)
                        write_fd = None
                        with os.fdopen(read_fd) as ready:
                            read_fd = None
                            ready.readline()
                        browser_ready.set_result(None)
                        result = process.wait()
                finally:
                    for descriptor in (read_fd, write_fd):
                        if descriptor is not None:
                            os.close(descriptor)
            else:
                result = subprocess.run(command, env=env, stdout=log, stderr=subprocess.STDOUT,
                                        check=False).returncode
        except (OSError, ValueError) as error:
            print(f"Could not run {step}: {error}", file=log)
        finally:
            if step == "browser" and not browser_ready.done():
                browser_ready.set_result(None)
    return result, time.monotonic() - started


def run_checks(total):
    scratch = Path(os.environ.get("TMPDIR") or os.environ.get("TMP") or os.environ.get("TEMP") or "tmp")
    scratch.mkdir(parents=True, exist_ok=True)
    directory = Path(tempfile.mkdtemp(prefix="check-release.", dir=scratch)).resolve()
    print(f"Release checks: process budget={total}; logs: {directory}", flush=True)
    pending = list(PRIORITY)
    running = {}
    completed = {}
    satisfied = set()
    browser_ready = concurrent.futures.Future()
    browser_started = False
    available = total
    with concurrent.futures.ThreadPoolExecutor(max_workers=min(total, len(STEPS))) as executor:
        while pending or running:
            for step in pending[:]:
                budget = process_budget(step, total)
                if budget > available or not dependencies(step).issubset(satisfied):
                    continue
                print(f"Starting {step}: budget={budget}; log: {directory / (step + '.log')}", flush=True)
                running[executor.submit(run_step, step, budget, directory, browser_ready)] = (step, budget)
                if step == "browser":
                    browser_started = True
                available -= budget
                pending.remove(step)
            waiting = set(running)
            if browser_started and "browser-assets" not in satisfied:
                waiting.add(browser_ready)
            done, _ = concurrent.futures.wait(waiting, return_when=concurrent.futures.FIRST_COMPLETED)
            if browser_ready in done:
                satisfied.add("browser-assets")
                done.remove(browser_ready)
            for future in done:
                step, budget = running.pop(future)
                completed[step] = future.result()
                satisfied.add(step)
                available += budget
                print(f"\n--- {step} ---", flush=True)
                with (directory / f"{step}.log").open() as log:
                    shutil.copyfileobj(log, sys.stdout)
                result, elapsed = completed[step]
                print(f"Finished {step}: exit={result} wall={elapsed:.1f}s", flush=True)
    print("\nRelease check summary:")
    for step in STEPS:
        result, elapsed = completed[step]
        print(f"  {step}: {'PASS' if result == 0 else 'FAIL'} exit={result} wall={elapsed:.1f}s log={directory / (step + '.log')}")
    failed = any(result != 0 for result, _ in completed.values())
    if not failed:
        print("Release checks passed.")
    return int(failed)


def git(*args):
    return subprocess.check_output(["git", *args], stderr=subprocess.PIPE).decode()


def validated_release_base():
    if git("rev-parse", "--is-shallow-repository").strip() != "false":
        return None
    tags = git("tag", "--merged", "HEAD", "--list", "v*", "--sort=-version:refname").splitlines()
    validated = {}
    for tag in tags:
        if not re.fullmatch(r"v(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)", tag):
            continue
        ref = f"refs/tags/{tag}"
        if git("cat-file", "-t", ref).strip() != "tag":
            continue
        contents = git("for-each-ref", "--format=%(contents)", ref)
        match = re.search(r"<!-- detent-release-provenance:(.*?) -->", contents)
        if not match:
            continue
        try:
            provenance = json.loads(match[1])
            commit = git("rev-parse", f"{ref}^{{commit}}").strip()
            checks = provenance["checks"]
            if (provenance["schema"] == 1 and provenance["tag"] == tag
                    and provenance["commit"] == commit
                    and any(check["name"] == "scheduled-full-ci"
                            and check["status"] == "completed" and check["conclusion"] == "success"
                            and isinstance(check["status_id"], int) and check["status_id"] > 0
                            for check in checks)):
                validated.setdefault(commit, tag)
        except (KeyError, TypeError, ValueError):
            continue
    if not validated:
        return None
    for commit in git("rev-list", "--topo-order", "HEAD").splitlines():
        if commit in validated:
            print(f"Browser comparison: validated release={validated[commit]} base={commit}", file=sys.stderr)
            return commit
    return None


def browser_specs():
    try:
        base = validated_release_base()
        if base is None:
            print("Browser comparison: no validated reachable release; running full suite.", file=sys.stderr)
            return ["tests/visual"]
        changed = set(git("diff", "--name-only", "--no-renames", "-z", base, "HEAD").split("\0"))
        changed.update(git("diff", "--name-only", "--no-renames", "-z", "HEAD").split("\0"))
        changed.update(git("ls-files", "--others", "--exclude-standard", "-z").split("\0"))
        changed.discard("")
        for path in sorted(changed):
            name = Path(path).name
            if (path.startswith("docs/") or path.endswith(("_test.go", ".md"))
                    or re.fullmatch(r"(?:test_.+|.+_test)\.py", name)
                    or (path.startswith("scripts/") and re.fullmatch(r"(?:test_.+|.+_test)\.(?:sh|bash|bats)", name))):
                continue
            print(f"Browser comparison: browser-relevant or unknown input changed ({path}); running full suite.", file=sys.stderr)
            return ["tests/visual"]
        if not all(Path(path).is_file() for path in SMOKE_SPECS):
            print("Browser comparison: smoke spec missing; running full suite.", file=sys.stderr)
            return ["tests/visual"]
        return sorted(SMOKE_SPECS)
    except (OSError, subprocess.CalledProcessError, UnicodeError) as error:
        print(f"Browser comparison unavailable ({error}); running full suite.", file=sys.stderr)
        return ["tests/visual"]


if __name__ == "__main__":
    if sys.argv[1:] == ["browser-specs"]:
        print("\n".join(browser_specs()))
    else:
        raise SystemExit(run_checks(int(sys.argv[1])))
