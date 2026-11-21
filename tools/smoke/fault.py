import importlib.util
import socket
import time

import stack
from evidence import expect, time_key


def load_operator(fixture):
    source = fixture / "scripts/replay-fault.py"
    spec = importlib.util.spec_from_file_location("fixture_fault", source)
    operator = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(operator)
    return operator


class Fault:
    def __init__(self, fixture, project, item_path, definition, specification):
        self.operator = load_operator(fixture)
        self.project = project
        self.item_path = item_path
        self.specification = specification
        self.action = definition["action"]
        self.definition = definition
        for field, value in definition.items():
            if field != "description":
                expect(f"fault {field} matches its expectation", value, specification[field])
        self.samples = []
        self.observation = None
        self.refusal = None
        self.since = time.time()

    def arm(self):
        if self.action != "refuse_loopback":
            return
        self.refusal = socket.socket()
        self.refusal.bind(("127.0.0.1", 0))
        with (stack.STATE / "startup-refusal").open("x") as marker:
            marker.write(str(self.refusal.getsockname()[1]))

    def close(self):
        if self.refusal:
            self.refusal.close()

    def startup_observation(self):
        if not (stack.STATE / "startup-refusal.used").exists():
            return None
        with (stack.LOGS / "runner.log").open() as log:
            for line in log:
                if "injected local worker startup refusal" in line:
                    entry = stack.log_fields(line)
                    return {"at": entry["observed_at"], "action": self.action, "pid": int(entry["pid"])}
        return None

    def active_validator(self, attempt):
        try:
            session = self.project.request("/runner-sessions/find", "POST", {
                "Lookup": {"IssueID": self.item_path.rsplit("/", 1)[-1], "NativeAttemptID": attempt,
                           "AgentRole": "validator"}
            })
        except stack.Failure as error:
            if "HTTP 404" not in str(error):
                raise
            return None
        if session.get("Finish") is None:
            return (session.get("Provider") or {}).get("ThreadID")
        return None

    def poll(self, item, attempts):
        board = {}
        if self.action == "kill_worker" and attempts:
            org, admin = stack.current()
            board = self.operator.read_board(stack, org, self.project.id, item["work_item_id"], admin)
        if self.specification["instance_failure"]:
            observed = [{key: attempt[key] for key in ("attempt_id", "status", "lease_id")} for attempt in attempts]
            sample = {"at": self.operator.timestamp(), "lane": item["state"], "attempts": observed, "board": board}
            if not self.samples or any(sample[field] != self.samples[-1][field] for field in ("lane", "attempts", "board")):
                self.samples.append(sample)
        if self.observation:
            return
        if self.action == "refuse_loopback":
            self.observation = self.startup_observation()
        else:
            active = next((record for record in attempts if record["status"] == "running"), None)
            if active:
                self.inject(active)
        if self.observation:
            stack.write(stack.ROOT / "fault.json", self.observation)

    def inject(self, active):
        attempt_id = active["attempt_id"]
        turns = self.operator.running_turns(stack, attempt_id, self.since)
        if self.action == "kill_worker":
            detail = self.project.request(self.item_path + "/attempts/" + attempt_id)
            runtime = detail.get("runtime") or {}
            if runtime.get("phase") != "implementation" or not runtime.get("worker_launch"):
                return
            turn = next(turns, None)
            if not turn:
                return
            launch = runtime["worker_launch"]
            at = self.operator.timestamp()
            if not self.operator.kill_worker(stack, launch):
                return
            proof = {"at": at, "worker": launch, **turn}
        elif self.action == "restart_hub":
            validator = self.active_validator(attempt_id)
            turn = next((turn for turn in turns if turn["thread"] == validator), None) if validator else None
            if not turn:
                return
            stack.stop("hub")
            at = self.operator.timestamp()
            time.sleep(self.definition["down_seconds"])
            stack.start_hub()
            proof = {"at": at, "ready_at": self.operator.timestamp(), **turn}
        else:
            raise stack.Failure(f"unknown fault action {self.action!r}")
        self.observation = {"action": self.action, "attempt_id": attempt_id, "lease_id": active["lease_id"], **proof}

    def verify(self, evidence):
        proof = self.observation
        difference = self.operator.verify_fault(self.specification, proof, self.samples, evidence["history"])
        stack.write(stack.ROOT / "fault.evidence.json", {"fault": proof, "samples": self.samples, "difference": difference})
        if difference:
            raise stack.Failure(difference)
        if self.action == "kill_worker" or self.specification.get("resumed_implementation") and len(evidence["details"]) == 1:
            attempt_id = evidence["details"][0]["attempt_id"]
            interrupted = next((event for event in self.operator.instance_failure(evidence["history"])
                                if event["data"]["run"]["attempt_id"] == attempt_id), None)
            expect("interrupted implementation on the resumed attempt", True, bool(interrupted))
            evidence["interrupted_implementation_at"] = interrupted["recorded_at"]
        if self.action == "refuse_loopback":
            self.verify_startup(evidence, proof)

    def verify_startup(self, evidence, proof):
        runs = sorted(evidence["details"], key=lambda row: time_key(row["started_at"]))
        first, successor = runs[0], runs[-1]
        interruption = next((row for row in self.operator.instance_failure(evidence["history"])
                             if row["data"]["run"]["attempt_id"] == first["attempt_id"]), None)
        expect("first admission durably interrupted", True, bool(interruption))
        interrupted_at = interruption["recorded_at"]
        expect("interruption follows injected refusal", True, time_key(proof["at"]) <= time_key(interrupted_at))
        markers = {run["attempt_id"].removeprefix("attempt_") for run in runs}
        provider_starts = []
        first_marker = first["attempt_id"].removeprefix("attempt_")
        for path in stack.provider_sessions(self.since):
            metadata = next(self.operator.read_json_lines(path), {})
            session = metadata.get("payload", {})
            checkout = session.get("cwd", "")
            if not any(marker in checkout for marker in markers):
                continue
            at = session.get("timestamp") or metadata["timestamp"]
            expect("initial refusal preceded provider launch", True, time_key(interrupted_at) <= time_key(at))
            if len(runs) == 2:
                expect("released bootstrap has no provider rollout", False, first_marker in checkout)
            provider_starts.append(at)
        expect("recovery has a provider launch", True, bool(provider_starts))
        evidence["admission_restore_window"] = [proof["at"], min(provider_starts, key=time_key)]
        if len(runs) != 2:
            return
        expect("initial bootstrap completed before replacement", True,
               bool(first.get("claim_released_at")) and time_key(first["claim_released_at"]) <= time_key(successor["started_at"]))
        expect("initial bootstrap has no source phase", False,
               any(row["attempt_id"] == first["attempt_id"] for row in evidence["phases"]))
        expect("initial bootstrap has no round", False,
               any(row["attempt_id"] == first["attempt_id"] for row in evidence["rounds"]))
        with (stack.LOGS / "runner.log").open() as log:
            expect("initial bootstrap has no item worktree", False,
                   any("hook=after_create" in line and first_marker in line for line in log))
        evidence["pre_provider_retry"] = True
