import argparse
import json
import os
import posixpath
import subprocess
import sys


POLICY = "internal/invariants/tenancy_policy.json"
CHECK = "internal/invariants/tenancy_test.go"


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


def exists(revision, path):
    try:
        git("rev-parse", "--verify", "--quiet", "--end-of-options", revision + ":" + path)
        return True
    except subprocess.CalledProcessError:
        return False


def read_policy(ref, introduced_allowed=False):
    revision = git("rev-parse", "--verify", "--end-of-options", ref + "^{commit}")
    has_policy, has_check = exists(revision, POLICY), exists(revision, CHECK)
    if introduced_allowed and not has_policy and not has_check:
        return revision, None
    if not has_policy or not has_check:
        raise ValueError(f"{revision}: {POLICY} and {CHECK} must both be present")
    policy = json.loads(git("show", revision + ":" + POLICY), object_pairs_hook=unique_object)
    if not isinstance(policy, dict) or set(policy) != {"test_support", "exceptions", "debt"}:
        raise ValueError(f"{revision}: policy must contain test_support, exceptions and debt")
    support = policy["test_support"]
    if not isinstance(support, list) or len(support) != len(set(support)):
        raise ValueError(f"{revision}: test_support must be a list of distinct directories")
    for directory in support:
        if not isinstance(directory, str) or not directory or posixpath.normpath(directory) != directory or directory.startswith(("/", "..")):
            raise ValueError(f"{revision}: test_support entry {directory!r} must be a clean relative directory")
    for group in ("exceptions", "debt"):
        entries = policy[group]
        if not isinstance(entries, dict):
            raise ValueError(f"{revision}: {group} must be an object")
        for name, counts in entries.items():
            if not isinstance(counts, dict) or not counts:
                raise ValueError(f"{revision}: {group}/{name} must have counts")
            for symbol, count in counts.items():
                if not symbol or type(count) is not int or count <= 0:
                    raise ValueError(f"{revision}: {group}/{name}/{symbol} must have a positive integer count")
    return revision, policy


def main():
    parser = argparse.ArgumentParser(description="Compare tenancy debt, exceptions and test-support exemptions at explicit landing refs")
    parser.add_argument("target_ref")
    parser.add_argument("candidate_ref")
    args = parser.parse_args()
    try:
        target_ref, target = read_policy(args.target_ref, introduced_allowed=True)
        candidate_ref, candidate = read_policy(args.candidate_ref)
        if target is None:
            print(f"Tenancy policy introduced: target={target_ref} has no tenancy invariant; candidate={candidate_ref}")
            return 0
        problems = []
        for directory in candidate["test_support"]:
            if directory not in target["test_support"]:
                problems.append(f"{directory}: test-support exemption is not in the target set")
        for group in ("exceptions", "debt"):
            for name, counts in candidate[group].items():
                for symbol, count in counts.items():
                    allowed = target[group].get(name, {}).get(symbol, 0)
                    if count > allowed:
                        problems.append(f"{name}: {group} {symbol} count {count} exceeds target baseline {allowed}")
        if problems:
            raise ValueError("Tenancy policy growth refused:\n" + "\n".join(sorted(problems)))
    except (OSError, ValueError, subprocess.CalledProcessError) as error:
        print(f"Tenancy policy check failed: {error}", file=sys.stderr)
        return 1
    print(f"Tenancy policy monotonicity passed: target={target_ref} candidate={candidate_ref}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
