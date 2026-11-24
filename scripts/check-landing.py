import hashlib
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import sys
import time


REQUIRED_POSTGRES = {}


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


def package_digest(packages):
    return hashlib.sha256("".join(package + "\n" for package in packages).encode()).hexdigest()


def package_set(arguments, listed):
    count = next((argument.split("=", 1)[1] for argument in arguments if argument.startswith("--package-count=")), None)
    digest = next((argument.split("=", 1)[1] for argument in arguments if argument.startswith("--package-sha256=")), None)
    rest = [argument for argument in arguments if not argument.startswith(("--package-count=", "--package-sha256="))]
    if count is None and digest is None:
        return rest
    packages = [package for package in (listed or "").split("\n") if package]
    if count != str(len(packages)) or digest != package_digest(packages):
        raise ValueError("landing package list does not match its recorded count and digest")
    return rest + packages


def contract_packages():
    return ["./" + package.split("/detent/", 1)[1] for package in REQUIRED_POSTGRES]


def thread_budget(arguments):
    index = arguments.index("-p")
    procs = int(arguments[index + 1])
    packages = [argument for argument in arguments if argument == "." or argument.startswith("./")]
    width = max(1, min(len(packages), procs))
    threads = max(4, procs)
    return [*arguments[:index], "-p", str(width), *arguments[index + 2:]], threads


def run_tests(arguments):
    environment = dict(os.environ)
    environment.pop("DETENT_API_TOKEN", None)
    listed = environment.pop("DETENT_LANDING_PACKAGES", None)
    try:
        arguments, threads = thread_budget(package_set(arguments, listed))
    except ValueError as error:
        print(error, file=sys.stderr, flush=True)
        return 1
    environment["DETENT_TEST_POSTGRES"] = "binary"
    environment["GOMAXPROCS"] = str(threads)
    with subprocess.Popen(["go", "test", "-json", *arguments], env=environment, stdout=subprocess.PIPE, text=True) as process:
        inventory = test_events(process.stdout, lambda line: print(line, flush=True))
        result = process.wait()
    return result if result else inventory


if __name__ == "__main__":
    command, *arguments = sys.argv[1:]
    if command == "supervise":
        sys.exit(supervise(arguments))
    if command == "packages":
        for package in contract_packages():
            print(package)
    elif command == "digest":
        print(package_digest(arguments))
    elif command == "tests":
        sys.exit(run_tests(arguments))
    elif command == "inventory":
        with Path(arguments[0]).open() as stream:
            sys.exit(test_events(stream))
