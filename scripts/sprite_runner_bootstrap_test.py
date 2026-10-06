"""Bootstrap boundary tests; no network, package installs or real enrollment."""

import io
import hashlib
import json
import os
import pathlib
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
elif name == "sha256sum":
    checksum, filename = sys.stdin.read().split()
    sys.exit(0 if hashlib.sha256(pathlib.Path(filename).read_bytes()).hexdigest() == checksum else 1)
elif name == "go":
    if args == ["version"]:
        print("go version go1.26.6 linux/amd64")
    else:
        shutil.copy(__file__, pathlib.Path(os.environ["GOBIN"]) / "detent")
elif name == "npm":
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
    print("install ok installed " + installed.read_text())
elif name == "apt-get":
    for arg in args:
        if arg.startswith(("postgresql-client-17=", "docker.io=")):
            package, version = arg.split("=", 1)
            (root / (package + ".version")).write_text(version)
elif name == "node":
    script = sys.stdin.read()
    if args[:1] == ["-p"] or (len(args) == 3 and args[1].endswith("/yaml")):
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
elif name == "psql":
    print("psql (PostgreSQL) 17.10")
elif name == "rg":
    print("ripgrep 0.0.0" if pathlib.Path(__file__).parent == root / "bin" else "ripgrep 15.2.0")
elif name == "fd":
    print("fd 0.0.0" if pathlib.Path(__file__).parent == root / "bin" else "fd 10.3.0")
elif name == "detent":
    if args[:3] == ["hub", "runner", "register"]:
        with (root / "tokens.txt").open("a") as tokens:
            tokens.write(os.environ.get("DETENT_RUNNER_ENROLLMENT_TOKEN", "") + "\n")
        config = pathlib.Path(args[args.index("--config") + 1])
        config.parent.mkdir(parents=True, exist_ok=True)
        if not config.exists():
            config.write_text("client:\n  hub_url: https://hub.example\nglobal:\n  max_concurrent_agents: 2\n")
            (config.parent / "identity.json").write_text("original identity\n")
    else:
        (root / "detent-cwd").write_text(os.getcwd())
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
        sys.exit(1)
    if name == "gh" and pathlib.Path(__file__).parent == root / "bin":
        print("gh version 2.0.0 (fixture)")
    else:
        print({"codex": "codex-cli 0.160.0", "claude": "2.1.289 (Claude Code)", "gh": "gh version 2.101.0 (fixture)"}[name])
'''

TOKEN = "det_enroll_fixture_secret"
ENROLLMENT = f"detent hub runner register --url https://hub.example/organizations/org_example --token {TOKEN} --name 'Build host' --capacity 2 --service\n"


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
        for name in ("uname", "curl", "npm", "gh", "rg", "fd", "codex", "claude", "sprite-env", "sha256sum", "id", "sudo", "unshare", "dpkg-query", "apt-get", "node", "psql", "docker", "dockerd"):
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
        self.assertEqual(module.returncode, 0, "Install yaml@2.8.3 and expose it through NODE_PATH: " + module.stderr)
        self.env = dict(os.environ, HOME=str(self.home), FIXTURE_ROOT=str(self.root),
                        FIXTURE_NODE=node, FIXTURE_YAML_MODULE=str(pathlib.Path(module.stdout.strip()).parent),
                        PATH=str(self.fake_bin) + os.pathsep + os.environ["PATH"],
                        TMPDIR=str(self.root))
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
        second = self.run_bootstrap(enrollment=enrollment)
        self.assertEqual(second.returncode, 0, second.stderr)
        self.assertEqual(config.read_text(), human_config)
        self.assertEqual(config.with_name("identity.json").read_bytes(), identity)
        self.assertFalse(list(self.root.rglob("SHOULD_NOT_EXIST")))
        registrations = self.registrations()
        self.assertEqual(len(registrations), 2)
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
        self.assertEqual((self.root / "detent-cwd").read_text(), str(work))
        self.assertIn(["detent", ["--config", str(config), "--headless"]], self.calls())
        self.assertIn("claude auth login", first.stdout)
        self.assertIn("codex login", first.stdout)
        self.assertFalse(list(self.root.glob("sprite-bootstrap.*")))
        downloads = [args for command, args in self.calls() if command == "curl"]
        self.assertTrue(any("/v0.117.41/" in " ".join(args) for args in downloads))
        self.assertFalse(any(command == "go" and args[:1] == ["install"] for command, args in self.calls()))

    def test_pressure_floor_config_merge(self):
        cases = (
            ("fresh", None, {"client": {"hub_url": "https://hub.example"}, "global": {"max_concurrent_agents": 2}}, 1, 1),
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
                    identity.write_text("operator identity\n")
                result = self.run_bootstrap(extra=("--config", str(config)), enrollment="" if original is not None else None)
                self.assertEqual(result.returncode, 0, result.stderr)
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
                    self.assertEqual(identity.read_text(), "operator identity\n")
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
        npm_installs = [args for command, args in self.calls() if command == "npm"]
        self.assertEqual(len(npm_installs), 1)
        self.assertTrue(set(("@openai/codex@0.160.0", "@anthropic-ai/claude-code@2.1.289", "playwright@1.63.0")).issubset(npm_installs[0]))
        before = len(self.calls())
        rerun = self.run_bootstrap(enrollment="")
        self.assertEqual(rerun.returncode, 0, rerun.stderr)
        self.assertEqual(len(self.registrations()), 1)
        restarts = [args for command, args in self.calls() if command == "sprite-env" and args[:1] == ["curl"]]
        self.assertEqual(len(restarts), 1)
        rerun_calls = self.calls()[before:]
        self.assertFalse(any(command in ("curl", "npm", "apt-get") for command, _ in rerun_calls))
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
            "detent start --yes\n",
            "detent hub runner register --url https://hub.example/organizations/org_example --service\n",
        ):
            with self.subTest(enrollment=enrollment):
                result = self.run_bootstrap(enrollment=enrollment)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(self.calls(), [])
                self.assertNotIn(TOKEN, result.stderr)

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
        self.assertIn(["apt-get", ["install", "-y", "--no-install-recommends", "--allow-downgrades", "postgresql-client-17=17.10-0ubuntu0.25.10.1"]], self.calls())
        self.assertIn(["playwright", ["install", "--with-deps", "chromium"]], self.calls())
        self.assertIn("headless screenshot verified", update.stdout)

    def test_docker_install_requires_sprite_capabilities(self):
        skipped = self.run_bootstrap()
        self.assertEqual(skipped.returncode, 0, skipped.stderr)
        self.assertFalse((self.root / "docker.io.version").exists())
        self.env["DOCKER_SUPPORTED"] = "1"
        available = self.run_bootstrap(enrollment="")
        self.assertEqual(available.returncode, 0, available.stderr)
        self.assertEqual((self.root / "docker.io.version").read_text(), "29.1.3-0ubuntu3~25.10.1")
        self.assertTrue((self.root / "service-detent-docker.json").exists())
        self.assertIn("Docker version 29.1.3", available.stdout)
        before = len(self.calls())
        rerun = self.run_bootstrap(enrollment="")
        self.assertEqual(rerun.returncode, 0, rerun.stderr)
        self.assertFalse(any(command == "apt-get" for command, _ in self.calls()[before:]))
        self.assertFalse(any(command == "sprite-env" and args[:3] == ["services", "create", "detent-docker"] for command, args in self.calls()[before:]))


if __name__ == "__main__":
    unittest.main()
