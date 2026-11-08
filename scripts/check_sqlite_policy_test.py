import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest


ROOT = Path(__file__).resolve().parent.parent
POLICY = "internal/invariants/sqlite_policy.json"


class SQLitePolicyLandingTest(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory(dir=os.environ.get("TMPDIR"))
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.git_path = shutil.which("git")
        self.env = {key: value for key, value in os.environ.items() if not key.startswith("GIT_")}
        self.env.update(
            GIT_CONFIG_NOSYSTEM="1", GIT_CONFIG_GLOBAL=os.devnull,
            GIT_AUTHOR_NAME="Example", GIT_AUTHOR_EMAIL="example@example.org",
            GIT_COMMITTER_NAME="Example", GIT_COMMITTER_EMAIL="example@example.org",
            GIT_AUTHOR_DATE="2000-01-01T00:00:00Z", GIT_COMMITTER_DATE="2000-01-01T00:00:00Z",
        )
        self.git("init", "-q", "--template=", "--initial-branch=fixture")
        (self.root / POLICY).parent.mkdir(parents=True)
        binaries = self.root / ".git/bin"
        binaries.mkdir()
        wrapper = "#!" + sys.executable + "\n" + f'''
import json
import os
from pathlib import Path
import sys

args = sys.argv[1:]
if not args or args[0] not in ("rev-parse", "show"):
    sys.exit("history traversal or fetching is forbidden")
if any(value in ("HEAD", "origin/develop") for value in args):
    sys.exit("an implicit ref was used")
with Path(".git/policy-commands").open("a") as output:
    output.write(json.dumps(args) + "\\n")
os.execv({self.git_path!r}, [{self.git_path!r}] + args)
'''
        (binaries / "git").write_text(wrapper)
        (binaries / "git").chmod(0o700)
        self.check_env = dict(self.env, PATH=str(binaries) + os.pathsep + self.env["PATH"])

    def git(self, *args, input_data=None):
        return subprocess.check_output([self.git_path, *args], cwd=self.root, env=self.env, input=input_data, text=True, stderr=subprocess.PIPE).strip()

    def commit_policy(self, policy):
        data = policy if isinstance(policy, str) else json.dumps(policy)
        (self.root / POLICY).write_text(data)
        self.git("add", POLICY)
        tree = self.git("write-tree")
        return self.git("commit-tree", tree, "-m", "test(invariants): seed policy fixture")

    def check(self, target, candidate):
        return subprocess.run([sys.executable, str(ROOT / "scripts/check-sqlite-policy.py"), target, candidate], cwd=self.root, env=self.check_env, capture_output=True, text=True, timeout=30)

    def test_exact_and_shallow_commit_policies(self):
        target = {
            "files": {"existing.go": {"driver_import": 2}},
            "modules": {"go.mod": {"modernc.org/sqlite": 1}, "go.sum": {"modernc.org/sqlite": 2}},
        }
        cases = [
            ("unchanged", target, ""),
            ("removed debt", {"files": {}, "modules": {}}, ""),
            ("new file despite overall reduction", {"files": {"new.go": {"driver_import": 1}}, "modules": {}}, "new.go: SQLite driver_import count 1 exceeds target baseline 0"),
            ("new kind", {"files": {"existing.go": {"sql_pragma": 1}}, "modules": {}}, "existing.go: SQLite sql_pragma count 1 exceeds target baseline 0"),
            ("committed inflated count", {"files": {"existing.go": {"driver_import": 3}}, "modules": {}}, "existing.go: SQLite driver_import count 3 exceeds target baseline 2"),
            ("new module", {"files": {}, "modules": {"go.mod": {"github.com/mattn/go-sqlite3": 1}}}, "go.mod: SQLite github.com/mattn/go-sqlite3 count 1 exceeds target baseline 0"),
            ("checksum count increase", {"files": {}, "modules": {"go.sum": {"modernc.org/sqlite": 3}}}, "go.sum: SQLite modernc.org/sqlite count 3 exceeds target baseline 2"),
        ]
        for shallow in (False, True):
            for name, candidate, want in cases:
                with self.subTest(name=name, shallow=shallow):
                    target_ref = self.commit_policy(target)
                    candidate_ref = self.commit_policy(candidate)
                    if shallow:
                        (self.root / ".git/shallow").write_text("\n".join(sorted({target_ref, candidate_ref})) + "\n")
                    result = self.check(target_ref, candidate_ref)
                    self.assertEqual(result.returncode, int(bool(want)), result.stdout + result.stderr)
                    if want:
                        self.assertIn(want, result.stderr)
                    else:
                        self.assertIn("SQLite policy monotonicity passed", result.stdout)

    def test_explicit_objects_ignore_working_policy_and_need_no_head(self):
        policy = {"files": {"existing.go": {"driver_import": 1}}, "modules": {}}
        target_ref = self.commit_policy(policy)
        candidate_ref = self.commit_policy(policy)
        (self.root / POLICY).write_text("invalid working JSON")
        result = self.check(target_ref, candidate_ref)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_missing_or_invalid_policies_fail_closed(self):
        empty = {"files": {}, "modules": {}}
        target_ref = self.commit_policy(empty)
        invalid = [
            "{", '{"files":{},"files":{},"modules":{}}',
            {"files": {"existing.go": {"driver_import": True}}, "modules": {}},
            {"files": {"existing.go": {"driver_import": -1}}, "modules": {}},
        ]
        for policy in invalid:
            with self.subTest(policy=policy):
                candidate_ref = self.commit_policy(policy)
                result = self.check(target_ref, candidate_ref)
                self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        candidate_ref = self.commit_policy(empty)
        missing_policy = self.git("commit-tree", self.git("mktree", input_data=""), "-m", "test(invariants): seed empty target")
        for target in ("missing-ref", missing_policy):
            with self.subTest(target=target):
                self.assertEqual(self.check(target, candidate_ref).returncode, 1)


if __name__ == "__main__":
    unittest.main()
