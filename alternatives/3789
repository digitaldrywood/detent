import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest


ROOT = Path(__file__).resolve().parent.parent


class ReleaseCutTest(unittest.TestCase):
    def test_cut_source_and_dispatch(self):
        scratch = next((os.environ[key] for key in ("TMPDIR", "TMP", "TEMP") if os.environ.get(key)), None)
        self.assertIsNotNone(scratch, "a supplied scratch directory is required")
        active_cases = {
            "manual queued cut": ("release-cut.yml", "queued", "develop", "workflow_dispatch"),
            "scheduled pending cut": ("release-cut.yml", "pending", "develop", "schedule"),
            "manual running finalizer": ("release-finalize.yml", "in_progress", "release/v1.1", "workflow_dispatch"),
            "queued finalizer": ("release-finalize.yml", "queued", "release/v1.1", "workflow_dispatch"),
            "manual requested build": ("release.yml", "requested", "v1.1.0", "workflow_dispatch"),
            "manual running signed build": ("release.yml", "in_progress", "v1.1.0", "workflow_dispatch"),
            "manual waiting deploy": ("release.yml", "waiting", "v1.1.0", "workflow_dispatch"),
            "manual running deploy": ("release.yml", "in_progress", "develop", "workflow_dispatch"),
            "develop finalizer active": ("release-finalize.yml", "in_progress", "develop", "workflow_dispatch"),
            "current cut": ("release-cut.yml", "in_progress", "develop", "schedule"),
            "manual explicit active": ("release.yml", "in_progress", "develop", "workflow_dispatch"),
            "recheck active": ("release.yml", "in_progress", "develop", "workflow_dispatch"),
        }
        completion_cases = {
            "recent production newer develop": "2026-10-09T06:37:00Z",
            "expired production": "2026-10-05T06:36:59Z",
            "UTC lower boundary": "2026-10-05T06:37:00Z",
            "UTC inside boundary": "2026-10-05T06:37:01Z",
            "UTC offset boundary": "2026-10-04T23:37:01-07:00",
            "UTC upper boundary": "2026-10-12T06:37:00Z",
            "future production": "2026-10-12T06:37:01Z",
            "daily cadence": "2026-10-09T06:37:00Z",
            "failed release": "2026-10-12T05:00:00Z",
            "cancelled release": "2026-10-12T05:00:00Z",
            "tag production failed": "2026-10-12T05:00:00Z",
            "production smoke failed": "2026-10-12T05:00:00Z",
            "production MCP smoke failed": "2026-10-12T05:00:00Z",
            "artifacts only": "2026-10-12T05:00:00Z",
            "manual explicit recent": "2026-10-12T05:00:00Z",
            "old attempt recent retry": "2026-10-12T05:00:00Z",
            "jobs API failure": "2026-10-12T05:00:00Z",
            "recheck completion": "2026-10-12T05:00:00Z",
        }
        frozen_cases = {
            "inactive failed release CI": [("v1.1", "green")],
            "current tagged production failed": [("v1.1", "green")],
            "current production smoke failed": [("v1.1", "green")],
            "current deployed head": [("v1.1", "green")],
            "current branch advanced": [("v1.1", "tip")],
            "superseded leftover series": [("v1.0", "green")],
            "newer frozen series": [("v1.0", "green"), ("v1.2", "tip")],
            "multiple frozen series": [("v1.2", "green"), ("v1.3", "tip")],
            "manual explicit frozen series": [("v1.1", "green")],
            "nonstable production cannot complete series": [("v1.1", "green")],
            "older production rerun recent": [("v1.2", "green")],
            "recheck branch advanced": [("v1.1", "green")],
            "recheck frozen branch": [],
        }
        skipped = set(active_cases) - {"develop finalizer active", "current cut", "manual explicit active"}
        skipped.update(("recent production newer develop", "UTC inside boundary", "UTC offset boundary", "UTC upper boundary", "old attempt recent retry", "recheck completion"))
        frozen_skips = set(frozen_cases) - {"current deployed head", "superseded leftover series", "manual explicit frozen series", "older production rerun recent"}
        frozen_skips.update(("existing", "same source unreleased", "advanced series", "newer uncovered green", "covered tip no green"))
        skipped.add("older production rerun recent")
        cases = ("green", "no green", "skipped run", "explicit", "outside develop", "existing", "explicit existing", "next minor", "invalid version", "api failure", "superseded green", "newer release success", "newer release failure", "release status only", "failed develop publication", "missing release scripts", "missing finalize workflow", "same source unreleased", "advanced series", "stable tag only", "prerelease tag only", "newer uncovered green", "covered tip no green", "stable tip", "explicit covered series", "explicit covered tag", "admission API failure", "recheck API failure", "simultaneous triggers", "refs read failure", "recheck refs failure") + tuple(active_cases) + tuple(completion_cases) + tuple(frozen_cases)
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
                    "scripts/release-validate-local.sh": "#!/usr/bin/env bash\ntrue\n",
                    "scripts/release-finalize.sh": "#!/usr/bin/env bash\ntrue\n",
                    ".github/workflows/release-finalize.yml": "on: workflow_dispatch\n",
                    ".github/workflows/release.yml": "on: workflow_dispatch\n",
                    ".github/workflows/release-cut.yml": "on:\n  schedule:\n    - cron: '37 6 * * 1'\n",
                }
                if case == "daily cadence":
                    files[".github/workflows/release-cut.yml"] = "on:\n  schedule:\n    - cron: '37 6 * * *'\n"
                if case == "missing release scripts":
                    del files["scripts/release-cut.sh"]
                if case == "missing finalize workflow":
                    del files[".github/workflows/release-finalize.yml"]
                for name, content in files.items():
                    target = checkout / name
                    target.parent.mkdir(parents=True, exist_ok=True)
                    target.write_text(content)
                git("add", ".")
                git("commit", "-m", "initial release flow")
                green = git("rev-parse", "HEAD")
                uncovered = green
                if case == "newer uncovered green":
                    git("commit", "--allow-empty", "-m", "uncovered green")
                    uncovered = git("rev-parse", "HEAD")
                git("commit", "--allow-empty", "-m", "develop tip")
                tip = git("rev-parse", "HEAD")
                git("push", "origin", "develop")
                git("switch", "-c", "unlanded")
                git("commit", "--allow-empty", "-m", "unlanded")
                outside = git("rev-parse", "HEAD")
                if case in ("existing", "explicit existing"):
                    git("push", "origin", "develop:release/v1.2")
                if case == "next minor":
                    git("tag", "v1.9.2", green)
                    git("push", "origin", "v1.9.2", green + ":refs/heads/release/v1.10")
                if case in ("same source unreleased", "advanced series", "newer uncovered green", "covered tip no green", "explicit covered series"):
                    existing_head = tip if case in ("advanced series", "covered tip no green") else green
                    git("push", "origin", existing_head + ":refs/heads/release/v1.1")
                if case in ("stable tag only", "prerelease tag only", "stable tip", "explicit covered tag", "tag production failed"):
                    tag = "v1.1.0-op.example" if case == "prerelease tag only" else "v1.1.0"
                    git("tag", tag, tip if case == "stable tip" else green)
                    git("push", "origin", tag)
                if case in frozen_cases:
                    for series, head in frozen_cases[case]:
                        git("push", "origin", (tip if head == "tip" else green) + ":refs/heads/release/" + series)
                    if case not in ("inactive failed release CI", "manual explicit frozen series"):
                        tag = "v1.2.0" if case == "older production rerun recent" else "v1.1.0"
                        git("tag", tag, green)
                        git("push", "origin", tag)
                runs = [] if case in ("no green", "covered tip no green") else [{"head_sha": green, "html_url": "https://github.com/example-org/example-project/actions/runs/1"}, {"head_sha": tip, "html_url": "https://github.com/example-org/example-project/actions/runs/2"}]
                if case == "newer uncovered green":
                    runs.append({"head_sha": uncovered, "html_url": "https://github.com/example-org/example-project/actions/runs/5"})
                for run in runs:
                    run.update(head_branch="develop", event="schedule", conclusion="success")
                if case == "superseded green":
                    runs.append({"head_sha": green, "html_url": "https://github.com/example-org/example-project/actions/runs/3", "head_branch": "develop", "event": "workflow_dispatch", "conclusion": "failure"})
                if case in ("newer release success", "newer release failure", "release status only"):
                    runs.append({"head_sha": green, "html_url": "https://github.com/example-org/example-project/actions/runs/4", "head_branch": "release/v1.2", "event": "workflow_dispatch", "conclusion": "success"})
                if case == "failed develop publication":
                    runs[0]["conclusion"] = "failure"
                active = []
                release_runs = []
                jobs = []
                if case in active_cases:
                    workflow, status, head_branch, event = active_cases[case]
                    active = [{"id": 100 if case == "current cut" else 90, "path": ".github/workflows/" + workflow, "status": status, "head_branch": head_branch, "head_sha": green, "event": event, "html_url": "https://github.com/example-org/example-project/actions/runs/90"}]
                if case in completion_cases:
                    release_runs = [{"id": 90, "status": "completed", "conclusion": "failure" if case == "failed release" else "cancelled" if case == "cancelled release" else "success", "head_branch": "v1.1.0", "head_sha": green, "run_attempt": 2, "created_at": "2026-09-01T00:00:00Z", "updated_at": "2026-10-12T06:37:00Z", "html_url": "https://github.com/example-org/example-project/actions/runs/90"}]
                    jobs = [{"name": "production / Deploy and smoke production", "status": "completed", "conclusion": "failure" if case == "tag production failed" else "success", "completed_at": completion_cases[case], "steps": [{"name": name, "conclusion": "failure" if (case == "production smoke failed" and name == "Smoke deployed release") or (case == "production MCP smoke failed" and name == "Exercise production MCP tools") else "success"} for name in ("Deploy signed release", "Smoke deployed release", "Exercise production MCP tools")]}]
                    if case == "artifacts only":
                        jobs[0]["name"] = "GoReleaser"
                if case in frozen_cases and case not in ("inactive failed release CI", "manual explicit frozen series"):
                    release_runs = [{"id": 90, "status": "completed", "conclusion": "failure" if case == "current tagged production failed" else "success", "head_branch": "develop" if case == "nonstable production cannot complete series" else "v1.2.0" if case == "older production rerun recent" else "v1.1.0", "head_sha": green, "run_attempt": 2, "updated_at": "2026-09-01T00:00:00Z", "html_url": "https://github.com/example-org/example-project/actions/runs/90"}]
                    jobs = [{"name": "production / Deploy and smoke production", "status": "completed", "conclusion": "success", "completed_at": "2026-09-01T00:00:00Z", "steps": [{"name": name, "conclusion": "failure" if case == "current production smoke failed" and name == "Smoke deployed release" else "success"} for name in ("Deploy signed release", "Smoke deployed release", "Exercise production MCP tools")]}]
                    if case == "older production rerun recent":
                        release_runs.append({**release_runs[0], "id": 91, "head_branch": "v1.1.0", "updated_at": "2026-10-12T05:00:00Z", "html_url": "https://github.com/example-org/example-project/actions/runs/91"})
                fixture = root / "fixture.json"
                fixture.write_text(json.dumps({"case": case, "runs": runs, "green": green, "tip": tip, "remote": str(remote), "uncovered": uncovered, "active": active, "release_runs": release_runs, "jobs": jobs}))
                date = root / "date"
                date.write_text("#!/bin/sh\nprintf '%s\\n' '2026-10-12T06:37:00Z'\n")
                date.chmod(0o700)
                gh = root / "gh"
                gh.write_text("#!" + sys.executable + "\n" + r'''
import json, os, subprocess, sys
from pathlib import Path
args = sys.argv[1:]
with Path(os.environ["CALL_LOG"]).open("a") as f:
    f.write(json.dumps(args) + "\n")
fixture = json.loads(Path(os.environ["FIXTURE"]).read_text())
query = " ".join(args)
calls = [json.loads(line) for line in Path(os.environ["CALL_LOG"]).read_text().splitlines()]
admission_pass = sum("actions/runs?status=queued" in " ".join(call) for call in calls)
if "actions/runs?status=" in query:
    if fixture["case"] == "admission API failure" or (fixture["case"] == "recheck API failure" and admission_pass > 1): sys.exit("history unavailable")
    active = fixture["active"]
    if admission_pass > 1 and fixture["case"] in ("recheck branch advanced", "recheck frozen branch"):
        branch = "release/v1.1" if fixture["case"] == "recheck branch advanced" else "release/v1.2"
        subprocess.run([os.environ["REAL_GIT"], "--git-dir", fixture["remote"], "update-ref", "refs/heads/" + branch, fixture["tip"]], check=True)
    if fixture["case"] == "recheck active" and admission_pass == 1:
        active = []
    if fixture["case"] == "simultaneous triggers" and any("git/refs" in " ".join(call) for call in calls):
        active = [{"id": 90, "path": ".github/workflows/release-cut.yml", "status": "queued", "head_branch": "release/v1.2", "head_sha": fixture["green"], "html_url": "https://github.com/example-org/example-project/actions/runs/90"}]
    print(json.dumps([{"workflow_runs": [run for run in active if "status=" + run["status"] + "&" in query]}]))
elif "workflows/release.yml/runs?" in query:
    runs = fixture["release_runs"]
    if fixture["case"] == "recheck completion" and admission_pass == 1:
        runs = []
    print(json.dumps([{"workflow_runs": runs}]))
elif "/attempts/2/jobs?" in query:
    if fixture["case"] == "jobs API failure": sys.exit("jobs unavailable")
    jobs = fixture["jobs"]
    if fixture["case"] == "older production rerun recent" and "runs/91/" in query:
        jobs = [{**jobs[0], "completed_at": "2026-10-12T05:00:00Z"}]
    print(json.dumps([{"jobs": jobs}]))
elif "/statuses?" in query:
    if fixture["case"] == "api failure": sys.exit(1)
    statuses = []
    if fixture["green"] in query and fixture["case"] not in ("skipped run", "no green", "covered tip no green"):
        statuses = [{"context": "develop-full-ci", "state": "failure" if fixture["case"] == "failed develop publication" else "success"}]
    if fixture["case"] == "superseded green" and statuses:
        statuses.insert(0, {"context": "develop-full-ci", "state": "failure", "target_url": "https://github.com/example-org/example-project/actions/runs/3"})
    if fixture["case"] in ("newer release success", "newer release failure", "release status only") and statuses:
        release = {"context": "scheduled-full-ci", "state": "failure" if fixture["case"] == "newer release failure" else "success", "target_url": "https://github.com/example-org/example-project/actions/runs/4"}
        statuses = [release] if fixture["case"] == "release status only" else [release] + statuses
    if fixture["case"] == "newer uncovered green" and fixture["uncovered"] in query:
        statuses = [{"context": "develop-full-ci", "state": "success", "target_url": "https://github.com/example-org/example-project/actions/runs/5"}]
    print(json.dumps([statuses]))
elif "git/refs" in query:
    pass
else:
    sys.exit("unexpected gh call: " + query)
''')
                gh.chmod(0o700)
                git_wrapper = root / "git"
                git_wrapper.write_text("#!" + sys.executable + "\n" + r'''
import os, sys
from pathlib import Path
if sys.argv[1:2] == ["ls-remote"]:
    count_path = Path(os.environ["REF_READS"])
    count = int(count_path.read_text()) + 1 if count_path.exists() else 1
    count_path.write_text(str(count))
    if os.environ["REF_CASE"] == "refs read failure" or os.environ["REF_CASE"] == "recheck refs failure" and count > 1:
        sys.exit("release refs unavailable")
os.execv(os.environ["REAL_GIT"], [os.environ["REAL_GIT"], *sys.argv[1:]])
''')
                git_wrapper.chmod(0o700)
                log = root / "calls.jsonl"
                env.update(REAL_GIT=shutil.which("git"), REF_READS=str(root / "ref-reads"), REF_CASE=case, PATH=str(root) + os.pathsep + env["PATH"], CALL_LOG=str(log), FIXTURE=str(fixture), GITHUB_REPOSITORY="example-org/example-project", GITHUB_RUN_ID="100", GITHUB_EVENT_NAME="schedule")
                version = "" if case == "next minor" else "bad" if case == "invalid version" else "v1.2"
                source = tip if case in ("explicit", "explicit existing", "next minor", "manual explicit active", "manual explicit recent", "manual explicit frozen series") else outside if case == "outside develop" else green if case in ("explicit covered series", "explicit covered tag") else ""
                if source:
                    env["GITHUB_EVENT_NAME"] = "workflow_dispatch"
                result = subprocess.run(["bash", str(ROOT / "scripts/release-cut.sh"), version, source], cwd=checkout, env=env, capture_output=True, text=True, timeout=30)
                accepted = case not in ("outside develop", "explicit existing", "invalid version", "api failure", "missing release scripts", "missing finalize workflow", "explicit covered series", "explicit covered tag", "admission API failure", "recheck API failure", "jobs API failure", "refs read failure", "recheck refs failure")
                self.assertEqual(result.returncode == 0, accepted, result.stdout + result.stderr)
                calls = [json.loads(line) for line in log.read_text().splitlines()] if log.exists() else []
                writes = [call for call in calls if "POST" in call]
                dispatches = [call for call in calls if call[:2] == ["workflow", "run"]]
                if not accepted:
                    self.assertEqual(writes + dispatches, [], "rejected cuts must not create a branch or run CI")
                    if case in ("admission API failure", "recheck API failure", "jobs API failure", "refs read failure", "recheck refs failure"):
                        self.assertIn("Release admission read failed", result.stderr)
                        self.assertNotIn("no-op", result.stdout)
                    continue
                if case in frozen_skips:
                    self.assertEqual(writes + dispatches, [], "unfinished current series must not create a branch or run CI")
                    self.assertIn("Release admission: no-op; unfinished frozen series", result.stdout)
                    branch = "v1.3" if case == "multiple frozen series" else "v1.2" if case in ("existing", "newer frozen series", "recheck frozen branch") else "v1.1"
                    self.assertIn("https://github.com/example-org/example-project/tree/release/" + branch, result.stdout)
                    continue
                if case in skipped:
                    self.assertEqual(writes + dispatches, [], "satisfied opportunities must not create a branch or run CI")
                    self.assertIn("Release admission: no-op", result.stdout)
                    self.assertIn("https://github.com/example-org/example-project/actions/runs/" + ("91" if case == "older production rerun recent" else "90"), result.stdout)
                    self.assertIn("(2026-10-05T06:37:00Z, 2026-10-12T06:37:00Z]", result.stdout)
                    continue
                if case == "stable tip":
                    self.assertEqual(writes + dispatches, [], "covered develop must not create duplicate releases")
                    self.assertIn("No unreleased develop commits", result.stdout)
                    continue
                expected = tip if case in ("no green", "skipped run", "explicit", "next minor", "superseded green", "release status only", "failed develop publication", "stable tag only", "tag production failed", "manual explicit active", "manual explicit recent", "manual explicit frozen series", "current deployed head", "superseded leftover series") else green
                branch = "release/v1.11" if case == "next minor" else "release/v1.2"
                self.assertEqual(len(writes), 1)
                self.assertIn("sha=" + expected, writes[0])
                self.assertIn("ref=refs/heads/" + branch, writes[0])
                self.assertEqual(dispatches, [], "cutting a release never dispatches GitHub Actions validation")
                self.assertIn("scripts/release-validate-local.sh " + branch, result.stdout)
                if case in ("manual explicit active", "manual explicit recent"):
                    self.assertFalse(any("actions/runs?status=" in " ".join(call) for call in calls))
                if case == "simultaneous triggers":
                    env.update(GITHUB_RUN_ID="101", GITHUB_EVENT_NAME="workflow_dispatch")
                    second = subprocess.run(["bash", str(ROOT / "scripts/release-cut.sh"), version], cwd=checkout, env=env, capture_output=True, text=True, timeout=30)
                    self.assertEqual(second.returncode, 0, second.stdout + second.stderr)
                    self.assertIn("Release admission: no-op", second.stdout)
                    later_calls = [json.loads(line) for line in log.read_text().splitlines()][len(calls):]
                    self.assertFalse(any("POST" in call or call[:2] == ["workflow", "run"] for call in later_calls))


if __name__ == "__main__":
    unittest.main()
