import re
import shlex
from pathlib import Path


ROOT = Path(__file__).resolve().parent.parent


def check(document, source):
    available = set(re.findall(
        r'cmd\.Flags\(\)\.\w+\(\s*(?:&\w+\s*,\s*)?"([a-z][a-z-]*)"',
        source,
    ))
    snippets = re.findall(r"(?<!`)`([^`]+)`(?!`)", document)
    snippets += re.findall(
        r"(?ms)^[ \t]*```(?:sh|bash|shell)\s*\n(.*?)^[ \t]*```", document,
    )
    flags = set()
    commands = 0
    for snippet in snippets:
        for line in snippet.replace("\\\n", " ").splitlines():
            if not re.match(r"\s*detent\s+hub\s+runner\s+register\b", line):
                continue
            words = shlex.split(line)
            if words[:4] != ["detent", "hub", "runner", "register"]:
                continue
            commands += 1
            flags.update(word.split("=", 1)[0] for word in words[4:] if word.startswith("--"))
    errors = []
    if not available:
        errors.append("no runner registration flag declarations found")
    if not commands or not flags:
        errors.append("no runner registration command with flags found in self-hosting docs")
    for flag in sorted(flags):
        if flag[2:] not in available:
            errors.append(f"unknown runner registration flag in self-hosting docs: {flag}")
        if flag == "--token":
            errors.append("runner enrollment tokens must use DETENT_RUNNER_ENROLLMENT_TOKEN, never argv")
    return errors


if __name__ == "__main__":
    errors = check(
        (ROOT / "docs/hub-self-hosting.md").read_text(),
        (ROOT / "internal/cli/hub_runner_register.go").read_text(),
    )
    if errors:
        raise SystemExit("\n".join(errors))
    print("Self-hosted runner registration flags match the CLI declarations.")
