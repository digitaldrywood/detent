import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


ROOT = Path(__file__).resolve().parent.parent


class ReleaseCutTest(unittest.TestCase):
    def test_cut_source_and_dispatch(self):
        scratch = next((os.environ[key] for key in ("TMPDIR", "TMP", "TEMP") if os.environ.get(key)), None)
        self.assertIsNotNone(scratch, "a supplied scratch directory is required")
        cases = ("green", "no green", "skipped run", "explicit", "outside develop", "existing", "next minor", "invalid version", "api failure", "superseded green", "newer release success", "newer release failure", "release status only", "failed develop publication", "legacy develop evidence", "missing release scripts", "legacy CI", "legacy finalizer", "same source unreleased", "advanced series", "stable tag only", "prerelease tag only")
        for case in cases:
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
                files = {
                    "scripts/release-cut.sh": "#!/usr/bin/env bash\ntrue\n",
                    "scripts/scheduled-ci-finish.sh": 'release="${CI_RELEASE_BRANCH:-}"\n',
                    ".github/workflows/ci.yml": "env:\n  CI_RELEASE_BRANCH: ${{ needs.preflight.outputs.release_branch }}\n",
                    ".github/workflows/release.yml": "on: workflow_dispatch\n",
                }
                if case == "missing release scripts":
                    del files["scripts/release-cut.sh"]
                if case == "legacy CI":
                    files[".github/workflows/ci.yml"] = "on: workflow_dispatch\n"
                if case == "legacy finalizer":
                    files["scripts/scheduled-ci-finish.sh"] = "release=develop\n"
                for name, content in files.items():
                    target = checkout / name
                    target.parent.mkdir(parents=True, exist_ok=True)
                    target.write_text(content)
                git("add", ".")
                git("commit", "-m", "initial release flow")
                green = git("rev-parse", "HEAD")
                git("commit", "--allow-empty", "-m", "develop tip")
                tip = git("rev-parse", "HEAD")
                git("push", "origin", "develop")
                git("switch", "-c", "unlanded")
                git("commit", "--allow-empty", "-m", "unlanded")
                outside = git("rev-parse", "HEAD")
                if case == "existing":
                    git("push", "origin", "develop:release/v1.2")
                if case == "next minor":
                    git("tag", "v1.9.2", green)
                    git("push", "origin", "v1.9.2", green + ":refs/heads/release/v1.10")
                if case in ("same source unreleased", "advanced series"):
                    existing_head = tip if case == "advanced series" else green
                    git("push", "origin", existing_head + ":refs/heads/release/v1.1")
                if case in ("stable tag only", "prerelease tag only"):
                    tag = "v1.1.0" if case == "stable tag only" else "v1.1.0-op.example"
                    git("tag", tag, green)
                    git("push", "origin", tag)
                runs = [] if case == "no green" else [{"head_sha": green, "html_url": "https://github.com/example/repo/actions/runs/1"}, {"head_sha": tip, "html_url": "https://github.com/example/repo/actions/runs/2"}]
                for run in runs:
                    run.update(head_branch="develop", event="schedule", conclusion="success")
                if case == "superseded green":
                    runs.append({"head_sha": green, "html_url": "https://github.com/example/repo/actions/runs/3", "head_branch": "develop", "event": "workflow_dispatch", "conclusion": "failure"})
                if case in ("newer release success", "newer release failure", "release status only"):
                    runs.append({"head_sha": green, "html_url": "https://github.com/example/repo/actions/runs/4", "head_branch": "release/v1.2", "event": "workflow_dispatch", "conclusion": "success"})
                if case == "failed develop publication":
                    runs[0]["conclusion"] = "failure"
                fixture = root / "fixture.json"
                fixture.write_text(json.dumps({"case": case, "runs": runs, "green": green}))
                gh = root / "gh"
                gh.write_text("#!" + sys.executable + "\n" + r'''
import json, os, sys
from pathlib import Path
args = sys.argv[1:]
with Path(os.environ["CALL_LOG"]).open("a") as f:
    f.write(json.dumps(args) + "\n")
fixture = json.loads(Path(os.environ["FIXTURE"]).read_text())
query = " ".join(args)
if "workflows/ci.yml/runs?" in query:
    if fixture["case"] == "api failure": sys.exit(1)
    if "branch=develop" not in query or "status=completed" not in query:
        sys.exit("release selection must query completed develop runs")
    print(json.dumps([{"workflow_runs": fixture["runs"]}]))
elif "/statuses?" in query:
    statuses = []
    if fixture["green"] in query and fixture["case"] != "skipped run":
        statuses = [{"context": "scheduled-full-ci" if fixture["case"] == "legacy develop evidence" else "develop-full-ci", "state": "success", "target_url": fixture["runs"][0]["html_url"]}]
    if fixture["case"] == "superseded green" and statuses:
        statuses.insert(0, {"context": "develop-full-ci", "state": "failure", "target_url": "https://github.com/example/repo/actions/runs/3"})
    if fixture["case"] in ("newer release success", "newer release failure", "release status only") and statuses:
        release = {"context": "scheduled-full-ci", "state": "failure" if fixture["case"] == "newer release failure" else "success", "target_url": "https://github.com/example/repo/actions/runs/4"}
        statuses = [release] if fixture["case"] == "release status only" else [release] + statuses
    print(json.dumps([statuses]))
elif "git/refs" in query or args[:2] == ["workflow", "run"]:
    pass
else:
    sys.exit("unexpected gh call: " + query)
''')
                gh.chmod(0o700)
                log = root / "calls.jsonl"
                env.update(PATH=str(root) + os.pathsep + env["PATH"], CALL_LOG=str(log), FIXTURE=str(fixture), GITHUB_REPOSITORY="example/repo")
                version = "" if case == "next minor" else "bad" if case == "invalid version" else "v1.2"
                source = tip if case in ("explicit", "next minor") else outside if case == "outside develop" else ""
                result = subprocess.run(["bash", str(ROOT / "scripts/release-cut.sh"), version, source], cwd=checkout, env=env, capture_output=True, text=True, timeout=30)
                accepted = case not in ("outside develop", "existing", "invalid version", "api failure", "missing release scripts", "legacy CI", "legacy finalizer", "same source unreleased", "advanced series", "stable tag only")
                self.assertEqual(result.returncode == 0, accepted, result.stdout + result.stderr)
                calls = [json.loads(line) for line in log.read_text().splitlines()] if log.exists() else []
                writes = [call for call in calls if "POST" in call]
                dispatches = [call for call in calls if call[:2] == ["workflow", "run"]]
                if not accepted:
                    self.assertEqual(writes + dispatches, [], "rejected cuts must not create a branch or run CI")
                    continue
                expected = tip if case in ("no green", "skipped run", "explicit", "next minor", "superseded green", "release status only", "failed develop publication") else green
                branch = "release/v1.11" if case == "next minor" else "release/v1.2"
                self.assertEqual(len(writes), 1)
                self.assertIn("sha=" + expected, writes[0])
                self.assertIn("ref=refs/heads/" + branch, writes[0])
                self.assertEqual(dispatches, [["workflow", "run", "ci.yml", "--repo", "example/repo", "--ref", branch, "-f", "force=true"]])


if __name__ == "__main__":
    unittest.main()
