"""Sprite Docker provisioning checks using isolated command fixtures."""

import io
import hashlib
import json
import os
import pathlib
import signal
import shlex
import shutil
import subprocess
import sys
import tarfile
import tempfile
import unittest
from concurrent.futures import ThreadPoolExecutor


PYTHON_LOG = r'''import fcntl, json, os, pathlib, sys
sys.argv = sys.argv[1:]
__file__ = sys.argv[0]
root = pathlib.Path(os.environ["FIXTURE_ROOT"])
name, args = pathlib.Path(sys.argv[0]).name, sys.argv[1:]
record = json.dumps([name, args]) + "\n"
with (root / "calls.jsonl").open("a", encoding="utf-8") as log:
    fcntl.flock(log, fcntl.LOCK_EX)
    log.write(record)
'''

PYTHON_STUB = PYTHON_LOG + r'''
if name == "sha256sum" and args[:1] != ["--check"]:
    import hashlib
    print(hashlib.sha256(pathlib.Path(args[0]).read_bytes()).hexdigest() + "  " + args[0])
elif name == "sha256sum":
    import hashlib
    checksum, filename = sys.stdin.read().split()
    sys.exit(0 if hashlib.sha256(pathlib.Path(filename).read_bytes()).hexdigest() == checksum else 1)
elif name == "npm":
    import shutil
    if os.environ.get("FIXTURE_NPM_POPULATES"):
        npm_cache = pathlib.Path(os.path.join(os.environ["HOME"], ".npm"))
        for entry in ("_cacache", "_logs"):
            (npm_cache / entry).mkdir(parents=True, exist_ok=True)
    prefix = pathlib.Path(args[args.index("--prefix") + 1]) / "bin"
    prefix.mkdir(parents=True, exist_ok=True)
    packages = {"@openai/codex@0.160.0": "codex", "@anthropic-ai/claude-code@2.1.289": "claude", "playwright@1.63.0": "playwright"}
    for package, binary in packages.items():
        if package in args:
            shutil.copy(__file__, prefix / binary)
    if "yaml@2.8.3" in args:
        module = prefix.parent / "lib/node_modules/yaml"
        shutil.copytree(os.environ["FIXTURE_YAML_MODULE"], module, dirs_exist_ok=True)
elif name == "dpkg-query":
    installed = root / (args[-1] + ".version")
    if not installed.exists():
        sys.exit(1)
    if args[0] == "-L":
        print(root / args[-1] / "bin/psql")
        print("/usr/share/doc/postgresql-client/copyright\n" * 4096)
    else:
        print("install ok installed " + installed.read_text())
elif name == "apt-cache":
    candidates = json.loads(os.environ.get("PSQL_CANDIDATES", '{"postgresql-client-17": "17.10-0ubuntu0.25.10.1"}'))
    candidates["docker.io"] = os.environ.get("DOCKER_CANDIDATE", "29.1.3-0ubuntu3~25.10.1")
    print("  Candidate: " + candidates.get(args[-1], "(none)"))
    print("        500 http://archive.ubuntu.com/ubuntu resolute/main amd64 Packages\n" * 4096)
elif name == "apt-get":
    import shutil
    for package, binary in (("bubblewrap", "bwrap"), ("socat", "socat")):
        if package in args:
            shutil.copy(__file__, root / "bin" / binary)
    for arg in args:
        if arg.startswith(("postgresql-client-", "docker.io=")):
            package, version = arg.split("=", 1)
            if package == "docker.io" and version != os.environ.get("DOCKER_CANDIDATE", "29.1.3-0ubuntu3~25.10.1"):
                sys.exit(100)
            if package.startswith("postgresql-client-"):
                candidates = json.loads(os.environ.get("PSQL_CANDIDATES", '{"postgresql-client-17": "17.10-0ubuntu0.25.10.1"}'))
                if candidates.get(package) != version:
                    print(f"E: Version '{version}' for '{package}' was not found", file=sys.stderr)
                    sys.exit(100)
                if os.environ.get("PSQL_INSTALL_FAIL"):
                    print("E: Failed to fetch https://example.invalid/package", file=sys.stderr)
                    sys.exit(100)
                binary = root / package / "bin/psql"
                if not os.environ.get("PSQL_BINARY_MISSING"):
                    binary.parent.mkdir(parents=True, exist_ok=True)
                    binary.write_text('#!/bin/sh\nprintf "psql (PostgreSQL) ' + package.rsplit("-", 1)[1] + '.10\\n"\n')
                    binary.chmod(0o755)
            (root / (package + ".version")).write_text(version)
elif name == "detent":
    import hashlib
    if args[:3] == ["hub", "runner", "register"]:
        tier = "sandbox"
        for index, arg in enumerate(args):
            if arg == "--isolation-tier":
                tier = args[index + 1]
            elif arg.startswith("--isolation-tier="):
                tier = arg.split("=", 1)[1]
        if tier not in ("sandbox", "native-trusted"):
            sys.exit("--isolation-tier must be sandbox or native-trusted")
        with (root / "tokens.txt").open("a") as tokens:
            tokens.write(os.environ.get("DETENT_RUNNER_ENROLLMENT_TOKEN", "") + "\n")
        config = pathlib.Path(args[args.index("--config") + 1])
        config.parent.mkdir(parents=True, exist_ok=True)
        if not config.exists():
            config.write_text("client:\n  hub_url: https://hub.example\nglobal:\n  max_concurrent_agents: 2\n")
            runner_key = hashlib.sha256(str(config).encode()).hexdigest()
            identity = {"schema": 1, "identity": {"runner_id": "runner_" + runner_key, "project_ids": ["prj_example", "prj_second"]}, "credential": "fixture_credential_" + runner_key}
            (config.parent / "identity.json").write_text(json.dumps(identity))
            (root / (identity["identity"]["runner_id"] + ".json")).write_text(json.dumps(identity))
    elif args == ["--format", "pretty", "version"]:
        print("version: 0.117.41\ncommit: " + "a" * 40 + "\nbuild date: unknown")
    else:
        if "--headless" in args:
            config = pathlib.Path(args[args.index("--config") + 1])
            identity = json.loads(config.with_name("identity.json").read_text())
            registered = json.loads((root / (identity["identity"]["runner_id"] + ".json")).read_text())
            if identity != registered:
                sys.exit("runner credential does not match enrollment")
        (root / "detent-cwd").write_text(os.getcwd())
        keys = ("MAKEFLAGS", "TMPDIR", "XDG_CACHE_HOME", "GOMODCACHE", "npm_config_cache", "PLAYWRIGHT_BROWSERS_PATH")
        (root / "detent-env.json").write_text(json.dumps({key: os.environ.get(key) for key in keys}))
        print("v0.117.41")
elif name == "sprite-env":
    if args[:2] == ["services", "get"]:
        sys.exit(0 if (root / ("service-" + args[2] + ".json")).exists() else 1)
    elif args[:2] == ["services", "create"]:
        (root / ("service-" + args[2] + ".json")).write_text(json.dumps(args))
    elif args[:2] == ["checkpoints", "create"]:
        if os.environ.get("CHECKPOINT_FAIL"):
            sys.exit(1)
        print('{"id":"v1"}')

'''

STUB = r'''#!/bin/bash
set -e
name=${0##*/}
status=0
execute=false
case $name in
    curl)
        output=
        previous=
        for arg in "$@"; do
            [[ $previous != --location ]] || url=$arg
            [[ $previous != --output ]] || destination=$arg
            previous=$arg
        done
        if [[ $url == */go.mod ]]; then
            printf 'module fixture\ngo 1.26\ntoolchain go1.26.6\n' > "$destination"
        elif [[ $url == */go1.*.sha256 ]]; then
            cp -- "$FIXTURE_ROOT/go.sha256" "$destination"
        elif [[ $url == *checksums.txt || $url == *.sha256 ]]; then
            case $url in
                */cli/cli/*) prefix=gh_ ;;
                */ripgrep/*) prefix=ripgrep- ;;
                *) prefix=detent_ ;;
            esac
            manifest="$FIXTURE_ROOT/${prefix}checksums.txt"
            [[ ${BAD_CHECKSUM:-} != "$prefix" ]] || manifest="$FIXTURE_ROOT/${prefix}bad-checksums.txt"
            cp -- "$manifest" "$destination"
        else
            archive=${url##*/}
            [[ $url != */go1.* ]] || archive=go.tar.gz
            cp -- "$FIXTURE_ROOT/$archive" "$destination"
            if [[ $url == */sharkdp/fd/* && -n ${FD_CORRUPT:-} ]]; then
                printf 'corrupt archive' > "$destination"
            fi
        fi ;;
    uname) if [[ $1 == -s ]]; then output=Linux; else output=${FIXTURE_ARCH:-x86_64}; fi ;;
    nproc) output=${FIXTURE_NPROC:-8} ;;
    id) if [[ $1 == -u ]]; then output=1000; else output=sprite; fi ;;
    findmnt)
        if [[ $* == *--task* ]]; then
            output="rw,upperdir=${DOCKER_SNAPSHOT_ROOT:-/var/lib/docker/containerd/daemon/io.containerd.snapshotter.v1.overlayfs}/snapshots/1/fs"
        elif [[ $* == *snapshots/1/fs ]]; then output=${DOCKER_SNAPSHOT_FILESYSTEM:-ext4}
        elif [[ $* == *--mountpoint* && $* == */var/lib/docker ]]; then output=${DOCKER_FILESYSTEM:-ext4}
        else output="ext4 ${FIXTURE_LOCAL_DISK:-}"; [[ -n ${FIXTURE_LOCAL_DISK:-} ]] || status=1; fi ;;
    unshare) output=; [[ -n ${DOCKER_SUPPORTED:-} ]] || status=1 ;;
    sudo)
        output=
        if [[ $1 == test && $2 == -w && $3 == /sys/fs/cgroup ]]; then
            [[ -n ${DOCKER_SUPPORTED:-} ]] || status=1
        elif [[ $1 == test && ( ${3:-} == /var/run/docker.sock || ${3:-} == /etc/docker/daemon.json ) ]]; then
            status=1
        elif [[ $1 == find && $2 == /var/lib/docker ]]; then
            if [[ -n ${DOCKER_OPERATOR_LAUNCHER:-} ]]; then
                printf '%s\n' "$DOCKER_OPERATOR_LAUNCHER" > "$HOME/.local/lib/detent-sprite-runner/start-docker"
            fi
            output=${DOCKER_ROOT_ENTRY:-}
        else execute=true; fi ;;
    go)
        output=
        case $1 in
            version) output='go version go1.26.6 linux/amd64' ;;
            env) if [[ $2 == GOCACHE ]]; then output=${GOCACHE:-$HOME/.cache/go-build}; else output=${GOMODCACHE:-$HOME/go/pkg/mod}; fi ;;
            clean) if [[ $2 == -cache ]]; then rm -rf -- "${GOCACHE:-$HOME/.cache/go-build}"; else rm -rf -- "${GOMODCACHE:-$HOME/go/pkg/mod}"; fi ;;
            *) if [[ -n ${GO_INSTALL_FAIL:-} ]]; then status=1; else cp -- "$0" "$GOBIN/detent"; fi ;;
        esac ;;
    npm)
        output=
        if [[ $1 == --version ]]; then output=${FIXTURE_NPM_VERSION:-11.6.2}
        elif [[ $1 == config && $2 == get && $3 == cache ]]; then
            output=${npm_config_cache:-${NPM_CONFIG_CACHE:-${FIXTURE_NPMRC_CACHE:-$HOME/.npm}}}
            [[ $output == /* ]] || output="$PWD/$output"
        elif [[ $1 == cache && $2 == verify ]]; then mkdir -p -- "${npm_config_cache:-$HOME/.npm}/_cacache"
        else ''' + "exec " + shlex.quote(sys.executable) + " -S -c " + shlex.quote(PYTHON_STUB) + r''' "$0" "$@"; fi ;;
    bwrap|socat) output="$name fixture"; [[ ${SANDBOX_TOOL_BROKEN:-} != "$name" ]] || status=1 ;;
    rg|fd)
        if [[ ${0%/*} == "$FIXTURE_ROOT/bin" ]]; then
            output="$name 0.0.0"
            [[ $name != rg ]] || output='ripgrep 0.0.0'
        elif [[ $name == rg ]]; then output='ripgrep 15.2.0'; else output='fd 10.3.0'; fi ;;
    codex|claude|gh)
        if [[ ${!#} == status ]]; then
            output=
            case $name in
                codex) auth=.codex/auth.json ;;
                claude) auth=.claude/.credentials.json ;;
                gh) auth=.config/gh/hosts.yml ;;
            esac
            [[ -f $HOME/$auth ]] || status=1
        else
            case $name in
                codex) output='codex-cli 0.160.0' ;;
                claude) output='2.1.289 (Claude Code)' ;;
                gh) if [[ ${0%/*} == "$FIXTURE_ROOT/bin" ]]; then output='gh version 2.0.0 (fixture)'; else output='gh version 2.101.0 (fixture)'; fi ;;
            esac
        fi ;;
    node)
        output=
        if [[ $1 == --version ]]; then output=${FIXTURE_NODE_VERSION:-v24.11.1}
        elif [[ $1 == -p || ${2:-} == */yaml || ${2:-} == setup-manifest ]]; then execute=true
        elif [[ ! -f $FIXTURE_ROOT/chromium-ready || -n ${BROWSER_FAIL:-} ]]; then status=1
        else printf 'PNG fixture' > "${!#}"; output='Chromium fixture: headless screenshot verified'; fi ;;
    docker)
        output=
        args=("$@")
        if [[ $1 == --host ]]; then args=("${args[@]:2}"); fi
        cid=abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789
        case ${args[0]} in
            context) output=${DOCKER_FIXTURE_ENDPOINT:-unix:///var/run/docker.sock} ;;
            info)
                if [[ ( ! -f $FIXTURE_ROOT/service-detent-docker.json && -z ${EXISTING_DOCKER:-} ) || -n ${OFFLINE_DOCKER:-} ]]; then
                    status=1
                else output='/var/lib/docker overlayfs'; fi ;;
            --version) output='Docker version 29.1.3, build fixture' ;;
            create)
                if [[ ${DOCKER_PROBE_FAILURE:-} == create-rejected ]]; then
                    printf 'daemon rejected create\n' >&2; status=1
                else
                    printf '%s\n' "${args[4]:-}" "${args[6]:-}" > "$FIXTURE_ROOT/probe-container"
                    if [[ ${DOCKER_PROBE_FAILURE:-} == create ]]; then
                        printf 'Docker create response lost\n' >&2; status=124
                    else
                        printf '%s\n' "$cid" > "${args[2]}"
                    fi
                fi ;;
            ps)
                if [[ -f $FIXTURE_ROOT/probe-container ]]; then
                    { read -r probe_name; read -r probe_label; } < "$FIXTURE_ROOT/probe-container"
                    if [[ -n $probe_name && -n $probe_label && ${args[4]} == "name=^/$probe_name\$" && ${args[6]} == "label=$probe_label" ]]; then
                        output=$cid
                    fi
                fi ;;
            start)
                if [[ ${DOCKER_PROBE_FAILURE:-} == start ]]; then
                    printf 'failed to mount overlay: invalid argument\n' >&2; status=125
                fi ;;
            exec)
                if [[ ${DOCKER_PROBE_FAILURE:-} == exec ]]; then
                    printf 'OCI runtime exec failed: failed to open /proc/70/ns/ipc: Permission denied\n' >&2; status=126
                elif [[ ${DOCKER_PROBE_FAILURE:-} == init && ${args[4]:-} == *docker-init* ]]; then
                    status=1
                fi ;;
            inspect) if [[ ${args[2]} == '{{.State.Pid}}' ]]; then output=1234; else output=0; fi ;;
            stop) [[ -f $FIXTURE_ROOT/probe-container ]] || status=1 ;;
            rm) [[ ${!#} == "$cid" ]] && rm "$FIXTURE_ROOT/probe-container" || status=1 ;;
            *) status=2 ;;
        esac ;;
    dockerd) output= ;;
    playwright) if [[ $1 == install ]]; then : > "$FIXTURE_ROOT/chromium-ready"; output=; else output='Version 1.63.0'; fi ;;
    *) ''' + "exec " + shlex.quote(sys.executable) + " -S -c " + shlex.quote(PYTHON_STUB) + r''' "$0" "$@" ;;
esac
''' + shlex.quote(sys.executable) + " -S -c " + shlex.quote(PYTHON_LOG) + r''' "$0" "$@"
if [[ $execute == true ]]; then
    if [[ $name == node ]]; then exec "$FIXTURE_NODE" "$@"; else exec "$@"; fi
fi
[[ -z $output ]] || printf '%s\n' "$output"
exit "$status"
'''


TOKEN = "det_enroll_fixture_secret"
ENROLLMENT = f"detent hub runner register --url https://hub.example/organizations/org_example --token {TOKEN} --name 'Build host' --capacity 2 --isolation-tier native-trusted --service\n"


def run_fixture_command(command, *, input, env):
    with subprocess.Popen(command, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                          stderr=subprocess.PIPE, env=env, text=True,
                          start_new_session=True) as process:
        try:
            stdout, stderr = process.communicate(input, timeout=20)
        except BaseException:
            try:
                os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            process.communicate()
            raise
        return subprocess.CompletedProcess(command, process.returncode, stdout, stderr)


class SpriteBootstrapTests(unittest.TestCase):
    def setUp(self):
        # Worker scratch is mandatory; don't let tempfile select host scratch.
        scratch = os.environ.get("TMPDIR") or os.environ.get("TMP") or os.environ.get("TEMP")
        if not scratch:
            self.fail("set TMPDIR, TMP or TEMP for bootstrap tests")
        self.directory = tempfile.TemporaryDirectory(dir=scratch)
        self.addCleanup(self.directory.cleanup)
        self.root = pathlib.Path(self.directory.name)
        self.home = self.root / "sprite home"
        self.home.mkdir()
        self.fake_bin = self.root / "bin"
        self.fake_bin.mkdir()
        for name in ("uname", "nproc", "findmnt", "curl", "npm", "gh", "rg", "fd", "codex", "claude", "sprite-env", "sha256sum", "id", "sudo", "unshare", "dpkg-query", "apt-cache", "apt-get", "node", "docker", "dockerd", "bwrap", "socat"):
            path = self.fake_bin / name
            path.write_text(STUB)
            path.chmod(0o755)
        archives = [("go.tar.gz", "go/bin/go")]
        for arch, target in (("amd64", "x86_64-unknown-linux-musl"), ("arm64", "aarch64-unknown-linux-musl")):
            archives.extend((
                (f"detent_0.117.41_linux_{arch}.tar.gz", "detent"),
                (f"gh_2.101.0_linux_{arch}.tar.gz", f"gh_2.101.0_linux_{arch}/bin/gh"),
                (f"ripgrep-15.2.0-{target}.tar.gz", f"ripgrep-15.2.0-{target}/rg"),
                (f"fd-v10.3.0-{target}.tar.gz", f"fd-v10.3.0-{target}/fd"),
            ))
        for archive, binary in archives:
            with tarfile.open(self.root / archive, "w:gz") as bundle:
                data = STUB.encode()
                info = tarfile.TarInfo(binary)
                info.size, info.mode = len(data), 0o755
                bundle.addfile(info, io.BytesIO(data))
        (self.root / "go.sha256").write_text(hashlib.sha256((self.root / "go.tar.gz").read_bytes()).hexdigest())
        for prefix in ("gh_", "ripgrep-", "detent_"):
            bundles = list(self.root.glob(prefix + "*.tar.gz"))
            for bad in (False, True):
                manifest = "".join(("0" * 64 if bad else hashlib.sha256(bundle.read_bytes()).hexdigest())
                                   + "  " + bundle.name + "\n" for bundle in bundles)
                (self.root / (prefix + ("bad-" if bad else "") + "checksums.txt")).write_text(manifest)
        self.script = self.root / "sprite-runner-bootstrap.sh"
        shutil.copyfile(pathlib.Path(__file__).with_name(self.script.name), self.script)
        script = self.script.read_text()
        for target, checksum in (
            ("x86_64-unknown-linux-musl", "2b6bfaae8c48f12050813c2ffe1884c61ea26e750d803df9c9114550a314cd14"),
            ("aarch64-unknown-linux-musl", "996b9b1366433b211cb3bbedba91c9dbce2431842144d925428ead0adf32020b"),
        ):
            digest = hashlib.sha256((self.root / f"fd-v10.3.0-{target}.tar.gz").read_bytes()).hexdigest()
            script = script.replace(checksum, digest)
        self.script.write_text(script)
        node = shutil.which("node")
        module = subprocess.run([node, "-p", "require.resolve('yaml/package.json')"],
                                text=True, capture_output=True, timeout=10)
        self.assertEqual(module.returncode, 0, "Run npm ci from the repository root to install the fixture dependencies: " + module.stderr)
        self.env = dict(os.environ, HOME=str(self.home), FIXTURE_ROOT=str(self.root),
                        FIXTURE_NODE=node, FIXTURE_YAML_MODULE=str(pathlib.Path(module.stdout.strip()).parent),
                        PATH=str(self.fake_bin) + os.pathsep + os.environ["PATH"],
                        TMPDIR=str(self.root))
        for key in ("GOPATH", "GOCACHE", "GOMODCACHE", "XDG_CACHE_HOME", "npm_config_cache", "NPM_CONFIG_CACHE",
                    "GH_CONFIG_DIR", "CODEX_HOME", "DOCKER_CONFIG", "DOCKER_CONTEXT", "DOCKER_HOST", "CLAUDE_CONFIG_DIR", "XDG_CONFIG_HOME"):
            self.env.pop(key, None)
        # The image's existing local Codex link cannot be npm's install prefix.
        bundled = self.home / ".local/bin"
        bundled.mkdir(parents=True)
        (bundled / "codex").symlink_to(self.fake_bin / "codex")

    def run_bootstrap(self, extra=(), enrollment=None):
        if enrollment is None:
            enrollment = ENROLLMENT
        return run_fixture_command(["bash", str(self.script), *extra], input=enrollment,
                                   env=self.env)

    def calls(self):
        log = self.root / "calls.jsonl"
        if not log.exists():
            return []
        return [json.loads(line) for line in log.read_text().splitlines()]

    def registrations(self):
        return [args for command, args in self.calls() if command == "detent" and args[:3] == ["hub", "runner", "register"]]

    def test_concurrent_stub_calls_preserve_complete_log_records(self):
        payload = ('path\\component/"quoted"\n\t\x01' * 512)
        calls = [(name, ["trace", str(index), payload])
                 for index in range(8) for name in ("id", "sprite-env")]

        def invoke(call):
            name, args = call
            return run_fixture_command([str(self.fake_bin / name), *args], input="", env=self.env)

        with ThreadPoolExecutor(max_workers=4) as pool:
            for result in pool.map(invoke, calls):
                self.assertEqual(result.returncode, 0, result.stderr)
        self.assertCountEqual(self.calls(), [[name, args] for name, args in calls])

    def test_docker_install_requires_sprite_capabilities(self):
        self.env.update(DOCKER_CANDIDATE="(none)", DOCKER_SUPPORTED="1")
        unavailable = self.run_bootstrap()
        self.assertNotEqual(unavailable.returncode, 0)
        self.assertIn("configured APT repositories provide no Docker package", unavailable.stderr)
        self.assertFalse(any(command in ("curl", "npm", "playwright") for command, _ in self.calls()), unavailable.stdout)
        self.assertFalse(self.registrations())
        self.assertIn("Last completed step: enrollment input and Sprite prerequisites checked", unavailable.stderr)
        self.assertIn("Resume inside this same Sprite:", unavailable.stderr)
        self.assertNotIn(TOKEN, unavailable.stdout + unavailable.stderr)
        self.env.pop("DOCKER_SUPPORTED")
        (self.root / "calls.jsonl").unlink()
        skipped = self.run_bootstrap()
        self.assertEqual(skipped.returncode, 0, skipped.stderr)
        self.assertIn("Docker: skipped:", skipped.stdout)
        self.assertFalse((self.root / "docker.io.version").exists())
        self.assertFalse(any(command == "apt-cache" and args[-1] == "docker.io" for command, args in self.calls()))

        self.env["DOCKER_SUPPORTED"] = "1"
        for candidate in ("29.1.3-0ubuntu3~25.10.1", "29.1.3-0ubuntu4.1"):
            with self.subTest(candidate=candidate):
                self.fresh_sprite()
                self.env["DOCKER_CANDIDATE"] = candidate
                available = self.run_bootstrap()
                self.assertEqual(available.returncode, 0, available.stderr)
                self.assertEqual((self.root / "docker.io.version").read_text(), candidate)
                self.assertIn("PostgreSQL exec and graceful stop verified", available.stdout)
                service = json.loads((self.root / "service-detent-docker.json").read_text())
                launcher = pathlib.Path(service[service.index("--cmd") + 1])
                launched = run_fixture_command(["bash", str(launcher)], input="", env=self.env)
                self.assertEqual(launched.returncode, 0, launched.stderr)
                self.assertIn(["dockerd", ["--data-root", "/var/lib/docker", "--group", "sprite", "--init"]], self.calls())
                self.assertIn(["apt-get", ["install", "-y", "--no-install-recommends", "--allow-downgrades", "docker.io=" + candidate]], self.calls())
                self.assertEqual(self.setup_manifest()["toolchain"]["docker"], candidate)
                identity = self.home / ".config/detent-runner/identity.json"
                enrolled = identity.read_bytes()
                before = len(self.calls())
                rerun = self.run_bootstrap(enrollment="")
                self.assertEqual(rerun.returncode, 0, rerun.stderr)
                self.assertEqual(identity.read_bytes(), enrolled)
                self.assert_preserved_docker(before)
                self.assertFalse((self.root / "probe-container").exists())

    def assert_preserved_docker(self, before):
        for command, args in self.calls()[before:]:
            if command == "apt-get" and args[:1] == ["install"]:
                self.assertFalse(any(arg.startswith("docker.io=") for arg in args))
            if command == "sprite-env":
                self.assertFalse(any("detent-docker" in arg for arg in args) and args[:2] != ["services", "get"])

    def test_docker_probe_preserves_existing_service_and_reports_runtime_failures(self):
        self.env.update(DOCKER_SUPPORTED="1", DOCKER_CANDIDATE="29.1.3-0ubuntu4.1")
        initial = self.run_bootstrap()
        self.assertEqual(initial.returncode, 0, initial.stderr)
        service = self.root / "service-detent-docker.json"
        service.write_text('["operator configuration"]')
        launcher = self.home / ".local/lib/detent-sprite-runner/start-docker"
        launcher.write_text("#!/bin/bash\nexec dockerd --data-root /operator/docker\n")
        identity = self.home / ".config/detent-runner/identity.json"
        paths = (service, launcher, identity, self.root / "docker.io.version",
                 self.root / "unrelated-container", self.root / "unrelated-volume")
        for path in paths[-2:]:
            path.write_text("existing state")
        original = {path: path.read_bytes() for path in paths}
        self.env["DOCKER_CANDIDATE"] = "30.0.0-1"

        for stage, diagnostic, expected in (
            ("create", "Docker create response lost", ["create", "ps", "rm"]),
            ("create-rejected", "daemon rejected create", ["create", "ps"]),
            ("start", "failed to mount overlay: invalid argument", ["create", "start", "rm"]),
            ("exec", "failed to open /proc/70/ns/ipc: Permission denied", ["create", "start", "exec", "rm"]),
            ("", "", ["create", "start", "exec", "stop", "inspect", "rm"]),
        ):
            with self.subTest(stage=stage):
                self.env["DOCKER_PROBE_FAILURE"] = stage
                before = len(self.calls())
                result = self.run_bootstrap(enrollment="")
                if stage:
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn(diagnostic, result.stderr)
                    self.assertNotIn("PostgreSQL readiness timed out", result.stderr)
                    self.assertFalse(any(cmd == "sprite-env" and args[:2] in (["checkpoints", "create"], ["services", "create"])
                                         for cmd, args in self.calls()[before:]))
                else:
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertEqual(self.setup_manifest()["toolchain"]["docker"], "29.1.3-0ubuntu4.1")
                self.assert_preserved_docker(before)
                docker_calls = [args[2:] for cmd, args in self.calls()[before:] if cmd == "docker" and args[0] == "--host"]
                self.assertEqual(docker_calls[0], ["info"])
                probe = [args for args in docker_calls if args[0] not in ("info", "--version")]
                self.assertFalse((self.root / "probe-container").exists())
                self.assertEqual([args[0] for args in probe], expected)
                if expected[-1] == "rm":
                    self.assertEqual(probe[-1][:-1], ["rm", "--volumes", "--force"] if stage else ["rm", "--volumes"])
                self.assertEqual({path: path.read_bytes() for path in paths}, original)
                self.assertNotIn(TOKEN, result.stdout + result.stderr)

        self.env["OFFLINE_DOCKER"] = "1"
        before = len(self.calls())
        offline = self.run_bootstrap(enrollment="")
        self.assertNotEqual(offline.returncode, 0)
        self.assertIn("existing Docker installation is unavailable", offline.stderr)
        self.assert_preserved_docker(before)
        self.assertEqual({path: path.read_bytes() for path in paths}, original)

    def test_docker_probe_uses_the_selected_local_endpoint(self):
        for endpoint, probed in (("unix:///run/operator/docker.sock", True), ("tcp://example.invalid:2376", False)):
            with self.subTest(endpoint=endpoint):
                self.fresh_sprite()
                self.env.update(DOCKER_FIXTURE_ENDPOINT=endpoint, EXISTING_DOCKER="1", DOCKER_SUPPORTED="1")
                result = self.run_bootstrap()
                self.assertEqual(result.returncode, 0, result.stderr)
                calls = [args for cmd, args in self.calls() if cmd == "docker" and args[0] == "--host"]
                if probed:
                    self.assertTrue(any(args[:3] == ["--host", endpoint, "start"] for args in calls))
                    self.assertEqual(self.setup_manifest()["toolchain"]["docker"], "external")
                else:
                    self.assertEqual(calls, [])
                    self.assertIn("Docker endpoint is not a local Unix socket", result.stdout)
                self.assert_preserved_docker(0)

    def test_managed_docker_checks_cover_the_effective_snapshot_mount_and_init_on_retry(self):
        self.env["DOCKER_SUPPORTED"] = "1"
        for setting, value, diagnostic, installs in (
            ("DOCKER_FILESYSTEM", "overlay", "requires an ext4 mount at /var/lib/docker", False),
            ("DOCKER_ROOT_ENTRY", "/var/lib/docker/volumes", "existing Docker installation is unavailable", False),
            ("DOCKER_SNAPSHOT_ROOT", "/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs",
             "snapshot upperdir is outside /var/lib/docker", True),
            ("DOCKER_SNAPSHOT_FILESYSTEM", "overlay", "snapshot upperdir is not on ext4", True),
            ("DOCKER_PROBE_FAILURE", "init", "must start PostgreSQL with root docker-init as PID 1", True),
        ):
            with self.subTest(setting=setting):
                self.fresh_sprite()
                self.env[setting] = value
                for attempt in range(2 if installs else 1):
                    with self.subTest(attempt=attempt + 1):
                        before = len(self.calls())
                        result = self.run_bootstrap()
                        self.assertNotEqual(result.returncode, 0)
                        self.assertIn(diagnostic, result.stderr)
                        self.assertFalse(self.registrations())
                        self.assertFalse(any(cmd == "sprite-env" and args[:2] == ["checkpoints", "create"]
                                             for cmd, args in self.calls()[before:]))
                        self.assertFalse((self.root / "probe-container").exists())
                        if attempt:
                            self.assert_preserved_docker(before)
                self.env.pop(setting)
                self.assertEqual((self.root / "docker.io.version").exists(), installs)

    def test_docker_launcher_created_during_bootstrap_is_preserved(self):
        operator_launcher = "#!/bin/bash\nexec dockerd --data-root /operator/docker\n"
        self.env.update(DOCKER_SUPPORTED="1", DOCKER_OPERATOR_LAUNCHER=operator_launcher.rstrip("\n"))
        result = self.run_bootstrap()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("could not create the Docker launcher", result.stderr)
        launcher = self.home / ".local/lib/detent-sprite-runner/start-docker"
        self.assertEqual(launcher.read_bytes(), operator_launcher.encode())
        self.assertEqual(launcher.stat().st_mode & 0o777, 0o600)
        self.assertFalse((self.root / "service-detent-docker.json").exists())
        self.assertFalse(self.registrations())

    def setup_manifest(self):
        return json.loads((self.home / ".local/lib/detent-sprite-runner/setup-manifest.json").read_text())

    def fresh_sprite(self):
        shutil.rmtree(self.home)
        bundled = self.home / ".local/bin"
        bundled.mkdir(parents=True)
        (bundled / "codex").symlink_to(self.fake_bin / "codex")
        for state in ("calls.jsonl", "tokens.txt", "chromium-ready", "postgresql-client-17.version", "docker.io.version"):
            (self.root / state).unlink(missing_ok=True)
        for service in self.root.glob("service-*.json"):
            service.unlink()
        shutil.rmtree(self.root / "postgresql-client-17", ignore_errors=True)

if __name__ == "__main__":
    unittest.main()
