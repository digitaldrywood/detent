import re
import shlex
import subprocess
from pathlib import Path


ROOT = Path(__file__).resolve().parent.parent


def command_lines(document):
    for snippet in re.findall(r"(?<!`)`([^`]+)`(?!`)", document):
        yield re.sub(r"\s+", " ", snippet)
    for snippet in re.findall(
        r"(?ms)^[ \t]*```(?:sh|bash|shell)\s*\n(.*?)^[ \t]*```", document,
    ):
        yield from snippet.replace("\\\n", " ").splitlines()


def check_subcommands(document, help_text):
    section = help_text.partition("\nAvailable Commands:\n")[2].split("\n\n", 1)[0]
    available = set(re.findall(r"(?m)^  ([a-z][a-z0-9-]*)[ \t]{2,}", section))
    if not available:
        return ["no runner subcommands found in CLI help"]
    mentioned = set()
    for line in command_lines(document):
        mentioned.update(re.findall(r"\bdetent[ \t]+hub[ \t]+runner[ \t]+([a-z][a-z0-9-]*)", line))
    return [f"unknown runner subcommand in docs: {name}" for name in sorted(mentioned - available)]


def check(document, source):
    available = set(re.findall(
        r'cmd\.Flags\(\)\.\w+\(\s*(?:&\w+\s*,\s*)?"([a-z][a-z-]*)"',
        source,
    ))
    flags = set()
    commands = 0
    for line in command_lines(document):
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
    help_text = subprocess.run(
        ["go", "run", "-p", "4", "./cmd/detent", "hub", "runner", "--help"],
        cwd=ROOT, check=True, stdout=subprocess.PIPE, text=True,
    ).stdout
    for path in sorted((ROOT / "docs").rglob("*")):
        if path.is_file() and (path.name.endswith(".md") or path.name.endswith(".md.in")):
            errors.extend(f"{path.relative_to(ROOT)}: {error}" for error in check_subcommands(path.read_text(), help_text))
    if errors:
        raise SystemExit("\n".join(errors))
    print("Documented runner subcommands and registration flags match the CLI.")
