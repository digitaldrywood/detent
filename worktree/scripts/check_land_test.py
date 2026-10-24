import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest


ROOT = Path(__file__).resolve().parent.parent
TOOL = '#!' + sys.executable + r'''
import fcntl
import json
import os
from pathlib import Path
import sys
import time

args = sys.argv[1:]
name = Path(sys.argv[0]).name
if name == "make":
    if os.environ["PROBE_TARGET"] == "check-land":
        if "build" in args:
            Path("browser-build-args").write_text(json.dumps(args))
            sys.exit(0)
        if "check-app" not in args:
            os.execv(os.environ["PROBE_MAKE"], ["make"] + args)
    elif "test" not in args and "test-race" not in args:
        sys.exit(0)
    else:
        os.execv(os.environ["PROBE_MAKE"], ["make"] + args)
if name == "python3":
    if args[:1] == ["-m"]:
        sys.exit(0)
    os.execv(os.environ["PROBE_PYTHON"], ["python3"] + args)
if name == "playwright":
    with open("browser-args", "a") as output:
        output.write(json.dumps(args) + "\n")
    sys.exit("browser journeys must not run in the source gate")
if name == "git":
    if args[0] == "merge-base":
        print("base")
    elif args[0] == "diff":
        if "--name-only" in args:
            print(os.environ.get("PROBE_CHANGED", ""))
            sys.exit(0)
        sys.exit(int(Path("app-changed").exists()))
    elif args[0] == "rev-parse":
        print("head")
    sys.exit(0)
if args[:1] == ["run"] and "--help" in args:
    print("Usage:\n  detent hub runner [command]\n\nAvailable Commands:\n  enroll      Redeem one enrollment token\n  init        Generate a host identity\n  register    Register this host as a runner\n  renew       Refresh the host credential\n  rotate      Refresh the host credential\n")
    sys.exit(0)
if args[:1] == ["env"]:
    print({"GOHOSTOS": "fixture", "GOHOSTARCH": "fixture", "GOVERSION": "fixture"}[args[1]])
    sys.exit(0)
if args[:1] == ["list"]:
    if os.environ["PROBE_TARGET"] == "test-cover":
        print("github.com/digitaldrywood/detent/internal/hubserver")
    print("fixture")
    sys.exit(0)
if args[:2] == ["tool", "cover"]:
    sys.exit(0)
if "./tools/covermerge" in args:
    with open("trace", "a") as output:
        output.write(json.dumps({"stage": "coverage-merge", "action": "start", "args": args}) + "\n")
    print("mode: set")
    sys.exit(0)
if name == "make":
    stage = "app"
    budget = int(os.environ["GOMAXPROCS"])
elif name == "lint":
    stage = "lint"
    budget = int(next(arg.split("=")[1] for arg in args if arg.startswith("--concurrency=")))
elif args[0] in ("build", "vet", "test"):
    stage = "compile" if "-c" in args else "invariants" if "./internal/invariants" in args else {"test": "unit-short"}.get(args[0], args[0])
    budget = int(args[args.index("-p") + 1]) if "-p" in args else int(os.environ["GOMAXPROCS"])
    for arg in args:
        if arg.startswith("-coverprofile="):
            Path(arg.split("=", 1)[1]).write_text("mode: set\n")
else:
    stage = "workspace" if "./tools/testgate" in args else "docs" if "./internal/config/cmd/configdoc" in args else "migrations" if "./tools/migrationcheck" in args else "invariant-manifest" if "./tools/invariantcheck" in args else "sqlc"
    budget = int(os.environ["GOMAXPROCS"])

def record(action):
    with open("trace", "a") as output:
        fcntl.flock(output, fcntl.LOCK_EX)
        output.write(json.dumps({"stage": stage, "action": action, "budget": budget, "gomaxprocs": os.environ["GOMAXPROCS"], "args": args}) + "\n")

record("start")
if os.environ["PROBE_TARGET"] == "check-land" and stage in ("lint", "vet", "build", "unit-short"):
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
if any(arg == "-coverprofile=tmp/" + os.environ.get("PROBE_FAILURE", "") + "-cover.raw.out" for arg in args):
    sys.exit("deliberate coverage partition failure")
if stage == os.environ.get("PROBE_FAILURE"):
    sys.exit("deliberate " + stage + " failure")
'''


@unittest.skipIf(os.name == "nt", "landing gate requires a POSIX shell")
class LandingGateTest(unittest.TestCase):
    def test_stage_budget_and_failures(self):
        scratch = next((os.environ[key] for key in ("TMPDIR", "TMP", "TEMP") if os.environ.get(key)), None)
        self.assertIsNotNone(scratch, "a supplied scratch directory is required")
        cases = [("check-land", budget, failure, "") for budget, failure in ((1, ""), (2, ""), (4, ""), (7, ""), (4, "lint"), (4, "vet"), (4, "build"), (4, "unit-short"), (4, "docs"), (4, "sqlc"), (4, "invariant-manifest"), (4, "invariants"), (4, "migrations"))]
        cases += [(target, 4, "", "") for target in ("test", "test-fast", "test-race", "test-cover")]
        cases += [("test-cover", 4, failure, shard) for failure, shard in (("", "2/3"), ("", "3/3"), ("hub-2", "2/3"))]
        shared_ui = "web/conversation/src/app/App.tsx"
        cases += [("check-land", 4, "", path) for path in (shared_ui, "web/conversation/src/app/main.tsx", "web/conversation/src/app/router.tsx", "web/conversation/src/components/ui/button.tsx", "tests/visual/cloud-reports.spec.js")]
        cases += [("check-land", 4, failure, shared_ui) for failure in ("app", "lint", "unit-short")]
        for target, budget, failure, changed in cases:
            with self.subTest(target=target, budget=budget, failure=failure, changed=changed), tempfile.TemporaryDirectory(prefix="npm-nilaway-", dir=scratch) as directory:
                root = Path(directory)
                (root / "scripts").mkdir()
                for name in ("Makefile", ".golangci-version", "go.mod", "scripts/check-land.sh", "scripts/check-evidence.sh", "scripts/check-browser.sh", "scripts/test-workspace.sh", "scripts/cover-packages.sh", "scripts/check-runner-enrollment-docs.py", "docs/hub-self-hosting.md", "internal/cli/hub_runner_register.go"):
                    (root / name).parent.mkdir(parents=True, exist_ok=True)
                    shutil.copy2(ROOT / name, root / name)
                for name in ("go", "git", "lint"):
                    path = root / name
                    path.write_text(TOOL)
                    path.chmod(0o700)
                app_changed = changed.startswith("web/conversation/")
                if app_changed:
                    (root / "app-changed").touch()
                (root / "tests/visual").mkdir(parents=True)
                (root / "tests/visual/cloud-reports.spec.js").touch()
                for name in ("make", "node_modules/.bin/playwright"):
                    path = root / name
                    path.parent.mkdir(parents=True, exist_ok=True)
                    path.write_text(TOOL)
                    path.chmod(0o700)
                env = {key: value for key, value in os.environ.items() if key not in ("MAKEFLAGS", "MFLAGS", "MAKEOVERRIDES")}
                env.update(PATH=str(root) + os.pathsep + env["PATH"], PROBE_BUDGET=str(budget), PROBE_FAILURE=failure, PROBE_TARGET=target, PROBE_MAKE=shutil.which("make"), PROBE_PYTHON=sys.executable)
                env["PROBE_CHANGED"] = changed
                if target == "test-cover" and changed:
                    env["COVER_SHARD"] = changed
                failed = bool(failure)
                result = subprocess.run([env["PROBE_MAKE"], target, f"TEST_PROCS={budget}", f"GOLANGCI_LINT={root / 'lint'}"], cwd=root, env=env, capture_output=True, text=True, timeout=600)
                self.assertEqual(result.returncode == 0, not failed, result.stdout + result.stderr)
                events = [json.loads(line) for line in (root / "trace").read_text().splitlines()]
                tests = [event for event in events if event["action"] == "start" and event["stage"] in ("unit-short", "invariants")]
                if failure not in ("app", "docs", "sqlc"):
                    self.assertTrue(tests)
                for event in tests:
                    self.assertEqual("-count=1" in event["args"], target in ("test-race", "test-cover"), event)
                if target == "test-cover":
                    for event in tests:
                        self.assertTrue(any(arg in ("fixture", "./internal/hubserver", "./internal/web") for arg in event["args"]), event)
                if target != "check-land":
                    continue
                self.assertEqual("Landing checks passed." in result.stdout, not failed)
                starts = [event["stage"] for event in events if event["action"] == "start"]
                self.assertFalse((root / "browser-args").exists())
                self.assertFalse((root / "browser-build-args").exists())
                if app_changed:
                    self.assertEqual(starts.pop(0), "app")
                if failure == "app":
                    self.assertEqual(starts, [])
                    self.assertIn("deliberate app failure", result.stderr)
                    continue
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
                tail = ["invariant-manifest", "invariants", "migrations"]
                if failure in tail:
                    tail = tail[:tail.index(failure) + 1]
                elif failure:
                    tail = []
                self.assertEqual(starts[6:], tail)
                output = result.stdout.replace(str(root), "<fixture>")
                self.assertNotIn("npm", output)
                self.assertNotIn("nilaway", output)
                self.assertFalse((root / "static/app/conversation/app.js").exists())
                evidence = [json.loads(line.split(": ", 1)[1]) for line in result.stdout.splitlines() if line.startswith("detent-check-evidence:")]
                expected_scopes = {"generated", "lint", "vet", "build", "unit-short"}
                expected_scopes.update(tail)
                if app_changed:
                    expected_scopes.add("app")
                self.assertEqual({item["scope"] for item in evidence}, expected_scopes)
                self.assertEqual(sum(item["exit_code"] != 0 for item in evidence), int(failed))


if __name__ == "__main__":
    unittest.main()
