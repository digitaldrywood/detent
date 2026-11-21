from contextlib import ExitStack
import json
import os
from pathlib import Path
import runpy
import shutil
import signal
import socket
import subprocess
import sys
import time
from urllib.parse import urlencode

import stack
from evidence import compare
from fault import Fault

FIXTURE = Path(os.environ["DETENT_SMOKE_FIXTURE"])
REPO = "digitaldrywood/detent-canary-todo"
DEVELOP = "refs/heads/develop"
BASELINE = "refs/tags/canary-baseline-v2"
TIMEOUT = int(os.environ["DETENT_SMOKE_TIMEOUT"])
LANES = ("Triage", "Backlog", "Todo", "In Progress", "Rework", "Human Review", "Blocked", "Done", "Cancelled")


def git(root, *args):
    return stack.command(["git", *args], cwd=root)


def fixture_path(relative):
    path = (FIXTURE / relative).resolve()
    if not path.is_relative_to(FIXTURE):
        raise stack.Failure("Scenario file is outside the pinned fixture")
    return path


class Project:
    def __init__(self, org, token, index, name, adjustments, patch):
        self.repo = REPO if index == 1 else f"{REPO}-peer-{index}"
        self.mirror = stack.MIRROR / f"{self.repo}.git"
        self.admin = token
        org_path = f"/api/v2/organizations/{org}"
        self.mirror.parent.mkdir(parents=True, exist_ok=True)
        git(stack.ROOT, "init", "--bare", "--initial-branch=develop", self.mirror)
        self.git("fetch", "--no-tags", FIXTURE, f"{BASELINE}:{BASELINE}")
        self.git("config", "http.receivepack", "true")
        allowed = stack.STATE / "repositories.json"
        stack.write(allowed, [*stack.read(allowed, []), self.repo])
        self.source = self.prepare_source(index, adjustments, patch)
        self.git("update-ref", DEVELOP, self.source)
        states = [
            {"name": name, "terminal": name in {"Done", "Cancelled"},
             "dispatchable": name in {"Todo", "In Progress", "Rework"},
             "transitions": [candidate for candidate in LANES if candidate != name]}
            for name in LANES
        ]
        record = stack.request(org_path + "/projects", "POST", {
            "name": name, "idempotency_key": f"smoke-project-{index}", "states": states,
        }, token)
        self.id = record["project_id"]
        self.path = org_path + "/projects/" + self.id
        self.workspace = stack.WORKSPACES / self.id
        integration = self.request("/integration")
        self.request("/integration/repository", "POST", {
            "idempotency_key": f"smoke-repository-{index}", "expected_revision": integration["revision"],
            "repository": self.repo, "source": "runner_checkout",
        })

    def git(self, *args):
        return git(self.mirror, *args)

    def request(self, suffix="", method="GET", body=None, query=None):
        return stack.request(self.path + suffix + ("?" + urlencode(query) if query else ""),
                             method, body, self.admin)

    def item(self, issue, state="Backlog"):
        return self.request("/work-items", "POST", {
            "idempotency_key": issue["title"], "title": issue["title"],
            "body": issue["description"], "labels": issue["labels"], "state": state, "type": "change_request",
        })

    def prepare_source(self, index, adjustments, patch):
        if not adjustments and not patch and self.repo == REPO:
            return self.git("rev-parse", BASELINE)
        checkout = stack.ROOT / f"seed{index}"
        self.git("worktree", "add", "--detach", checkout, BASELINE)
        if patch:
            git(checkout, "apply", "--index", fixture_path(patch))
        fields = dict(adjustments)
        if self.repo != REPO:
            fields["tracker.repository"] = self.repo
        if fields:
            config = checkout / "detent.yaml"
            program = """data = YAML.safe_load(File.read(ARGV[0]))
JSON.parse(STDIN.read).each do |name, value|
  keys = name.split(".")
  parent = keys[0...-1].reduce(data) { |node, key| node[key] ||= {} }
  parent[keys.last] = value
end
puts YAML.dump(data).delete_prefix("---\n")
"""
            output = stack.command(["ruby", "-ryaml", "-rjson", "-e", program, config],
                                   cwd=checkout, data=json.dumps(fields))
            config.write_text(output + "\n")
            git(checkout, "add", "detent.yaml")
        git(checkout, "commit", "-m", "test(canary): seed replay source")
        return git(checkout, "rev-parse", "HEAD")

    def approve_policy(self):
        def observed():
            state = self.request("/onboarding")
            for entry in state.get("observed_policies", []):
                policy = entry["policy"]
                source = entry.get("repository_source") or {}
                if (source.get("repository") != self.repo or source.get("commit") != self.source
                        or source.get("default_branch_head") != self.source or not source.get("default_branch_reachable")):
                    continue
                self.request("/policy", "PUT", {
                    "expected_policy_id": "", "observed_policy_id": policy["policy_id"],
                })
                return policy
            return None

        return stack.wait_for(f"policy observation for {self.repo}", observed, 60)


def configure_models(org, token):
    path = f"/api/v2/organizations/{org}/model-selection"
    models = stack.request(path, token=token)
    selection = {
        "normal_model": "gpt-6.2-sol",
        "stages": {stage: {"effort": "high"} for stage in ("code", "rework", "merge")},
    }
    stack.request(path, "PUT", {"expected_revision": models["revision"], "idempotency_key": "smoke-models",
                                "selection": selection}, token)


def page(project, item_path, suffix):
    cursor = None
    result = []
    while True:
        query = {"limit": 50}
        if cursor:
            query["cursor"] = cursor
        data = project.request(item_path + suffix, query=query)
        if data.get("data_omission"):
            raise stack.Failure(f"Hub omitted {suffix} records")
        result.extend(data["items"])
        cursor = data.get("next_cursor")
        if not cursor:
            return result


def capture(project, case_name, item_path, repository_before, include_checklists=False):
    item = project.request(item_path)
    attempts = page(project, item_path, "/attempts")
    details = [project.request(item_path + "/attempts/" + record["attempt_id"]) for record in attempts]
    phases = project.request(item_path + "/phases")
    explain = project.request(item_path + "/explanation")
    runtime = explain.get("native_runtime") or {}
    history = page(project, item_path, "/history")
    changes = {}
    for event in history:
        change = (event.get("data") or {}).get("change") or {}
        if event["type"] == "change.version_published" and change.get("change_id"):
            changes[change["change_id"]] = None
    version = runtime.get("current_version") or {}
    if version.get("change_id"):
        changes[version["change_id"]] = None
    for key in changes:
        changes[key] = project.request(item_path + "/changes/" + key)
    rounds = []
    seen = set()
    key = "latest"
    while key:
        record = project.request(item_path + "/progress", query={"round": key})
        if record is None and key == "latest":
            break
        if not record or (key != "latest" and key != record["round_id"]) or record["round_id"] in seen:
            raise stack.Failure("Missing or repeated linked round evidence")
        seen.add(record["round_id"])
        rounds.append(record)
        key = record.get("previous_round_id")
    evidence = {"item": item, "attempts": attempts, "details": details, "explanation": explain,
                "history": history, "rounds": list(reversed(rounds)), "phases": list(reversed(phases)),
                "changes": list(changes.values()), "develop_before": repository_before,
                "develop_after": project.git("rev-parse", DEVELOP)}
    if include_checklists:
        evidence["checklists"] = [project.request(item_path + "/progress", query={"checklist_attempt": attempt["attempt_id"]})
                                  for attempt in attempts]
    stack.write(stack.ROOT / (case_name + ".evidence.json"), evidence)
    return evidence


def advance_when_staged(project, operator, original, advanced):
    marker_path = Path(operator["marker"])
    if marker_path.is_absolute() or ".." in marker_path.parts:
        raise stack.Failure("Expected an item-relative operator marker")
    matches = list((project.workspace / ".detent/workspaces").glob(f"*/{marker_path}"))
    markers = [path for path in matches if path.read_text().strip() == original]
    if not markers:
        return False
    if len(markers) != 1:
        raise stack.Failure("Expected one workspace staging the conflict")
    checkout = markers[0].parents[len(marker_path.parts) - 1]
    if git(checkout, "rev-parse", "HEAD") != original:
        raise stack.Failure("Conflict workspace HEAD changed before the advance")
    if git(checkout, "diff", "--cached", "--name-only").splitlines() != operator["staged_paths"]:
        raise stack.Failure("Conflict workspace did not stage the expected paths")
    project.git("merge-base", "--is-ancestor", original, advanced)
    project.git("update-ref", DEVELOP, advanced, original)
    print(f"Operator advanced isolated develop from {original} to {advanced}", flush=True)
    return True


def inspect_preview(project):
    for pid_file in (project.workspace / ".detent/workspaces").glob("*/.canary/preview.pid"):
        value = pid_file.read_text().strip()
        if not value.isdecimal():
            continue
        pid = int(value)
        identity = stack.process_info(pid)
        if not identity or not identity[-1].startswith(".canary/preview"):
            continue
        checkout = pid_file.parent.parent
        probe = subprocess.run([sys.executable, "-c", "import socket; s = socket.socket(socket.AF_UNIX);"
                                "s.settimeout(1); s.connect('.canary/preview.sock'); s.close()"],
                               cwd=checkout, capture_output=True)
        if probe.returncode == 0:
            return {"pid": pid, "identity": identity, "checkout": str(checkout)}
    return None


def run_case(project, case, policy, routed, original=None, advanced=None, item=None):
    source = project.git("rev-parse", DEVELOP)
    if item is None:
        item = project.item(stack.read(case / "issue.json"))
    key = "/work-items/" + item["work_item_id"]
    definition = stack.read(case / "scenario.json")
    project.request(key + "/workflow", "POST", {
        "idempotency_key": f"smoke-start-{case.name}", "expected_revision": item["revision"],
        "state": "Todo", "reason": "user_requested", "reason_detail": "Local smoke scenario",
    })
    org, admin = stack.current()
    expected = stack.read(case / "expected.json")
    loop_outcome = expected.get("loop_outcome", False)
    include_checklists = loop_outcome or "tool_loop" in expected
    fault = Fault(FIXTURE, project, key, definition, expected["fault"]) if "fault" in expected else None
    service = None
    try:
        if fault:
            fault.arm()
        stack.route(routed, admin, org)
        stack.stop("runner")
        stack.start_runner()
        deadline = time.monotonic() + TIMEOUT
        operator = definition.get("operator")
        moved = False
        while time.monotonic() < deadline:
            if definition.get("retained_service") and not service:
                service = inspect_preview(project)
            if operator and not moved:
                moved = advance_when_staged(project, operator, original, advanced)
            current = project.request(key)
            is_stopping = current["state"] in {"Done", "Blocked", "Cancelled", "Human Review"}
            attempts = page(project, key, "/attempts") if fault or is_stopping or loop_outcome else []
            if fault:
                fault.poll(current, attempts)
            if (is_stopping or loop_outcome) and attempts and all(attempt.get("claim_released_at") for attempt in attempts):
                break
            time.sleep(1)
        else:
            evidence = capture(project, case.name, key, source, include_checklists)
            if fault:
                fault.verify(evidence)
            raise stack.Failure(f"replay timed out in {evidence['item']['state']}")
    finally:
        if fault:
            fault.close()
        runner = stack.runner_status(admin, org)
        if not (runner.get("used") or runner.get("leases")):
            stack.route([], admin, org)
    evidence = capture(project, case.name, key, source, include_checklists)
    evidence["policy"] = policy
    if operator and not moved:
        raise stack.Failure("required isolated target advance did not occur")
    if service:
        evidence["service"] = service
    if fault:
        fault.verify(evidence)
    stack.write(stack.ROOT / (case.name + ".evidence.json"), evidence)
    compare(project, policy, evidence, expected, definition, advanced)
    return item, evidence


def add_dependency(project, item, dependency):
    project.request("/work-items/" + item["work_item_id"] + "/dependencies", "POST", {
        "idempotency_key": "smoke-prerequisite", "expected_revision": item["revision"],
        "related_work_item_id": dependency["work_item_id"], "operation": "add",
    })
    return project.request("/work-items/" + item["work_item_id"])


def held_items(project, specification):
    rows = []
    for lane, count in specification.items():
        for index in range(count):
            item = project.item({
                "title": f"Held item {lane} {index + 1}",
                "description": "This item remains in its holding lane during the local replay.",
                "labels": ["canary:held"],
            }, state=lane)
            rows.append((project, item, lane))
    return rows


def run_next(project, case, scenario, mode, capacity):
    worker = runpy.run_path(str(fixture_path("scripts/replay-next.py")))

    class Replay(worker["Replay"]):
        def hook_count(self, path):
            with (stack.LOGS / "runner.log").open() as log:
                fields = (stack.log_fields(line) for line in log if "running workspace hook" in line)
                return sum(row.get("hook") == "after_create" and row.get("path") == path for row in fields)

    names = [scenario["control"]] if mode == "control" else scenario["items"]
    items = {name: project.item(stack.read(fixture_path(f"issues/{name}.json"))) for name in names}
    if "dependency" in scenario:
        child, parent = scenario["dependency"]
        items[child] = add_dependency(project, items[child], items[parent])
    for name, item in items.items():
        project.request("/work-items/" + item["work_item_id"] + "/workflow", "POST", {
            "idempotency_key": f"smoke-start-{name}", "expected_revision": item["revision"],
            "state": "Todo", "reason": "user_requested",
        })
    expected = stack.read(case / "expected.json")
    replay = Replay(stack, scenario, expected, mode, project=project.id, mirror=project.mirror,
                    items={name: item["work_item_id"] for name, item in items.items()})
    stem = f"{case.name}.{mode}"
    records = {}
    difference = ""
    with ExitStack() as sensors:
        action = scenario["action"]
        if action in {"restart_runner", "refuse_gate_service"}:
            shutil.copyfile(fixture_path("scripts/replay-gate.py"), stack.STATE / "replay-gate.py")
            armed = {"kind": action}
            if action == "refuse_gate_service":
                refusal = sensors.enter_context(socket.socket())
                refusal.bind(("127.0.0.1", 0))
                armed["port"] = refusal.getsockname()[1]
            stack.write(stack.STATE / "gate.armed.json", armed)
        start, since = time.monotonic(), time.time()
        stack.route([project.id], replay.admin, replay.org, capacity=capacity)
        try:
            records = replay.observe(start + TIMEOUT, since, stack.ROOT / (stem + ".fault.json"))
            for name, record in records.items():
                replay.verify_landing(name, record)
            replay.verify_fault(records)
        except (KeyError, IndexError, OSError, StopIteration, ValueError, stack.Failure) as error:
            difference = str(error) or "Missing required session or receipt"
            raise stack.Failure(difference) from error
        finally:
            seconds = time.monotonic() - start
            stack.write(stack.ROOT / (stem + ".evidence.json"), {
                "difference": difference, "seconds": round(seconds, 2), "fault": replay.fault,
                "samples": replay.samples, "records": records,
            })
    stack.route([], replay.admin, replay.org, capacity=capacity)
    print(f"{stem} {seconds:.1f}s", flush=True)
    return seconds


def execute(profile, case):
    started = time.monotonic()
    scenario = stack.read(case / "scenario.json")
    action = scenario.get("action") if "items" in scenario else None
    capacity = scenario.get("capacity", 1)
    clock = {}
    if action == "pause_runner":
        clock = {"heartbeat_interval_seconds": scenario["heartbeat_seconds"],
                 "lease_ttl_seconds": scenario["lease_ttl_seconds"]}
    if action == "restart_runner":
        clock = {"heartbeat_interval_seconds": 5, "lease_ttl_seconds": 75}
    stack.up(capacity, clock, scripted_codex=scenario.get("scripted_codex", False))
    org, admin = stack.current()
    configure_models(org, admin)
    profile_policy = stack.read(FIXTURE / f"smoke/profiles/{profile}.json")["policy"]
    policy_fields = profile_policy | scenario.get("policy", {})
    if action == "pause_runner":
        policy_fields |= {"plan.enabled": True, "plan.review": "automated"}
    projects = scenario.get("projects", [{"name": "Todo smoke", "case": case.name}])
    seeded = [
        Project(org, admin, index, spec["name"], policy_fields, scenario.get("source_patch"))
        for index, spec in enumerate(projects, 1)
    ]
    ids = [project.id for project in seeded]
    stack.route(ids, admin, org, capacity=capacity)
    stack.start_runner()
    policy = [project.approve_policy() for project in seeded]
    stack.route([], admin, org, capacity=capacity)
    print(f"Replay seed ready in {time.monotonic() - started:.1f}s", flush=True)
    if action:
        mode = "phase-receipt" if action == "pause_runner" else "default"
        seconds = run_next(seeded[0], case, scenario, mode, capacity)
        if scenario.get("control"):
            control = Project(org, admin, 2, "Single-item control", policy_fields, scenario.get("source_patch"))
            stack.route([control.id], admin, org)
            control.approve_policy()
            stack.route([], admin, org)
            single = run_next(control, case, scenario, "control", 1)
            print(f"Parallel {seconds:.1f}s, single-item control {single:.1f}s", flush=True)
        return
    if "projects" in scenario:
        expected = stack.read(case / "expected.json")
        held = [item for project, spec in zip(seeded, projects) for item in held_items(project, spec.get("held", {}))]
        if len(held) != expected["unchanged_held_items"]:
            raise stack.Failure("holding inventory differs from the scenario")
        before = {project.id: project.git("rev-parse", DEVELOP) for project in seeded}
        active = next(index for index, spec in enumerate(projects) if spec.get("case") == expected["active"])
        run_case(seeded[active], case.parent / projects[active]["case"], policy[active], ids)
        snapshot = []
        for project, item, lane in held:
            path = "/work-items/" + item["work_item_id"]
            settled = project.request(path)
            runs = page(project, path, "/attempts")
            if settled["state"] != lane or runs:
                raise stack.Failure(f"holding item {item['work_item_id']} changed lane or was claimed")
            snapshot.append({"work_item_id": item["work_item_id"], "state": lane, "attempts": len(runs)})
        for index, project in enumerate(seeded):
            if index != active and project.git("rev-parse", DEVELOP) != before[project.id]:
                raise stack.Failure("the inactive project's local develop changed")
        stack.write(stack.ROOT / (case.name + ".evidence.json"), {"held": snapshot, "projects": ids})
        return
    project = seeded[0]
    if not scenario.get("prerequisite"):
        run_case(project, case, policy[0], ids)
        return
    before = project.git("rev-parse", DEVELOP)
    prerequisite = case.parent / scenario["prerequisite"]
    if profile_policy != policy_fields:
        raise stack.Failure("The prerequisite and conflict must use the same approved policy")
    happy, evidence = run_case(project, prerequisite, policy[0], ids)
    after = evidence["develop_after"]
    if before == after:
        raise stack.Failure("Prerequisite produced no target advance")
    project.git("update-ref", f"refs/heads/canary-seed/{prerequisite.name}", after)
    project.git("update-ref", DEVELOP, before, after)
    item = add_dependency(project, project.item(stack.read(case / "issue.json")), happy)
    run_case(project, case, policy[0], ids, original=before, advanced=after, item=item)


def stop_on_signal(number, frame):
    raise InterruptedError(f"interrupted by signal {number}")


if __name__ == "__main__":
    signal.signal(signal.SIGTERM, stop_on_signal)
    try:
        profile, path = sys.argv[1:]
        execute(profile, Path(path))
        print("PASS local smoke", flush=True)
    except (KeyError, IndexError, OSError, ValueError, stack.Failure, subprocess.SubprocessError) as error:
        print(f"FAIL {error}", flush=True)
        sys.exit(1)
    finally:
        stack.down()
