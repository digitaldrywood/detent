import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parent.parent
TOOL = r'''#!/usr/bin/env python3
import fcntl
import json
import os
from pathlib import Path
import sys
import time

args = sys.argv[1:]
name = Path(sys.argv[0]).name
if name == "git":
    if args[0] == "merge-base":
        print("base")
    elif args[0] == "diff":
        sys.exit(int(Path("app-changed").exists()))
    elif args[0] == "rev-parse":
        print("head")
    sys.exit(0)
if args[:1] == ["env"]:
    print({"GOHOSTOS": "fixture", "GOHOSTARCH": "fixture", "GOVERSION": "fixture"}[args[1]])
    sys.exit(0)
if name == "lint":
    stage = "lint"
    budget = int(next(arg.split("=")[1] for arg in args if arg.startswith("--concurrency=")))
elif args[0] in ("build", "vet", "test"):
    stage = "invariants" if "./internal/invariants" in args else {"test": "unit-short"}.get(args[0], args[0])
    budget = int(args[args.index("-p") + 1])
else:
    stage = "docs" if "./internal/config/cmd/configdoc" in args else "migrations" if "./tools/migrationcheck" in args else "sqlc"
    budget = int(os.environ["GOMAXPROCS"])

def record(action):
    with open("trace", "a") as output:
        fcntl.flock(output, fcntl.LOCK_EX)
        output.write(json.dumps({"stage": stage, "action": action, "budget": budget, "gomaxprocs": os.environ["GOMAXPROCS"]}) + "\n")

record("start")
if stage in ("lint", "vet", "build", "unit-short"):
    Path("ready-" + stage).touch()
    group = ("lint", "vet", "build", "unit-short")
    workers = min(4, int(os.environ["PROBE_BUDGET"]))
    offset = group.index(stage) // workers * workers
    expected = group[offset:offset + workers]
    deadline = time.monotonic() + 10
    while not all(Path("ready-" + item).exists() for item in expected):
        if time.monotonic() > deadline:
            sys.exit("independent stages did not overlap")
        time.sleep(0.01)
record("end")
if stage == os.environ.get("PROBE_FAILURE"):
    sys.exit("deliberate " + stage + " failure")
'''


@unittest.skipIf(os.name == "nt", "landing gate requires a POSIX shell")
class LandingGateTest(unittest.TestCase):
    def test_stage_budget_and_failures(self):
        scratch = next((os.environ[key] for key in ("TMPDIR", "TMP", "TEMP") if os.environ.get(key)), None)
        self.assertIsNotNone(scratch, "a supplied scratch directory is required")
        for budget, failure in ((1, ""), (2, ""), (4, ""), (7, ""), (4, "lint"), (4, "unit-short"), (4, "docs"), (4, "sqlc")):
            with self.subTest(budget=budget, failure=failure), tempfile.TemporaryDirectory(dir=scratch) as directory:
                root = Path(directory)
                (root / "scripts").mkdir()
                for name in ("Makefile", ".golangci-version", "go.mod", "scripts/check-land.sh", "scripts/check-evidence.sh"):
                    shutil.copy2(ROOT / name, root / name)
                for name in ("go", "git", "lint"):
                    path = root / name
                    path.write_text(TOOL)
                    path.chmod(0o700)
                env = {key: value for key, value in os.environ.items() if key not in ("MAKEFLAGS", "MFLAGS", "MAKEOVERRIDES")}
                env.update(PATH=str(root) + os.pathsep + env["PATH"], PROBE_BUDGET=str(budget), PROBE_FAILURE=failure)
                result = subprocess.run(["make", "check-land", f"TEST_PROCS={budget}", f"GOLANGCI_LINT={root / 'lint'}"], cwd=root, env=env, capture_output=True, text=True, timeout=30)
                self.assertEqual(result.returncode == 0, not failure, result.stdout + result.stderr)
                self.assertEqual("Landing checks passed." in result.stdout, not failure)
                events = [json.loads(line) for line in (root / "trace").read_text().splitlines()]
                starts = [event["stage"] for event in events if event["action"] == "start"]
                self.assertEqual(starts.count("docs"), 1)
                if failure in ("docs", "sqlc"):
                    self.assertFalse(set(starts) & {"lint", "vet", "build", "unit-short"})
                    continue
                self.assertEqual(starts[:2], ["docs", "sqlc"])
                active = {}
                for event in events:
                    if event["stage"] not in ("lint", "vet", "build", "unit-short"):
                        continue
                    self.assertEqual(int(event["gomaxprocs"]), event["budget"])
                    if event["action"] == "start":
                        active[event["stage"]] = event["budget"]
                        self.assertLessEqual(sum(active.values()), budget)
                    else:
                        del active[event["stage"]]
                self.assertFalse(active)
                self.assertEqual(set(starts[2:6]), {"lint", "vet", "build", "unit-short"})
                if failure:
                    self.assertNotIn("invariants", starts)
                else:
                    self.assertEqual(starts[6:], ["invariants", "migrations"])
                self.assertNotIn("npm", result.stdout)
                self.assertNotIn("nilaway", result.stdout)
                self.assertFalse((root / "static/app/conversation/app.js").exists())
                evidence = [json.loads(line.split(": ", 1)[1]) for line in result.stdout.splitlines() if line.startswith("detent-check-evidence:")]
                self.assertEqual({item["scope"] for item in evidence}, {"generated", "lint", "vet", "build", "unit-short"} | ({"invariants", "migrations"} if not failure else set()))
                self.assertEqual(sum(item["exit_code"] != 0 for item in evidence), int(bool(failure)))


if __name__ == "__main__":
    unittest.main()
