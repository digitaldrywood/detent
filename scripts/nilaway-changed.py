import hashlib
import json
import re
import subprocess
import sys
from pathlib import Path


ROOT = Path(__file__).resolve().parent.parent
BASELINE = ROOT / "scripts" / "nilaway-baseline.json"
DIAGNOSTIC = re.compile(r"^(?P<path>\S+\.go):(?P<line>\d+):(?P<column>\d+):\s*(?:error:\s*)?(?P<message>.*)")
ANSI = re.compile(r"\x1b\[[0-9;]*m")


def git(*args):
    return subprocess.check_output(["git", *args], cwd=ROOT, text=True).strip()


def main():
    version, include_packages = sys.argv[1:]
    try:
        previous = git("describe", "--tags", "--abbrev=0", "--match", "v[0-9]*", "HEAD")
        changed = git("diff", "--name-only", "--diff-filter=ACMRT", previous, "HEAD", "--", "*.go")
    except subprocess.CalledProcessError:
        changed = git("ls-files", "--", "*.go")
    packages = sorted({"./" + str(Path(name).parent) for name in changed.splitlines() if (ROOT / name).is_file()})
    if not packages:
        print("No changed Go packages to audit")
        return 0

    baseline = json.loads(BASELINE.read_text())
    allowed = {(entry["file"], entry["line"], entry["column"], entry["rule"]): entry["source_sha256"] for entry in baseline}
    command = ["go", "run", f"go.uber.org/nilaway/cmd/nilaway@{version}", f"-include-pkgs={include_packages}", *packages]
    result = subprocess.run(command, cwd=ROOT, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, check=False)
    unexpected = []
    diagnostics = 0
    for raw in result.stdout.splitlines():
        line = ANSI.sub("", raw)
        match = DIAGNOSTIC.search(line)
        if not match:
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
    print(f"Audited {len(packages)} changed Go packages; {diagnostics} reviewed legacy diagnostics")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
