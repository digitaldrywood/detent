import contextlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import re
import secrets
import shlex
import shutil
import signal
import socket
import ssl
import subprocess
import sys
import time
import urllib.error
import urllib.request
import uuid

ROOT = Path(os.environ["DETENT_SMOKE_STACK"])
SOURCE = Path(os.environ["DETENT_SMOKE_BUILD_SOURCE"])
PG = Path(os.environ["DETENT_SMOKE_POSTGRES"])
LOGS = ROOT / "logs"
STATE = ROOT / "state"
SANDBOX_HOME = ROOT / "home"
CODEX = ROOT / "codex"
PGDATA = ROOT / "postgres"
SOCKETS = ROOT / "pg-socket"
WORKSPACES = ROOT / "workspaces"
MIRROR = ROOT / "git"
BINARY = ROOT / "bin/detent"
PORT_NAMES = ("postgres", "hub", "runner", "git")
OPERATIONS = ["read", "claim", "heartbeat", "events", "collaborate"]
LOGINS = {"runtime": "DETENT_LOCAL_RUNTIME_PASSWORD", "migration": "DETENT_LOCAL_MIGRATION_PASSWORD"}
BASE_ENV = {
    "PATH": f"{ROOT / 'bin'}:{os.environ['PATH']}",
    "LC_ALL": "C",
    "HOME": str(SANDBOX_HOME),
    "USER": os.environ.get("USER", "smoke"),
    "TMPDIR": str(ROOT / "tmp"),
    "XDG_CONFIG_HOME": str(SANDBOX_HOME / ".config"),
    "XDG_CACHE_HOME": str(ROOT / "cache"),
    "CODEX_HOME": str(CODEX),
    "GH_CONFIG_DIR": str(SANDBOX_HOME / ".config/gh"),
    "GIT_TERMINAL_PROMPT": "0",
    "GIT_CONFIG_NOSYSTEM": "1",
    "GIT_CONFIG_GLOBAL": str(SANDBOX_HOME / ".gitconfig"),
    "GOCACHE": os.environ["DETENT_SMOKE_GO_CACHE"],
    "GOPATH": str(ROOT / "cache/gopath"),
    "GOTOOLCHAIN": "local",
    "DETENT_SMOKE_GIT": os.environ["DETENT_SMOKE_GIT"],
    "DETENT_SMOKE_STACK": str(ROOT),
    "DETENT_SMOKE_BUILD_SOURCE": str(SOURCE),
    "DETENT_SMOKE_GO_CACHE": os.environ["DETENT_SMOKE_GO_CACHE"],
    "DETENT_SMOKE_POSTGRES": str(PG),
}


class Failure(Exception):
    pass


def available_ports():
    ports = read(ROOT / "ports.json")
    validate_ports(ports, complete=True)
    return ports


def read(path, default=None):
    return json.loads(path.read_text()) if path.exists() else default


def write(path, value):
    swap = path.with_suffix(path.suffix + ".new")
    swap.write_text(json.dumps(value, indent=2) + "\n")
    swap.replace(path)


def log_fields(line):
    if line.startswith("{"):
        return json.loads(line)
    return dict(field.split("=", 1) for field in shlex.split(line) if "=" in field)


def command(args, cwd=ROOT, env=None, log=None, data=None, timeout=600):
    args = [str(arg) for arg in args]
    with open(LOGS / log, "ab") if log else contextlib.nullcontext() as stream:
        result = subprocess.run(args, cwd=cwd, env=BASE_ENV | (env or {}), input=data,
                                text=True, stdout=stream or subprocess.PIPE,
                                stderr=stream or subprocess.PIPE, timeout=timeout)
    if result.returncode:
        raise Failure(f"{Path(args[0]).name} {args[1]} exited {result.returncode}"
                      + (f". See {LOGS / log}" if log else ""))
    return result.stdout.strip() if result.stdout is not None else ""


def process_state(name):
    return STATE / (name + ".process.json")


def process_info(pid):
    out = subprocess.run(["ps", "-o", "stat=,lstart=,command=", "-p", str(pid)],
                         text=True, capture_output=True).stdout.strip()
    if not out:
        return None
    status, age, month, day, hour, year, args = out.split(maxsplit=6)
    if status.startswith("Z"):
        return None
    return [age, month, day, hour, year, args]



def running(name, stored):
    saved = stored.get("identity")
    live = process_info(stored["pid"])
    if not saved or not live or saved[:5] != live[:5]:
        return False
    command_line = live[5]
    if name == "git":
        return str(Path(__file__).resolve()) in command_line and command_line.endswith(" serve-git")
    return command_line.startswith(str(BINARY) + " ") and (
        " hub serve " in command_line if name == "hub" else str(STATE / "global.yaml") in command_line
    )

def stop(name):
    path = process_state(name)
    stored = read(path)
    if not stored:
        return
    pid = stored["pid"]
    if running(name, stored):
        os.killpg(pid, signal.SIGTERM)
        until = time.monotonic() + 15
        while running(name, stored) and time.monotonic() < until:
            time.sleep(.15)
        if running(name, stored):
            os.killpg(pid, signal.SIGKILL)
    path.unlink()


def spawn(name, args, env=None):
    path = process_state(name)
    stored = read(path)
    if stored and running(name, stored):
        return
    with (LOGS / f"{name}.log").open("ab") as out:
        child = subprocess.Popen([str(arg) for arg in args], cwd=SOURCE, start_new_session=True,
                                 stdin=subprocess.DEVNULL, stdout=out, stderr=out,
                                 env=BASE_ENV | (env or {}))
    write(path, {"pid": child.pid, "identity": process_info(child.pid)})



def validate_ports(ports, complete):
    if not isinstance(ports, dict) or not set(ports).issubset(PORT_NAMES) or (complete and set(ports) != set(PORT_NAMES)):
        raise Failure("Invalid stack port mapping")
    if any(type(port) is not int or port in (4000, 4010, 5432) or not 1024 < port < 65536 for port in ports.values()):
        raise Failure("Invalid stack port number")
    if len(set(ports.values())) != len(ports):
        raise Failure("Stack ports must be distinct")


def check_ports(owned=()):
    for name, port in available_ports().items():
        if name in owned:
            continue
        with socket.socket() as listener:
            listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            try:
                listener.bind(("127.0.0.1", port))
                listener.listen(1)
            except OSError as error:
                raise Failure(f"Fixed {name} port 127.0.0.1:{port} is unavailable ({error.strerror})") from None



def url(name="hub"):
    return f"http://127.0.0.1:{available_ports()[name]}"


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


CLIENT = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())


def request(path, method="GET", body=None, token=None, service="hub", timeout=12):
    headers = {"Accept": "application/json"}
    if token:
        headers["Authorization"] = f"Bearer {token}"
    if body is not None:
        headers["Content-Type"] = "application/json"
    req = urllib.request.Request(url(service) + path, method=method, headers=headers,
                                 data=json.dumps(body).encode() if body is not None else None)
    try:
        with CLIENT.open(req, timeout=timeout) as response:
            raw = response.read()
            return json.loads(raw) if raw else None
    except urllib.error.HTTPError as error:
        raw = error.read()
        try:
            result = json.loads(raw)
            detail = result.get("code", "") + " " + result.get("message", "")
        except (ValueError, TypeError, AttributeError):
            detail = ""
        raise Failure(f"{method} {path} HTTP {error.code} {detail}".rstrip()) from None


def wait_for(description, probe, seconds=30):
    deadline = time.monotonic() + seconds
    while True:
        try:
            value = probe()
            if value:
                return value
        except (OSError, Failure) as error:
            last = str(error)
        else:
            last = "condition pending"
        if time.monotonic() >= deadline:
            raise Failure(f"{description} after {seconds}s. {last}")
        time.sleep(.75)



def database(credentials):
    port = str(available_ports()["postgres"])
    complete = STATE / "postgres-provisioned"
    if not (PGDATA / "PG_VERSION").exists():
        command([PG / "initdb", "-D", PGDATA, "-U", "postgres", "--auth-local=trust", "--auth-host=scram-sha-256",
                 "--no-locale", "--encoding=UTF8"], log="postgres-init.log")
    command([PG / "pg_ctl", "-D", PGDATA, "start", "-w", "-t", "30", "-l", LOGS / "postgres.log",
             "-o", f"-h 127.0.0.1 -p {port} -k {SOCKETS}"], log="postgres-control.log", timeout=45)
    sql = [PG / "psql", "-X", "-v", "ON_ERROR_STOP=1", "-h", SOCKETS, "-p", port, "-U", "postgres"]
    if not complete.exists():
        command(sql + ["-d", "postgres"], log="postgres-init.log", data=r"""
SELECT 'CREATE DATABASE detent' WHERE NOT EXISTS (SELECT 1 FROM pg_database WHERE datname='detent') \gexec
""")
        command(sql + ["-d", "detent", "-f", SOURCE / "internal/hubserver/postgres/roles.sql"], log="postgres-init.log")
        for role, variable in LOGINS.items():
            password = credentials[variable]
            command(sql + ["-d", "detent"], log="postgres-init.log", data=f"""
DO $$ BEGIN
CREATE ROLE stack_{role} LOGIN;
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;
ALTER ROLE stack_{role} PASSWORD '{password}';
GRANT detent_{role}_v2 TO stack_{role};
""")
        complete.touch()
    else:
        command(sql + ["-d", "detent", "-f", SOURCE / "internal/hubserver/postgres/roles.sql"], log="postgres-init.log")
    command([BINARY, "hub", "migrate-postgres", "--host", "127.0.0.1", "--port", port,
             "--database", "detent", "--user", "stack_migration", "--sslmode", "disable",
             "--password-env", LOGINS["migration"]], SOURCE, credentials, log="migrate.log")


def credentials():
    path = STATE / "secrets.json"
    value = read(path)
    if value is None:
        value = {key: secrets.token_hex(32) for key in [*LOGINS.values(), "DETENT_HUB_ADMIN_TOKEN"]}
        import base64
        value["DETENT_HUB_SECRET_KEYS"] = json.dumps({"1": base64.b64encode(secrets.token_bytes(32)).decode()})
        value["DETENT_HUB_SECRET_KEY_VERSION"] = "1"
        write(path, value)
    return value


def org_bootstrap(admin):
    organizations = request("/api/v2/organizations", token=admin)["items"]
    local = [row for row in organizations if row.get("local")]
    if len(local) != 1:
        raise Failure("Expected the Hub-designated local organization")
    write(STATE / "hub.json", local[0])
    return local[0]["organization_id"]


def runner_enroll(org, admin, capacity, clock):
    path = STATE / "runner.json"
    config = STATE / "global.yaml"
    if read(path):
        return
    grant = request(f"/api/v2/organizations/{org}/runner-enrollments", "POST",
                    {"scope": "organization", "operations": OPERATIONS, "ttl_seconds": 300}, admin)
    record = json.loads(command([BINARY, "hub", "runner", "register", "--enrollment-token-stdin", "--format", "json",
                                 "--url", url(), "--organization", org, "--name", "Local Canary Runner",
                                 "--capacity", str(capacity), "--isolation-tier", "sandbox",
                                 "--config", config, "--workspace-root", WORKSPACES], SOURCE,
                                data=grant["token"] + "\n"))
    raw = config.read_text()
    if clock:
        settings = "".join(f"    {key}: {value}\n" for key, value in clock.items())
        raw = raw.replace("client:\n", "client:\n" + settings, 1)
    raw = re.sub(r"(?m)^github_token:.*$", 'github_token: ""', raw)
    raw = re.sub(r"(?m)^port:.*$", f'port: {available_ports()["runner"]}', raw)
    raw = re.sub(r"(?m)^service_name:.*$", "service_name: detent.runner.local-stack", raw)
    config.write_text(raw + "\nupdate:\n  auto_check_enabled: false\n  auto_apply_enabled: false\n")
    write(path, {field: record[field] for field in ("runner_id", "machine_id")})


def git_env_setup():
    command(["git", "config", "--global", "--replace-all", "protocol.allow", "never"])
    for protocol in ("file", "https"):
        command(["git", "config", "--global", "--replace-all", f"protocol.{protocol}.allow", "always"])
    command(["git", "config", "--global", "user.name", "Local Canary Runner"])
    command(["git", "config", "--global", "user.email", "local-canary@runners.detent.invalid"])
    command(["git", "config", "--global", "commit.gpgsign", "false"])



def start_runner():
    ports = available_ports()
    logfile = LOGS / "runner.log"
    offset = logfile.stat().st_size if logfile.exists() else 0
    spawn("runner", [BINARY, "--config", STATE / "global.yaml", "--host", "127.0.0.1",
                     "--port", ports["runner"], "--headless", "--log-level", "debug"],
          git_transport() | {"DETENT_LOCAL_SOURCE_WORKERS": str(ROOT / "cache/workers"),
                             "DETENT_LOCAL_STARTUP_REFUSAL": str(STATE / "startup-refusal"),
                             "DETENT_LOCAL_GATE_ARMED": str(STATE / "gate.armed.json"),
                             "DETENT_LOCAL_GATE_DRIVER": str(STATE / "replay-gate.py"),
                             "DETENT_LOCAL_GATE_PYTHON": sys.executable})
    def runner_started():
        with logfile.open() as log:
            log.seek(offset)
            return "global config reconciliation completed" in log.read() and running("runner", read(process_state("runner")))
    wait_for(f"Runner startup. See {logfile}", runner_started)


def stop_postgres():
    if (PGDATA / "postmaster.pid").exists():
        command([PG / "pg_ctl", "-D", PGDATA, "stop", "-w", "-t", "30", "-m", "fast"],
                log="postgres-control.log", timeout=40)


def down():
    stop("runner")
    stop("hub")
    stop("git")
    stop_postgres()



def ensure_git_certificate():
    certificate = STATE / "git-cert.pem"
    key = STATE / "git-key.pem"
    if not certificate.exists() or not key.exists():
        command(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "365",
                 "-subj", "/CN=github.com", "-addext", "subjectAltName=DNS:github.com",
                 "-keyout", key, "-out", certificate], log="git-bootstrap.log")


def git_transport():
    return {"HTTPS_PROXY": url("git"), "https_proxy": url("git"),
            "DETENT_LOCAL_GIT_PROXY": url("git"),
            "DETENT_LOCAL_GIT_CERT": str(STATE / "git-cert.pem"),
            "NO_PROXY": "127.0.0.1,localhost,chatgpt.com,openai.com,openai.com.cdn.cloudflare.net,oaiusercontent.com,oaistatic.com",
            }


def git_proxy_ready():
    with CLIENT.open(url("git") + "/health", timeout=2) as response:
        return response.status == 204


class GitProxy(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path != "/health":
            self.send_error(404)
            return
        self.send_response(204)
        self.end_headers()

    def do_CONNECT(self):
        if self.path != "github.com:443":
            self.send_error(403)
            return
        self.send_response(200, "Connection Established")
        self.end_headers()
        self.wfile.flush()
        with self.server.tls.wrap_socket(self.connection, server_side=True) as connection:
            GitHTTP(connection, self.client_address, self.server)


class GitHTTP(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/health":
            self.send_response(204)
            self.end_headers()
            return
        self.git()

    def do_POST(self):
        self.git()

    def git(self):
        from urllib.parse import urlsplit

        endpoint = urlsplit(self.path)
        target = re.fullmatch(r"/([a-z0-9-]+/[a-z0-9-]+?)(?:\.git)?/(info/refs|git-upload-pack|git-receive-pack)", endpoint.path)
        if endpoint.netloc or not target or target[1] not in read(STATE / "repositories.json", []):
            self.send_error(404)
            return
        backend_path = f"/{target[1]}.git/{target[2]}"
        size = int(self.headers.get("Content-Length", "0"))
        if size > 32 * 1024 * 1024:
            self.send_error(413)
            return
        env = BASE_ENV | {
            "GIT_PROJECT_ROOT": str(MIRROR), "GIT_HTTP_EXPORT_ALL": "1",
            "REQUEST_METHOD": self.command, "PATH_INFO": backend_path,
            "QUERY_STRING": endpoint.query, "CONTENT_LENGTH": str(size),
            "CONTENT_TYPE": self.headers.get("Content-Type", ""),
            "HTTP_GIT_PROTOCOL": self.headers.get("Git-Protocol", ""),
            "REMOTE_ADDR": "127.0.0.1", "REMOTE_USER": "local-canary",
        }
        result = subprocess.run([BASE_ENV["DETENT_SMOKE_GIT"], "http-backend"], input=self.rfile.read(size),
                                stdout=subprocess.PIPE, env=env, timeout=90)
        headers, separator, body = result.stdout.partition(b"\r\n\r\n")
        if not separator:
            self.send_error(500)
            return
        headers = dict(line.decode("latin1").split(": ", 1) for line in headers.split(b"\r\n"))
        self.send_response(int(headers.pop("Status", "200").split()[0]))
        for key, value in headers.items():
            self.send_header(key, value)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)



def runner_status(admin, org):
    runner_id = read(STATE / "runner.json")["runner_id"]

    response = request(f"/api/v2/organizations/{org}/runners", token=admin)
    runners = response if isinstance(response, list) else response["runners"]
    return next((row for row in runners if row["runner_id"] == runner_id), None)


def route(projects, admin, org, state="active", capacity=1):
    runner = runner_status(admin, org)
    if not runner:
        raise Failure("Enrolled runner was missing from the fleet")
    project_ids = sorted(projects)
    same_projects = runner.get("scope") == "projects" and sorted(runner.get("project_ids", [])) == project_ids
    if same_projects and runner.get("state") == state and runner.get("capacity_limit") == capacity:
        return runner
    if not same_projects and (runner.get("used") or runner.get("leases")):
        raise Failure("Runner has unsettled leases")
    retained = ("display_name", "tags", "machine_use", "isolation_tier", "host_services", "availability")
    return request(f'/api/v2/organizations/{org}/runners/{runner["runner_id"]}/routing', "PUT",
                   {key: runner[key] for key in retained if key in runner} | {
                       "expected_revision": runner["revision"], "scope": "projects",
                       "project_ids": project_ids, "capacity_limit": capacity, "state": state,
                       "project_capacity_limits": {project: capacity for project in project_ids},
                       "idempotency_key": uuid.uuid4().hex,
                   }, admin)



def provider_sessions(since):
    return sorted(path for folder in (CODEX, SANDBOX_HOME, WORKSPACES)
                  for path in folder.rglob("rollout-*.jsonl") if path.stat().st_mtime >= since)


def current():
    org = read(STATE / "hub.json")["organization_id"]
    return org, credentials()["DETENT_HUB_ADMIN_TOKEN"]


def operator_token():
    return current()[1]


def start_hub():
    cfg = credentials()
    port = available_ports()["hub"]
    app_key = STATE / "uninstalled-app.pem"
    spawn("hub", [BINARY, "hub", "serve", "--hosted-config", STATE / "hub.yaml",
                  "--listen", f"127.0.0.1:{port}", "--log-level", "info"],
          cfg | {"DETENT_HUB_GITHUB_APP_ID": "1", "DETENT_HUB_GITHUB_APP_PRIVATE_KEY": app_key.read_text(),
                 "HTTPS_PROXY": url("git"), "HTTP_PROXY": url("git")})
    admin = cfg["DETENT_HUB_ADMIN_TOKEN"]
    wait_for("Hub readiness", lambda: request("/health", token=admin))


def up(capacity=1, clock=None):
    check_ports()
    for directory in (ROOT / "bin", LOGS, STATE, SANDBOX_HOME, CODEX, SOCKETS, WORKSPACES, ROOT / "tmp"):
        directory.mkdir(exist_ok=True)
    os.link(os.environ["DETENT_SMOKE_BUILD_BINARY"], BINARY)
    shutil.copyfile(Path(__file__).parent / "bin/git", ROOT / "bin/git")
    (ROOT / "bin/git").chmod(0o700)
    shutil.copyfile(os.environ["DETENT_SMOKE_AUTH"], CODEX / "auth.json")

    cfg = credentials()
    database(cfg)
    port = available_ports()["hub"]
    (STATE / "hub.yaml").write_text(f"""postgres:
  host: 127.0.0.1
  port: {available_ports()["postgres"]}
  database: detent
  user: stack_runtime
  password_env: {LOGINS["runtime"]}
  sslmode: disable
  max_connections: 12
  serving_connections: 8
  background_connections: 4
""")
    app_key = STATE / "uninstalled-app.pem"
    command(["openssl", "genrsa", "-out", app_key, "2048"], log="hub-bootstrap.log")
    git_env_setup()
    ensure_git_certificate()
    spawn("git", [sys.executable, __file__, "serve-git"])
    wait_for("loopback Git proxy", git_proxy_ready)
    start_hub()
    admin = cfg["DETENT_HUB_ADMIN_TOKEN"]
    org = org_bootstrap(admin)
    runner_enroll(org, admin, capacity, clock)
    route([], admin, org, capacity=capacity)


if __name__ == "__main__":
    if sys.argv[1:] == ["down"]:
        down()
        sys.exit(0)
    if sys.argv[1:] != ["serve-git"]:
        sys.exit("Expected serve-git or down")
    with ThreadingHTTPServer(("127.0.0.1", available_ports()["git"]), GitProxy) as server:
        server.tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        server.tls.load_cert_chain(STATE / "git-cert.pem", STATE / "git-key.pem")
        server.serve_forever()
