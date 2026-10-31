import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


class ReleaseAuditLocalTest(unittest.TestCase):
    def test_verdict_publication(self):
        source = Path(__file__).resolve().parent
        finding = {"severity": "p1", "file": "example.go", "line": 1, "summary": "Confirmed injection"}
        for name, message, codex_exit, state, suite in (
            ("pass", {"verdict": "pass", "findings": []}, 0, "success", "success"),
            ("suite absent", {"verdict": "pass", "findings": []}, 0, "success", "absent"),
            ("suite superseded", {"verdict": "pass", "findings": []}, 0, "success", "superseded"),
            ("suite wrong SHA", {"verdict": "pass", "findings": []}, 0, "success", "wrong-sha"),
            ("fail", {"verdict": "fail", "findings": [finding]}, 0, "failure", "success"),
            ("inconsistent", {"verdict": "pass", "findings": [finding]}, 0, None, "success"),
            ("malformed", "not json", 0, None, "success"),
            ("multiple objects", '{}\n{}', 0, None, "success"),
            ("provider failure", {"verdict": "pass", "findings": []}, 1, None, "success"),
        ):
            with self.subTest(name=name), tempfile.TemporaryDirectory() as directory:
                root = Path(directory).resolve()
                repo = root / "repo"
                repo.mkdir()
                env = {"PATH": os.environ["PATH"], "HOME": str(root), "TMPDIR": str(root), "GIT_CONFIG_NOSYSTEM": "1"}

                def git(*args):
                    return subprocess.check_output(["git", *args], cwd=repo, env=env, text=True, stderr=subprocess.DEVNULL).strip()

                git("init")
                git("config", "user.name", "Example")
                git("config", "user.email", "example@example.test")
                (repo / "example.go").write_text("package example\n")
                git("add", ".")
                git("commit", "-m", "base")
                base = git("rev-parse", "HEAD")
                git("branch", "release/v1.9")
                git("branch", "release/v1.2")
                (repo / "example.go").write_text("package example\n\nvar Value = 1\n")
                git("commit", "-am", "head")
                head = git("rev-parse", "HEAD")
                git("branch", "release/v1.10")
                git("branch", "release/v2.0")
                git("remote", "add", "origin", str(repo))
                scripts = repo / "scripts"
                scripts.mkdir()
                for filename in ("release-audit-local.sh", "release-audit-prompt.md"):
                    shutil.copyfile(source / filename, scripts / filename)
                binaries = root / "bin"
                binaries.mkdir()
                calls = root / "calls"
                (binaries / "gh").write_text(
                    '#!/usr/bin/env python3\n'
                    'import json, os, sys\n'
                    'args = sys.argv[1:]\n'
                    'with open(os.environ["CALLS"], "a") as f: f.write(json.dumps(args) + "\\n")\n'
                    'if args[:2] == ["repo", "view"]: print("example-org/example-project")\n'
                    'elif args[:2] == ["variable", "get"]: print(json.dumps(["operator"]))\n'
                    'elif any("/statuses?" in a for a in args):\n'
                    '    audited = json.loads(os.environ["AUDITED"])\n'
                    '    sha = next(a for a in args if "/commits/" in a).split("/commits/")[1].split("/")[0]\n'
                    '    print(json.dumps([{"context": "release-security-audit", "state": "failure", "creator": {"login": audited[sha]}}] if sha in audited else []))\n'
                    'elif any("/status?" in a for a in args):\n'
                    '    statuses = [] if os.environ["SUITE"] == "absent" else [{"context": "scheduled-full-ci", "state": "success", "id": 1}]\n'
                    '    if os.environ["SUITE"] == "superseded": statuses.append({"context": "scheduled-full-ci", "state": "failure", "id": 2})\n'
                    '    print(json.dumps([{"sha": "other" if os.environ["SUITE"] == "wrong-sha" else os.environ["HEAD"], "statuses": statuses}]))\n'
                )
                (binaries / "codex").write_text(
                    '#!/usr/bin/env python3\n'
                    'import os, pathlib, sys\n'
                    'a = sys.argv[1:]\n'
                    'assert a[0] == "exec" and a[a.index("--sandbox") + 1] == "read-only"\n'
                    'assert a[a.index("-m") + 1] == "gpt-6.1-terra" and a[a.index("-c") + 1] == "model_reasoning_effort=max" and sys.stdin.read() == ""\n'
                    'assert os.environ["BASE"] in a[-1] and os.environ["HEAD"] in a[-1]\n'
                    'pathlib.Path(a[a.index("-o") + 1]).write_text(os.environ["MESSAGE"])\n'
                    'sys.exit(int(os.environ["CODEX_EXIT"]))\n'
                )
                for binary in binaries.iterdir():
                    binary.chmod(0o755)
                env.update(PATH=str(binaries) + os.pathsep + env["PATH"], CALLS=str(calls), BASE=base, HEAD=head,
                           MESSAGE=json.dumps(message) if isinstance(message, dict) else message, CODEX_EXIT=str(codex_exit), SUITE=suite, AUDITED="{}")
                result = subprocess.run(["bash", str(scripts / "release-audit-local.sh"), "release/v1.10"], cwd=repo, env=env, text=True, capture_output=True)
                self.assertEqual(result.returncode == 0, state == "success", result.stdout + result.stderr)
                requests = [json.loads(line) for line in calls.read_text().splitlines()]
                posts = [args for args in requests if "POST" in args]
                self.assertEqual(len(posts), int(state is not None), result.stdout + result.stderr)
                if state:
                    self.assertIn(f"repos/example-org/example-project/statuses/{head}", posts[0])
                    self.assertIn(f"state={state}", posts[0])
                    self.assertIn("context=release-security-audit", posts[0])
                    retained = list((repo / "tmp").glob("release-audit.*/findings.json"))
                    self.assertEqual(json.loads(retained[0].read_text()), message)
                dispatches = [args for args in requests if args[:2] == ["workflow", "run"]]
                self.assertEqual(dispatches, [["workflow", "run", "release-finalize.yml", "--repo", "example-org/example-project", "--ref", "release/v1.10"]] if state == "success" and suite == "success" else [])
                if state == "success" and suite != "success":
                    self.assertIn("Waiting for local validation", result.stdout)
                self.assertEqual(git("worktree", "list", "--porcelain").count("worktree "), 1)

    def test_audit_scope(self):
        source = Path(__file__).resolve().parent
        for name, audited_by, expect, override, filed in (
            ("first audit covers the series", {}, "base", None, None),
            ("later audit covers the delta from the last allowlisted audit", {"cut": "stranger", "pick": "operator"}, "pick", None, "#1939"),
            ("audits by other creators are ignored", {"pick": "stranger"}, "base", None, None),
            ("an operator can choose an earlier audited base", {"cut": "operator", "pick": "operator"}, "cut", "cut", "#1939,#1959"),
            ("an unaudited override is refused", {"pick": "operator"}, None, "cut", "#1939"),
            ("a failed base needs its findings filed", {"pick": "operator"}, None, None, None),
            ("a passing base needs nothing filed", {"pick": "operator:success"}, "pick", None, None),
        ):
            with self.subTest(name=name), tempfile.TemporaryDirectory() as directory:
                root = Path(directory).resolve()
                repo = root / "repo"
                repo.mkdir()
                env = {"PATH": os.environ["PATH"], "HOME": str(root), "TMPDIR": str(root), "GIT_CONFIG_NOSYSTEM": "1"}

                def git(*args):
                    return subprocess.check_output(["git", *args], cwd=repo, env=env, text=True, stderr=subprocess.DEVNULL).strip()

                git("init")
                git("config", "user.name", "Example")
                git("config", "user.email", "example@example.test")
                commits = {}
                for label in ("base", "cut", "pick", "head"):
                    (repo / "example.go").write_text(f"package example\n\nconst Label = {label!r}\n".replace("'", '"'))
                    git("add", ".")
                    git("commit", "-m", label)
                    commits[label] = git("rev-parse", "HEAD")
                    if label == "base":
                        git("branch", "release/v1.9")
                git("branch", "release/v1.10")
                git("remote", "add", "origin", str(repo))
                scripts = repo / "scripts"
                scripts.mkdir()
                for filename in ("release-audit-local.sh", "release-audit-prompt.md"):
                    shutil.copyfile(source / filename, scripts / filename)
                binaries = root / "bin"
                binaries.mkdir()
                (binaries / "gh").write_text(
                    '#!/usr/bin/env python3\n'
                    'import json, os, sys\n'
                    'args = sys.argv[1:]\n'
                    'if args[:2] == ["repo", "view"]: print("example-org/example-project")\n'
                    'elif args[:2] == ["variable", "get"]: print(json.dumps(["operator"]))\n'
                    'elif any("/statuses?" in a for a in args):\n'
                    '    audited = json.loads(os.environ["AUDITED"])\n'
                    '    sha = next(a for a in args if "/commits/" in a).split("/commits/")[1].split("/")[0]\n'
                    '    login, _, state = audited.get(sha, ":").partition(":")\n'
                    '    print(json.dumps([{"id": 1, "context": "release-security-audit", "state": state or "failure", "creator": {"login": login}}] if sha in audited else []))\n'
                    'elif any("/status?" in a for a in args): print(json.dumps([{"sha": os.environ["HEAD"], "statuses": []}]))\n'
                )
                (binaries / "codex").write_text(
                    '#!/usr/bin/env python3\n'
                    'import json, os, pathlib, sys\n'
                    'a = sys.argv[1:]\n'
                    'pathlib.Path(os.environ["PROMPT"]).write_text(a[-1])\n'
                    'pathlib.Path(a[a.index("-o") + 1]).write_text(json.dumps({"verdict": "pass", "findings": []}))\n'
                )
                for binary in binaries.iterdir():
                    binary.chmod(0o755)
                prompt = root / "prompt"
                env.update(PATH=str(binaries) + os.pathsep + env["PATH"], HEAD=commits["head"], PROMPT=str(prompt),
                           AUDITED=json.dumps({commits[label]: login for label, login in audited_by.items()}))
                env.pop("DETENT_AUDIT_BASE", None)
                env.pop("DETENT_AUDIT_FILED", None)
                if override:
                    env["DETENT_AUDIT_BASE"] = commits[override]
                if filed:
                    env["DETENT_AUDIT_FILED"] = filed
                result = subprocess.run(["bash", str(scripts / "release-audit-local.sh"), "release/v1.10"], cwd=repo, env=env, text=True, capture_output=True)
                if expect is None:
                    self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
                    self.assertFalse(prompt.exists())
                    continue
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertIn(f"git diff {commits[expect]}..{commits['head']}", prompt.read_text().replace("\n", " "))


if __name__ == "__main__":
    unittest.main()
