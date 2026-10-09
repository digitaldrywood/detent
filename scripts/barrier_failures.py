#!/usr/bin/env python3
import json
import re
import sys

PREFIX = "detent-barrier-failed:"
GO_SUMMARY = re.compile(r"^(?:FAIL|ok)\s+(\S+)")
GO_TEST = re.compile(r"^--- FAIL: (\S+)")
GO_TIMEOUT = re.compile(r"^panic: test timed out")
SPEC = re.compile(r"\[[^\]]+\] › (\S+\.spec\.[cm]?[jt]s):(\d+):\d+")


def go_failures(lines):
    failed = {}
    pending, timed_out = [], False
    for line in lines:
        if line.startswith("{"):
            try:
                event = json.loads(line)
            except ValueError:
                event = None
            if isinstance(event, dict) and event.get("Action") == "fail" and event.get("Package"):
                tests = failed.setdefault(event["Package"], set())
                test = event.get("Test") or ""
                if test and "/" not in test:
                    tests.add(test)
            continue
        if GO_TIMEOUT.match(line):
            timed_out = True
            continue
        match = GO_TEST.match(line)
        if match:
            if "/" not in match.group(1):
                pending.append(match.group(1))
            continue
        match = GO_SUMMARY.match(line)
        if match:
            if line.startswith("FAIL"):
                tests = failed.setdefault(match.group(1), set())
                if timed_out:
                    tests.add("*")
                tests.update(pending)
            pending, timed_out = [], False
    items = []
    for package in sorted(failed):
        tests = failed[package]
        if not tests or "*" in tests:
            items.append(package)
        else:
            items.append(package + ":" + "|".join(sorted(tests)))
    return items


def browser_failures(lines):
    items, summary = [], False
    for line in lines:
        if re.match(r"^\s*\d+ failed\b", line):
            summary = True
            continue
        if summary:
            if re.match(r"^\s*\d+ (passed|flaky|skipped|did not run)\b", line):
                break
            match = SPEC.search(line)
            if match:
                item = match.group(1)
                if item not in items:
                    items.append(item)
    return items


def select(lines, scope):
    items = []
    for line in lines:
        line = line.strip()
        if not line.startswith(PREFIX):
            continue
        fields = line[len(PREFIX):].split()
        if fields and fields[0] == scope:
            items.extend(fields[1:])
    return items


def main(argv):
    if len(argv) == 3 and argv[0] == "extract":
        with open(argv[2], encoding="utf-8", errors="replace") as log:
            lines = log.read().splitlines()
        items = go_failures(lines) if argv[1] == "go" else browser_failures(lines)
    elif len(argv) == 2 and argv[0] == "select":
        items = select(sys.stdin.read().splitlines(), argv[1])
    else:
        print("usage: barrier_failures.py extract go|browser LOG | select go|browser", file=sys.stderr)
        return 2
    print("\n".join(items))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
