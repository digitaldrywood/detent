import json
import importlib.util
import os
import shutil
import socket
import subprocess
import sys
import tempfile
import threading
import unittest
from unittest import mock
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
SPEC = importlib.util.spec_from_file_location("check_release", ROOT / "scripts/check-release.py")
CHECK_RELEASE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(CHECK_RELEASE)


class ReleaseValidateLocalTest(unittest.TestCase):
    def test_status_publication(self):
        scratch = Path(os.environ.get("TMPDIR", tempfile.gettempdir()))
        cases = {
            "release success": ("release/v1.2", "pass", "scheduled-full-ci", "success", True),
            "release failure": ("release/v1.2", "fail", "scheduled-full-ci", "failure", False),
            "develop success": ("develop", "pass", "develop-full-ci", "success", False),
            "build failure": ("release/v1.2", "build", "scheduled-full-ci", "failure", False),
            "passing run that changes tracked source": ("release/v1.2", "mutate", "scheduled-full-ci", "failure", False),
        }
        for name, (branch, mode, context, state, dispatch) in cases.items():
            with self.subTest(name=name), tempfile.TemporaryDirectory(dir=scratch) as directory:
                root = Path(directory)
                repo = root / "repo"
                repo.mkdir()
                env = dict(os.environ, GIT_CONFIG_NOSYSTEM="1", GIT_CONFIG_GLOBAL=os.devnull)

                def git(*args):
                    result = subprocess.run(["git", *args], cwd=repo, env=env, text=True, capture_output=True, timeout=30)
                    self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                    return result.stdout.strip()

                git("init", "-q", "-b", "develop")
                git("config", "user.name", "Example")
                git("config", "user.email", "example@example.com")
                (repo / "scripts").mkdir()
                shutil.copyfile(ROOT / "scripts/release-validate-local.sh", repo / "scripts/release-validate-local.sh")
                (repo / ".gitignore").write_text("tmp/\n")
                (repo / "Makefile").write_text(
                    "app:\n\t@test \"$$MODE\" != build\n"
                    "build:\n\t@mkdir -p tmp && printf '%s\\n' '#!/bin/sh' "
                    "'unset GH_TOKEN' 'printf \"go test receipts: 3 of 5 test binaries reused, 2 recorded, 0 not recordable, 0 receipt service failures\\\\n\"' "
                    "'printf \"%s\\\\n\" \"$$*\" > \"$$ARGS_LOG\"' 'if [ \"$$MODE\" = mutate ]; then echo changed >> \"$$(dirname \"$$0\")/../Makefile\"; exit 0; fi' '[ \"$$MODE\" = pass ]' > tmp/detent && chmod +x tmp/detent\n"
                )
                git("add", ".")
                git("commit", "-qm", "head")
                head = git("rev-parse", "HEAD")
                git("branch", "release/v1.2")
                git("remote", "add", "origin", str(repo))
                binaries = root / "bin"
                binaries.mkdir()
                calls = root / "calls"
                (binaries / "gh").write_text(
                    "#!/usr/bin/env python3\n"
                    "import json, os, sys\n"
                    "args = sys.argv[1:]\n"
                    "assert os.environ.get('GH_TOKEN') == 'fixture-status-token'\n"
                    "with open(os.environ['CALLS'], 'a') as f: f.write(json.dumps(args) + '\\n')\n"
                    "if args[:2] == ['repo', 'view']: print('example-org/example-project')\n"
                )
                (binaries / "gh").chmod(0o755)
                env.update(PATH=str(binaries) + os.pathsep + env["PATH"], GH_TOKEN="fixture-status-token", CALLS=str(calls), MODE=mode, ARGS_LOG=str(root / "args"), TEST_PROCS="2")
                result = subprocess.run(["bash", "scripts/release-validate-local.sh", branch], cwd=repo, env=env, text=True, capture_output=True, timeout=120)
                self.assertEqual(result.returncode == 0, state == "success", result.stdout + result.stderr)
                requests = [json.loads(line) for line in calls.read_text().splitlines()]
                posts = [args for args in requests if "POST" in args]
                self.assertEqual(len(posts), 2, requests)
                for post in posts:
                    self.assertIn(f"repos/example-org/example-project/statuses/{head}", post)
                    self.assertIn(f"context={context}", post)
                self.assertIn("state=pending", posts[0])
                self.assertIn(f"state={state}", posts[1])
                description = next(arg for arg in posts[1] if arg.startswith("description="))
                if mode == "mutate":
                    self.assertIn("changed the checkout", description)
                elif mode != "build":
                    self.assertIn("3 of 5 test binaries reused", description)
                    arguments = (root / "args").read_text()
                    self.assertIn("validate --project example-project --dir ", arguments)
                    self.assertIn("make check-release TEST_PROCS=2", arguments)
                dispatches = [args for args in requests if args[:2] == ["workflow", "run"]]
                self.assertEqual(dispatches, [["workflow", "run", "release-finalize.yml", "--repo", "example-org/example-project", "--ref", branch]] if dispatch else [])
                self.assertEqual(git("worktree", "list", "--porcelain").count("worktree "), 1)

    def test_release_checks_overlap_and_report_all_failures(self):
        scratch = ROOT / "tmp"
        scratch.mkdir(exist_ok=True)
        for budget, failures in ((1, []), (2, ["generated", "lint"]), (4, []), (4, ["browser-build"]), (8, ["app", "full"])):
            with self.subTest(budget=budget, failures=failures), tempfile.TemporaryDirectory(dir=scratch) as directory:
                root = Path(directory)
                (root / "scripts").mkdir()
                for name in ("check-release.sh", "check-release.py", "check-evidence.sh", "check-browser.sh"):
                    shutil.copyfile(ROOT / "scripts" / name, root / "scripts" / name)
                (root / "go.mod").write_text("module example.test/project\n")
                binaries = root / "bin"
                binaries.mkdir()
                tool = binaries / "tool"
                tool.write_text("#!" + sys.executable + r'''
import fcntl
import json
import os
from pathlib import Path
import socket
import sys

name = Path(sys.argv[0]).name
args = sys.argv[1:]
if name == "tool":
    name = args.pop(0)
if name == "python3":
    if args[:1] == ["-m"]:
        stage = "scripts"
    else:
        os.execv(os.environ["REAL_PYTHON"], [os.environ["REAL_PYTHON"], *args])
elif name == "make":
    stage = {"check-app": "app", "check-generated": "generated", "check-migrations": "migrations",
             "check-invariants": "invariants", "test-race": "race", "test": "full"}.get(args[0], args[0])
    if stage == "-o":
        stage = "browser-build"
elif name == "playwright":
    stage = "browser-" + args[0]
else:
    stage = "subprocess-lifecycle" if "-race" in args else "preview" if "-c" in args else args[0]
    if "GOOS" in os.environ:
        stage = "windows-" + stage

def record(action):
    with open("trace", "a") as output:
        fcntl.flock(output, fcntl.LOCK_EX)
        output.write(json.dumps({"stage": stage, "action": action, "budget": int(os.environ["TEST_PROCS"]),
                                 "gomaxprocs": os.environ["GOMAXPROCS"], "goflags": os.environ["GOFLAGS"],
                                 "makeflags": os.environ.get("MAKEFLAGS", ""), "args": args}) + "\n")

record("start")
print("output from " + stage, flush=True)
if os.environ.get("BARRIER") and (stage in ("app", "generated") or
        (os.environ.get("BROWSER_BARRIER") and stage in ("full", "browser-test"))):
    with socket.socket(socket.AF_UNIX) as connection:
        connection.connect(os.environ["BARRIER"])
        connection.sendall(stage.encode())
        assert connection.recv(1) == b"x"
record("end")
sys.exit(7 if stage in json.loads(os.environ["FAILURES"]) else 0)
''')
                tool.chmod(0o755)
                (binaries / "make").symlink_to(tool)
                (binaries / "git").write_text(
                    '#!/bin/sh\nif [ "$1" = rev-parse ]; then\n'
                    'if [ "$2" = --is-shallow-repository ]; then echo false; else echo head; fi\nfi\n'
                )
                (binaries / "go").write_text(
                    '#!/bin/sh\nif [ "$1" = env ]; then echo fixture; exit 0; fi\n'
                    'exec "$REAL_PYTHON" "$PROBE_TOOL" go "$@"\n'
                )
                (binaries / "python3").write_text(
                    '#!/bin/sh\nif [ "$1" = -m ]; then exec "$REAL_PYTHON" "$PROBE_TOOL" python3 "$@"; fi\n'
                    'exec "$REAL_PYTHON" "$@"\n'
                )
                for name in ("git", "go", "python3"):
                    (binaries / name).chmod(0o755)
                playwright = root / "node_modules/.bin"
                playwright.mkdir(parents=True)
                (playwright / "playwright").symlink_to(tool)
                env = dict(os.environ, PATH=str(binaries) + os.pathsep + os.environ["PATH"],
                           REAL_PYTHON=sys.executable, PROBE_TOOL=str(tool), TMPDIR=str(root), FAILURES=json.dumps(failures))
                env.pop("BARRIER", None)
                env.pop("BROWSER_BARRIER", None)
                server = socket.socket(socket.AF_UNIX)
                clients = []
                errors = []
                barrier = None
                if budget > 1:
                    address = str(root / "barrier")
                    server.bind(address)
                    server.listen(2)
                    server.settimeout(15)
                    env["BARRIER"] = address
                    if budget >= 4 and "browser-build" not in failures:
                        env["BROWSER_BARRIER"] = "true"

                    def release_pair():
                        try:
                            for _ in range(2 if env.get("BROWSER_BARRIER") else 1):
                                pair = []
                                for _ in range(2):
                                    connection, _ = server.accept()
                                    connection.settimeout(15)
                                    clients.append(connection)
                                    pair.append(connection)
                                    connection.recv(32)
                                for connection in pair:
                                    connection.sendall(b"x")
                        except OSError as error:
                            errors.append(error)
                        finally:
                            for connection in clients:
                                connection.close()

                    barrier = threading.Thread(target=release_pair)
                    barrier.start()
                try:
                    result = subprocess.run(["bash", "scripts/check-release.sh", str(budget)], cwd=root,
                                            env=env, text=True, capture_output=True, timeout=45)
                finally:
                    server.close()
                    if barrier:
                        barrier.join(timeout=20)
                self.assertFalse(errors, errors)
                self.assertEqual(result.returncode, int(bool(failures)), result.stdout + result.stderr)
                evidence = [json.loads(line.removeprefix("detent-check-evidence: "))
                            for line in result.stdout.splitlines() if line.startswith("detent-check-evidence: ")]
                self.assertEqual({row["scope"] for row in evidence}, set(CHECK_RELEASE.STEPS))
                self.assertEqual(len(evidence), len(CHECK_RELEASE.STEPS))
                failed_scopes = {"browser" if step == "browser-build" else step for step in failures}
                self.assertEqual({row["scope"] for row in evidence if row["exit_code"]}, failed_scopes)
                summary = result.stdout.split("Release check summary:\n")[1]
                for step in CHECK_RELEASE.STEPS:
                    status = "FAIL exit=7" if step in failed_scopes else "PASS exit=0"
                    self.assertRegex(summary, rf"  {step}: {status} wall=\d+\.\ds log=")
                self.assertIn(f"check-release finished: exit={int(bool(failures))} wall=", summary)
                self.assertEqual("Release checks passed." in summary, not failures)
                logs = list(root.glob("check-release.*"))
                self.assertEqual(len(logs), 1)
                for step in CHECK_RELEASE.STEPS:
                    log = (logs[0] / f"{step}.log").read_text()
                    self.assertIn(f'"scope":"{step}"', log)
                    self.assertEqual(log.count("detent-check-evidence: "), 1)
                trace = [json.loads(line) for line in (root / "trace").read_text().splitlines()]
                active = 0
                ended = set()
                for event in trace:
                    if event["action"] == "start":
                        active += event["budget"]
                        self.assertLessEqual(active, budget, trace)
                        self.assertEqual(event["gomaxprocs"], "1")
                        self.assertIn(f'-p={event["budget"]}', event["goflags"])
                        if event["stage"] not in ("app", "generated", "scripts", "migrations"):
                            self.assertTrue({"app", "generated"}.issubset(ended), trace)
                            if not event["stage"].startswith("browser-"):
                                self.assertIn("browser-build", ended, trace)
                    else:
                        active -= event["budget"]
                        ended.add(event["stage"])
                self.assertEqual(active, 0)
                if "browser-build" not in failures:
                    browser = next(event for event in trace if event["stage"] == "browser-test")
                    self.assertIn("--max-failures=0", browser["args"])
                    self.assertEqual(browser["args"][-1], "tests/visual")

    def test_browser_selection_uses_validated_ancestor_and_fails_safe(self):
        scratch = ROOT / "tmp"
        scratch.mkdir(exist_ok=True)
        with tempfile.TemporaryDirectory(dir=scratch) as directory:
            root = Path(directory)
            env = {key: value for key, value in os.environ.items() if not key.startswith("GIT_")}
            env.update(GIT_CONFIG_NOSYSTEM="1", GIT_CONFIG_GLOBAL=os.devnull)

            def git(*args):
                return subprocess.check_output(["git", *args], cwd=root, env=env, stderr=subprocess.PIPE).decode().strip()

            def select():
                with mock.patch.dict(os.environ, env, clear=True):
                    previous = Path.cwd()
                    try:
                        os.chdir(root)
                        return CHECK_RELEASE.browser_specs()
                    finally:
                        os.chdir(previous)

            git("init", "-q", "-b", "develop")
            git("config", "user.name", "Example")
            git("config", "user.email", "example@example.com")
            specs = (*CHECK_RELEASE.SMOKE_SPECS, "tests/visual/affected.spec.js", "tests/visual/unrelated.spec.js")
            for spec in specs:
                path = root / spec
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text("test fixture\n")
            git("add", ".")
            git("commit", "-qm", "initial")
            self.assertEqual(select(), ["tests/visual"])
            commit = git("rev-parse", "HEAD")
            provenance = {"schema": 1, "tag": "v1.0.0", "commit": commit,
                          "checks": [{"name": "scheduled-full-ci", "status": "completed", "conclusion": "success", "status_id": 1}]}
            git("tag", "-a", "v1.0.0", "-m", "<!-- detent-release-provenance:" + json.dumps(provenance) + " -->")
            expected = sorted(CHECK_RELEASE.SMOKE_SPECS)
            self.assertEqual(select(), expected)
            for path in ("docs/browser.md", "docs/browser.svg", "README.md", "internal/hubserver/preview_test.go",
                         "scripts/check_release_test.py", "tests/test_release.py", "scripts/browser_test.sh"):
                with self.subTest(path=path):
                    target = root / path
                    target.parent.mkdir(parents=True, exist_ok=True)
                    target.write_text("irrelevant\n")
                    try:
                        self.assertEqual(select(), expected)
                    finally:
                        target.unlink()
            for path in ("tests/visual/runtime.js", "web/conversation/src/App.tsx", "scripts/check-browser.sh",
                         "internal/hubserver/migration_steps/00064_cloud_attachment_references.sql",
                         "internal/hubserver/native_pages.go", "internal/store/migrations/example.sql",
                         "internal/web/example.templ", "app/example.ts", "config.yaml", "unknown.input"):
                with self.subTest(path=path):
                    target = root / path
                    target.parent.mkdir(parents=True, exist_ok=True)
                    target.write_text("shared\n")
                    try:
                        self.assertEqual(select(), ["tests/visual"])
                    finally:
                        target.unlink()
            (root / "tests/visual/affected.spec.js").write_text("changed\n")
            git("add", ".")
            git("commit", "-qm", "changed journey")
            git("tag", "v9.0.0")
            git("tag", "-a", "v9.0.1", "-m", "unvalidated")
            self.assertEqual(select(), ["tests/visual"])
            provenance.update(tag="v1.0.1", commit=git("rev-parse", "HEAD"))
            git("tag", "-a", "v1.0.1", "-m", "<!-- detent-release-provenance:" + json.dumps(provenance) + " -->")
            self.assertEqual(select(), expected)
            (root / "tests/visual/unrelated.spec.js").write_text("uncommitted change\n")
            self.assertEqual(select(), ["tests/visual"])
            (root / "tests/visual/unrelated.spec.js").write_text("test fixture\n")
            with mock.patch.object(CHECK_RELEASE, "git", side_effect=subprocess.CalledProcessError(1, "git")):
                self.assertEqual(select(), ["tests/visual"])
            with mock.patch.object(CHECK_RELEASE, "git", return_value="true\n"):
                self.assertEqual(select(), ["tests/visual"])


if __name__ == "__main__":
    unittest.main()
