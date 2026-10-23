import json
import os
from pathlib import Path
import shutil
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
    stream.write(json.dumps({"tool": name, "args": args, "budget": os.environ.get("GOMAXPROCS"), "token": "DETENT_API_TOKEN" in os.environ}) + "\n")
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
        for name in ("check-landing.sh", "check-evidence.sh"):
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
        for name in ("go", "make", "golangci-lint"):
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
                trace = self.trace()
                self.assertEqual([(call["tool"], call["args"][0]) for call in trace], [("make", "-j1"), ("go", "build"), ("go", "list"), ("go", "test"), ("make", "-j1"), ("golangci-lint", "run")])
                test = trace[3]
                self.assertEqual([arg for arg in test["args"] if arg.startswith("./")], expected)
                self.assertFalse(test["token"])
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
        self.assertEqual(test["args"][-5:], ["./app", "./integration", "./lib", "./removed", "./unrelated"])
        self.assertEqual(trace[-1]["tool"], "golangci-lint")
        self.assertEqual(trace[-1]["args"][-5:], test["args"][-5:])
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
                self.assertEqual(test["args"][-3:], ["./app", "./integration", "./lib"])
                self.assertFalse(any(call["tool"] == "golangci-lint" for call in trace))

    def test_deleted_package_is_not_executed(self):
        (self.root / "removed/source.go").unlink()
        self.commit()
        result = self.run_gate()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertFalse(any(call["args"][0] == "test" or call["tool"] == "golangci-lint" for call in self.trace()))

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


if __name__ == "__main__":
    unittest.main()
