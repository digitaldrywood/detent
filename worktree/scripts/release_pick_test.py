import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


ROOT = Path(__file__).resolve().parent.parent


class ReleasePickTest(unittest.TestCase):
    def test_develop_to_release_pr(self):
        scratch = next((os.environ[key] for key in ("TMPDIR", "TMP", "TEMP") if os.environ.get(key)), None)
        self.assertIsNotNone(scratch, "a supplied scratch directory is required")
        for case in ("pick", "outside develop", "invalid target", "dirty", "conflict"):
            with self.subTest(case=case), tempfile.TemporaryDirectory(dir=scratch) as directory:
                root = Path(directory)
                remote = root / "remote.git"
                checkout = root / "checkout"
                env = dict(os.environ, GIT_CONFIG_NOSYSTEM="1", GIT_CONFIG_GLOBAL=os.devnull)

                def git(*args):
                    result = subprocess.run(["git", *args], cwd=checkout, env=env, capture_output=True, text=True, timeout=30)
                    self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                    return result.stdout.strip()

                subprocess.run(["git", "init", "--bare", str(remote)], env=env, check=True, capture_output=True, timeout=30)
                subprocess.run(["git", "clone", str(remote), str(checkout)], env=env, check=True, capture_output=True, timeout=30)
                git("config", "user.name", "Example")
                git("config", "user.email", "example@example.com")
                git("switch", "-c", "develop")
                (checkout / "fix.txt").write_text("original\n")
                git("add", "fix.txt")
                git("commit", "-m", "initial")
                base = git("rev-parse", "HEAD")
                git("push", "origin", "HEAD:develop", "HEAD:release/v1.2")
                (checkout / "fix.txt").write_text("fixed\n")
                git("commit", "-am", "fix")
                sha = git("rev-parse", "HEAD")
                if case != "outside develop":
                    git("push", "origin", "develop")
                if case == "conflict":
                    git("switch", "-c", "release-change", base)
                    (checkout / "fix.txt").write_text("release change\n")
                    git("commit", "-am", "release change")
                    git("push", "origin", "HEAD:release/v1.2")
                    git("switch", "develop")
                if case == "dirty":
                    (checkout / "fix.txt").write_text("uncommitted\n")
                log = root / "calls.jsonl"
                gh = root / "gh"
                gh.write_text("#!" + sys.executable + "\nimport json, sys\nfrom pathlib import Path\nwith Path(" + repr(str(log)) + ").open('a') as f: f.write(json.dumps(sys.argv[1:]) + '\\n')\n")
                gh.chmod(0o700)
                env["PATH"] = str(root) + os.pathsep + env["PATH"]
                target = "develop" if case == "invalid target" else "release/v1.2"
                result = subprocess.run(["bash", str(ROOT / "scripts/release-pick.sh"), sha, target], cwd=checkout, env=env, capture_output=True, text=True, timeout=30)
                self.assertEqual(result.returncode == 0, case == "pick", result.stdout + result.stderr)
                if case == "pick":
                    self.assertIn("(cherry picked from commit " + sha + ")", git("log", "-1", "--format=%B"))
                    self.assertEqual((checkout / "fix.txt").read_text(), "fixed\n")
                    calls = [json.loads(line) for line in log.read_text().splitlines()]
                    self.assertEqual(len(calls), 1)
                    self.assertEqual(calls[0][:4], ["pr", "create", "--base", "release/v1.2"])
                    branch = "pick/v1.2/" + sha[:12]
                    self.assertEqual(git("ls-remote", "origin", "refs/heads/" + branch).split()[0], git("rev-parse", "HEAD"))
                    self.assertEqual(git("ls-remote", "origin", "refs/heads/develop").split()[0], sha)
                else:
                    self.assertFalse(log.exists(), "rejected picks must not open a PR")
                    self.assertEqual(git("ls-remote", "origin", "refs/heads/pick/*"), "", "rejected picks must not push")
                    if case == "conflict":
                        self.assertEqual(git("rev-parse", "CHERRY_PICK_HEAD"), sha)
                        self.assertIn("Resolve the cherry-pick", result.stderr)
                    else:
                        self.assertEqual(git("branch", "--show-current"), "develop")


if __name__ == "__main__":
    unittest.main()
