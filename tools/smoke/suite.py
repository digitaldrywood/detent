import argparse
from concurrent.futures import ThreadPoolExecutor
from contextlib import contextmanager, suppress
import fcntl
import json
import os
from pathlib import Path
import re
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
from threading import Event
import time

TOOLS = Path(__file__).resolve().parent
REPOSITORY = TOOLS.parents[1]
PORT_NAMES = ("postgres", "hub", "runner", "git")
PROFILES = ("baseline", "pyro")
FIXTURE_REMOTE = "git@github.com:digitaldrywood/detent-canary-todo.git"
DEFAULT_REF = "origin/develop"
STOP = Event()


class Failure(ValueError):
    pass


def command(*args, cwd=REPOSITORY, log=None, env=None):
    with subprocess.Popen([str(arg) for arg in args], cwd=cwd, env=env, stdin=subprocess.DEVNULL,
                          stdout=log or subprocess.PIPE, stderr=log or subprocess.PIPE, text=True,
                          start_new_session=True) as process:
        try:
            output, _ = process.communicate(timeout=900)
        except BaseException:
            with suppress(ProcessLookupError):
                os.killpg(process.pid, signal.SIGKILL)
            raise
    if process.returncode:
        raise Failure(f"{Path(args[0]).name} {args[1]} exited {process.returncode}"
                      + (f". See {log.name}" if log else ""))
    return (output or "").strip()


def stop_setup(number, _frame):
    raise Failure(f"setup interrupted by signal {number}")


def checkout_fixture(root):
    configured = os.environ.get("DETENT_SMOKE_FIXTURE", FIXTURE_REMOTE)
    tag = "canary-baseline-v2"
    ref = os.environ.get("DETENT_SMOKE_FIXTURE_REF")
    if not ref and configured == FIXTURE_REMOTE:
        ref = "test/smoke-scenarios"
    target = root / "fixture"
    clone = ["git", "clone", "--quiet", "--no-local"]
    if ref:
        clone.extend(("--branch", ref))
    env = os.environ | {"GIT_TERMINAL_PROMPT": "0", "GIT_SSH_COMMAND": "ssh -o BatchMode=yes"}
    command(*clone, "--", configured, target, env=env)
    command("git", "rev-parse", "--verify", f"{tag}^{{commit}}", cwd=target)
    return target


def build(ref, root):
    candidate = Path(ref).expanduser()
    checkout = candidate.resolve() if candidate.is_dir() else REPOSITORY
    revision = "HEAD" if candidate.is_dir() else ref
    if candidate.is_dir() and command("git", "status", "--porcelain", cwd=checkout):
        raise Failure("The requested source worktree has uncommitted changes")
    commit = command("git", "rev-parse", "--verify", "--end-of-options", revision + "^{commit}", cwd=checkout)
    source = root / "source"
    target = root / "detent"
    app_env = os.environ | {"npm_config_audit": "false", "npm_config_fund": "false"}
    if sys.platform == "linux":
        app_env.setdefault("UV_THREADPOOL_SIZE", "1")
        app_env.setdefault("MALLOC_ARENA_MAX", "2")
    with (root / "build.log").open("w") as log:
        command("git", "worktree", "add", "--detach", source, commit, cwd=checkout, log=log)
        command("make", "app", cwd=source, log=log, env=app_env)
        overlay = root / "overlay"
        command(sys.executable, TOOLS / "source-worker-overlay.py", source, overlay, log=log)
        command("go", "build", "-p", "4", "-overlay", overlay / "overlay.json",
                "-ldflags", f"-X main.version=dev-local -X main.commit={commit}",
                "-o", target, "./cmd/detent", cwd=source, log=log)
    return commit, source, target


def read(path):
    return json.loads(path.read_text())


def load_cases(fixture, pack):
    cases = {
        path.name: path for path in sorted((fixture / "smoke/scenarios").glob("*/"))
        if (path / "scenario.json").is_file()
    }
    if pack:
        for path in sorted((pack / "scenarios").glob("*/")):
            if not (path / "run").is_file():
                continue
            if path.name in cases:
                raise Failure(f"Repeated scenario {path.name}")
            cases[path.name] = path
    if not cases:
        raise Failure("The fixture does not contain smoke scenario data")
    for name in cases:
        if not re.fullmatch(r"[a-z0-9][a-z0-9-]*", name):
            raise Failure(f"Invalid scenario name {name}")
    return cases


def ports_free(ports):
    listeners = []
    try:
        for port in ports:
            listener = socket.socket()
            listeners.append(listener)
            listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            listener.bind(("127.0.0.1", port))
            listener.listen(1)
        return True
    except OSError:
        return False
    finally:
        for listener in listeners:
            listener.close()


@contextmanager
def slot(base, count):
    namespace = Path(tempfile.gettempdir())
    for index in range(count):
        ports = range(base + index * len(PORT_NAMES), base + (index + 1) * len(PORT_NAMES))
        path = namespace / f"detent-smoke-{os.getuid()}-{ports.start}.lock"
        try:
            descriptor = os.open(path, os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
        except FileExistsError:
            continue
        with os.fdopen(descriptor, "w") as lock:
            try:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError:
                continue
            if ports_free(ports):
                yield dict(zip(PORT_NAMES, ports))
                return
    raise Failure("No fixed loopback port group is available")


def replay_result(args, root, env, log, seconds, harness):
    with subprocess.Popen([str(arg) for arg in args], cwd=root, env=env,
                          stdin=subprocess.DEVNULL, stdout=log, stderr=subprocess.STDOUT) as process:
        deadline = time.monotonic() + seconds
        while True:
            try:
                return process.wait(timeout=1)
            except subprocess.TimeoutExpired:
                if not STOP.is_set() and time.monotonic() < deadline:
                    continue
            process.terminate()
            try:
                process.wait(timeout=45)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()
                command(sys.executable, harness / "stack.py", "down", cwd=root, env=env, log=log)
            raise Failure("replay interrupted" if STOP.is_set() else "replay process timed out")


def execute(row, paths, config):
    index, profile, name, case = row
    root = paths["root"] / f"r{index:02}"
    root.mkdir(mode=0o700)
    started = time.monotonic()
    output = root / "run.log"
    status = 1
    difference = ""
    with output.open("w") as log:
        try:
            if STOP.is_set():
                raise Failure("replay interrupted before startup")
            with slot(config.port, max(8, config.parallel * 2)) as ports:
                (root / "ports.json").write_text(json.dumps(ports) + "\n")
                env = paths["env"] | {"DETENT_SMOKE_STACK": str(root)}
                harness = paths["harness"]
                args = [sys.executable, "-B", harness / "replay.py", profile, case]
                if (case / "run").is_file():
                    args = [case / "run", "--profile", profile, "--ref", paths["commit"], "--artifacts", root]
                    env["DETENT_SMOKE_SOURCE_REF"] = paths["commit"]
                status = replay_result(args, root, env, log, config.timeout * 2 + 180, harness)
        except (OSError, ValueError, subprocess.SubprocessError) as error:
            difference = str(error)
            log.write(f"FAIL {difference}\n")
    if status:
        failure = next((line[5:] for line in output.read_text().splitlines() if line.startswith("FAIL ")), None)
        difference = failure or difference or f"replay exited {status}"
    return (name, profile, "FAIL" if status else "PASS", f"{time.monotonic() - started:.1f}", difference, str(root))


def table(results):
    header = ("Scenario", "Profile", "Result", "Seconds", "First difference", "Evidence")
    widths = [max(len(value), *(len(row[index]) for row in results)) for index, value in enumerate(header)]
    for row in [header, *results]:
        print("  ".join(value.ljust(width) for value, width in zip(row[:-1], widths)) + "  " + row[-1], flush=True)


def main():
    parser = argparse.ArgumentParser(description="Replay local Detent scenarios with real provider sessions")
    parser.add_argument("scenarios", nargs="*")
    parser.add_argument("--profile", choices=(*PROFILES, "both"), default="baseline")
    parser.add_argument("--ref", default=DEFAULT_REF, help="clean worktree path or Git ref to build")
    parser.add_argument("--parallel", type=int, default=int(os.environ.get("DETENT_SMOKE_PARALLEL", "4")))
    parser.add_argument("--port", type=int, default=int(os.environ.get("DETENT_SMOKE_PORT", "57000")))
    parser.add_argument("--timeout", type=int, default=int(os.environ.get("DETENT_SMOKE_TIMEOUT", "1200")))
    parser.add_argument("--list", action="store_true")
    config = parser.parse_args()
    if not 1 <= config.parallel <= 16 or not 1024 < config.port <= 65000 or not 1 <= config.timeout <= 3600:
        parser.error("Invalid parallelism, base port or replay timeout")
    for event in (signal.SIGINT, signal.SIGTERM):
        signal.signal(event, stop_setup)
    os.umask(0o077)
    archive = Path(os.environ.get("DETENT_SMOKE_ARTIFACTS", "/tmp")).expanduser().resolve()
    archive.mkdir(parents=True, exist_ok=True)
    root = Path(tempfile.mkdtemp(prefix="ds-", dir=archive))
    print(f"Evidence {root}", flush=True)
    fixture = checkout_fixture(root)
    pack_value = os.environ.get("DETENT_SMOKE_PRIVATE_PACK")
    pack = Path(pack_value).resolve() if pack_value else None
    cases = load_cases(fixture, pack)
    if config.list:
        print("\n".join(cases))
        return 0
    selected = config.scenarios or ["all"]
    if selected == ["all"]:
        selected = list(cases)
    elif set(selected) - cases.keys() or len(set(selected)) != len(selected):
        raise Failure("Select all or unique scenario names from --list")
    auth = Path(os.environ.get("DETENT_SMOKE_CODEX_AUTH", str(Path.home() / ".codex/auth.json"))).resolve()
    uses_live_codex = any(not (cases[name] / "scenario.json").is_file()
                          or not read(cases[name] / "scenario.json").get("scripted_codex") for name in selected)
    if uses_live_codex and not auth.is_file():
        raise Failure("Codex authentication is missing. Set DETENT_SMOKE_CODEX_AUTH to an existing credential file")
    pg = os.environ.get("DETENT_SMOKE_POSTGRES") or command("pg_config", "--bindir")
    if not (Path(pg) / "initdb").is_file():
        raise Failure("PostgreSQL binaries are missing")
    git = shutil.which("git")
    if not git:
        raise Failure("Git is missing")
    commit, source, binary = build(config.ref, root)
    print(f"Detent {commit}", flush=True)
    print(f"Fixture {command('git', 'rev-parse', 'HEAD', cwd=fixture)}", flush=True)
    cache = os.environ.get("DETENT_SMOKE_GO_CACHE", str(root / "go-cache"))
    env = {key: os.environ[key] for key in ("PATH", "USER") if key in os.environ}
    env.update(DETENT_SMOKE_FIXTURE=str(fixture), DETENT_SMOKE_BUILD_SOURCE=str(source),
               DETENT_SMOKE_BUILD_BINARY=str(binary), DETENT_SMOKE_POSTGRES=pg, DETENT_SMOKE_GIT=git,
               DETENT_SMOKE_GO_CACHE=cache, DETENT_SMOKE_AUTH=str(auth), DETENT_SMOKE_TIMEOUT=str(config.timeout))
    profiles = PROFILES if config.profile == "both" else (config.profile,)
    matrix = [(profile, name, cases[name]) for profile in profiles for name in selected]
    rows = [(index, *row) for index, row in enumerate(matrix, 1)]
    harness = root / "harness"
    shutil.copytree(TOOLS, harness, ignore=shutil.ignore_patterns("__pycache__"))
    paths = {"commit": commit, "root": root, "env": env, "harness": harness}
    for event in (signal.SIGINT, signal.SIGTERM):
        signal.signal(event, lambda *_: STOP.set())
    with ThreadPoolExecutor(max_workers=config.parallel) as pool:
        results = list(pool.map(lambda row: execute(row, paths, config), rows))
    table(results)
    (root / "summary.json").write_text(json.dumps(results, indent=2) + "\n")
    return int(any(row[2] != "PASS" for row in results))


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (Failure, OSError, subprocess.SubprocessError) as error:
        sys.exit(f"FAIL {error}")
