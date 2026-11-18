#!/usr/bin/env python3

import argparse
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile


def main():
    parser = argparse.ArgumentParser(
        description="Run a retained manual Go check with its package's test fixtures."
    )
    parser.add_argument("--compile", action="store_true")
    parser.add_argument("--timeout", default="40m")
    parser.add_argument("package")
    parser.add_argument("check")
    args = parser.parse_args()
    root = Path(__file__).resolve().parent.parent
    package = (root / args.package).resolve()
    if not package.is_relative_to(root):
        parser.error("package must be inside this worktree")
    sources = sorted((package / "testdata" / "manual").glob("*_test.go"))
    if not sources:
        parser.error("package has no retained manual checks")
    target = "./" + package.relative_to(root).as_posix()
    scratch = root / "tmp"
    scratch.mkdir(exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="manual-go-check-", dir=scratch) as work:
        work = Path(work)
        overlay = work / "overlay.json"
        replacements = {
            str(package / ("manual_" + source.name)): str(source)
            for source in sources
        }
        overlay.write_text(json.dumps({"Replace": replacements}))
        env = os.environ.copy()
        env["DETENT_TEST_POSTGRES"] = "binary"
        postgres = {
            "linux": "/usr/lib/postgresql/17/bin",
            "darwin": "/opt/homebrew/opt/postgresql@17/bin",
        }.get(sys.platform)
        if postgres:
            env["PATH"] = postgres + os.pathsep + env.get("PATH", "")
        for key in ("TMPDIR", "TMP", "TEMP"):
            env[key] = str(work)
        coverage = work / "coverage"
        coverage.mkdir()
        env["GOCOVERDIR"] = str(coverage)
        listed = subprocess.run(
            ["go", "list", "-json", "-overlay=" + str(overlay), target],
            cwd=root,
            env=env,
            text=True,
            stdout=subprocess.PIPE,
        )
        if listed.returncode:
            return listed.returncode
        enabled = set(json.loads(listed.stdout).get("TestGoFiles", []))
        checks = set()
        for virtual, source in replacements.items():
            if Path(virtual).name in enabled:
                checks.update(
                    re.findall(
                        r"^func (Test\w+)\(t \*testing\.T\)",
                        Path(source).read_text(),
                        re.MULTILINE,
                    )
                )
        if args.check not in checks:
            parser.error("check is unavailable on this platform: " + args.check)
        command = [
            "go",
            "test",
            "-overlay=" + str(overlay),
            "-failfast",
            "-p=2",
            "-parallel=2",
            "-timeout=" + args.timeout,
        ]
        if args.compile:
            command += ["-c", "-o", str(work / "manual.test")]
        else:
            command += ["-v", "-run=^" + re.escape(args.check) + "$"]
        return subprocess.run(command + [target], cwd=root, env=env).returncode


if __name__ == "__main__":
    sys.exit(main())
