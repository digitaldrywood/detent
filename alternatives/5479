import json
import os
from pathlib import Path
import plistlib
import shlex
import subprocess
import sys
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[1]
EXAMPLES = ROOT / "docs/examples/hub"


class HubExamplesTest(unittest.TestCase):
    def load_plist(self):
        with (EXAMPLES / "com.digitaldrywood.detent.hub.plist").open("rb") as source:
            return plistlib.load(source)

    def test_service_commands_use_current_hub_flags(self):
        plist = self.load_plist()
        unit = (EXAMPLES / "detent-hub.service").read_text()
        systemd = next(shlex.split(line.partition("=")[2]) for line in unit.splitlines()
                       if line.startswith("ExecStart="))
        with tempfile.TemporaryDirectory() as directory:
            binary = str(Path(directory) / "detent")
            result = subprocess.run(
                ["go", "build", "-p", os.environ.get("TEST_PROCS", "4"), "-o", binary, "./cmd/detent"],
                cwd=ROOT, capture_output=True, text=True, timeout=180,
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            for name, args in (("launchd", plist["ProgramArguments"][4:]), ("systemd", systemd)):
                with self.subTest(service=name):
                    self.assertEqual(args[1:3], ["hub", "serve"])
                    result = subprocess.run(
                        [binary, *args[1:], "--help"], cwd=ROOT,
                        capture_output=True, text=True, timeout=10,
                    )
                    self.assertEqual(result.returncode, 0, result.stderr)

    def test_launchd_token_is_loaded_from_file_into_environment(self):
        plist = self.load_plist()
        self.assertEqual(plist["Label"], "com.digitaldrywood.detent.hub")
        self.assertEqual(plist["Umask"], 0o077)
        for key in ("StandardOutPath", "StandardErrorPath"):
            self.assertTrue(Path(plist[key]).is_absolute())
        args = plist["ProgramArguments"]
        self.assertEqual(args[:2], ["/bin/sh", "-c"])
        self.assertNotIn("DETENT_HUB_ADMIN_TOKEN", plist.get("EnvironmentVariables", {}))
        with tempfile.TemporaryDirectory() as directory:
            home = Path(directory)
            token_file = home / "Library/Application Support/Detent Hub/admin-token"
            token_file.parent.mkdir(parents=True)
            token = "example-private-token-'$-with-spaces"
            token_file.write_text(token + "\n")
            probe = home / "probe.py"
            probe.write_text(
                "import json, os, sys\n"
                "print(json.dumps([sys.argv[1:], os.environ['DETENT_HUB_ADMIN_TOKEN']]))\n"
            )
            env = {**os.environ, "HOME": str(home)}
            env.pop("DETENT_HUB_ADMIN_TOKEN", None)
            command = args[:4] + [sys.executable, str(probe)] + args[5:]
            result = subprocess.run(command, env=env, capture_output=True, text=True, check=True, timeout=10)
            argv, loaded = json.loads(result.stdout)
            self.assertEqual(argv, args[5:])
            self.assertEqual(loaded, token)
            self.assertNotIn(token, " ".join(command + argv))
            token_file.unlink()
            result = subprocess.run(command, env=env, capture_output=True, text=True, timeout=10)
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual(result.stdout, "")


if __name__ == "__main__":
    unittest.main()
