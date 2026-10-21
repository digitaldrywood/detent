"""Bootstrap boundary tests; no network, package installs or real enrollment."""

import io
import hashlib
import json
import os
import pathlib
import shlex
import shutil
import subprocess
import tarfile
import tempfile
import unittest


STUB = r'''#!/usr/bin/env python3
import hashlib, json, os, pathlib, shutil, subprocess, sys
root = pathlib.Path(os.environ["FIXTURE_ROOT"])
name, args = pathlib.Path(sys.argv[0]).name, sys.argv[1:]
with (root / "calls.jsonl").open("a") as log:
    log.write(json.dumps([name, args]) + "\n")
if name == "uname":
    print("Linux" if args == ["-s"] else os.environ.get("FIXTURE_ARCH", "x86_64"))
elif name == "findmnt":
    if not os.environ.get("FIXTURE_LOCAL_DISK"):
        sys.exit(1)
    print("ext4 " + os.environ["FIXTURE_LOCAL_DISK"])
elif name == "nproc":
    print(os.environ.get("FIXTURE_NPROC", "8"))
elif name == "id":
    print("1000" if args == ["-u"] else "sprite")
elif name == "sudo":
    if args == ["test", "-w", "/sys/fs/cgroup"]:
        sys.exit(0 if os.environ.get("DOCKER_SUPPORTED") else 1)
    sys.exit(subprocess.run(args).returncode)
elif name == "unshare":
    sys.exit(0 if os.environ.get("DOCKER_SUPPORTED") else 1)
elif name == "curl":
    url = args[args.index("--location") + 1]
    dest = pathlib.Path(args[args.index("--output") + 1])
    if url.endswith("/go.mod"):
        dest.write_text("module fixture\ngo 1.26\ntoolchain go1.26.6\n")
    elif "/go1." in url and url.endswith(".sha256"):
        dest.write_text(hashlib.sha256((root / "go.tar.gz").read_bytes()).hexdigest())
    elif url.endswith("checksums.txt") or url.endswith(".sha256"):
        prefix = "gh_" if "/cli/cli/" in url else "ripgrep-" if "/ripgrep/" in url else "detent_"
        dest.write_text("".join(
            ("0" * 64 if os.environ.get("BAD_CHECKSUM") == prefix else hashlib.sha256(p.read_bytes()).hexdigest())
            + "  " + p.name + "\n" for p in root.glob(prefix + "*.tar.gz")))
    else:
        shutil.copyfile(root / ("go.tar.gz" if "/go1." in url else url.rsplit("/", 1)[1]), dest)
        if "/sharkdp/fd/" in url and os.environ.get("FD_CORRUPT"):
            dest.write_bytes(b"corrupt archive")
elif name == "sha256sum" and args[:1] != ["--check"]:
    print(hashlib.sha256(pathlib.Path(args[0]).read_bytes()).hexdigest() + "  " + args[0])
elif name == "sha256sum":
    checksum, filename = sys.stdin.read().split()
    sys.exit(0 if hashlib.sha256(pathlib.Path(filename).read_bytes()).hexdigest() == checksum else 1)
elif name == "go":
    caches = {"GOCACHE": os.environ.get("GOCACHE") or os.path.join(os.environ["HOME"], ".cache/go-build"),
              "GOMODCACHE": os.environ.get("GOMODCACHE") or os.path.join(os.environ["HOME"], "go/pkg/mod")}
    if args == ["version"]:
        print("go version go1.26.6 linux/amd64")
    elif args[:1] == ["env"]:
        print(caches[args[1]])
    elif args[:1] == ["clean"]:
        shutil.rmtree(caches["GOCACHE" if args[1] == "-cache" else "GOMODCACHE"], ignore_errors=True)
    elif os.environ.get("GO_INSTALL_FAIL"):
        sys.exit(1)
    else:
        shutil.copy(__file__, pathlib.Path(os.environ["GOBIN"]) / "detent")
elif name == "npm" and args == ["--version"]:
    print(os.environ.get("FIXTURE_NPM_VERSION", "11.6.2"))
elif name == "npm" and args[:3] == ["config", "get", "cache"]:
    print(os.path.join(os.getcwd(), os.environ.get("npm_config_cache") or os.environ.get("NPM_CONFIG_CACHE") or os.environ.get("FIXTURE_NPMRC_CACHE") or os.path.join(os.environ["HOME"], ".npm")))
elif name == "npm" and args[:2] == ["cache", "verify"]:
    (pathlib.Path(os.environ.get("npm_config_cache") or os.path.join(os.environ["HOME"], ".npm")) / "_cacache").mkdir(parents=True, exist_ok=True)
elif name == "npm":
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
                if candidates.get(package) != version or os.environ.get("PSQL_INSTALL_FAIL"):
                    sys.exit(100)
                binary = root / package / "bin/psql"
                if not os.environ.get("PSQL_BINARY_MISSING"):
                    binary.parent.mkdir(parents=True, exist_ok=True)
                    binary.write_text('#!/bin/sh\nprintf "psql (PostgreSQL) ' + package.rsplit("-", 1)[1] + '.10\\n"\n')
                    binary.chmod(0o755)
            (root / (package + ".version")).write_text(version)
elif name in ("bwrap", "socat"):
    if os.environ.get("SANDBOX_TOOL_BROKEN") == name:
        sys.exit(1)
    print(name + " fixture")
elif name == "node" and args == ["--version"]:
    print(os.environ.get("FIXTURE_NODE_VERSION", "v24.11.1"))
elif name == "node":
    script = sys.stdin.read()
    if args[:1] == ["-p"] or (len(args) > 1 and args[1].endswith("/yaml")) or args[1:2] == ["setup-manifest"]:
        sys.exit(subprocess.run([os.environ["FIXTURE_NODE"], *args], input=script, text=True).returncode)
    if not (root / "chromium-ready").exists() or os.environ.get("BROWSER_FAIL"):
        sys.exit(1)
    pathlib.Path(args[-1]).write_bytes(b"PNG fixture")
    print("Chromium fixture: headless screenshot verified")
elif name == "playwright":
    if args[:1] == ["install"]:
        (root / "chromium-ready").touch()
    else:
        print("Version 1.63.0")
elif name == "docker":
    if args == ["info"]:
        sys.exit(0 if (root / "service-detent-docker.json").exists() else 1)
    print("Docker version 29.1.3, build fixture")
elif name == "rg":
    print("ripgrep 0.0.0" if pathlib.Path(__file__).parent == root / "bin" else "ripgrep 15.2.0")
elif name == "fd":
    print("fd 0.0.0" if pathlib.Path(__file__).parent == root / "bin" else "fd 10.3.0")
elif name == "detent":
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
elif name in ("codex", "claude", "gh"):
    if args[-1:] == ["status"]:
        auth_path = {"codex": ".codex/auth.json", "claude": ".claude/.credentials.json", "gh": ".config/gh/hosts.yml"}[name]
        sys.exit(0 if (pathlib.Path.home() / auth_path).is_file() else 1)
    if name == "gh" and pathlib.Path(__file__).parent == root / "bin":
        print("gh version 2.0.0 (fixture)")
    else:
        print({"codex": "codex-cli 0.160.0", "claude": "2.1.289 (Claude Code)", "gh": "gh version 2.101.0 (fixture)"}[name])
'''

TOKEN = "det_enroll_fixture_secret"
ENROLLMENT = f"detent hub runner register --url https://hub.example/organizations/org_example --token {TOKEN} --name 'Build host' --capacity 2 --isolation-tier native-trusted --service\n"


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
                    "GH_CONFIG_DIR", "CODEX_HOME", "DOCKER_CONFIG", "CLAUDE_CONFIG_DIR", "XDG_CONFIG_HOME"):
            self.env.pop(key, None)
        # The image's existing local Codex link cannot be npm's install prefix.
        bundled = self.home / ".local/bin"
        bundled.mkdir(parents=True)
        (bundled / "codex").symlink_to(self.fake_bin / "codex")

    def run_bootstrap(self, extra=(), enrollment=None):
        if enrollment is None:
            enrollment = ENROLLMENT
        return subprocess.run(["bash", str(self.script), *extra], input=enrollment,
                              env=self.env, text=True, capture_output=True, timeout=20)

    def calls(self):
        log = self.root / "calls.jsonl"
        if not log.exists():
            return []
        return [json.loads(line) for line in log.read_text().splitlines()]

    def registrations(self):
        return [args for command, args in self.calls() if command == "detent" and args[:3] == ["hub", "runner", "register"]]

    def test_sandbox_prerequisites_checked_before_enrollment(self):
        for tool in ("bwrap", "socat"):
            with self.subTest(tool=tool):
                self.env["SANDBOX_TOOL_BROKEN"] = tool
                result = self.run_bootstrap()
                del self.env["SANDBOX_TOOL_BROKEN"]
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("Codex sandbox prerequisite " + tool, result.stderr)
                self.assertEqual(self.registrations(), [])
        result = self.run_bootstrap()
        self.assertEqual(result.returncode, 0, result.stderr)
        calls = self.calls()
        for package in ("bubblewrap", "socat"):
            self.assertTrue(any(name == "apt-get" and package in args for name, args in calls))
        registered = next(i for i, (name, args) in enumerate(calls) if name == "detent" and args[:3] == ["hub", "runner", "register"])
        for tool, flag in (("bwrap", "--version"), ("socat", "-V")):
            self.assertTrue(any(name == tool and args == [flag] for name, args in calls[:registered]))

    def test_rerun_preserves_identity_and_keeps_token_out_of_arguments(self):
        # Catches shell evaluation of the pasted command, the token in an
        # argument list or launcher, host-service installation and duplicate services.
        config = self.home / "custom config" / "global.yaml"
        work = self.home / "custom work"
        enrollment = ENROLLMENT.rstrip("\n") + " \\\n  --name 'Sprite $(touch SHOULD_NOT_EXIST); \"quoted\"'" + \
            f" --config='{config}' --workspace-root \"{work}\"\n"
        first = self.run_bootstrap(enrollment=enrollment)
        self.assertEqual(first.returncode, 0, first.stderr)
        identity = config.with_name("identity.json").read_bytes()
        human_config = "instance_name: human edits\nglobal:\n  io:\n    degraded_max_concurrent_agents: 0\n  cpu:\n    degraded_max_concurrent_agents: 3\n"
        config.write_text(human_config)
        second = self.run_bootstrap(enrollment=enrollment.replace("--isolation-tier native-trusted", "--isolation-tier=native-trusted"))
        self.assertEqual(second.returncode, 0, second.stderr)
        self.assertEqual(config.read_text(), human_config)
        self.assertEqual(config.with_name("identity.json").read_bytes(), identity)
        for project in ("prj_example", "prj_second"):
            self.assertIn(str(work / project), first.stderr)
        self.assertFalse(list(self.root.rglob("SHOULD_NOT_EXIST")))
        registrations = self.registrations()
        self.assertEqual(len(registrations), 2)
        self.assertEqual(registrations[0][registrations[0].index("--isolation-tier") + 1], "native-trusted")
        self.assertIn("--isolation-tier=native-trusted", registrations[1])
        for args in registrations:
            self.assertNotIn("--service", args)
            self.assertEqual(args[len(args) - 1 - args[::-1].index("--name") + 1], 'Sprite $(touch SHOULD_NOT_EXIST); "quoted"')
            self.assertEqual(args[args.index("--config") + 1], str(config))
        self.assertEqual((self.root / "tokens.txt").read_text().split(), [TOKEN, TOKEN])
        for _, args in self.calls():
            self.assertFalse(any(TOKEN in arg for arg in args))
        self.assertNotIn(TOKEN, first.stdout + first.stderr + second.stdout + second.stderr)
        services = [args for command, args in self.calls() if command == "sprite-env" and args[:2] == ["services", "create"]]
        self.assertEqual([args[2] for args in services], ["detent-runner", "detent-runner"])
        restarts = [args for command, args in self.calls() if command == "sprite-env" and args[:1] == ["curl"]]
        self.assertEqual(restarts, [["curl", "-X", "POST", "http://sprite/v1/services/detent-runner/restart?duration=1s"]])
        launcher = self.home / ".local/lib/detent-sprite-runner/start"
        self.assertNotIn(TOKEN, launcher.read_text())
        service = subprocess.run(["bash", str(launcher)], env=self.env, text=True, capture_output=True, timeout=20)
        self.assertEqual(service.returncode, 0, service.stderr)
        self.assertEqual((self.root / "detent-cwd").read_text(), str(work.resolve()))
        launched = json.loads((self.root / "detent-env.json").read_text())
        self.assertEqual(launched["MAKEFLAGS"], "-l2")
        self.assertIsNone(launched["XDG_CACHE_HOME"])
        self.assertIn("Sprite resource profile: 8 CPUs; runner Make load limit -l2", first.stdout)
        self.assertIn(["detent", ["--config", str(config), "--headless"]], self.calls())
        self.assertIn("claude auth login", first.stdout)
        self.assertIn("codex login", first.stdout)
        self.assertFalse(list(self.root.glob("sprite-bootstrap.*")))
        downloads = [args for command, args in self.calls() if command == "curl"]
        self.assertTrue(any("/v0.117.41/" in " ".join(args) for args in downloads))
        self.assertFalse(any(command == "go" and args[:1] == ["install"] for command, args in self.calls()))
        for flag in ("--isolation-tier sandbox", "--isolation-tier=sandbox"):
            with self.subTest(flag=flag):
                result = self.run_bootstrap(enrollment=enrollment.replace("--isolation-tier native-trusted", flag))
                self.assertEqual(result.returncode, 0, result.stderr)
                args = self.registrations()[-1]
                if "=" in flag:
                    self.assertIn(flag, args)
                else:
                    self.assertEqual(args[args.index("--isolation-tier") + 1], "sandbox")
                self.assertNotIn(TOKEN, result.stdout + result.stderr + str(self.calls()))

    def test_release_install_records_install_lock(self):
        # Catches a release install the updater reports as an unknown source.
        lock = self.home / ".detent/install.lock"
        binary = self.home / ".local/bin/detent"
        first = self.run_bootstrap()
        self.assertEqual(first.returncode, 0, first.stderr)
        fields = dict(line.split("=", 1) for line in lock.read_text().splitlines())
        self.assertEqual(set(fields), {"binary", "version", "commit", "installed_at", "sha256"})
        self.assertEqual(fields["binary"], str(binary))
        self.assertEqual(fields["version"], "0.117.41")
        self.assertEqual(fields["commit"], "a" * 40)
        self.assertEqual(fields["sha256"], hashlib.sha256(binary.read_bytes()).hexdigest())
        self.assertRegex(fields["installed_at"], r"^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$")
        lock.unlink()
        (self.root / "calls.jsonl").unlink()
        repair = self.run_bootstrap(enrollment="")
        self.assertEqual(repair.returncode, 0, repair.stderr)
        self.assertTrue(any("/v0.117.41/" in " ".join(args) for command, args in self.calls() if command == "curl"))
        self.assertIn("binary=" + str(binary), lock.read_text().splitlines())
        source = self.root / "source checkout"
        source.mkdir()
        (source / "go.mod").write_text("module fixture\ngo 1.26.6\n")
        self.env["GO_INSTALL_FAIL"] = "1"
        failed = self.run_bootstrap(extra=("--from-source", str(source)), enrollment="")
        del self.env["GO_INSTALL_FAIL"]
        self.assertNotEqual(failed.returncode, 0)
        self.assertIn("binary=" + str(binary), lock.read_text().splitlines())
        built = self.run_bootstrap(extra=("--from-source", str(source)), enrollment="")
        self.assertEqual(built.returncode, 0, built.stderr)
        self.assertFalse(lock.exists())

    def test_pressure_floor_config_merge(self):
        cases = (
            ("fresh", None, {"client": {"hub_url": "https://hub.example"}, "global": {"max_concurrent_agents": 2}}, 1, 1),
            ("retained workspace", f"workspace_root: {self.home / 'retained root'}\n", {"workspace_root": str(self.home / "retained root")}, 1, 1),
            ("no global", "client: {hub_url: https://hub.example}\n", {"client": {"hub_url": "https://hub.example"}}, 1, 1),
            ("missing keys", "# operator config\nclient: {capacity: 4}\nglobal:\n  max_concurrent_agents: 4\n  io:\n    pressure_full_avg10_threshold: 8\n  cpu:\n    pressure_some_avg10_threshold: 60\n",
             {"client": {"capacity": 4}, "global": {"max_concurrent_agents": 4, "io": {"pressure_full_avg10_threshold": 8}, "cpu": {"pressure_some_avg10_threshold": 60}}}, 1, 1),
            ("partial floor", "global: {io: {degraded_max_concurrent_agents: 0}}\n", {"global": {}}, 0, 1),
            ("operator floors", "# keep these floors\nglobal:\n  io: {degraded_max_concurrent_agents: 0}\n  cpu: {degraded_max_concurrent_agents: 3}\nprojects: []\n", {"global": {}, "projects": []}, 0, 3),
            ("null sections", "global:\n  io:\n  cpu:\n", {"global": {}}, 1, 1),
        )
        for name, original, preserved, io_floor, cpu_floor in cases:
            with self.subTest(name=name):
                config = self.home / name / "global.yaml"
                config.parent.mkdir()
                identity = config.with_name("identity.json")
                if original is not None:
                    config.write_text(original)
                    config.chmod(0o640)
                    identity.write_text(json.dumps({"schema": 1, "identity": {"runner_id": "runner_operator", "project_ids": []}, "credential": "operator_fixture_credential"}))
                result = self.run_bootstrap(extra=("--config", str(config)), enrollment="" if original is not None else None)
                self.assertEqual(result.returncode, 0, result.stderr)
                service = json.loads((self.root / "service-detent-runner.json").read_text())
                self.assertEqual(service[service.index("--dir") + 1], str(self.home))
                launch = preserved.get("workspace_root", str(self.home / "detent-runner"))
                self.assertIn(["cd", "--", launch], [shlex.split(line) for line in (self.home / ".local/lib/detent-sprite-runner/start").read_text().splitlines()])
                self.assertNotIn("operator_fixture_credential", result.stdout + result.stderr)
                parsed = subprocess.run([self.env["FIXTURE_NODE"], "-e",
                                         "const fs = require('node:fs'); const YAML = require(process.argv[1]); console.log(JSON.stringify(YAML.parse(fs.readFileSync(process.argv[2], 'utf8'))));",
                                         self.env["FIXTURE_YAML_MODULE"], str(config)],
                                        text=True, capture_output=True, timeout=10)
                self.assertEqual(parsed.returncode, 0, parsed.stderr)
                expected = json.loads(json.dumps(preserved))
                pressure = expected.setdefault("global", {})
                pressure.setdefault("io", {})["degraded_max_concurrent_agents"] = io_floor
                pressure.setdefault("cpu", {})["degraded_max_concurrent_agents"] = cpu_floor
                self.assertEqual(json.loads(parsed.stdout), expected)
                self.assertIn(f"Sprite pressure capacity: io={io_floor}, cpu={cpu_floor}", result.stdout)
                if original is not None:
                    self.assertEqual(json.loads(identity.read_text())["identity"], {"runner_id": "runner_operator", "project_ids": []})
                    self.assertEqual(config.stat().st_mode & 0o777, 0o640)
                    for line in original.splitlines():
                        if line.startswith("#"):
                            self.assertIn(line, config.read_text())
                if name == "operator floors":
                    self.assertEqual(config.read_text(), original)
                merged = config.read_bytes()
                rerun = self.run_bootstrap(extra=("--config", str(config)), enrollment="")
                self.assertEqual(rerun.returncode, 0, rerun.stderr)
                self.assertEqual(config.read_bytes(), merged)

    def test_configured_workspace_root_uses_runner_path_expansion(self):
        default = self.home / "detent-runner"
        cases = (
            ("home relative", "~/retained root", self.home / "retained root", self.home / "retained root"),
            ("home", "~", self.home, self.home),
            ("working directory relative", "relative root", default / "relative root", default),
        )
        for name, configured, root, launch_dir in cases:
            with self.subTest(name=name):
                config = self.home / name / "global.yaml"
                config.parent.mkdir()
                config.write_text(json.dumps({"workspace_root": configured}) + "\n")
                config.with_name("identity.json").write_text(json.dumps({"schema": 1, "identity": {"runner_id": "runner_operator", "project_ids": ["prj_example"]}, "credential": "operator_fixture_credential"}))
                result = self.run_bootstrap(extra=("--config", str(config)), enrollment="")
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn(f"Managed checkout root: {root}\n", result.stdout)
                self.assertIn(f"New managed checkout for prj_example: {root / 'prj_example'}", result.stderr)
                service = json.loads((self.root / "service-detent-runner.json").read_text())
                self.assertEqual(service[service.index("--dir") + 1], str(self.home))
                launcher = (self.home / ".local/lib/detent-sprite-runner/start").read_text()
                self.assertIn(["cd", "--", str(launch_dir)], [shlex.split(line) for line in launcher.splitlines()])
                self.assertTrue((root / ".tmp").is_dir())
                self.assertNotIn("operator_fixture_credential", result.stdout + result.stderr)

    def test_local_disk_hosts_checkouts_and_caches(self):
        # Catches checkouts or caches left on the persistent network disk, and
        # migrations that override an operator-chosen workspace root.
        disk = self.root / "local disk"
        disk.mkdir()
        self.env["FIXTURE_LOCAL_DISK"] = str(disk)
        self.env["FIXTURE_NPM_POPULATES"] = "1"
        (self.home / ".npm/_logs").mkdir(parents=True)
        local = disk / "detent-runner"
        first = self.run_bootstrap()
        self.assertEqual(first.returncode, 0, first.stderr)
        args = self.registrations()[0]
        self.assertEqual(args[args.index("--workspace-root") + 1], str(local / "work"))
        launcher = self.home / ".local/lib/detent-sprite-runner/start"
        service = subprocess.run(["bash", str(launcher)], env=self.env, text=True, capture_output=True, timeout=20)
        self.assertEqual(service.returncode, 0, service.stderr)
        self.assertEqual((self.root / "detent-cwd").read_text(), str((local / "work").resolve()))
        self.assertEqual(json.loads((self.root / "detent-env.json").read_text()), {
            "MAKEFLAGS": "-l2",
            "TMPDIR": str(local / "work/.tmp"),
            "XDG_CACHE_HOME": None,
            "GOMODCACHE": None,
            "npm_config_cache": None,
            "PLAYWRIGHT_BROWSERS_PATH": None,
        })
        native = {
            self.home / ".cache/go-build": local / "cache/go-build",
            self.home / ".cache/detent": local / "cache/detent",
            self.home / ".cache/golangci-lint": local / "cache/golangci-lint",
            self.home / "go/pkg/mod": local / "cache/go-mod",
            self.home / ".npm": local / "cache/npm",
        }
        for link, target in native.items():
            self.assertTrue(link.is_symlink(), link)
            self.assertEqual(pathlib.Path(os.readlink(link)), target)
        self.assertTrue((local / "cache/npm/_cacache").is_dir())
        self.assertFalse((self.home / ".cache/ms-playwright").is_symlink())
        self.assertIn(f"Local disk: checkouts in {local / 'work'}; native caches linked into {local / 'cache'}", first.stdout)
        shutil.rmtree(local)
        (self.root / "detent-cwd").unlink()
        lost = subprocess.run(["bash", str(launcher)], env=self.env, text=True, capture_output=True, timeout=20)
        self.assertEqual(lost.returncode, 0, lost.stderr)
        self.assertEqual((self.root / "detent-cwd").read_text(), str((local / "work").resolve()))
        for link in native:
            self.assertTrue(link.resolve().is_dir(), link)
        shutil.rmtree(local)
        recovered = self.run_bootstrap(enrollment="")
        self.assertEqual(recovered.returncode, 0, recovered.stderr)
        for link in native:
            self.assertTrue(link.resolve().is_dir(), link)
        for link in (self.home / "go/pkg/mod", self.home / ".npm"):
            link.unlink()
        (self.home / ".npm").mkdir()
        (self.home / ".npm/_cacache").mkdir()
        self.env.update(GOMODCACHE=str(self.root / "operator-modules"))
        operator = self.run_bootstrap(enrollment="")
        del self.env["GOMODCACHE"]
        self.assertEqual(operator.returncode, 0, operator.stderr)
        self.assertFalse((self.home / "go/pkg/mod").exists())
        self.assertFalse((self.home / ".npm").is_symlink())
        self.assertIn(f"Native cache {self.home / '.npm'} already has content", operator.stderr)
        shutil.rmtree(self.home / ".npm")
        (self.home / ".npmrc").write_text("cache = /operator/npm\n")
        npmrc = self.run_bootstrap(enrollment="")
        self.assertEqual(npmrc.returncode, 0, npmrc.stderr)
        self.assertFalse((self.home / ".npm").is_symlink())
        (self.home / ".npmrc").unlink()
        self.assertNotIn("GOMODCACHE", launcher.read_text())
        (self.home / ".config/detent-runner/global.yaml").write_text("workspace_root: custom-relative\n")
        relative = self.run_bootstrap(enrollment="")
        self.assertEqual(relative.returncode, 0, relative.stderr)
        self.assertIn(f"Managed checkout root: {self.home / 'detent-runner' / 'custom-relative'}\n", relative.stdout)
        config = self.home / ".config/detent-runner/global.yaml"
        for name, root, want in (
            ("legacy default", self.home / "detent-runner", local / "work"),
            ("operator root", self.home / "chosen", self.home / "chosen"),
        ):
            with self.subTest(name=name):
                config.write_text(f"# operator config\nworkspace_root: {root}\n")
                rerun = self.run_bootstrap(enrollment="")
                self.assertEqual(rerun.returncode, 0, rerun.stderr)
                parsed = subprocess.run([self.env["FIXTURE_NODE"], "-e",
                                         "const fs = require('node:fs'); const YAML = require(process.argv[1]); console.log(YAML.parse(fs.readFileSync(process.argv[2], 'utf8')).workspace_root);",
                                         self.env["FIXTURE_YAML_MODULE"], str(config)],
                                        text=True, capture_output=True, timeout=10)
                self.assertEqual(parsed.stdout.strip(), str(want), parsed.stderr)
                self.assertIn("# operator config", config.read_text())
                self.assertIn("cd -- " + str(want).replace(" ", "\\ "), launcher.read_text())

    def test_empty_input_reuses_an_existing_registration_only(self):
        # Catches reruns that need a fresh token, and a fresh Sprite that
        # silently starts an unregistered service.
        fresh = self.run_bootstrap(enrollment="")
        self.assertNotEqual(fresh.returncode, 0)
        self.assertIn("not registered yet", fresh.stderr)
        self.assertEqual(self.calls(), [])
        first = self.run_bootstrap()
        self.assertEqual(first.returncode, 0, first.stderr)
        for version in ("gh version 2.101.0", "ripgrep 15.2.0", "fd 10.3.0", "psql (PostgreSQL) 17.10", "Version 1.63.0"):
            self.assertIn(version, first.stdout)
        self.assertIn(["playwright", ["install", "--with-deps", "chromium"]], self.calls())
        npm_installs = [args for command, args in self.calls() if command == "npm" and args[:1] == ["install"]]
        self.assertEqual(len(npm_installs), 1)
        self.assertTrue(set(("@openai/codex@0.160.0", "@anthropic-ai/claude-code@2.1.289", "playwright@1.63.0")).issubset(npm_installs[0]))
        before = len(self.calls())
        rerun = self.run_bootstrap(enrollment="")
        self.assertEqual(rerun.returncode, 0, rerun.stderr)
        self.assertEqual(len(self.registrations()), 1)
        restarts = [args for command, args in self.calls() if command == "sprite-env" and args[:1] == ["curl"]]
        self.assertEqual(len(restarts), 1)
        rerun_calls = self.calls()[before:]
        self.assertFalse(any(command == "curl" or command in ("npm", "apt-get") and args[:1] == ["install"] for command, args in rerun_calls))
        self.assertFalse(any(command == "playwright" and args[:1] == ["install"] for command, args in rerun_calls))
        self.assertIn("headless screenshot verified", rerun.stdout)
        self.assertIn("Docker: skipped:", rerun.stdout)

    def test_rejects_shell_syntax_and_foreign_commands(self):
        # Catches pasted input that would need a shell to mean what it says.
        for enrollment in (
            ENROLLMENT.rstrip("\n") + " --name $(id)\n",
            ENROLLMENT.rstrip("\n") + "; rm -rf /\n",
            ENROLLMENT.rstrip("\n") + " --name \"`id`\"\n",
            ENROLLMENT.rstrip("\n") + " --name 'open\n",
            ENROLLMENT.replace("--token ", ""),
            ENROLLMENT.rstrip("\n") + " --isolation-tier\n",
            ENROLLMENT.rstrip("\n") + " --unknown=value\n",
            ENROLLMENT.rstrip("\n") + " --unknown value\n",
            "detent start --yes\n",
            "detent hub runner register --url https://hub.example/organizations/org_example --service\n",
        ):
            with self.subTest(enrollment=enrollment):
                result = self.run_bootstrap(enrollment=enrollment)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(self.calls(), [])
                self.assertNotIn(TOKEN, result.stderr)
        for flag in ("--isolation-tier 'root'", "--isolation-tier='$(touch SHOULD_NOT_EXIST)'", "--isolation-tier="):
            with self.subTest(flag=flag):
                result = self.run_bootstrap(enrollment=ENROLLMENT.replace("--isolation-tier native-trusted", flag))
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("--isolation-tier must be sandbox or native-trusted", result.stderr)
                args = self.registrations()[-1]
                if "=" in flag:
                    self.assertIn(flag.replace("'", ""), args)
                else:
                    self.assertEqual(args[args.index("--isolation-tier") + 1], "root")
                self.assertFalse(list(self.root.rglob("SHOULD_NOT_EXIST")))
                self.assertNotIn(TOKEN, result.stdout + result.stderr + str(self.calls()))

    def test_source_install_requires_explicit_opt_in(self):
        # Catches building from a checkout during a release bootstrap, and
        # ignoring the source checkout's toolchain when explicitly requested.
        source = self.root / "source checkout"
        source.mkdir()
        (source / "go.mod").write_text("module fixture\ngo 1.26.6\n")
        result = self.run_bootstrap(extra=("--from-source", str(source)))
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn(["go", ["install", "./cmd/detent"]], self.calls())
        downloads = [args for command, args in self.calls() if command == "curl"]
        self.assertFalse(any("digitaldrywood/detent/releases/download" in " ".join(args) for args in downloads))

    def test_failures_do_not_claim_a_clean_checkpoint(self):
        # A corrupt release must never enroll; a failed checkpoint must return
        # failure while leaving already-enrolled state available for retry.
        for mode, value, expected_registration in (("BAD_CHECKSUM", "gh_", False), ("BAD_CHECKSUM", "ripgrep-", False), ("FD_CORRUPT", "1", False), ("BAD_CHECKSUM", "detent_", False), ("CHECKPOINT_FAIL", "1", True), ("BROWSER_FAIL", "1", False)):
            with self.subTest(mode=mode):
                (self.root / "calls.jsonl").unlink(missing_ok=True)
                self.env[mode] = value
                result = self.run_bootstrap()
                del self.env[mode]
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(bool(self.registrations()), expected_registration)
                self.assertNotIn("Restore this same Sprite:", result.stdout)
                self.assertIn("Last completed step:", result.stderr)
                self.assertIn("Resume inside this same Sprite:", result.stderr)
                self.assertNotIn(TOKEN, result.stdout + result.stderr)
                if mode == "CHECKPOINT_FAIL":
                    original_home = self.env["HOME"]
                    identities = []
                    try:
                        for home in (self.home, self.root / "second sprite home"):
                            with self.subTest(home=home.name):
                                self.env["HOME"] = str(home)
                                auth_files = {}
                                for relative in (".codex/auth.json", ".claude/.credentials.json", ".config/gh/hosts.yml"):
                                    path = home / relative
                                    path.parent.mkdir(parents=True, exist_ok=True)
                                    auth_files[path] = (home.name + ": " + relative).encode()
                                    path.write_bytes(auth_files[path])
                                    path.chmod(0o600)
                                self.env[mode] = value
                                failed = self.run_bootstrap(enrollment="" if home == self.home else ENROLLMENT)
                                del self.env[mode]
                                self.assertNotEqual(failed.returncode, 0)
                                self.assertIn("runner Service configured; verify running state", failed.stderr)
                                config = home / ".config/detent-runner/global.yaml"
                                identity = config.with_name("identity.json").read_bytes()
                                original = config.read_bytes()
                                identities.append(json.loads(identity))
                                (self.root / "calls.jsonl").unlink()
                                resumed = self.run_bootstrap(enrollment="")
                                self.assertEqual(resumed.returncode, 0, resumed.stderr)
                                self.assertFalse(self.registrations())
                                self.assertEqual(config.read_bytes(), original)
                                self.assertEqual(config.with_name("identity.json").read_bytes(), identity)
                                for path, contents in auth_files.items():
                                    self.assertEqual(path.read_bytes(), contents)
                                    self.assertEqual(path.stat().st_mode & 0o777, 0o600)
                                for command in ("codex login", "claude auth login", "gh auth login"):
                                    self.assertNotIn(command, resumed.stdout)
                                self.assertIn("Last completed step: installation checkpoint created", resumed.stdout)
                                launcher = home / ".local/lib/detent-sprite-runner/start"
                                service = subprocess.run(["bash", str(launcher)], env=self.env, text=True, capture_output=True, timeout=20)
                                self.assertEqual(service.returncode, 0, service.stderr)
                        self.assertNotEqual(identities[0]["identity"]["runner_id"], identities[1]["identity"]["runner_id"])
                        self.assertNotEqual(identities[0]["credential"], identities[1]["credential"])
                        for home in (self.home, self.root / "second sprite home"):
                            self.env["HOME"] = str(home)
                            service = subprocess.run(["bash", str(home / ".local/lib/detent-sprite-runner/start")], env=self.env, text=True, capture_output=True, timeout=20)
                            self.assertEqual(service.returncode, 0, service.stderr)
                    finally:
                        self.env["HOME"] = original_home
                        self.env.pop(mode, None)

    def test_updates_outdated_tools_and_repairs_browser(self):
        self.env["FIXTURE_ARCH"] = "aarch64"
        first = self.run_bootstrap()
        self.assertEqual(first.returncode, 0, first.stderr)
        for name in ("gh", "rg", "fd"):
            (self.home / ".local/bin" / name).write_text("#!/bin/sh\nprintf 'old version\\n'\n")
        for name in ("codex", "claude", "playwright"):
            (self.home / ".local/share/detent-runner/npm/bin" / name).write_text("#!/bin/sh\nprintf 'old version\\n'\n")
        (self.root / "postgresql-client-17.version").write_text("old")
        (self.root / "chromium-ready").unlink()
        (self.root / "calls.jsonl").unlink()
        update = self.run_bootstrap(enrollment="")
        self.assertEqual(update.returncode, 0, update.stderr)
        self.assertTrue(any("linux_arm64.tar.gz" in " ".join(args) for command, args in self.calls() if command == "curl"))
        self.assertTrue(any("aarch64-unknown-linux-musl" in " ".join(args) for command, args in self.calls() if command == "curl"))
        self.assertIn(["apt-get", ["install", "-y", "--no-install-recommends", "--allow-downgrades", "--reinstall", "postgresql-client-17=17.10-0ubuntu0.25.10.1"]], self.calls())
        self.assertIn(["playwright", ["install", "--with-deps", "chromium"]], self.calls())
        self.assertIn("headless screenshot verified", update.stdout)

    def test_docker_install_requires_sprite_capabilities(self):
        self.env["DOCKER_CANDIDATE"] = "(none)"
        self.env["DOCKER_SUPPORTED"] = "1"
        unavailable = self.run_bootstrap()
        self.assertNotEqual(unavailable.returncode, 0)
        self.assertFalse(any(command in ("curl", "npm", "playwright") for command, _ in self.calls()), unavailable.stdout)
        self.assertFalse(self.registrations())
        self.assertIn("Last completed step: enrollment input and Sprite prerequisites checked", unavailable.stderr)
        self.assertIn("Resume inside this same Sprite:", unavailable.stderr)
        self.assertNotIn(TOKEN, unavailable.stdout + unavailable.stderr)
        self.env.pop("DOCKER_SUPPORTED")
        (self.root / "calls.jsonl").unlink()
        skipped = self.run_bootstrap()
        self.assertEqual(skipped.returncode, 0, skipped.stderr)
        self.assertFalse((self.root / "docker.io.version").exists())
        self.assertFalse(any(command == "apt-cache" and args[-1] == "docker.io" for command, args in self.calls()))
        self.env["DOCKER_SUPPORTED"] = "1"
        for candidate in ("29.1.3-0ubuntu3~25.10.1", "29.1.3-0ubuntu4.1"):
            with self.subTest(candidate=candidate):
                self.env["DOCKER_CANDIDATE"] = candidate
                available = self.run_bootstrap(enrollment="")
                self.assertEqual(available.returncode, 0, available.stderr)
                self.assertEqual((self.root / "docker.io.version").read_text(), candidate)
                self.assertTrue((self.root / "service-detent-docker.json").exists())
                self.assertIn("Docker version 29.1.3", available.stdout)
                self.assertIn(["apt-get", ["install", "-y", "--no-install-recommends", "--allow-downgrades", "docker.io=" + candidate]], self.calls())
                before = len(self.calls())
                rerun = self.run_bootstrap(enrollment="")
                self.assertEqual(rerun.returncode, 0, rerun.stderr)
                self.assertFalse(any(command == "apt-get" and args[:1] == ["install"] for command, args in self.calls()[before:]))
                self.assertFalse(any(command == "sprite-env" and args[:3] == ["services", "create", "detent-docker"] for command, args in self.calls()[before:]))
        self.env["DOCKER_CANDIDATE"] = "(none)"
        (self.root / "docker.io.version").unlink()
        (self.root / "service-detent-docker.json").unlink()
        before = len(self.calls())
        registrations = len(self.registrations())
        unavailable = self.run_bootstrap()
        self.assertNotEqual(unavailable.returncode, 0)
        self.assertIn("configured APT repositories provide no Docker package", unavailable.stderr)
        self.assertFalse((self.root / "service-detent-docker.json").exists())
        self.assertEqual(len(self.registrations()), registrations)
        self.assertNotIn(TOKEN, unavailable.stdout + unavailable.stderr)
        self.assertFalse(any(command == "apt-get" and args[:1] == ["install"] for command, args in self.calls()[before:]))

    def test_postgresql_repository_candidates_and_partial_installation(self):
        cases = (
            ("older image", {"postgresql-client-17": "17.10-0ubuntu0.25.10.1"}, "17", None),
            ("updated revision", {"postgresql-client-17": "17.11-0ubuntu0.25.10.2"}, "17", None),
            ("resolute", {"postgresql-client-18": "18.6-0ubuntu0.26.04.1"}, "18", None),
            ("prefer newer client", {"postgresql-client-17": "17.10", "postgresql-client-18": "18.6"}, "18", None),
            ("no compatible package", {}, None, "configured APT repositories"),
            ("install fails", {"postgresql-client-18": "18.6"}, None, "PSQL_INSTALL_FAIL"),
            ("package lacks executable", {"postgresql-client-18": "18.6"}, None, "PSQL_BINARY_MISSING"),
        )
        link = self.home / ".local/bin/psql"
        for name, candidates, major, failure in cases:
            with self.subTest(name=name):
                for package in ("postgresql-client-17", "postgresql-client-18"):
                    (self.root / (package + ".version")).unlink(missing_ok=True)
                    shutil.rmtree(self.root / package, ignore_errors=True)
                link.unlink(missing_ok=True)
                link.symlink_to(self.root / "absent/psql")
                self.env["PSQL_CANDIDATES"] = json.dumps(candidates)
                for flag in ("PSQL_INSTALL_FAIL", "PSQL_BINARY_MISSING"):
                    self.env.pop(flag, None)
                if failure and failure.startswith("PSQL_"):
                    self.env[failure] = "1"
                before = len(self.registrations())
                result = self.run_bootstrap()
                self.assertNotIn(TOKEN, result.stdout + result.stderr)
                if failure:
                    self.assertNotEqual(result.returncode, 0)
                    if failure == "configured APT repositories":
                        self.assertTrue(link.is_symlink())
                    else:
                        self.assertFalse(link.is_symlink())
                    self.assertEqual(len(self.registrations()), before)
                    if failure == "configured APT repositories":
                        self.assertIn(failure, result.stderr)
                    continue
                self.assertEqual(result.returncode, 0, result.stderr)
                package = "postgresql-client-" + major
                binary = self.root / package / "bin/psql"
                self.assertEqual(link.resolve(), binary)
                self.assertEqual((self.root / (package + ".version")).read_text(), candidates[package])
                before = len(self.calls())
                rerun = self.run_bootstrap(enrollment="")
                self.assertEqual(rerun.returncode, 0, rerun.stderr)
                self.assertFalse(any(command == "apt-get" and args[:1] == ["install"] for command, args in self.calls()[before:]))
                binary.unlink()
                repair = self.run_bootstrap(enrollment="")
                self.assertEqual(repair.returncode, 0, repair.stderr)
                self.assertTrue(link.is_file())
                self.assertEqual(link.resolve(), binary)

    def setup_manifest(self):
        return json.loads((self.home / ".local/lib/detent-sprite-runner/setup-manifest.json").read_text())

    def fresh_sprite(self):
        shutil.rmtree(self.home)
        bundled = self.home / ".local/bin"
        bundled.mkdir(parents=True)
        (bundled / "codex").symlink_to(self.fake_bin / "codex")
        for state in ("calls.jsonl", "tokens.txt", "chromium-ready", "postgresql-client-17.version"):
            (self.root / state).unlink(missing_ok=True)
        for service in self.root.glob("service-*.json"):
            service.unlink()
        shutil.rmtree(self.root / "postgresql-client-17", ignore_errors=True)

    def test_setup_revision_is_recorded_credential_free_before_enrollment(self):
        result = self.run_bootstrap()
        self.assertEqual(result.returncode, 0, result.stderr)
        manifest = self.setup_manifest()
        self.assertEqual(manifest.pop("revision"), setup_revision(manifest))
        self.assertTrue(manifest["toolchain"].pop("os"))
        self.assertEqual(manifest, {
            "schema": 1,
            "platform": "linux/amd64",
            "bootstrap": "v0.117.41",
            "toolchain": {"claude": "2.1.289", "codex": "0.160.0", "docker": "unavailable", "fd": "10.3.0", "gh": "2.101.0",
                          "go": "1.26.6", "node": "24.11.1", "npm": "11.6.2", "playwright": "1.63.0", "psql": "17.10-0ubuntu0.25.10.1", "rg": "15.2.0", "yaml": "2.8.3"},
            "caches": ["go-build", "go-mod", "npm"],
        })
        text = json.dumps(manifest)
        self.assertNotIn(TOKEN, text)
        self.assertNotIn("identity", text)
        self.assertNotIn(str(self.home), text)
        calls = self.calls()
        written = next(i for i, (command, args) in enumerate(calls) if command == "node" and args[1:3] == ["setup-manifest", "write"])
        registered = next(i for i, (command, args) in enumerate(calls) if command == "detent" and args[:3] == ["hub", "runner", "register"])
        self.assertLess(written, registered)
        self.assertIn("(recovery only)", " ".join(next(args for command, args in calls if command == "sprite-env" and args[:2] == ["checkpoints", "create"])))
        self.assertNotIn("cannot reproduce", result.stderr)

    def test_reports_manual_customizations_the_setup_cannot_reproduce(self):
        (self.home / ".local/bin/custom-tool").write_text("#!/bin/sh\n")
        module = self.home / ".local/share/detent-runner/npm/lib/node_modules"
        (module / "left-pad").mkdir(parents=True)
        (module / "@example/private-tool").mkdir(parents=True)
        result = self.run_bootstrap()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.setup_manifest()["unreproducible"], ["bin/custom-tool", "npm/@example/private-tool", "npm/left-pad"])
        self.assertIn("cannot reproduce", result.stderr)
        for entry in ("bin/custom-tool", "npm/@example/private-tool", "npm/left-pad"):
            self.assertIn(entry, result.stderr)

    def test_reproduces_setup_revision_without_seed(self):
        seed = self.run_bootstrap()
        self.assertEqual(seed.returncode, 0, seed.stderr)
        declared = self.root / "declared-setup.json"
        shutil.copyfile(self.home / ".local/lib/detent-sprite-runner/setup-manifest.json", declared)
        original = json.loads(declared.read_text())

        def declare(name, change, recompute=True):
            manifest = json.loads(json.dumps(original))
            change(manifest)
            if recompute:
                manifest["revision"] = setup_revision(manifest)
            path = self.root / (name + "-setup.json")
            path.write_text(json.dumps(manifest))
            return path

        cases = (
            ("seed unavailable", declared, (), {}, None),
            ("platform mismatch", declared, (), {"FIXTURE_ARCH": "aarch64"}, "targets linux/amd64; this Sprite is linux/arm64"),
            ("bootstrap mismatch", declared, ("--version", "v0.117.42"), {}, "rerun with --version v0.117.41"),
            ("declared manual customization", declare("customized", lambda m: m.update(unreproducible=["bin/custom-tool"])), (), {}, "cannot reproduce: bin/custom-tool"),
            ("toolchain drift", declare("toolchain", lambda m: m["toolchain"].update(go="1.26.5")), (), {}, "does not reproduce the setup revision (toolchain.go)"),
            ("node runtime drift", declared, (), {"FIXTURE_NODE_VERSION": "v25.2.0", "FIXTURE_NPM_VERSION": "11.7.0"}, "does not reproduce the setup revision (toolchain.node, toolchain.npm)"),
            ("cache allowlist drift", declare("caches", lambda m: m.update(caches=["go-mod"])), (), {}, "does not reproduce the setup revision (caches)"),
            ("tampered pin", declare("tampered", lambda m: m["toolchain"].update(go="1.26.5"), recompute=False), (), {}, "(revision, toolchain.go)"),
            ("reordered declaration", declare("reordered", lambda m: m.update(toolchain=dict(reversed(m["toolchain"].items())), caches=m["caches"][::-1]), recompute=False), (), {}, None),
        )
        for name, path, extra, env, failure in cases:
            with self.subTest(name=name):
                self.fresh_sprite()
                for key, value in env.items():
                    self.env[key] = value
                result = self.run_bootstrap(extra=("--setup-manifest", str(path), *extra))
                for key in env:
                    del self.env[key]
                self.assertNotIn(TOKEN, result.stdout + result.stderr)
                if failure:
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn(failure, result.stderr)
                    self.assertIn("--setup-manifest " + str(path).replace(" ", "\\ "), result.stderr)
                    self.assertEqual(self.registrations(), [])
                    self.assertFalse((self.home / ".config/detent-runner/identity.json").exists())
                    continue
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(self.setup_manifest(), original)
                self.assertEqual(len(self.registrations()), 1)
        self.fresh_sprite()
        (self.home / ".local/bin/custom-tool").write_text("#!/bin/sh\n")
        customized = self.run_bootstrap(extra=("--setup-manifest", str(declared)))
        self.assertNotEqual(customized.returncode, 0)
        self.assertIn("manual customizations outside the setup revision: bin/custom-tool", customized.stderr)
        self.assertEqual(self.registrations(), [])

    def test_cache_repair_respects_the_configured_workspace_root(self):
        first = self.run_bootstrap()
        self.assertEqual(first.returncode, 0, first.stderr)
        config = self.home / ".config/detent-runner/global.yaml"
        original = config.read_text()
        job = self.home / "go/pkg/mod/job-data"
        job.mkdir(parents=True)
        for root, failure in (("~/go/pkg/mod", True), ("~/go", False)):
            with self.subTest(root=root):
                config.write_text(original + f"workspace_root: {root}\n")
                rerun = self.run_bootstrap(extra=("--workspace-root", str(self.home / root[2:])), enrollment="")
                self.assertTrue(job.exists())
                self.assertNotIn(["go", ["clean", "-modcache"]], self.calls())
                if failure:
                    self.assertNotEqual(rerun.returncode, 0)
                    self.assertIn("native cache go-mod overlaps a credential, identity or job location", rerun.stderr)
                else:
                    self.assertEqual(rerun.returncode, 0, rerun.stderr)
                    self.assertIn("go-mod=unmanaged", rerun.stdout)

    def test_native_caches_are_optional_allowlisted_and_rebuilt(self):
        cache = self.root / "caches"

        def corrupt(path, entry):
            path.mkdir(parents=True, exist_ok=True)
            (path / entry).write_text("partial")

        cases = (
            ("cold", {}, None, "go-build=cold, go-mod=cold, npm=cold", []),
            ("warm", {}, lambda: [(self.home / p).mkdir(parents=True) for p in (".cache/go-build/README", "go/pkg/mod/cache", ".npm/_cacache")],
             "go-build=warm, go-mod=warm, npm=warm", []),
            ("corrupt module cache", {}, lambda: corrupt(self.home / "go/pkg/mod", "partial"), "go-mod=unverified", []),
            ("config file in home", {}, None, "go-build=cold", []),
            ("corrupt npm cache", {}, lambda: corrupt(self.home / ".npm", "_logs"), "npm=rebuilt", [["npm", ["cache", "verify"]]]),
            ("cache is a file", {"GOCACHE": str(cache / "go-build")}, lambda: cache.mkdir() or (cache / "go-build").write_text("x"), "native cache go-build is not a directory", []),
            ("operator override", {"GOMODCACHE": str(cache / "mod")}, lambda: corrupt(cache / "mod", "partial"), "go-mod=unmanaged", []),
            ("override at default path", {"GOMODCACHE": str(self.home / "go/pkg/mod")}, lambda: corrupt(self.home / "go/pkg/mod", "partial"), "go-mod=unmanaged", []),
            ("npm cache named off", {"npm_config_cache": "off"}, None, "npm=cold", []),
            ("uppercase npm override", {"NPM_CONFIG_CACHE": str(self.home / ".npm")}, lambda: corrupt(self.home / ".npm", "_logs"), "npm=unmanaged", []),
            ("npm cache inside credentials", {"NPM_CONFIG_CACHE": str(self.home / ".ssh/npm")}, None, "native cache npm overlaps a credential", []),
            ("go cache disabled", {"GOCACHE": "off"}, None, "go-build=disabled", []),
            ("provider login", {"GOMODCACHE": str(self.home / ".config/gh/mod")}, None, "native cache go-mod overlaps a credential", []),
            ("relocated GitHub login", {"GH_CONFIG_DIR": str(self.home / "go/pkg/mod")}, lambda: corrupt(self.home / "go/pkg/mod", "hosts.yml"), "native cache go-mod overlaps a credential", []),
            ("parent segments", {"GOMODCACHE": str(self.home / "tmp/../.ssh")}, lambda: corrupt(self.home / ".ssh", "id_ed25519"), "native cache go-mod overlaps a credential", []),
            ("symlinked credential", {"GOCACHE": str(cache / "link")}, lambda: cache.mkdir() or (self.home / ".codex").mkdir() or (cache / "link").symlink_to(self.home / ".codex"), "native cache go-build overlaps a credential", []),
            ("whole home", {"GOCACHE": str(self.home)}, None, "native cache go-build overlaps a credential", []),
            ("runner identity", {"npm_config_cache": str(self.home / ".config/detent-runner")}, None, "native cache npm overlaps a credential", []),
            ("workspace root inside cache", {"GOMODCACHE": str(self.home / "detent-runner")}, None, "native cache go-mod overlaps a credential, identity or job location", []),
        )
        for name, env, prepare, expected, repairs in cases:
            with self.subTest(name=name):
                self.fresh_sprite()
                shutil.rmtree(cache, ignore_errors=True)
                if prepare:
                    prepare()
                self.env.update(env)
                result = self.run_bootstrap(extra=("--config", str(self.home / "global.yaml")) if name == "config file in home" else ())
                for key in env:
                    del self.env[key]
                for repair in repairs:
                    self.assertIn(repair, self.calls())
                if name == "relocated GitHub login":
                    self.assertTrue((self.home / "go/pkg/mod/hosts.yml").exists())
                if name == "parent segments":
                    self.assertTrue((self.home / ".ssh/id_ed25519").exists())
                if "overlaps" in expected or "not a directory" in expected:
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn(expected, result.stderr)
                    self.assertIn("nothing was enrolled" if "overlaps" in expected else "move it aside", result.stderr)
                    self.assertEqual(self.registrations(), [])
                    continue
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn(expected, result.stdout)
                self.assertFalse(any(command in ("go", "npm") and args[:1] in (["clean"], ["cache"]) for command, args in self.calls() if [command, args] not in repairs))


def setup_revision(manifest):
    canonical = {"schema": manifest["schema"], "platform": manifest["platform"], "bootstrap": manifest["bootstrap"],
                 "toolchain": dict(sorted(manifest["toolchain"].items())), "caches": sorted(set(manifest["caches"]))}
    return hashlib.sha256(json.dumps(canonical, separators=(",", ":")).encode()).hexdigest()


if __name__ == "__main__":
    unittest.main()
