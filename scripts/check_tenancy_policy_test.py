import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest


ROOT = Path(__file__).resolve().parent.parent
POLICY = "internal/invariants/tenancy_policy.json"
CHECK = "internal/invariants/tenancy_test.go"


class TenancyPolicyLandingTest(unittest.TestCase):
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
        (self.root / CHECK).write_text("package invariants\n")
        binaries = self.root / ".git/bin"
        binaries.mkdir()
        wrapper = "#!" + sys.executable + "\n" + f'''
import os
import sys

args = sys.argv[1:]
if not args or args[0] not in ("rev-parse", "show"):
    sys.exit("history traversal or fetching is forbidden")
if any(value in ("HEAD", "origin/develop") for value in args):
    sys.exit("an implicit ref was used")
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
        self.git("add", POLICY, CHECK)
        return self.git("commit-tree", self.git("write-tree"), "-m", "test(invariants): seed tenancy policy fixture")

    def check(self, target, candidate):
        return subprocess.run([sys.executable, str(ROOT / "scripts/check-tenancy-policy.py"), target, candidate], cwd=self.root, env=self.check_env, capture_output=True, text=True, timeout=30)

    def test_committed_policy_changes(self):
        target = {
            "test_support": ["internal/example/fixturetest", "internal/example/other"],
            "exceptions": {"internal/runner/run.go": {"import:os/exec": 1}},
            "debt": {"internal/cloudentry/tenant/launcher.go": {"process:exec.CommandContext": 1}},
        }
        cases = [
            ("unchanged", target, ""),
            ("removed debt and exemption", {"test_support": ["internal/example/other"], "exceptions": {}, "debt": {}}, ""),
            ("committed inflated debt", dict(target, debt={"internal/cloudentry/tenant/launcher.go": {"process:exec.CommandContext": 2}}), "internal/cloudentry/tenant/launcher.go: debt process:exec.CommandContext count 2 exceeds target baseline 1"),
            ("new debt file despite overall reduction", dict(target, debt={"internal/hubserver/new.go": {"timer:time.NewTicker": 1}}), "internal/hubserver/new.go: debt timer:time.NewTicker count 1 exceeds target baseline 0"),
            ("new named exception", dict(target, exceptions={"internal/runner/run.go": {"import:os/exec": 1}, "internal/helper/run.go": {"import:os/exec": 1}}), "internal/helper/run.go: exceptions import:os/exec count 1 exceeds target baseline 0"),
            ("inflated named exception", dict(target, exceptions={"internal/runner/run.go": {"import:os/exec": 2}}), "internal/runner/run.go: exceptions import:os/exec count 2 exceeds target baseline 1"),
            ("new exemption", dict(target, test_support=target["test_support"] + ["internal/hubserver"]), "internal/hubserver: test-support exemption is not in the target set"),
            ("equal-count substitution", dict(target, test_support=["internal/hubserver", "internal/example/other"]), "internal/hubserver: test-support exemption is not in the target set"),
            ("parent widening", dict(target, test_support=["internal/example", "internal/example/other"]), "internal/example: test-support exemption is not in the target set"),
        ]
        for name, candidate, want in cases:
            with self.subTest(name=name):
                target_ref = self.commit_policy(target)
                candidate_ref = self.commit_policy(candidate)
                result = self.check(target_ref, candidate_ref)
                self.assertEqual(result.returncode, int(bool(want)), result.stdout + result.stderr)
                if want:
                    self.assertIn(want, result.stderr)
                else:
                    self.assertIn("Tenancy policy monotonicity passed", result.stdout)

    def test_invalid_policies_fail_closed(self):
        empty = {"test_support": [], "exceptions": {}, "debt": {}}
        target_ref = self.commit_policy(empty)
        invalid = [
            "{",
            '{"test_support":[],"test_support":[],"exceptions":{},"debt":{}}',
            {"test_support": ["internal/a", "internal/a"], "exceptions": {}, "debt": {}},
            {"test_support": ["internal/a/../b"], "exceptions": {}, "debt": {}},
            {"test_support": [], "exceptions": {}, "debt": {"x.go": {"timer:time.After": 0}}},
            {"exceptions": {}, "debt": {}},
        ]
        for policy in invalid:
            with self.subTest(policy=policy):
                result = self.check(target_ref, self.commit_policy(policy))
                self.assertEqual(result.returncode, 1, result.stdout + result.stderr)

    def test_introduction_and_missing_target_policy(self):
        candidate_ref = self.commit_policy({"test_support": [], "exceptions": {}, "debt": {}})
        before = self.git("commit-tree", self.git("mktree", input_data=""), "-m", "test(invariants): seed target without the invariant")
        result = self.check(before, candidate_ref)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("Tenancy policy introduced", result.stdout)
        check_blob = self.git("hash-object", "-w", str(self.root / CHECK))
        inner = self.git("mktree", input_data=f"100644 blob {check_blob}\ttenancy_test.go\n")
        middle = self.git("mktree", input_data=f"040000 tree {inner}\tinvariants\n")
        top = self.git("mktree", input_data=f"040000 tree {middle}\tinternal\n")
        dropped = self.git("commit-tree", top, "-m", "test(invariants): seed target that lost its policy")
        self.assertEqual(self.check(dropped, candidate_ref).returncode, 1)
        self.assertEqual(self.check("missing-ref", candidate_ref).returncode, 1)

    def test_working_policy_is_ignored(self):
        policy = {"test_support": [], "exceptions": {}, "debt": {"a.go": {"timer:time.After": 1}}}
        target_ref = self.commit_policy(policy)
        candidate_ref = self.commit_policy(policy)
        (self.root / POLICY).write_text("invalid working JSON")
        self.assertEqual(self.check(target_ref, candidate_ref).returncode, 0)


if __name__ == "__main__":
    unittest.main()
