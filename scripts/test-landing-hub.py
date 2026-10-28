import os
import re
import subprocess
import sys


def main():
    number, procs, timeout, *filters = sys.argv[1:]
    env = dict(os.environ, GOMAXPROCS=procs)
    env.pop("DETENT_API_TOKEN", None)
    command = ["go", "test", "-p", "1", f"-timeout={timeout}", "./internal/hubserver"]
    listed = subprocess.run([*command, "-list", "."], env=env, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    if listed.returncode:
        print(listed.stdout, end="")
        return listed.returncode
    names = sorted(name for name in listed.stdout.splitlines() if re.fullmatch(r"(?:Test|Fuzz|Example)\w*", name))
    for flag, pattern in zip(filters[::2], filters[1::2], strict=True):
        if flag not in ("-run", "-skip"):
            raise ValueError("unsupported Hub slice filter")
        names = [name for name in names if bool(re.search(pattern, name)) == (flag == "-run")]
    batches = [names[offset:offset + 10] for offset in range(0, len(names), 10)]

    for index, batch in enumerate(batches):
        pattern = "^(" + "|".join(re.escape(name) for name in batch) + ")$"
        result = subprocess.run([
            "bash", "-c", 'source scripts/check-evidence.sh; check_with_evidence "$@"',
            "hub-slice", f"unit-full-hub-{number}-batch-{index + 1}",
            "env", f"GOMAXPROCS={procs}", *command, "-run", pattern,
        ], env=env, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
        print(result.stdout, end="", flush=True)
        if result.returncode:
            return result.returncode
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
