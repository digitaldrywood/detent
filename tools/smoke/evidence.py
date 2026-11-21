from datetime import datetime, timezone
import re
from pathlib import Path
import subprocess

import stack


def expect(label, expected, actual):
    if expected != actual:
        raise stack.Failure(f"{label}: expected {expected!r}, observed {actual!r}")


def instant(value):
    return datetime.fromisoformat(value[:-1] + "+00:00" if value.endswith("Z") else value)


def time_key(value):
    timestamp = instant(value).astimezone(timezone.utc)
    fraction = re.search(r"\.(\d+)(?:Z|[+-]\d\d:\d\d)$", value)
    return timestamp.replace(microsecond=0), (fraction[1] if fraction else "").ljust(9, "0")


def check_history(evidence):
    restore_window = evidence.get("admission_restore_window")
    restored = False
    for event in sorted(evidence["history"], key=lambda row: int(row["aggregate_sequence"])):
        kind = event["type"]
        if event.get("data_omission") and kind not in {"run.checkpointed", "run.observed", "run.finished"}:
            raise stack.Failure(f"history omitted {kind}")
        if kind != "workflow.transitioned":
            continue
        transition = event["data"]
        source, target = transition["from_state"], transition["to_state"]
        if source in {"In Progress", "Rework", "Human Review", "Done"} and target in {"Backlog", "Todo", "Rework"}:
            if restore_window and not restored and (source, target) == ("In Progress", "Todo"):
                at = time_key(event["recorded_at"])
                if time_key(restore_window[0]) <= at <= time_key(restore_window[1]):
                    restored = True
                    continue
            raise stack.Failure(f"backward lane transition {source} -> {target}")


def assert_binding(record, attempt, fields, label):
    for field in fields:
        expect(f"{label} {field}", attempt[field], record.get(field))


def check_workspace(project, evidence, attempt, expected, service, retained):
    item = evidence["item"]
    reference = f"{project.id}#{item['number']}"
    marker = attempt["attempt_id"].removeprefix("attempt_")
    begin = instant(attempt["started_at"])
    released = instant(attempt["claim_released_at"])
    starts = []
    execution_groups = set()
    provider_groups = set()
    with (stack.LOGS / "runner.log").open() as log:
        for line in log:
            if not any(message in line for message in ("after_create", "execution worker selected", "worker_process_reap_decision")):
                continue
            if not any(value in line for value in (reference, marker, item["work_item_id"])):
                continue
            row = stack.log_fields(line)
            if not begin <= instant(row["time"]) <= released:
                continue
            if row.get("hook") == "after_create" and row["msg"] == "running workspace hook":
                starts.append(Path(row["path"]))
            if row["msg"] == "execution worker selected" and row.get("issue") == reference:
                execution_groups.add(int(row["pgid"]))
            if row.get("event") == "worker_process_reap_decision" and row.get("issue_id") == item["work_item_id"]:
                provider_groups.add(int(row["pgid"]))
    expect("worktrees created", expected, len(starts))
    checkout = starts[0]
    expect("item worktree parent", (project.workspace / ".detent/workspaces").resolve(), checkout.parent.resolve())
    expect("worktree belongs to the same attempt", True, marker in checkout.name)
    if evidence["phases"]:
        branch = evidence["phases"][-1]["branch"]
        expect("worktree matches phase branch", True, checkout.name.endswith("--" + branch.rsplit("/", 1)[-1]))
    refs = stack.command(["git", "worktree", "list", "--porcelain"], cwd=project.workspace)
    registered = [Path(line.removeprefix("worktree ")).resolve() for line in refs.splitlines()
                  if line.startswith("worktree ") and marker in line]
    expect("item worktree registration", [checkout.resolve()] if retained else [], registered)
    expect("item worktree present on disk", retained, checkout.exists())
    if retained:
        expect("retained source remains on the base", evidence["develop_before"],
               stack.command(["git", "rev-parse", "HEAD"], cwd=checkout))
        expect("retained source clean", "", stack.command(["git", "status", "--porcelain"], cwd=checkout))
    expect("recorded execution worker", True, bool(execution_groups))
    expect("recorded provider turns", True, bool(provider_groups))
    live = {int(group) for group in subprocess.check_output(["ps", "-Ao", "pgid="], text=True).split()}
    expect("recorded worker groups still active", [], sorted(live & (execution_groups | provider_groups)))
    if service:
        observed = evidence.get("service")
        expect("preview accepted a connection at the item worktree", True, bool(observed and Path(observed["checkout"]).resolve() == checkout.resolve()))
        expect("preview process still active after cleanup", False,
               stack.process_info(observed["pid"]) == observed["identity"])
    return checkout


def check_reviews(review_policy, reviews, version_id, gates):
    approved = [review for review in reviews if review["version_id"] == version_id and review["decision"] == "approved"]
    if gates["automated_review"] in {"required", "off"}:
        expect("implementation review approval", gates["automated_review"] == "required",
               any(review.get("implementation_review") for review in approved))
    if gates["validator"]:
        expect("independent acceptance approval", True, any(review.get("validator") for review in approved))
    if gates["security_audit"]:
        expect("independent source audit", True, any(
            audit.get("verdict") == "pass" and not audit.get("landing") and audit["version_id"] == version_id
            for review in approved if (audit := review.get("security_audit"))
        ))
    if review_policy.get("required_checks"):
        raise stack.Failure("The todo scenario unexpectedly requires an external check")


def check_gate_orders(pipeline, old, new, landed_at, command):
    def gate(source):
        applicable = [
            span for span in pipeline if span.get("stage") == "finalization" and span.get("command") == command
            and span.get("head_sha") == source[0] and span.get("tree_sha") == source[1]
            and span.get("execution") == "executed" and span.get("exit_code") == 0
        ]
        expect(f"fresh gate on {source[0]}", True, bool(applicable))
        return applicable[-1]

    before, after = gate(old), gate(new)
    failures = [span for span in pipeline if span["stage"] == "rebase" and span.get("outcome") == "conflict"]
    repairs = [span for span in pipeline if span["stage"] == "conflict_resolution" and span.get("outcome") == "agent-resolved"]
    expect("ordered failed rebase, repair and fresh gate", True, any(
        time_key(before["finished_at"]) < time_key(failure["started_at"]) <= time_key(failure["finished_at"])
        <= time_key(repair["started_at"]) < time_key(after["started_at"]) <= time_key(after["finished_at"])
        <= time_key(repair["finished_at"]) < time_key(landed_at)
        for failure in failures for repair in repairs
    ))


def phase_expectations(expected, gates):
    labels = list(expected["phases"])
    if gates["automated_review"] == "off":
        labels = expected.get("phases_without_implementation_review", [label for label in labels if not label.startswith("review:")])
    if gates["security_audit"]:
        result = []
        for label in labels:
            if label == "landing:landed":
                result.append("landing:audit:pass")
            result.append(label)
            if label == "gate:pass":
                result.append("audit:pass")
        return result
    return labels


def check_conflict(project, evidence, labels, versions, reviews, landing, policy, command, advanced):
    old, new = (next(version for version in versions if version["base_sha"] == base)
                for base in (evidence["develop_before"], advanced))
    expect("two immutable source versions", 2, len(versions))
    expect("rewritten source head", True, old["head_sha"] != new["head_sha"])
    expect("advanced base is in rewritten source", advanced, project.git("merge-base", advanced, new["head_sha"]))
    expect("landing on advanced base", advanced, project.git("rev-parse", landing["merge_sha"] + "^"))
    expect("reported landing base", advanced, landing["base_sha"])
    names = {"implement": "implement", "gate:pass": "project_gate", "review:approved": "implementation_review",
             "validator:pass": "acceptance", "audit:pass": "security_audit", "landing:landed": "merge"}
    conflict = labels.index("landing:conflict")
    expect("recorded rebase after conflict", "landing:rebased", labels[conflict + 1])
    expected = []
    sources = []
    for version, phases in zip((old, new), (labels[:conflict], labels[conflict + 2:])):
        source = (version["head_sha"], project.git("rev-parse", version["head_sha"] + "^{tree}"))
        sources.append(source)
        expected.extend((names[phase], *source) for phase in phases if phase != "landing:audit:pass")
        check_reviews(version["review_policy"], reviews, version["version_id"], policy)
    expect("ordered phase receipts on both exact source trees", expected,
           [(phase["phase"], phase["head_sha"], phase["tree_sha"]) for phase in evidence["phases"]])
    check_gate_orders(evidence["details"][0]["runtime"]["pipeline"], *sources, landing["observed_at"], command)


def gate_command(policy):
    config = policy["authored"]["files"]["detent.yaml"]
    program = "print YAML.safe_load(STDIN.read).fetch('gate').fetch('run')"
    return stack.command(["ruby", "-ryaml", "-e", program], data=config)


def check_landing(project, policy, evidence, attempt, expected, versions, reviews, landing, command):
    after = evidence["develop_after"]
    expect("landed", True, landing["landed"])
    expect("workspace retirement receipt", True, landing.get("workspace_retired"))
    expect("landed target SHA", after, landing["merge_sha"])
    expect("changed target", True, after != evidence["develop_before"])
    expect("landing method", policy["merge_method"], landing["method"])
    version = next(version for version in versions if version["version_id"] == landing["version_id"])
    head = version["head_sha"]
    expect("landed source", head, landing["head_sha"])
    tree = project.git("rev-parse", head + "^{tree}")
    expect("landed tree", tree, project.git("rev-parse", after + "^{tree}"))
    expect("published item branch at final head", 1, sum(
        line.split()[0] == head for line in project.git("for-each-ref", "refs/heads/detent/").splitlines()
    ))
    expect("landing before cleanup acknowledgement", True,
           time_key(landing["observed_at"]) <= time_key(attempt["cleanup_acknowledged_at"]))
    phases = evidence["phases"]
    expect("merge phase exact source", ("merge", head, tree),
           (phases[-1]["phase"], phases[-1]["head_sha"], phases[-1]["tree_sha"]))
    check_reviews(version["review_policy"], reviews, version["version_id"], policy)
    receipts = [span for span in attempt["runtime"]["pipeline"] if
                span.get("command") == command and span.get("head_sha") == head
                and span.get("tree_sha") == tree and span.get("exit_code") == 0]
    executed = {span["receipt_id"] for span in receipts if span.get("execution") == "executed"}
    expect("fresh command gate on exact landing tree", True, bool(executed))
    validations = [span for span in receipts if span.get("stage") == "landing_validation"
                   and (span.get("execution") == "executed" or span.get("reused_receipt_id") in executed)]
    expect("successful per-landing validation", True, bool(validations))
    if policy["security_audit"]:
        audit = landing.get("security_audit") or {}
        expect("landing audit scope", (True, version["version_id"], "pass"),
               (audit.get("landing"), audit.get("version_id"), audit.get("verdict")))
        record = [review for review in reviews if review["version_id"] == version["version_id"]
                  and review["decision"] == "approved" and review.get("security_audit") == audit]
        expect("published landing audit receipt", 1, len(record))
        source_audits = [review["security_audit"] for review in reviews if review["version_id"] == version["version_id"]
                         and review.get("security_audit") and not review["security_audit"].get("landing")]
        expect("independent landing audit session", True,
               bool(audit.get("session_id")) and all(source["session_id"] != audit["session_id"] for source in source_audits))
        expect("landing audit after integrated gate and before landing", True,
               time_key(validations[-1]["finished_at"]) <= time_key(record[0]["created_at"]) <= time_key(landing["observed_at"]))
    for name, needle in expected.get("files", {}).items():
        expect(f"landed {name} contains {needle!r}", True, needle in project.git("show", f"{after}:{name}"))
    for name, needle in expected.get("absent", {}).items():
        expect(f"landed {name} omits {needle!r}", True, needle not in project.git("show", f"{after}:{name}"))


def phase_events(attempt, receipt_phases, reviews, command, interrupted_at=None):
    runtime = attempt["runtime"]
    events = set()

    def add(when, label):
        events.add((time_key(when), label))

    implemented = [phase for phase in receipt_phases if phase["phase"] == "implement"]
    commands = [span for span in runtime.get("pipeline", [])
                if span.get("stage") == "finalization" and span.get("command") == command
                and span.get("execution") == "executed" and span.get("exit_code") is not None]
    for phase in runtime.get("phases", []):
        if phase["name"] == "planning":
            add(phase["started_at"], "plan")
        if not implemented and phase["name"] in {"implementation", "rework"}:
            add(phase["started_at"], "implement")
    if interrupted_at:
        add(interrupted_at, "implement")
    for phase in implemented:
        source = (phase["head_sha"], phase["tree_sha"])
        matching = [span["started_at"] for span in commands if source == (span.get("head_sha"), span.get("tree_sha"))]
        expect("implemented tree has a command gate attempt", True, bool(matching))
        add(min(matching, key=time_key), "implement")
    for span in commands:
        expect("gate source and tree identity", True, bool(span.get("head_sha") and span.get("tree_sha")))
        add(span["finished_at"], "gate:fail" if span["exit_code"] else "gate:pass")
    for review in reviews:
        if review.get("implementation_review") and review["decision"] in {"approved", "changes_requested"}:
            add(review["created_at"], f"review:{review['decision']}")
        if review.get("validator") and review["decision"] in {"approved", "changes_requested"}:
            label = "pass" if review["decision"] == "approved" else "changes_requested"
            add(review["created_at"], f"validator:{label}")
        audit = review.get("security_audit") or {}
        if audit.get("verdict") == "pass":
            add(review["created_at"], "landing:audit:pass" if audit.get("landing") else "audit:pass")
    if (runtime.get("landing") or {}).get("landed"):
        add(runtime["landing"]["observed_at"], "landing:landed")
    if (attempt.get("disposition") or {}).get("human_action"):
        add(attempt["claim_released_at"], "human_action")
    expect("phase timestamps resolve an order", len(events), len({stamp for stamp, _ in events}))
    return [label for _, label in sorted(events)]


def compare(project, policy, evidence, expected, definition, advanced):
    item, claims, runs = (evidence[field] for field in ("item", "attempts", "details"))
    expect("final lane", expected["final_lane"], item["state"])
    runs = sorted(runs, key=lambda run: time_key(run["started_at"]))
    admission_restore = evidence.get("pre_provider_retry", False)
    counts = expected["fault"]["released_retry"] if admission_restore else expected
    expect("attempts", counts["attempts"], len(runs))
    expect("leases", counts["leases"], len({run["lease_id"] for run in claims}))
    expect("fenced claims", counts["claims"], len({(run["lease_id"], run["fencing_token"]) for run in claims}))
    check_history(evidence)
    expect("current lease", None, (evidence["explanation"].get("native_runtime") or {}).get("current_lease"))
    attempt = runs[-1]
    fence = ("attempt_id", "lease_id", "fencing_token", "policy_id")
    assert_binding(next(run for run in claims if run["attempt_id"] == attempt["attempt_id"]), attempt, fence, "claim")
    expect("approved policy", policy["policy_id"], attempt["policy_id"])
    retained = expected.get("workspace_disposition") == "retained"
    expect("cleanup acknowledgement", expected.get("cleanup_acknowledged", not retained), bool(attempt.get("cleanup_acknowledged_at")))
    if attempt.get("cleanup_acknowledged_at"):
        expect("cleanup acknowledged before claim release", True,
               time_key(attempt["cleanup_acknowledged_at"]) <= time_key(attempt["claim_released_at"]))
    for phase in evidence["phases"]:
        assert_binding(phase, attempt, fence, "phase")
    accepted = [phase["phase"] for phase in evidence["phases"]]
    if policy["gates"]["plan_enabled"]:
        expect("one fenced plan receipt before implementation", True,
               accepted.count("plan") == 1 and "implement" in accepted and accepted.index("plan") < accepted.index("implement"))
    for verdict in evidence["rounds"]:
        assert_binding(verdict, attempt, fence, "round")
    actions = [record["action"] for record in evidence["rounds"]]
    if actions == ["no_progress"] and "alternative" in expected:
        expected = expected | expected["alternative"]
    expect("round verdicts", expected["verdicts"], actions)

    checks = attempt["runtime"]
    expect("complete phase and pipeline evidence", False, bool(checks.get("phases_dropped") or checks.get("pipeline_dropped")))
    versions = [version for change in evidence["changes"] for version in change["versions"]]
    reviews = [review for change in evidence["changes"] for review in change["reviews"]]
    for version in versions:
        assert_binding(version, attempt, ("attempt_id", "run_id", "policy_id"), "source version")
    gates = policy["gates"]
    command = gate_command(policy)
    labels = phase_expectations(expected, gates)
    landing = checks.get("landing") or {}
    if item["state"] == "Done":
        check_landing(project, gates, evidence, attempt, expected, versions, reviews, landing, command)
    else:
        expect("unlanded target unchanged", evidence["develop_before"], evidence["develop_after"])
        expect("no published source version", [], versions)
        expect("no published item branch", "", project.git("for-each-ref", "refs/heads/detent/"))
        if "human_action" in expected["verdicts"]:
            disposition = attempt["disposition"]
            expect("typed human action", True, disposition.get("human_action"))
            questions = {record.get("human_action", "").casefold() for record in evidence["rounds"]
                         if record["action"] == "human_action"}
            expect("one recorded typed question", 1, len(questions))
            terms = expected["question"]
            if isinstance(terms, str):
                terms = [terms]
            question, = questions
            expect("typed question covers each conflicting requirement", [], [term for term in terms if term.casefold() not in question])
    check_workspace(project, evidence, attempt, expected["worktrees"], definition.get("retained_service"), retained)
    if expected.get("rejected_candidate"):
        rejected = {review["version_id"] for review in reviews if review["decision"] == "changes_requested"
                    and (review.get("implementation_review") if gates["automated_review"] != "off" else review.get("validator"))}
        candidates = [version for version in versions if version["version_id"] in rejected and version["version_id"] != landing["version_id"]]
        expect("rejected source version", 1, len(candidates))
        for name, needle in expected["rejected_candidate"].items():
            expect(f"rejected source contains {needle!r}", True,
                   needle in project.git("show", f"{candidates[0]['head_sha']}:{name}"))
    if advanced:
        check_conflict(project, evidence, labels, versions, reviews, landing, gates, command, advanced)
    else:
        if (expected.get("fault") or {}).get("resumed_implementation") and not admission_restore:
            labels.insert(0, "implement")
        phases = phase_events(attempt, evidence["phases"], reviews, command, evidence.get("interrupted_implementation_at"))
        if definition.get("retained_service"):
            phases = ["hook:accepted", *phases, "hook:retired"]
        if actions == ["no_progress"]:
            phases.append("no_progress")
        expect("ordered phase evidence", labels, phases)
    print(f"Verified {attempt['attempt_id']} with {len(claims)} claim and {expected['worktrees']} worktree", flush=True)
