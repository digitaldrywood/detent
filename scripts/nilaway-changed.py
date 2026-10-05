import hashlib
import json
import os
import re
import subprocess
import sys
import tempfile
from pathlib import Path


ROOT = Path(__file__).resolve().parent.parent
BASELINE = ROOT / "scripts" / "nilaway-baseline.json"
DIAGNOSTIC = re.compile(r"^(?P<path>\S+\.go):(?P<line>\d+):(?P<column>\d+):\s*(?:error:\s*)?(?P<message>.*)")
ANSI = re.compile(r"\x1b\[[0-9;]*m")
RELATED_DIAGNOSTIC = re.compile(r"^\(Same nil source could also cause potential nil panic\(s\) at \d+ other place\(s\): .+\.\)$")


def main():
    version, include_packages = sys.argv[1:]
    baseline = json.loads(BASELINE.read_text())
    allowed = {(entry["file"], entry["line"], entry["column"], entry["rule"]): entry["source_sha256"] for entry in baseline}
    scratch = next((os.environ[key] for key in ("TMPDIR", "TMP", "TEMP") if os.environ.get(key)), None)
    if scratch is None:
        print("Set TMPDIR, TMP, or TEMP for the NilAway audit")
        return 1
    env = dict(os.environ)
    env.setdefault("GOMEMLIMIT", "1GiB")
    with tempfile.TemporaryDirectory(dir=scratch) as directory:
        env["GOBIN"] = directory
        install = subprocess.run(
            ["go", "install", "-p=1", f"go.uber.org/nilaway/cmd/nilaway@{version}"],
            cwd=ROOT, env=env, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, check=False,
        )
        if install.returncode:
            print(install.stdout)
            return 1
        binary = Path(directory) / ("nilaway.exe" if os.name == "nt" else "nilaway")
        command = ["go", "vet", "-p=1", f"-vettool={binary}", f"-include-pkgs={include_packages}", "./..."]
        result = subprocess.run(command, cwd=ROOT, env=env, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, check=False)
    if result.returncode not in (0, 1):
        print(result.stdout)
        return 1
    unexpected = []
    diagnostics = 0
    for raw in result.stdout.splitlines():
        line = ANSI.sub("", raw)
        match = DIAGNOSTIC.search(line)
        if not match:
            if line.strip() and not line.startswith(("# ", "\t", " ")) and not RELATED_DIAGNOSTIC.fullmatch(line):
                unexpected.append(line)
            continue
        diagnostics += 1
        path = Path(match["path"])
        if not path.is_absolute():
            path = ROOT / path
        try:
            relative = str(path.resolve().relative_to(ROOT))
            source = path.read_text().splitlines()[int(match["line"]) - 1]
        except (OSError, ValueError, IndexError):
            unexpected.append(line)
            continue
        rule = "Potential nil panic detected"
        key = (relative, int(match["line"]), int(match["column"]), rule)
        digest = hashlib.sha256(source.encode()).hexdigest()
        if rule not in match["message"] or allowed.get(key) != digest:
            unexpected.append(line)

    if unexpected:
        print("Unexpected NilAway diagnostics:")
        print("\n".join(unexpected))
        return 1
    if result.returncode and not diagnostics:
        print(result.stdout)
        return result.returncode
    print(f"Audited all Go packages; {diagnostics} reviewed legacy diagnostics")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
