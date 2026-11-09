import json
import os
from pathlib import Path
import runpy
import select
import shutil
import signal
import subprocess
import sys
import tempfile
import unittest


ROOT = Path(__file__).resolve().parent.parent
TOOL = "#!" + sys.executable + r'''
import json
import os
from pathlib import Path
import sys

name = Path(sys.argv[0]).name
args = sys.argv[1:]
if name == "go" and args[:1] == ["env"]:
    print("fixture")
    sys.exit(0)
with Path(".git/trace").open("a") as stream:
    stream.write(json.dumps({"tool": name, "args": args, "budget": os.environ.get("GOMAXPROCS"), "token": "DETENT_API_TOKEN" in os.environ, "postgres": os.environ.get("DETENT_TEST_POSTGRES"), "tmpdir": os.environ.get("TMPDIR")}) + "\n")
if (name == "go" and args[:1] == [os.environ.get("FAIL_STAGE")]) or (name == "golangci-lint" and os.environ.get("FAIL_STAGE") == "lint"):
    sys.exit(17)
if name == "go" and args[:1] == ["list"]:
    for directory, imports, test_imports in (
        ("lib", [], []),
        ("app", ["example.test/lib"], []),
        ("integration", [], ["example.test/app"]),
        ("unrelated", [], []),
        ("removed", [], []),
    ):
        if not list(Path(directory).glob("*.go")):
            continue
        print(json.dumps({"Dir": str(Path(directory).resolve()), "ImportPath": "example.test/" + directory, "Imports": imports, "TestImports": test_imports, "EmbedFiles": ["data.txt"] if directory == "lib" else []}))
if name == "go" and args[:1] == ["test"]:
    if "REAL_GO" in os.environ:
        environment = dict(os.environ, PATH=os.environ["REAL_PATH"])
        os.chdir(os.environ["SOURCE_ROOT"])
        os.execve(os.environ["REAL_GO"], ["go", "test", "-json", "-p", "2", "-timeout=120s", *json.loads(os.environ["LIFETIME_PACKAGES"])], environment)
    import runpy
    contract = runpy.run_path("scripts/check-landing.py")
    events = json.loads(os.environ.get("TEST_EVENTS", "null"))
    if events is None:
        events = []
        for package, tests in contract["REQUIRED_POSTGRES"].items():
            for test in tests:
                events.extend([{"Action": action, "Package": package, "Test": test} for action in ("run", "pass")])
            events.append({"Action": "pass", "Package": package})
    for event in events:
        print(json.dumps(event))
if name == "pg_config":
    print(os.environ["PG_BINDIR"])
'''


@unittest.skipIf(os.name == "nt", "landing gate requires a POSIX shell")
class ShortLandingGateTest(unittest.TestCase):
    def setUp(self):
        scratch = next((os.environ[key] for key in ("TMPDIR", "TMP", "TEMP") if os.environ.get(key)), None)
        self.assertIsNotNone(scratch, "a supplied scratch directory is required")
        self.directory = tempfile.TemporaryDirectory(dir=scratch)
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.git("init", "-q")
        self.git("config", "user.name", "Fixture")
        self.git("config", "user.email", "fixture@example.test")
        self.git("config", "commit.gpgsign", "false")
        (self.root / "scripts").mkdir()
        for name in ("check-landing.sh", "check-evidence.sh", "check-sqlite-policy.py", "check-landing.py"):
            shutil.copy2(ROOT / "scripts" / name, self.root / "scripts" / name)
        (self.root / "go.mod").write_text("module example.test\n\ngo 1.26\n")
        for directory in ("lib", "app", "integration", "unrelated", "removed"):
            (self.root / directory).mkdir()
            (self.root / directory / "source.go").write_text("package " + directory + "\n")
        (self.root / "lib/data.txt").write_text("old\n")
        (self.root / "lib/testdata").mkdir()
        (self.root / "lib/testdata/fixture.json").write_text("{}\n")
        policy = self.root / "internal/invariants/sqlite_policy.json"
        policy.parent.mkdir(parents=True)
        policy.write_text('{"files":{},"modules":{}}\n')
        self.commit()
        self.base = self.git("rev-parse", "HEAD").strip()
        self.git("update-ref", "refs/remotes/origin/develop", self.base)
        binaries = self.root / ".git/bin"
        binaries.mkdir()
        for name in ("go", "make", "golangci-lint", "initdb", "postgres", "pg_config"):
            tool = binaries / name
            tool.write_text(TOOL)
            tool.chmod(0o755)
        self.env = dict(os.environ, PATH=str(binaries) + os.pathsep + os.environ["PATH"], DETENT_API_TOKEN="fixture-token")
        self.linter = str(binaries / "golangci-lint")
        self.required_packages = ["./internal/pgtransport", "./internal/testenv/pgtest"]

    def git(self, *args):
        return subprocess.check_output(["git", *args], cwd=self.root, text=True, stderr=subprocess.STDOUT)

    def commit(self):
        self.git("add", ".")
        self.git("commit", "-qm", "fixture")

    def run_gate(self, base="origin/develop", budget="2"):
        (self.root / ".git/trace").write_text("")
        return subprocess.run(["bash", "scripts/check-landing.sh", base, budget, self.linter], cwd=self.root, env=self.env, capture_output=True, text=True, timeout=30)

    def trace(self):
        path = self.root / ".git/trace"
        return [json.loads(line) for line in path.read_text().splitlines()] if path.exists() else []

    def test_changed_package_includes_transitive_and_test_dependents(self):
        cases = (
            ("test only", ("lib/source_test.go",), ["./lib"], "1"),
            ("production", ("lib/source.go",), ["./app", "./integration", "./lib"], "4"),
            ("mixed", ("lib/source.go", "lib/source_test.go"), ["./app", "./integration", "./lib"], "7"),
            ("separate test and production", ("lib/source_test.go", "unrelated/source.go"), ["./lib", "./unrelated"], "2"),
        )
        for index, (name, paths, expected, budget) in enumerate(cases):
            with self.subTest(name=name):
                self.base = self.git("rev-parse", "HEAD").strip()
                self.git("update-ref", "refs/remotes/origin/develop", self.base)
                for path in paths:
                    target = self.root / path
                    value = "TestValue" if path.endswith("_test.go") else "Value"
                    target.write_text(f"package {target.parent.name}\nconst {value} = {index}\n")
                self.commit()
                result = self.run_gate(budget=budget)
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                evidence = [json.loads(line.split(": ", 1)[1]) for line in result.stdout.splitlines() if line.startswith("detent-check-evidence: ")]
                self.assertTrue(any(check["scope"] == "unit-short" and check["exit_code"] == 0 for check in evidence))
                trace = self.trace()
                self.assertEqual([(call["tool"], call["args"][0]) for call in trace], [("make", "-j1"), ("go", "build"), ("go", "list"), ("go", "test"), ("make", "-j1"), ("golangci-lint", "run")])
                test = trace[3]
                self.assertEqual([arg for arg in test["args"] if arg.startswith("./")], sorted(expected + self.required_packages))
                self.assertFalse(test["token"])
                self.assertEqual(test["postgres"], "binary")
                self.assertEqual(test["tmpdir"], self.env["TMPDIR"])
                self.assertIn("-json", test["args"])
                for call in trace:
                    if call["tool"] == "go":
                        self.assertEqual(call["budget"], "1")
                        if call["args"][0] in ("build", "test"):
                            package_procs = int(call["args"][call["args"].index("-p") + 1])
                            self.assertEqual(package_procs, int(budget))
                            self.assertLessEqual(package_procs * int(call["budget"]), int(budget))
                    elif call["tool"] == "golangci-lint":
                        self.assertEqual(call["budget"], budget)
                        self.assertIn("--concurrency=" + budget, call["args"])
                    else:
                        self.assertEqual(call["budget"], "1")
                        self.assertIn("TEST_PROCS=" + budget, call["args"])
                self.assertIn("-timeout=10m", test["args"])
                self.assertEqual([arg for arg in trace[-1]["args"] if arg.startswith("./")], sorted({"./" + str(Path(path).parent) for path in paths}))
                self.assertIn("--new-from-rev=" + self.base, trace[-1]["args"])
                self.assertIn("assets", trace[0]["args"])
                self.assertIn("generate-docs", trace[0]["args"])

    def test_module_change_checks_all_current_packages(self):
        with (self.root / "go.mod").open("a") as stream:
            stream.write("toolchain go1.26.0\n")
        self.commit()
        result = self.run_gate()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        trace = self.trace()
        test = next(call for call in trace if call["args"][0] == "test")
        self.assertEqual([arg for arg in test["args"] if arg.startswith("./")], sorted(["./app", "./integration", "./lib", "./removed", "./unrelated"] + self.required_packages))
        self.assertEqual(trace[-1]["tool"], "golangci-lint")
        self.assertEqual(trace[-1]["args"][-5:], ["./app", "./integration", "./lib", "./removed", "./unrelated"])
        self.assertFalse(any(arg.startswith("--new-from-rev") for arg in trace[-1]["args"]))

    def test_fixture_changes_select_tests_without_lint(self):
        for path in ("lib/testdata/fixture.json", "lib/data.txt"):
            with self.subTest(path=path):
                (self.root / path).write_text("changed\n")
                self.commit()
                result = self.run_gate()
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                trace = self.trace()
                test = next(call for call in trace if call["args"][0] == "test")
                self.assertEqual([arg for arg in test["args"] if arg.startswith("./")], sorted(["./app", "./integration", "./lib"] + self.required_packages))
                self.assertFalse(any(call["tool"] == "golangci-lint" for call in trace))

    def test_deleted_package_is_not_executed(self):
        (self.root / "removed/source.go").unlink()
        self.commit()
        result = self.run_gate()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        test = next(call for call in self.trace() if call["args"][0] == "test")
        self.assertEqual([arg for arg in test["args"] if arg.startswith("./")], self.required_packages)
        self.assertFalse(any(call["tool"] == "golangci-lint" for call in self.trace()))

    def test_postgres_scenarios_cannot_disappear_or_skip(self):
        for events in (
            [],
            [{"Action": "skip", "Package": "github.com/digitaldrywood/detent/internal/pgtransport", "Test": "TestTenantBinding"}],
        ):
            with self.subTest(events=events):
                self.env["TEST_EVENTS"] = json.dumps(events)
                result = self.run_gate()
                self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertIn("required PostgreSQL scenario", result.stdout + result.stderr)

    def test_postgres_tools_and_job_scratch_are_required(self):
        binaries = self.root / ".git/bin"
        isolated = self.root / ".git/isolated"
        isolated.mkdir()
        for name in ("bash", "python3", "awk", "git"):
            (isolated / name).symlink_to(shutil.which(name))
        for tool in ("initdb", "postgres"):
            (binaries / tool).rename(isolated / (tool + "-hidden"))
        self.env["PATH"] = str(isolated)
        for name in ("missing initdb", "missing postgres", "missing TMPDIR"):
            with self.subTest(name=name):
                result = self.run_gate()
                self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertIn("Landing PostgreSQL tests require", result.stderr)
                if name == "missing initdb":
                    (isolated / "initdb").symlink_to(isolated / "initdb-hidden")
                elif name == "missing postgres":
                    self.env.pop("TMPDIR")

    def test_postgres_binaries_are_discovered_from_pg_config(self):
        binaries = self.root / ".git/bin"
        postgres_bin = self.root / ".git/postgres"
        postgres_bin.mkdir()
        for name in ("initdb", "postgres"):
            (binaries / name).rename(postgres_bin / name)
        self.env["PG_BINDIR"] = str(postgres_bin)
        result = self.run_gate()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_gate_releases_every_postgres_on_failure_and_cancellation(self):
        fixture = r'''
package fixture

import (
    "context"
    "database/sql"
    "fmt"
    "os"
    "path/filepath"
    "strings"
    "testing"

    "github.com/digitaldrywood/detent/internal/testenv/pgtest"
)

var template = pgtest.NewTemplate(func(context.Context, *sql.DB) error { return nil })

func TestMain(m *testing.M) {
    result := m.Run()
    if err := template.Close(context.Background()); err != nil {
        fmt.Fprintln(os.Stderr, err)
        result = 1
    }
    os.Exit(result)
}

func TestGateLifetime(t *testing.T) {
    db := template.Database(t)
    var data, socket string
    if err := db.QueryRow("SHOW data_directory").Scan(&data); err != nil { t.Fatal(err) }
    if err := db.QueryRow("SHOW unix_socket_directories").Scan(&socket); err != nil { t.Fatal(err) }
    pid, err := os.ReadFile(filepath.Join(data, "postmaster.pid"))
    if err != nil { t.Fatal(err) }
    lines := strings.Split(string(pid), "\n")
    fmt.Printf("CGATE-READY %s %s %s %s\n", lines[0], data, socket, strings.Fields(lines[6])[1])
    control, err := os.Open(os.Getenv("GATE_CONTROL"))
    if err != nil { t.Fatal(err) }
    defer control.Close()
    if _, err := control.Read(make([]byte, 1)); err != nil { t.Fatal(err) }
    t.Fatal("intentional gate fixture failure")
}
'''
        real_path = os.environ["PATH"]
        real_go = shutil.which("go", path=real_path)
        for name, cancel in (("failure", None), ("SIGTERM", signal.SIGTERM), ("SIGINT", signal.SIGINT)):
            with self.subTest(name=name), tempfile.TemporaryDirectory(dir=ROOT / "tmp", prefix="gate-lifetime-") as directory:
                base = Path(directory)
                long = base
                while len(str(long)) < 300:
                    long /= "x" * min(80, 299 - len(str(long)))
                long.mkdir(parents=True)
                self.assertEqual(len(str(long)), 300)
                control = base / "control"
                os.mkfifo(control)
                descriptor = os.open(control, os.O_RDWR)
                self.addCleanup(os.close, descriptor)
                packages = []
                for index in range(2):
                    package = base / ("fixture" + str(index))
                    package.mkdir()
                    (package / "lifetime_test.go").write_text(fixture)
                    packages.append("./" + str(package.relative_to(ROOT)))
                environment = dict(self.env, REAL_GO=real_go, REAL_PATH=real_path, SOURCE_ROOT=str(ROOT), LIFETIME_PACKAGES=json.dumps(packages), GATE_CONTROL=str(control), TMPDIR=str(long))
                process = subprocess.Popen(["bash", "scripts/check-landing.sh", self.base, "2", self.linter], cwd=self.root, env=environment, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
                servers = []
                output = []
                watchers = []
                try:
                    while len(servers) < 2:
                        line = process.stdout.readline()
                        output.append(line)
                        self.assertTrue(line, "".join(output))
                        if line.startswith("CGATE-READY "):
                            _, pid, data, socket, segment = line.split()
                            self.assertTrue(Path(data).resolve().is_relative_to(long.resolve()))
                            servers.append((int(pid), Path(data), Path(socket), segment))
                            if hasattr(select, "kqueue"):
                                watcher = select.kqueue()
                                watcher.control([select.kevent(int(pid), filter=select.KQ_FILTER_PROC, flags=select.KQ_EV_ADD, fflags=select.KQ_NOTE_EXIT)], 0)
                            else:
                                watcher = os.pidfd_open(int(pid))
                            watchers.append(watcher)
                    if cancel is None:
                        os.write(descriptor, b"xx")
                    else:
                        process.send_signal(cancel)
                    output.extend(process.communicate(timeout=30)[0])
                    self.assertEqual(process.returncode, 1 if cancel is None else 128 + cancel, "".join(output))
                    for (pid, data, socket, segment), watcher in zip(servers, watchers):
                        if hasattr(select, "kqueue"):
                            self.assertTrue(watcher.control([], 1, 30), f"PostgreSQL {pid} did not exit")
                        else:
                            self.assertTrue(select.select([watcher], [], [], 30)[0], f"PostgreSQL {pid} did not exit")
                        self.assertFalse((socket / ".s.PGSQL.5432").exists())
                        segments = subprocess.check_output(["ipcs", "-m"], text=True)
                        for line in segments.splitlines():
                            fields = line.split()
                            if len(fields) >= 2 and (fields[0] == "m" or fields[0].startswith("0x")):
                                self.assertNotEqual(fields[1], segment, f"PostgreSQL {pid} left shared memory")
                finally:
                    os.write(descriptor, b"xx")
                    if process.poll() is None:
                        process.terminate()
                        process.communicate(timeout=30)
                    process.stdout.close()
                    for watcher in watchers:
                        watcher.close() if hasattr(watcher, "close") else os.close(watcher)
                    for _, data, _, _ in servers:
                        if (data / "postmaster.pid").exists():
                            subprocess.run(["pg_ctl", "stop", "-D", str(data), "-m", "immediate", "-w"], env=dict(os.environ, PATH=real_path), capture_output=True, check=False)

    def test_failures_stop_the_gate(self):
        for stage in ("build", "list", "test", "lint"):
            with self.subTest(stage=stage):
                (self.root / "lib/source.go").write_text("package lib\nconst Value = 1\n")
                if stage == "build":
                    self.commit()
                self.env["FAIL_STAGE"] = stage
                result = self.run_gate()
                self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertEqual(result.returncode, 1 if stage == "list" else 17)
                self.assertEqual(any(call["tool"] == "golangci-lint" for call in self.trace()), stage == "lint")

    def test_invalid_base_or_budget_runs_no_commands(self):
        for base, budget in (("missing-base", "2"), ("origin/develop", "0")):
            with self.subTest(base=base, budget=budget):
                result = self.run_gate(base, budget)
                self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertEqual(self.trace(), [])

    def test_policy_growth_stops_before_build(self):
        policy = self.root / "internal/invariants/sqlite_policy.json"
        policy.write_text('{"files":{"new.go":{"driver_import":1}},"modules":{}}\n')
        self.commit()
        result = self.run_gate(self.base)
        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("new.go: SQLite driver_import count 1 exceeds target baseline 0", result.stderr)
        self.assertEqual(self.trace(), [])

    def test_shallow_candidate_uses_explicit_target_without_origin(self):
        (self.root / "lib/source.go").write_text("package lib\nconst Value = 1\n")
        self.commit()
        head = self.git("rev-parse", "HEAD").strip()
        (self.root / ".git/shallow").write_text(head + "\n")
        self.git("update-ref", "-d", "refs/remotes/origin/develop")
        result = self.run_gate(self.base)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("Landing comparison: base=" + self.base, result.stdout)


class PostgresInventoryTest(unittest.TestCase):
    def test_inventory_rejects_incomplete_or_skipped_required_scenarios(self):
        contract = runpy.run_path(str(ROOT / "scripts/check-landing.py"))
        required = contract["REQUIRED_POSTGRES"]
        valid = []
        for package, tests in required.items():
            for test in tests:
                valid.extend({"Action": action, "Package": package, "Test": test} for action in ("run", "pass"))
            valid.append({"Action": "pass", "Package": package})
        owner = next(iter(required))
        test = required[owner][0]
        missing = [event for event in valid if event.get("Test") != test]
        cases = (
            ("executed", valid, 0, "passed=4"),
            ("absent", missing, 1, "did not run and pass"),
            ("pass without run", [event for event in valid if event["Action"] != "run"], 1, "did not run and pass"),
            ("package failed after test pass", valid[:-1] + [{"Action": "fail", "Package": list(required)[-1]}], 1, "failed go test event"),
            ("skipped subtest", valid + [{"Action": "skip", "Package": owner, "Test": test + "/scenario"}], 1, "required PostgreSQL scenario skipped"),
            ("missing PostgreSQL outside required list", valid + [
                {"Action": "output", "Package": "github.com/digitaldrywood/detent/internal/testenv/pgtest", "Test": "TestUnconfiguredPostgresFails", "Output": "PostgreSQL fixtures require DETENT_TEST_POSTGRES=binary"},
                {"Action": "skip", "Package": "github.com/digitaldrywood/detent/internal/testenv/pgtest", "Test": "TestUnconfiguredPostgresFails"},
            ], 1, "missing-postgres"),
            ("invalid JSON", ["broken"], 1, "invalid go test JSON"),
        )
        for name, events, expected, diagnostic in cases:
            with self.subTest(name=name):
                output = []
                result = contract["test_events"]((json.dumps(event) for event in events), output.append)
                self.assertEqual(result, expected, output)
                self.assertIn(diagnostic, "\n".join(output))

    def test_inventory_documents_skip_categories(self):
        categorize = runpy.run_path(str(ROOT / "scripts/check-landing.py"))["skip_category"]
        for package, test, output, expected in (
            ("example/empty", "", "[no test files]", "no-test-files"),
            ("example/helper", "TestHelper", "helper process only", "helper-process"),
            ("example/platform", "TestWindows", "requires windows", "platform-exclusion"),
            ("example/platform", "TestSandbox", "actual host enforcement is unavailable", "platform-exclusion"),
            ("example/slow", "TestSlow", "process lifecycle integration", "short-exclusion"),
            ("example/live", "TestLive", "explicit smoke model required", "opt-in-exclusion"),
            ("github.com/digitaldrywood/detent/internal/testenv/pgtest", "TestUnconfiguredPostgresFails", "PostgreSQL fixtures require DETENT_TEST_POSTGRES=binary", "missing-postgres"),
            ("example/database", "TestDatabase", "PostgreSQL fixtures require DETENT_TEST_POSTGRES=binary", "missing-postgres"),
            ("example/unknown", "TestUnknown", "unknown exclusion", "unclassified"),
        ):
            with self.subTest(expected=expected):
                self.assertEqual(categorize(package, test, output), expected)


if __name__ == "__main__":
    unittest.main()
