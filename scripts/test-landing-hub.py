from concurrent.futures import ThreadPoolExecutor
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

    def run_batch(index, budget):
        pattern = "^(" + "|".join(re.escape(name) for name in batches[index]) + ")$"
        return subprocess.run([
            "bash", "-c", 'source scripts/check-evidence.sh; check_with_evidence "$@"',
            "hub-slice", f"unit-full-hub-{number}-batch-{index + 1}",
            "env", f"GOMAXPROCS={budget}", *command, "-run", pattern,
        ], env=dict(env, GOMAXPROCS=str(budget)), text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)

    for offset in range(0, len(batches), int(procs)):
        count = min(int(procs), len(batches) - offset)
        with ThreadPoolExecutor(max_workers=count) as executor:
            futures = [executor.submit(run_batch, offset + slot, int(procs) // count + (slot < int(procs) % count)) for slot in range(count)]
            results = [future.result() for future in futures]
        for result in results:
            print(result.stdout, end="", flush=True)
        failure = next((result.returncode for result in results if result.returncode), 0)
        if failure:
            return failure
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
