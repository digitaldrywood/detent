"""Bootstrap boundary tests; no network, package installs or real enrollment."""

import io
import json
import os
import pathlib
import shutil
import subprocess
import tarfile
import tempfile
import unittest


STUB = r'''#!/usr/bin/env python3
import hashlib, json, os, pathlib, shutil, sys
root = pathlib.Path(os.environ["FIXTURE_ROOT"])
name, args = pathlib.Path(sys.argv[0]).name, sys.argv[1:]
with (root / "calls.jsonl").open("a") as log:
    log.write(json.dumps([name, args]) + "\n")
if name == "uname":
    print("Linux" if args == ["-s"] else "x86_64")
elif name == "curl":
    url = args[args.index("--location") + 1]
    dest = pathlib.Path(args[args.index("--output") + 1])
    if url.endswith("/go.mod"):
        dest.write_text("module fixture\ngo 1.26\ntoolchain go1.26.6\n")
    elif url.endswith(".sha256"):
        dest.write_text(hashlib.sha256((root / "go.tar.gz").read_bytes()).hexdigest())
    elif url.endswith("checksums.txt"):
        digest = hashlib.sha256((root / "detent.tar.gz").read_bytes()).hexdigest()
        if os.environ.get("BAD_CHECKSUM"):
            digest = "0" * 64
        dest.write_text(digest + "  detent_0.117.41_linux_amd64.tar.gz\n")
    else:
        shutil.copyfile(root / ("go.tar.gz" if "/go1." in url else "detent.tar.gz"), dest)
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
    for binary in ("codex", "claude"):
        shutil.copy(__file__, prefix / binary)
elif name == "detent":
    if args[:3] == ["hub", "runner", "register"]:
        with (root / "tokens.txt").open("a") as tokens:
            tokens.write(os.environ.get("DETENT_RUNNER_ENROLLMENT_TOKEN", "") + "\n")
        config = pathlib.Path(args[args.index("--config") + 1])
        config.parent.mkdir(parents=True, exist_ok=True)
        if not config.exists():
            config.write_text("original configuration\n")
            (config.parent / "identity.json").write_text("original identity\n")
    else:
        (root / "detent-cwd").write_text(os.getcwd())
        print("detent fixture")
elif name == "sprite-env":
    if args[:2] == ["services", "get"]:
        sys.exit(0 if (root / "service.json").exists() else 1)
    elif args[:2] == ["services", "create"]:
        (root / "service.json").write_text(json.dumps(args))
    elif args[:2] == ["checkpoints", "create"]:
        if os.environ.get("CHECKPOINT_FAIL"):
            sys.exit(1)
        print('{"id":"v1"}')
elif name in ("codex", "claude", "gh"):
    if args[-1:] == ["status"]:
        sys.exit(1)
    print(name + " fixture")
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
        for name in ("uname", "curl", "npm", "gh", "codex", "claude", "sprite-env", "sha256sum"):
            path = self.fake_bin / name
            path.write_text(STUB)
            path.chmod(0o755)
        for archive, binary in (("go.tar.gz", "go/bin/go"), ("detent.tar.gz", "detent")):
            with tarfile.open(self.root / archive, "w:gz") as bundle:
                data = STUB.encode()
                info = tarfile.TarInfo(binary)
                info.size, info.mode = len(data), 0o755
                bundle.addfile(info, io.BytesIO(data))
        self.script = self.root / "sprite-runner-bootstrap.sh"
        shutil.copyfile(pathlib.Path(__file__).with_name(self.script.name), self.script)
        self.env = dict(os.environ, HOME=str(self.home), FIXTURE_ROOT=str(self.root),
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
                              env=self.env, text=True, capture_output=True)

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
        config.write_text("human edits\n")
        second = self.run_bootstrap(enrollment=enrollment)
        self.assertEqual(second.returncode, 0, second.stderr)
        self.assertEqual(config.read_text(), "human edits\n")
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
        service = subprocess.run(["bash", str(launcher)], env=self.env, text=True, capture_output=True)
        self.assertEqual(service.returncode, 0, service.stderr)
        self.assertEqual((self.root / "detent-cwd").read_text(), str(work))
        self.assertIn(["detent", ["--config", str(config), "--headless"]], self.calls())
        self.assertIn("claude auth login", first.stdout)
        self.assertIn("codex login", first.stdout)
        self.assertFalse(list(self.root.glob("sprite-bootstrap.*")))
        downloads = [args for command, args in self.calls() if command == "curl"]
        self.assertTrue(any("/v0.117.41/" in " ".join(args) for args in downloads))
        self.assertFalse(any(command == "go" and args[:1] == ["install"] for command, args in self.calls()))

    def test_empty_input_reuses_an_existing_registration_only(self):
        # Catches reruns that need a fresh token, and a fresh Sprite that
        # silently starts an unregistered service.
        fresh = self.run_bootstrap(enrollment="")
        self.assertNotEqual(fresh.returncode, 0)
        self.assertIn("not registered yet", fresh.stderr)
        self.assertEqual(self.calls(), [])
        self.assertEqual(self.run_bootstrap().returncode, 0)
        rerun = self.run_bootstrap(enrollment="")
        self.assertEqual(rerun.returncode, 0, rerun.stderr)
        self.assertEqual(len(self.registrations()), 1)
        restarts = [args for command, args in self.calls() if command == "sprite-env" and args[:1] == ["curl"]]
        self.assertEqual(len(restarts), 1)

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
        self.assertFalse(any("releases/download" in " ".join(args) for args in downloads))

    def test_failures_do_not_claim_a_clean_checkpoint(self):
        # A corrupt release must never enroll; a failed checkpoint must return
        # failure while leaving already-enrolled state available for retry.
        for mode, expected_registration in (("BAD_CHECKSUM", False), ("CHECKPOINT_FAIL", True)):
            with self.subTest(mode=mode):
                (self.root / "calls.jsonl").unlink(missing_ok=True)
                self.env[mode] = "1"
                result = self.run_bootstrap()
                del self.env[mode]
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(bool(self.registrations()), expected_registration)
                self.assertNotIn("Restore this same Sprite:", result.stdout)


if __name__ == "__main__":
    unittest.main()
