import argparse
import json
import os
import subprocess
import sys


POLICY = "internal/invariants/sqlite_policy.json"


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError(f"duplicate policy key: {key}")
        result[key] = value
    return result


def git(*args):
    environment = {key: value for key, value in os.environ.items() if not key.startswith("GIT_")}
    return subprocess.check_output(["git", *args], env=environment, stderr=subprocess.PIPE, text=True).strip()


def read_policy(ref):
    revision = git("rev-parse", "--verify", "--end-of-options", ref + "^{commit}")
    policy = json.loads(git("show", revision + ":" + POLICY), object_pairs_hook=unique_object)
    if not isinstance(policy, dict) or set(policy) != {"files", "modules"}:
        raise ValueError(f"{revision}: policy must contain files and modules")
    for group, entries in policy.items():
        if not isinstance(entries, dict):
            raise ValueError(f"{revision}: {group} must be an object")
        for name, counts in entries.items():
            if group == "modules" and name not in ("go.mod", "go.sum"):
                raise ValueError(f"{revision}: unknown module file {name}")
            if not isinstance(counts, dict) or not counts:
                raise ValueError(f"{revision}: {group}/{name} must have counts")
            for kind, count in counts.items():
                if not kind or type(count) is not int or count <= 0:
                    raise ValueError(f"{revision}: {group}/{name}/{kind} must have a positive integer count")
    return revision, policy


def main():
    parser = argparse.ArgumentParser(description="Compare SQLite debt at explicit landing refs")
    parser.add_argument("target_ref")
    parser.add_argument("candidate_ref")
    args = parser.parse_args()
    try:
        target_ref, target = read_policy(args.target_ref)
        candidate_ref, candidate = read_policy(args.candidate_ref)
        problems = []
        for group, entries in candidate.items():
            for name, counts in entries.items():
                for kind, count in counts.items():
                    allowed = target[group].get(name, {}).get(kind, 0)
                    if count > allowed:
                        problems.append(f"{name}: SQLite {kind} count {count} exceeds target baseline {allowed}")
        if problems:
            raise ValueError("SQLite policy growth refused:\n" + "\n".join(sorted(problems)))
    except (OSError, ValueError, subprocess.CalledProcessError) as error:
        print(f"SQLite policy check failed: {error}", file=sys.stderr)
        return 1
    print(f"SQLite policy monotonicity passed: target={target_ref} candidate={candidate_ref}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
