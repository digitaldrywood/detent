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
        active_cases = {
            "manual queued cut": ("release-cut.yml", "queued", "develop", "workflow_dispatch"),
            "scheduled pending cut": ("release-cut.yml", "pending", "develop", "schedule"),
            "manual running CI": ("ci.yml", "in_progress", "release/v1.1", "workflow_dispatch"),
            "scheduled queued CI": ("ci.yml", "queued", "release/v1.1", "push"),
            "manual requested build": ("release.yml", "requested", "v1.1.0", "workflow_dispatch"),
            "manual running signed build": ("release.yml", "in_progress", "v1.1.0", "workflow_dispatch"),
            "manual waiting deploy": ("release.yml", "waiting", "v1.1.0", "workflow_dispatch"),
            "manual running deploy": ("release.yml", "in_progress", "develop", "workflow_dispatch"),
            "develop CI active": ("ci.yml", "in_progress", "develop", "schedule"),
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
        skipped = set(active_cases) - {"develop CI active", "current cut", "manual explicit active"}
        skipped.update(("recent production newer develop", "UTC inside boundary", "UTC offset boundary", "UTC upper boundary", "old attempt recent retry", "recheck completion"))
        cases = ("green", "no green", "skipped run", "explicit", "outside develop", "existing", "next minor", "invalid version", "api failure", "superseded green", "newer release success", "newer release failure", "release status only", "failed develop publication", "legacy develop evidence", "missing release scripts", "legacy CI", "legacy finalizer", "same source unreleased", "advanced series", "stable tag only", "prerelease tag only", "newer uncovered green", "covered tip no green", "stable tip", "explicit covered series", "explicit covered tag", "admission API failure", "recheck API failure", "simultaneous triggers", "dispatch failure") + tuple(active_cases) + tuple(completion_cases)
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
                    ".github/workflows/release-cut.yml": "on:\n  schedule:\n    - cron: '37 6 * * 1'\n",
                }
                if case == "daily cadence":
                    files[".github/workflows/release-cut.yml"] = "on:\n  schedule:\n    - cron: '37 6 * * *'\n"
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
                if case == "existing":
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
                fixture = root / "fixture.json"
                fixture.write_text(json.dumps({"case": case, "runs": runs, "green": green, "uncovered": uncovered, "active": active, "release_runs": release_runs, "jobs": jobs}))
                date = root / "date"
                date.write_text("#!/bin/sh\nprintf '%s\\n' '2026-10-12T06:37:00Z'\n")
                date.chmod(0o700)
                gh = root / "gh"
                gh.write_text("#!" + sys.executable + "\n" + r'''
import json, os, sys
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
    if fixture["case"] == "recheck active" and admission_pass == 1:
        active = []
    if fixture["case"] == "simultaneous triggers" and any(call[:2] == ["workflow", "run"] for call in calls):
        active = [{"id": 90, "path": ".github/workflows/ci.yml", "status": "queued", "head_branch": "release/v1.2", "head_sha": fixture["green"], "html_url": "https://github.com/example-org/example-project/actions/runs/90"}]
    print(json.dumps([{"workflow_runs": [run for run in active if "status=" + run["status"] + "&" in query]}]))
elif "workflows/release.yml/runs?" in query:
    runs = fixture["release_runs"]
    if fixture["case"] == "recheck completion" and admission_pass == 1:
        runs = []
    print(json.dumps([{"workflow_runs": runs}]))
elif "/attempts/2/jobs?" in query:
    if fixture["case"] == "jobs API failure": sys.exit("jobs unavailable")
    print(json.dumps([{"jobs": fixture["jobs"]}]))
elif "workflows/ci.yml/runs?" in query:
    if fixture["case"] == "api failure": sys.exit(1)
    if "branch=develop" not in query or "status=completed" not in query:
        sys.exit("release selection must query completed develop runs")
    print(json.dumps([{"workflow_runs": fixture["runs"]}]))
elif "/statuses?" in query:
    statuses = []
    if fixture["green"] in query and fixture["case"] != "skipped run":
        statuses = [{"context": "scheduled-full-ci" if fixture["case"] == "legacy develop evidence" else "develop-full-ci", "state": "success", "target_url": fixture["runs"][0]["html_url"]}]
    if fixture["case"] == "superseded green" and statuses:
        statuses.insert(0, {"context": "develop-full-ci", "state": "failure", "target_url": "https://github.com/example-org/example-project/actions/runs/3"})
    if fixture["case"] in ("newer release success", "newer release failure", "release status only") and statuses:
        release = {"context": "scheduled-full-ci", "state": "failure" if fixture["case"] == "newer release failure" else "success", "target_url": "https://github.com/example-org/example-project/actions/runs/4"}
        statuses = [release] if fixture["case"] == "release status only" else [release] + statuses
    if fixture["case"] == "newer uncovered green" and fixture["uncovered"] in query:
        statuses = [{"context": "develop-full-ci", "state": "success", "target_url": "https://github.com/example-org/example-project/actions/runs/5"}]
    print(json.dumps([statuses]))
elif "git/refs" in query or args[:2] == ["workflow", "run"]:
    if fixture["case"] == "dispatch failure" and args[:2] == ["workflow", "run"]: sys.exit("dispatch unavailable")
else:
    sys.exit("unexpected gh call: " + query)
''')
                gh.chmod(0o700)
                log = root / "calls.jsonl"
                env.update(PATH=str(root) + os.pathsep + env["PATH"], CALL_LOG=str(log), FIXTURE=str(fixture), GITHUB_REPOSITORY="example-org/example-project", GITHUB_RUN_ID="100", GITHUB_EVENT_NAME="schedule")
                version = "" if case == "next minor" else "bad" if case == "invalid version" else "v1.2"
                source = tip if case in ("explicit", "next minor", "manual explicit active", "manual explicit recent") else outside if case == "outside develop" else green if case in ("explicit covered series", "explicit covered tag") else ""
                if source:
                    env["GITHUB_EVENT_NAME"] = "workflow_dispatch"
                result = subprocess.run(["bash", str(ROOT / "scripts/release-cut.sh"), version, source], cwd=checkout, env=env, capture_output=True, text=True, timeout=30)
                accepted = case not in ("outside develop", "existing", "invalid version", "api failure", "missing release scripts", "legacy CI", "legacy finalizer", "explicit covered series", "explicit covered tag", "admission API failure", "recheck API failure", "jobs API failure", "dispatch failure")
                self.assertEqual(result.returncode == 0, accepted, result.stdout + result.stderr)
                calls = [json.loads(line) for line in log.read_text().splitlines()] if log.exists() else []
                writes = [call for call in calls if "POST" in call]
                dispatches = [call for call in calls if call[:2] == ["workflow", "run"]]
                if not accepted:
                    if case == "dispatch failure":
                        self.assertEqual(len(writes), 1)
                        self.assertEqual(len(dispatches), 1)
                        self.assertNotIn("and dispatched", result.stdout)
                    else:
                        self.assertEqual(writes + dispatches, [], "rejected cuts must not create a branch or run CI")
                    if case in ("admission API failure", "recheck API failure", "jobs API failure"):
                        self.assertIn("Release admission read failed", result.stderr)
                        self.assertNotIn("no-op", result.stdout)
                    continue
                if case in skipped:
                    self.assertEqual(writes + dispatches, [], "satisfied opportunities must not create a branch or run CI")
                    self.assertIn("Release admission: no-op", result.stdout)
                    self.assertIn("https://github.com/example-org/example-project/actions/runs/90", result.stdout)
                    self.assertIn("(2026-10-05T06:37:00Z, 2026-10-12T06:37:00Z]", result.stdout)
                    continue
                if case in ("advanced series", "covered tip no green", "stable tip"):
                    self.assertEqual(writes + dispatches, [], "covered develop must not create duplicate releases")
                    self.assertIn("No unreleased develop commits", result.stdout)
                    continue
                expected = uncovered if case == "newer uncovered green" else tip if case in ("no green", "skipped run", "explicit", "next minor", "superseded green", "release status only", "failed develop publication", "same source unreleased", "stable tag only", "tag production failed", "manual explicit active", "manual explicit recent") else green
                branch = "release/v1.11" if case == "next minor" else "release/v1.2"
                self.assertEqual(len(writes), 1)
                self.assertIn("sha=" + expected, writes[0])
                self.assertIn("ref=refs/heads/" + branch, writes[0])
                self.assertEqual(dispatches, [["workflow", "run", "ci.yml", "--repo", "example-org/example-project", "--ref", branch, "-f", "force=true"]])
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
