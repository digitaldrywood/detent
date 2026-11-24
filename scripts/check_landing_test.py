import hashlib
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
    stream.write(json.dumps({
        "tool": name, "args": args, "budget": os.environ.get("GOMAXPROCS"),
        "token": "DETENT_API_TOKEN" in os.environ, "postgres": os.environ.get("DETENT_TEST_POSTGRES"),
        "tmpdir": os.environ.get("TMPDIR"), "git_protocols": os.environ.get("GIT_ALLOW_PROTOCOL"),
    }) + "\n")
if (name == "go" and args[:1] == [os.environ.get("FAIL_STAGE")]) or (name == "golangci-lint" and os.environ.get("FAIL_STAGE") == "lint"):
    sys.exit(17)
if name == "go" and args[:1] == ["list"]:
    embeds = {
        "lib": ["data.txt"],
        ".": ["Makefile", "scripts/check-landing.sh", "scripts/check-evidence.sh", "scripts/check-landing.py"],
    }
    for directory, imports, test_imports in (
        ("lib", [], []),
        ("app", ["example.test/lib", "example.test"], []),
        ("integration", [], ["example.test/app"]),
        ("consumer", ["example.test/integration"], []),
        ("dual", ["example.test/lib"], ["example.test/app"]),
        ("dualconsumer", ["example.test/dual"], []),
        ("unrelated", [], []),
        ("removed", [], []),
        ("internal/workspace/validationcmd", ["example.test"], []),
        (".", [], []),
    ):
        if not list(Path(directory).glob("*.go")):
            continue
        print(json.dumps({"Dir": str(Path(directory).resolve()), "ImportPath": "example.test" if directory == "." else "example.test/" + directory, "Imports": imports, "TestImports": test_imports, "EmbedFiles": embeds.get(directory, [])}))
    for directory in sorted(Path("many").glob("*/source.go")):
        print(json.dumps({"Dir": str(directory.parent.resolve()), "ImportPath": "example.test/" + directory.parent.as_posix()}))
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
        for name in ("check-landing.sh", "check-evidence.sh", "check-landing.py"):
            shutil.copy2(ROOT / "scripts" / name, self.root / "scripts" / name)
        (self.root / "go.mod").write_text("module example.test\n\ngo 1.26\n")
        for directory in ("lib", "app", "integration", "unrelated", "removed"):
            (self.root / directory).mkdir()
            (self.root / directory / "source.go").write_text("package " + directory + "\n")
        (self.root / "lib/data.txt").write_text("old\n")
        (self.root / "lib/testdata").mkdir()
        (self.root / "lib/testdata/fixture.json").write_text("{}\n")
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

    def test_changed_package_includes_direct_importers_and_skips_assets_for_go_only(self):
        self.env["GIT_ALLOW_PROTOCOL"] = ""
        cases = (
            ("test only", ("lib/source_test.go",), ["./lib"], "1", 1, 4),
            ("test only with budget 4", ("lib/source_test.go",), ["./lib"], "4", 1, 4),
            ("production 1-hop", ("lib/source.go",), ["./app", "./lib"], "4", 2, 4),
            ("mixed", ("lib/source.go", "lib/source_test.go"), ["./app", "./lib"], "7", 2, 7),
            ("separate test and production", ("lib/source_test.go", "unrelated/source.go"), ["./lib", "./unrelated"], "2", 2, 4),
        )
        for index, (name, paths, expected, budget, width, threads) in enumerate(cases):
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
                self.assertEqual([(call["tool"], call["args"][0]) for call in trace], [("go", "build"), ("go", "list"), ("go", "test"), ("make", "-j1"), ("golangci-lint", "run")])
                self.assertEqual([call["git_protocols"] for call in trace], ["", "", "", "https", ""])
                test = trace[2]
                self.assertEqual([arg for arg in test["args"] if arg.startswith("./")], sorted(expected))
                self.assertFalse(test["token"])
                self.assertEqual(test["postgres"], "binary")
                self.assertEqual(test["tmpdir"], self.env["TMPDIR"])
                self.assertIn("-json", test["args"])
                for call in trace:
                    if call["tool"] == "go" and call["args"][0] == "test":
                        self.assertEqual((int(call["args"][call["args"].index("-p") + 1]), int(call["budget"])), (width, threads))
                    elif call["tool"] == "go":
                        self.assertEqual(call["budget"], budget)
                        if call["args"][0] == "build":
                            self.assertEqual(int(call["args"][call["args"].index("-p") + 1]), int(budget))
                    elif call["tool"] == "golangci-lint":
                        self.assertEqual(call["budget"], budget)
                        self.assertIn("--concurrency=" + budget, call["args"])
                    else:
                        self.assertEqual(call["budget"], budget)
                        self.assertIn("TEST_PROCS=" + budget, call["args"])
                self.assertIn("-timeout=30m", test["args"])
                self.assertEqual([arg for arg in trace[-1]["args"] if arg.startswith("./")], sorted({"./" + str(Path(path).parent) for path in paths}))
                self.assertIn("--new-from-rev=" + self.base, trace[-1]["args"])

    def test_embedded_landing_script_selects_its_evidence_consumer(self):
        consumer = "internal/workspace/validationcmd"
        (self.root / consumer).mkdir(parents=True)
        (self.root / consumer / "evidence.go").write_text("package validationcmd\n")
        (self.root / "embed.go").write_text("package fixture\n")
        (self.root / "Makefile").write_text("TEST_PROCS ?= 4\n")
        self.commit()
        for path, additional in (
            ("scripts/check-landing.sh", []),
            ("scripts/check-evidence.sh", []),
            ("scripts/check-landing.py", []),
            ("Makefile", ["./app"]),
        ):
            with self.subTest(path=path):
                self.git("update-ref", "refs/remotes/origin/develop", "HEAD")
                with (self.root / path).open("a") as stream:
                    stream.write("\n")
                self.commit()
                result = self.run_gate()
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                test = next(call for call in self.trace() if call["tool"] == "go" and call["args"][0] == "test")
                selected = [arg for arg in test["args"] if arg == "." or arg.startswith("./")]
                self.assertEqual(selected, sorted([".", "./" + consumer, *additional]))

    def test_test_imports_select_one_hop_without_transitive_explosion(self):
        for name in ("consumer", "dual", "dualconsumer"):
            (self.root / name).mkdir()
            (self.root / name / "source.go").write_text("package " + name + "\n")
        self.commit()
        self.git("update-ref", "refs/remotes/origin/develop", "HEAD")
        (self.root / "lib/source.go").write_text("package lib\nconst Value = 1\n")
        self.commit()
        result = self.run_gate()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        test = next(call for call in self.trace() if call["tool"] == "go" and call["args"][0] == "test")
        self.assertEqual([arg for arg in test["args"] if arg.startswith("./")], ["./app", "./dual", "./lib"])
        (self.root / "integration/source.go").write_text("package integration\nconst Value = 1\n")
        self.commit()
        self.git("update-ref", "refs/remotes/origin/develop", "HEAD~1")
        result = self.run_gate()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        test = next(call for call in self.trace() if call["tool"] == "go" and call["args"][0] == "test")
        self.assertEqual([arg for arg in test["args"] if arg.startswith("./")], ["./consumer", "./integration"])

    def test_wide_selection_records_bounded_unit_evidence(self):
        names = ["./many/" + "feature_package_with_a_long_descriptive_name_" + str(index) for index in range(150)]
        for name in names:
            (self.root / name).mkdir(parents=True)
            (self.root / name / "source.go").write_text("package fixture\n")
        self.commit()
        result = self.run_gate()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        packages = sorted(names)
        test = next(call for call in self.trace() if call["tool"] == "go" and call["args"][0] == "test")
        self.assertEqual([arg for arg in test["args"] if arg.startswith("./")], packages)
        evidence = [json.loads(line.split(": ", 1)[1]) for line in result.stdout.splitlines() if line.startswith("detent-check-evidence: ")]
        unit = next(check for check in evidence if check["scope"] == "unit-short")
        self.assertRegex(unit["command"], r"^[a-zA-Z0-9_. /:+,=-]{1,256}$")
        digest = hashlib.sha256("".join(package + "\n" for package in packages).encode()).hexdigest()
        self.assertEqual(unit["command"], f"python3 scripts/check-landing.py tests --package-count={len(packages)} --package-sha256={digest} -p 2 -short -timeout=30m")

    def test_test_threads_allow_parallel_subtests(self):
        contract = runpy.run_path(str(ROOT / "scripts/check-landing.py"))
        for selected, budget, width, threads in (
            (1, 2, 1, 4),
            (2, 2, 2, 4),
            (8, 2, 2, 4),
            (1, 4, 1, 4),
            (4, 4, 4, 4),
            (8, 8, 8, 8),
            (1, 1, 1, 4),
        ):
            with self.subTest(selected=selected, budget=budget):
                packages = ["./feature" + str(index) for index in range(selected)]
                arguments, gomaxprocs = contract["thread_budget"](["-p", str(budget), "-short", *packages])
                self.assertEqual(arguments, ["-p", str(width), "-short", *packages])
                self.assertEqual(gomaxprocs, threads)

    def test_unit_package_list_must_match_recorded_digest(self):
        contract = runpy.run_path(str(ROOT / "scripts/check-landing.py"))
        packages = ["./internal/pgtransport", "./lib"]
        recorded = ["--package-count=2", "--package-sha256=" + hashlib.sha256(b"./internal/pgtransport\n./lib\n").hexdigest(), "-short"]
        self.assertEqual(contract["package_set"](recorded, "\n".join(packages) + "\n"), ["-short"] + packages)
        for name, listed in (("missing", None), ("extra", "\n".join(packages + ["./unrelated"])), ("changed", "./internal/pgtransport\n./app\n")):
            with self.subTest(name=name), self.assertRaises(ValueError):
                contract["package_set"](recorded, listed)

    def test_module_change_checks_all_current_packages(self):
        with (self.root / "go.mod").open("a") as stream:
            stream.write("toolchain go1.26.0\n")
        self.commit()
        result = self.run_gate()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        trace = self.trace()
        test = next(call for call in trace if call["args"][0] == "test")
        self.assertEqual([arg for arg in test["args"] if arg.startswith("./")], ["./app", "./integration", "./lib", "./removed", "./unrelated"])
        self.assertEqual(trace[-1]["tool"], "golangci-lint")
        self.assertEqual(trace[-1]["args"][-5:], ["./app", "./integration", "./lib", "./removed", "./unrelated"])
        self.assertFalse(any(arg.startswith("--new-from-rev") for arg in trace[-1]["args"]))

    def test_fixture_changes_select_tests_without_lint(self):
        (self.root / "root.go").write_text("package example\n")
        self.commit()
        self.git("update-ref", "refs/remotes/origin/develop", "HEAD")
        for path, expected, expects_assets in (
            ("lib/testdata/fixture.json", ["./app", "./lib"], False),
            ("lib/data.txt", ["./app", "./lib"], False),
            ("lib/routing.json", ["./lib"], False),
            ("lib/mappings/v1.json", ["./lib"], False),
            ("docs/guide.md", ["."], True),
            ("internal/config/schema.json", ["."], True),
            ("internal/web/templates/classes.txt", ["."], True),
        ):
            with self.subTest(path=path):
                (self.root / path).parent.mkdir(parents=True, exist_ok=True)
                (self.root / path).write_text("changed\n")
                self.commit()
                result = self.run_gate()
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                trace = self.trace()
                test = next(call for call in trace if call["args"][0] == "test")
                self.assertEqual([arg for arg in test["args"] if arg == "." or arg.startswith("./")], sorted(expected))
                self.assertFalse(any(call["tool"] == "golangci-lint" for call in trace))
                self.assertEqual(any(call["tool"] == "make" and "assets" in call["args"] for call in trace), expects_assets)
                self.git("update-ref", "refs/remotes/origin/develop", "HEAD")

    def test_deleted_package_is_not_executed(self):
        (self.root / "removed/source.go").unlink()
        self.commit()
        result = self.run_gate()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertFalse(any(call["tool"] == "go" and call["args"][0] == "test" for call in self.trace()))
        self.assertFalse(any(call["tool"] == "golangci-lint" for call in self.trace()))

    def test_postgres_skip_is_rejected(self):
        (self.root / "lib/source.go").write_text("package lib\nconst Value = 1\n")
        self.commit()
        self.env["TEST_EVENTS"] = json.dumps([
            {"Action": "output", "Package": "example.test/lib", "Test": "TestDB", "Output": "PostgreSQL fixtures require DETENT_TEST_POSTGRES=binary"},
            {"Action": "skip", "Package": "example.test/lib", "Test": "TestDB"},
        ])
        result = self.run_gate()
        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("PostgreSQL scenario skipped", result.stdout + result.stderr)

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
        (self.root / "lib/source.go").write_text("package lib\nconst Value = 1\n")
        self.commit()
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
        (ROOT / "tmp").mkdir(exist_ok=True)
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

    def test_moved_target_compares_only_candidate_commits(self):
        (self.root / "notes.txt").write_text("fork\n")
        self.commit()
        fork = self.git("rev-parse", "HEAD").strip()
        (self.root / "unrelated/source.go").write_text("package unrelated\nconst Value = 1\n")
        self.commit()
        target = self.git("rev-parse", "HEAD").strip()

        def checkout(start):
            self.git("checkout", "-q", "--detach", start)
            return start

        def candidate(start):
            checkout(start)
            (self.root / "lib/source_test.go").write_text("package lib\nconst TestValue = 1\n")
            self.commit()
            return self.git("rev-parse", "HEAD").strip()

        def merged():
            candidate(fork)
            self.git("merge", "-q", "--no-edit", target)
            return target

        def criss_cross():
            candidate(fork)
            job = self.git("rev-parse", "HEAD").strip()
            self.git("checkout", "-q", "--detach", target)
            self.git("merge", "-q", "--no-commit", job)
            (self.root / "lib/source.go").write_text("package lib\nconst Value = 2\n")
            self.commit()
            self.git("update-ref", "refs/remotes/origin/develop", "HEAD")
            self.git("checkout", "-q", "--detach", job)
            self.git("merge", "-q", "--no-edit", target)
            return self.git("rev-parse", "origin/develop").strip()

        def disjoint():
            self.git("checkout", "-q", "--orphan", "disjoint")
            self.commit()

        cases = (
            ("behind target", lambda: checkout(fork), [], None, None),
            ("equal to target", lambda: checkout(target), [], None, None),
            ("based before target moved", lambda: candidate(fork) and fork, ["./lib"], ["./lib"], None),
            ("merged target", merged, ["./lib"], ["./lib"], None),
            ("criss-cross merge", criss_cross, ["./app", "./lib"], ["./lib"], None),
            ("no shared history", disjoint, None, None, "no merge base with landing target {landing}"),
        )
        for name, prepare, tests, lint, refusal in cases:
            with self.subTest(name=name):
                self.git("update-ref", "refs/remotes/origin/develop", target)
                comparison = prepare()
                landing = self.git("rev-parse", "origin/develop").strip()
                result = self.run_gate()
                trace = self.trace()
                if refusal:
                    self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
                    self.assertIn(refusal.format(landing=landing), result.stderr)
                    self.assertEqual(trace, [])
                    continue
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                head = self.git("rev-parse", "HEAD").strip()
                self.assertIn(f"Landing comparison: base={comparison} head={head} target={landing}", result.stdout)
                test_calls = [call for call in trace if call["tool"] == "go" and call["args"][0] == "test"]
                if not tests:
                    self.assertEqual(test_calls, [])
                else:
                    self.assertEqual([arg for arg in test_calls[0]["args"] if arg.startswith("./")], sorted(tests))
                linted = [call for call in trace if call["tool"] == "golangci-lint"]
                if lint is None:
                    self.assertEqual(linted, [])
                    continue
                self.assertEqual([arg for arg in linted[0]["args"] if arg.startswith("./")], lint)
                self.assertIn("--new-from-rev=" + comparison, linted[0]["args"])

    def test_shallow_candidate_uses_explicit_target_without_origin(self):
        (self.root / "lib/source.go").write_text("package lib\nconst Value = 1\n")
        self.commit()
        head = self.git("rev-parse", "HEAD").strip()
        self.git("update-ref", "-d", "refs/remotes/origin/develop")
        for name, boundary, passes in (("target in history", self.base, True), ("target outside history", head, False)):
            with self.subTest(name=name):
                (self.root / ".git/shallow").write_text(boundary + "\n")
                result = self.run_gate(self.base)
                if passes:
                    self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                    self.assertIn("Landing comparison: base=" + self.base, result.stdout)
                else:
                    self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
                    self.assertIn("no merge base with landing target " + self.base, result.stderr)
                    self.assertEqual(self.trace(), [])


class PostgresInventoryTest(unittest.TestCase):
    def test_inventory_rejects_failed_or_missing_postgres_events(self):
        contract = runpy.run_path(str(ROOT / "scripts/check-landing.py"))
        cases = (
            ("empty pass", [{"Action": "pass", "Package": "example.test/lib"}], 0, "PostgreSQL contract"),
            ("package failed", [{"Action": "fail", "Package": "example.test/lib"}], 1, "failed go test event"),
            ("missing PostgreSQL", [
                {"Action": "output", "Package": "example.test/lib", "Test": "TestDB", "Output": "PostgreSQL fixtures require DETENT_TEST_POSTGRES=binary"},
                {"Action": "skip", "Package": "example.test/lib", "Test": "TestDB"},
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
