import json
import os
from pathlib import Path
import re
import shutil
import signal
import subprocess
import sys
import tempfile
import unittest
from unittest import mock


ROOT = Path(__file__).resolve().parent.parent
TOOL = "#!" + sys.executable + r'''
import json
import os
from pathlib import Path
import re
import subprocess
import sys

name = Path(sys.argv[0]).name
args = sys.argv[1:]
if name == "python3" and args[:1] == ["-"]:
    os.execv(sys.executable, [sys.executable, *args])
if name == "go" and args[:1] == ["env"]:
    print("fixture")
    sys.exit(0)
with Path(".git/trace").open("a") as stream:
    stream.write(json.dumps({"tool": name, "args": args, "budget": os.environ.get("GOMAXPROCS"), "token": "DETENT_API_TOKEN" in os.environ}) + "\n")
if (name == "go" and args[:1] == [os.environ.get("FAIL_STAGE")]) or (name == "golangci-lint" and os.environ.get("FAIL_STAGE") == "lint"):
    sys.exit(17)
if name == "go" and args[:1] == [os.environ.get("HANG_STAGE")]:
    reader, writer = os.pipe()
    child = os.fork()
    if child == 0:
        print("fixture-ready", flush=True)
        os.read(reader, 1)
    else:
        os.waitpid(child, 0)
if name == "python3" and os.environ.get("FAIL_STAGE") == "nilaway":
    sys.exit(17)
if name == "go" and args[:1] == ["test"] and "-list" not in args and os.environ.get("FAIL_HUB_PATTERN") and "-run" in args and re.search(args[args.index("-run") + 1], os.environ["FAIL_HUB_PATTERN"]):
    sys.exit(17)
if name == "python3" and args[:1] == ["scripts/test-landing-hub.py"]:
    sys.exit(subprocess.run([sys.executable, *args]).returncode)
if name == "make" and any(target in args for target in ("test-landing-hub", "nilaway-changed")):
    sys.exit(subprocess.run([os.environ["REAL_MAKE"], *args]).returncode)
if name == "go" and args[:1] == ["test"] and "./internal/hubserver" in args and os.environ.get("HUB_TEST_BINARY"):
    flags = ["-test." + flag[1:] for flag in args if flag.startswith("-timeout=") or flag == "-short"]
    for flag in ("-run", "-list"):
        if flag in args:
            flags.extend(["-test." + flag[1:], args[args.index(flag) + 1]])
    sys.exit(subprocess.run([os.environ["HUB_TEST_BINARY"], *flags], cwd="internal/hubserver").returncode)
if name == "go" and args[:1] == ["test"] and os.environ.get("RUN_GO_TESTS"):
    sys.exit(subprocess.run([os.environ["REAL_GO"], *args]).returncode)
if name == "go" and args[:1] == ["list"]:
    for directory, imports, test_imports in (
        ("lib", [], []),
        ("app", ["example.test/lib"], []),
        ("integration", [], ["example.test/app"]),
        ("unrelated", [], []),
        ("removed", [], []),
        ("internal/hubserver", ["example.test/lib"], []),
    ):
        if not list(Path(directory).glob("*.go")):
            continue
        print(json.dumps({"Dir": str(Path(directory).resolve()), "ImportPath": "example.test/" + directory, "Imports": imports, "TestImports": test_imports, "EmbedFiles": ["data.txt"] if directory == "lib" else []}))
'''


@unittest.skipIf(os.name == "nt", "landing gate requires a POSIX shell")
class LandingGateTest(unittest.TestCase):
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
        for name in ("check-landing.sh", "check-evidence.sh", "test-landing-hub.py"):
            shutil.copy2(ROOT / "scripts" / name, self.root / "scripts" / name)
        for name in ("Makefile", ".golangci-version"):
            shutil.copy2(ROOT / name, self.root / name)
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
        for name in ("go", "make", "golangci-lint", "python3"):
            tool = binaries / name
            tool.write_text(TOOL)
            tool.chmod(0o755)
        self.env = dict(os.environ, PATH=str(binaries) + os.pathsep + os.environ["PATH"], DETENT_API_TOKEN="fixture-token", GOCACHEPROG="", REAL_GO=shutil.which("go"), REAL_MAKE=shutil.which("make"))
        self.linter = str(binaries / "golangci-lint")

    def git(self, *args):
        return subprocess.check_output(["git", *args], cwd=self.root, text=True, stderr=subprocess.STDOUT)

    def commit(self):
        self.git("add", ".")
        self.git("commit", "-qm", "fixture")

    def run_gate(self, base="origin/develop", budget="2"):
        (self.root / ".git/trace").write_text("")
        command = ["bash", "scripts/check-landing.sh", base, budget, self.linter]
        with subprocess.Popen(command, cwd=self.root, env=self.env, stdout=subprocess.PIPE,
                              stderr=subprocess.PIPE, text=True, start_new_session=True) as process:
            try:
                stdout, stderr = process.communicate(timeout=30)
            except BaseException:
                try:
                    os.killpg(process.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                process.communicate()
                raise
            return subprocess.CompletedProcess(command, process.returncode, stdout, stderr)

    def test_gate_timeout_joins_descendants_before_next_selection(self):
        self.env["HANG_STAGE"] = "build"
        communicate = subprocess.Popen.communicate
        killpg = os.killpg
        stopped = set()
        processes = []
        drained = set()

        def stop(group, sig):
            if sig == signal.SIGKILL:
                stopped.add(group)
            return killpg(group, sig)

        def expire(process, *args, **kwargs):
            if kwargs.get("timeout") == 30:
                processes.append(process)
                output = b""
                while b"fixture-ready\n" not in output:
                    chunk = os.read(process.stdout.fileno(), 65536)
                    self.assertTrue(chunk, "fixture exited before its child was ready")
                    output += chunk
                raise subprocess.TimeoutExpired(process.args, 30)
            result = communicate(process, *args, timeout=5, **kwargs)
            drained.add(process.pid)
            return result

        try:
            with mock.patch.object(os, "killpg", stop), mock.patch.object(subprocess.Popen, "communicate", expire):
                with self.assertRaises(subprocess.TimeoutExpired) as caught:
                    self.run_gate()
                self.assertEqual(caught.exception.timeout, 30)
                self.assertEqual(len(processes), 1)
                self.assertIn(processes[0].pid, stopped, "gate timeout left fixture descendants running")
                self.assertIn(processes[0].pid, drained, "gate timeout did not drain descendant output")
        finally:
            for process in processes:
                if process.poll() is None:
                    try:
                        killpg(process.pid, signal.SIGKILL)
                    except ProcessLookupError:
                        pass
                process.wait(timeout=5)

    def trace(self):
        path = self.root / ".git/trace"
        return [json.loads(line) for line in path.read_text().splitlines()] if path.exists() else []

    def test_non_short_failure_blocks_only_a_directly_changed_package(self):
        for directory, expected in (("lib", 1), ("app", 0)):
            with self.subTest(directory=directory):
                target = self.root / directory / "failure_test.go"
                target.write_text(
                    f'package {directory}\nimport "testing"\n'
                    'func TestFullFailure(t *testing.T) {\n'
                    'if testing.Short() { t.Skip("full suite") }\n'
                    't.Fatal("non-short regression executed")\n}\n'
                )
                self.commit()
                self.git("update-ref", "refs/remotes/origin/develop", self.git("rev-parse", "HEAD").strip())
                (self.root / "lib/source.go").write_text(f'package lib\nconst Value = "{directory}"\n')
                self.commit()
                self.env["RUN_GO_TESTS"] = "1"
                result = self.run_gate()
                self.assertEqual(result.returncode, expected, result.stdout + result.stderr)
                if expected:
                    self.assertIn("non-short regression executed", result.stdout)
                target.unlink()

    def test_changed_package_includes_transitive_and_test_dependents(self):
        cases = (
            ("test only", ("lib/source_test.go",), ["./lib"], [], "1"),
            ("production", ("lib/source.go",), ["./lib"], ["./app", "./integration"], "4"),
            ("mixed", ("lib/source.go", "lib/source_test.go"), ["./lib"], ["./app", "./integration"], "7"),
            ("direct dependent", ("lib/source.go", "app/source_test.go"), ["./app", "./lib"], ["./integration"], "2"),
            ("separate test and production", ("lib/source_test.go", "unrelated/source.go"), ["./lib", "./unrelated"], [], "2"),
        )
        for index, (name, paths, full, short, budget) in enumerate(cases):
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
                trace = self.trace()
                self.assertEqual([(call["tool"], call["args"][0]) for call in trace], [("make", "-j1"), ("go", "build"), ("go", "list"), ("go", "test")] + ([("go", "test")] if short else []) + [("make", "-j1"), ("python3", "scripts/nilaway-changed.py"), ("make", "-j1"), ("golangci-lint", "run")])
                tests = [call for call in trace if call["tool"] == "go" and call["args"][0] == "test"]
                self.assertEqual([arg for arg in tests[0]["args"] if arg.startswith("./")], full)
                self.assertNotIn("-short", tests[0]["args"])
                if short:
                    self.assertEqual([arg for arg in tests[1]["args"] if arg.startswith("./")], short)
                    self.assertIn("-short", tests[1]["args"])
                for test in tests:
                    self.assertFalse(test["token"])
                    self.assertIn("-timeout=10m", test["args"])
                nilaway = next(call for call in trace if "nilaway-changed" in call["args"])
                self.assertIn("NILAWAY_PACKAGES=" + " ".join(sorted(full + short)), nilaway["args"])
                self.assertFalse(nilaway["token"])
                audit = next(call for call in trace if call["tool"] == "python3")
                self.assertEqual(audit["args"][3:], sorted(full + short))
                self.assertFalse(audit["token"])
                receipts = [json.loads(line.removeprefix("detent-check-evidence: ")) for line in result.stdout.splitlines() if line.startswith("detent-check-evidence: ")]
                self.assertEqual([receipt["scope"] for receipt in receipts], ["assets", "build", "unit-full"] + (["unit-short"] if short else []) + ["nilaway", "lint"])
                for receipt in receipts:
                    self.assertEqual(receipt["head_sha"], self.git("rev-parse", "HEAD").strip())
                    self.assertEqual(receipt["tree_sha"], self.git("rev-parse", "HEAD^{tree}").strip())
                    self.assertEqual(receipt["exit_code"], 0)
                for call in trace:
                    if call["tool"] == "go":
                        full_test = call["args"][0] == "test" and "-short" not in call["args"]
                        self.assertEqual(call["budget"], budget if full_test else "1")
                        if call["args"][0] in ("build", "test"):
                            package_procs = int(call["args"][call["args"].index("-p") + 1])
                            self.assertEqual(package_procs, 1 if full_test else int(budget))
                            self.assertLessEqual(package_procs * int(call["budget"]), int(budget))
                    elif call["tool"] == "golangci-lint":
                        self.assertEqual(call["budget"], budget)
                        self.assertIn("--concurrency=" + budget, call["args"])
                    else:
                        self.assertEqual(call["budget"], "1")
                        if call["tool"] == "make":
                            self.assertIn("TEST_PROCS=" + budget, call["args"])
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
        self.assertNotIn("-short", test["args"])
        self.assertEqual(test["args"][-5:], ["./app", "./integration", "./lib", "./removed", "./unrelated"])
        self.assertEqual(trace[-1]["tool"], "golangci-lint")
        self.assertEqual(trace[-1]["args"][-5:], test["args"][-5:])
        self.assertFalse(any(arg.startswith("--new-from-rev") for arg in trace[-1]["args"]))

    def test_hub_uses_existing_slices_only_for_full_tests(self):
        hub = self.root / "internal/hubserver"
        hub.mkdir(parents=True)
        (hub / "source.go").write_text("package hubserver\n")
        names = [*"ABCDEFGHIJKLM", "NativeA", "NativeZ", "Normalize", *"OPQRSTUVWXYZ"]
        (hub / "source_test.go").write_text(
            'package hubserver\nimport ("os"; "testing")\n' + "\n".join(
                f'func Test{letter}(t *testing.T) {{\n'
                'if testing.Short() { t.Skip("full suite") }\n'
                'f, err := os.OpenFile("../../.git/test-names", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)\n'
                'if err != nil { t.Fatal(err) }; defer f.Close()\n'
                f'if _, err := f.WriteString("{letter}\\n"); err != nil {{ t.Fatal(err) }}\n}}'
                for letter in names
            )
        )
        binary = self.root / ".git/hub.test"
        subprocess.run([self.env["REAL_GO"], "test", "-p", "1", "-c", "-o", str(binary), "./internal/hubserver"],
                       cwd=self.root, env=self.env, check=True, capture_output=True, timeout=30)
        self.env["HUB_TEST_BINARY"] = str(binary)
        self.commit()
        for index, path in enumerate(("internal/hubserver/source.go", "go.mod", "lib/source.go", "internal/hubserver/source.go", "internal/hubserver/source.go")):
            with self.subTest(path=path, index=index):
                self.git("update-ref", "refs/remotes/origin/develop", self.git("rev-parse", "HEAD").strip())
                with (self.root / path).open("a") as stream:
                    stream.write("\n" if path == "go.mod" else f"const Value{index} = {index}\n")
                self.commit()
                self.env["RUN_GO_TESTS"] = "1"
                if index >= 3:
                    self.env["FAIL_HUB_PATTERN"] = "TestH" if index == 3 else "TestB"
                (self.root / ".git/test-names").write_text("")
                result = self.run_gate()
                self.assertEqual(result.returncode, 2 if index >= 3 else 0, result.stdout + result.stderr)
                tests = [call for call in self.trace() if call["tool"] == "go" and call["args"][0] == "test" and "-list" not in call["args"]]
                hub_tests = [call for call in tests if "./internal/hubserver" in call["args"]]
                if path == "lib/source.go":
                    self.assertEqual(len(hub_tests), 1)
                    self.assertIn("-short", hub_tests[0]["args"])
                    self.assertNotIn("test-landing-hub", result.stdout)
                else:
                    self.assertEqual(len(hub_tests), {3: 3, 4: 1}.get(index, 6))
                    for call in hub_tests:
                        self.assertNotIn("-short", call["args"])
                        self.assertIn("-timeout=10m", call["args"])
                        self.assertEqual(call["budget"], "2")
                        self.assertEqual(call["args"][call["args"].index("-p") + 1], "1")
                        self.assertFalse(call["token"])
                        pattern = call["args"][call["args"].index("-run") + 1]
                        self.assertLessEqual(sum(bool(re.search(pattern, "Test" + name)) for name in names), 10)
                    if index < 3:
                        self.assertEqual(sorted((self.root / ".git/test-names").read_text().splitlines()), sorted(names))
                        for number in range(1, 6):
                            self.assertIn(f'"scope":"unit-full-hub-{number}"', result.stdout)
                    else:
                        self.assertNotIn("nilaway-changed", result.stdout)
                        if index == 3:
                            self.assertIn('"scope":"unit-full-hub-2"', result.stdout)
                        else:
                            self.assertNotIn('"scope":"unit-full-hub-2"', result.stdout)
                        self.assertIn('"exit_code":17', result.stdout)

    def test_fixture_changes_select_tests_without_lint(self):
        for path in ("lib/testdata/fixture.json", "lib/data.txt"):
            with self.subTest(path=path):
                (self.root / path).write_text("changed\n")
                self.commit()
                result = self.run_gate()
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                trace = self.trace()
                tests = [call for call in trace if call["args"][0] == "test"]
                self.assertEqual(tests[0]["args"][-1:], ["./lib"])
                self.assertNotIn("-short", tests[0]["args"])
                self.assertEqual(tests[1]["args"][-2:], ["./app", "./integration"])
                self.assertIn("-short", tests[1]["args"])
                self.assertFalse(any(call["tool"] == "golangci-lint" for call in trace))

    def test_deleted_package_is_not_executed(self):
        (self.root / "removed/source.go").unlink()
        self.commit()
        result = self.run_gate()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertFalse(any(call["args"][0] == "test" or call["tool"] == "golangci-lint" for call in self.trace()))

    def test_failures_stop_the_gate(self):
        for stage in ("build", "list", "test", "nilaway", "lint"):
            with self.subTest(stage=stage):
                (self.root / "lib/source.go").write_text("package lib\nconst Value = 1\n")
                if stage == "build":
                    self.commit()
                self.env["FAIL_STAGE"] = stage
                result = self.run_gate()
                self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertEqual(result.returncode, {"list": 1, "nilaway": 2}.get(stage, 17))
                self.assertEqual(any(call["tool"] == "golangci-lint" for call in self.trace()), stage == "lint")

    def test_invalid_base_or_budget_runs_no_commands(self):
        for base, budget in (("missing-base", "2"), ("origin/develop", "0")):
            with self.subTest(base=base, budget=budget):
                result = self.run_gate(base, budget)
                self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertEqual(self.trace(), [])


if __name__ == "__main__":
    unittest.main()
