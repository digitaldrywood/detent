import json
import os
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]


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


if __name__ == "__main__":
    unittest.main()
