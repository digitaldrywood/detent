import json
import os
from pathlib import Path
import re
import signal
import subprocess
import sys
import time


REQUIRED_POSTGRES = {
    "github.com/digitaldrywood/detent/internal/pgtransport": (
        "TestTenantBinding",
        "TestTransactionCleanup",
    ),
    "github.com/digitaldrywood/detent/internal/testenv/pgtest": (
        "TestBinaryLifetime",
        "TestBinaryStartsUnderLongTempDirectory",
    ),
}


def supervise(arguments):
    process = None
    cancelled = 0

    def cancel(signum, frame):
        nonlocal cancelled
        cancelled = signum
        if process is not None:
            try:
                os.killpg(process.pid, signal.SIGTERM)
            except ProcessLookupError:
                pass

    signal.signal(signal.SIGTERM, cancel)
    signal.signal(signal.SIGINT, cancel)
    started = time.monotonic()
    process = subprocess.Popen(["bash", *arguments[:1], "--supervised", *arguments[1:]], start_new_session=True)
    if cancelled:
        cancel(cancelled, None)
    result = process.wait()
    result = 128 + cancelled if cancelled else (128 - result if result < 0 else result)
    print(f"check-landing finished: exit={result} wall={time.monotonic() - started:.3f}s", flush=True)
    return result


def skip_category(package, test, output):
    if not test and "no test files" in output:
        return "no-test-files"
    if re.search(r"DETENT_TEST_POSTGRES|postgres(?:ql)?.*(?:unavailable|require|not configured|not found)|initdb.*(?:not found|missing)", output, re.I):
        return "missing-postgres"
    if re.search(r"helper process|subprocess (?:helper|fixture)", output, re.I):
        return "helper-process"
    if re.search(r"windows|linux|darwin|platform|sandbox (?:enforcement|backend)|host enforcement", output, re.I):
        return "platform-exclusion"
    if "integration" in output:
        return "short-exclusion"
    if re.search(r"manual |preview|smoke|opt-in|authenticated|set DETENT_", output, re.I):
        return "opt-in-exclusion"
    return "unclassified"


def test_events(lines, emit=print):
    required = {(package, test) for package, tests in REQUIRED_POSTGRES.items() for test in tests}
    ran, passed, packages = set(), set(), set()
    output = {}
    errors = []
    skips = []
    for line in lines:
        try:
            event = json.loads(line)
        except (ValueError, TypeError):
            errors.append("invalid go test JSON output")
            continue
        if not isinstance(event, dict) or not isinstance(event.get("Package"), str):
            errors.append("invalid go test JSON event")
            continue
        package, test, action = event["Package"], event.get("Test", ""), event.get("Action")
        key = package, test
        text = event.get("Output", "")
        if text:
            emit(text.rstrip("\n"))
            output[key] = (output.get(key, "") + text)[-8192:]
        if action == "run":
            ran.add(key)
        elif action == "pass":
            if test:
                passed.add(key)
            else:
                packages.add(package)
        elif action == "fail":
            errors.append(f"failed go test event: {package}/{test}")
        elif action == "skip":
            category = skip_category(package, test, output.get(key, ""))
            skips.append((category, package, test))
            if category == "missing-postgres":
                errors.append(f"PostgreSQL scenario skipped: {package}/{test}")
            if any(package == owner and (test == name or test.startswith(name + "/")) for owner, name in required):
                errors.append(f"required PostgreSQL scenario skipped: {package}/{test}")
        if action in ("pass", "fail", "skip"):
            output.pop(key, None)
    for package, test in sorted(required):
        if (package, test) not in ran or (package, test) not in passed or package not in packages:
            errors.append(f"required PostgreSQL scenario did not run and pass: {package}/{test}")
    for category, package, test in skips:
        emit(f"skip-inventory: {category} {package}/{test}")
    for error in errors:
        emit(error)
    emit(f"PostgreSQL contract: required={len(required)} passed={len(required & ran & passed)} skips={len(skips)}")
    return 1 if errors else 0


def run_tests(arguments):
    environment = dict(os.environ)
    environment.pop("DETENT_API_TOKEN", None)
    environment["DETENT_TEST_POSTGRES"] = "binary"
    with subprocess.Popen(["go", "test", "-json", *arguments], env=environment, stdout=subprocess.PIPE, text=True) as process:
        inventory = test_events(process.stdout, lambda line: print(line, flush=True))
        result = process.wait()
    return result if result else inventory


if __name__ == "__main__":
    command, *arguments = sys.argv[1:]
    if command == "supervise":
        sys.exit(supervise(arguments))
    if command == "packages":
        for package in REQUIRED_POSTGRES:
            print("./" + package.split("/detent/", 1)[1])
    elif command == "tests":
        sys.exit(run_tests(arguments))
    elif command == "inventory":
        with Path(arguments[0]).open() as stream:
            sys.exit(test_events(stream))
