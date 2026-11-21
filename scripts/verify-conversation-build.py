import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tarfile
import tempfile


root = Path(__file__).resolve().parent.parent
scratch = next((os.environ[key] for key in ("TMPDIR", "TMP", "TEMP") if os.environ.get(key)), None)
if not scratch:
    raise SystemExit("This diagnostic requires a provided TMPDIR, TMP or TEMP")
env = dict(os.environ, GOMAXPROCS=os.environ.get("TEST_PROCS", "4"), CGO_ENABLED="0")
env.pop("DETENT_API_TOKEN", None)


def run(cwd, *args, command_env=None):
    result = subprocess.run(args, cwd=cwd, env=command_env or env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    if result.returncode:
        raise RuntimeError(f"{' '.join(args)} failed:\n{result.stdout.decode(errors='replace')}")
    return result.stdout.decode()


def commit(cwd, message, *paths):
    run(cwd, "git", "add", "--", *paths)
    run(cwd, "git", "commit", "-qm", message)
    if run(cwd, "git", "ls-files", "static/app/conversation").strip():
        raise RuntimeError("Generated conversation output entered a source commit")
    return run(cwd, "git", "rev-parse", "HEAD").strip()


def embedded_consumer(cwd, command_env=None):
    run(cwd, "go", "test", "-p", env["GOMAXPROCS"], "-count=1", "./internal/web", "-run",
        "^TestServerServesDefaultStaticAssetsFromArbitraryWorkingDirectory$", command_env=command_env)


with tempfile.TemporaryDirectory(prefix="conversation-build-", dir=scratch) as directory:
    work = Path(directory)
    repo = work / "source"
    repo.mkdir()
    files = run(root, "git", "ls-files", "--cached", "--others", "--exclude-standard", "-z").split("\0")
    for name in files:
        if not name or name.startswith(".detent/") or not (root / name).is_file():
            continue
        target = repo / name
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(root / name, target)
    run(repo, "git", "init", "-q", "--initial-branch=base")
    run(repo, "git", "config", "user.name", "Conversation build diagnostic")
    run(repo, "git", "config", "user.email", "conversation-build@localhost")
    run(repo, "git", "config", "commit.gpgsign", "false")
    run(repo, "git", "config", "tag.gpgsign", "false")
    run(repo, "git", "config", "core.hooksPath", str(repo / ".git/no-hooks"))
    run(repo, "git", "remote", "add", "origin", str(root))
    base = commit(repo, "test: pin source fixture", ".")
    run(repo, "git", "tag", "v0.0.0")
    raw = subprocess.run(["go", "build", "."], cwd=repo, env=env, capture_output=True)
    if raw.returncode == 0 or b"static/app/conversation/app.js" not in raw.stderr:
        raise RuntimeError("Unprepared source did not fail for missing conversation assets")

    main = "web/conversation/src/app/main.tsx"
    title = "web/conversation/src/app/pageTitle.ts"
    css = "web/conversation/src/app/index.css"
    run(repo, "git", "checkout", "-qb", "edit-a")
    with (repo / main).open("a") as output:
        output.write('\ncontainer.setAttribute("data-detent-build-edit-a", "source-a");\n')
    run(repo, "make", "app")
    js_a = hashlib.sha256((repo / "static/app/conversation/app.js").read_bytes()).hexdigest()
    edit_a = commit(repo, "test: independent client edit a", main)
    run(repo, "git", "checkout", "-qb", "edit-b", base)
    (repo / title).write_text((repo / title).read_text().replace('"Detent"', '"Detent source-b"'))
    with (repo / css).open("a") as output:
        output.write("\n:root { --detent-build-edit-b: source-b; }\n")
    run(repo, "make", "app")
    js_b = hashlib.sha256((repo / "static/app/conversation/app.js").read_bytes()).hexdigest()
    if js_a == js_b:
        raise RuntimeError("Independent client edits did not produce distinct shared bundles")
    edit_b = commit(repo, "test: independent client edit b", title, css)
    run(repo, "git", "merge", "--no-edit", "edit-a")
    combined = run(repo, "git", "rev-parse", "HEAD").strip()
    if run(repo, "git", "status", "--porcelain").strip():
        raise RuntimeError("Independent source edits did not integrate cleanly")
    (repo / "tmp").mkdir(exist_ok=True)
    (repo / "tmp/detent_release_provenance.json").write_text(json.dumps({"schema": 1, "commit": combined}))
    config = (repo / ".goreleaser.yaml").read_text()
    for key, value in (("goos", run(repo, "go", "env", "GOOS").strip()),
                       ("goarch", run(repo, "go", "env", "GOARCH").strip())):
        config = re.sub(rf"    {key}:\n(?:      - [^\n]+\n)+", f"    {key}:\n      - {value}\n", config)
    smoke_config = work / "goreleaser-smoke.yaml"
    smoke_config.write_text(config)
    run(repo, "goreleaser", "release", "--snapshot", "--clean", "--parallelism", env["GOMAXPROCS"],
        "--config", str(smoke_config), "--skip=sign,publish,announce,nfpm,homebrew,scoop,winget")
    client = repo / "static/app/conversation"
    if b"source-a" not in (client / "app.js").read_bytes() or b"source-b" not in (client / "app.js").read_bytes():
        raise RuntimeError("Combined-source JS does not contain both edits")
    if b"--detent-build-edit-b" not in (client / "app.css").read_bytes():
        raise RuntimeError("Combined-source CSS is missing")
    if b"MIT" not in (client / "app.js").read_bytes() or b"react@" not in (client / "THIRD_PARTY_LICENSES.txt").read_bytes():
        raise RuntimeError("Attribution is missing")
    expected = {str(path.relative_to(client)): hashlib.sha256(path.read_bytes()).hexdigest()
                for path in client.rglob("*") if path.is_file()}
    embedded_consumer(repo)

    artifacts = json.loads((repo / "tmp/goreleaser/artifacts.json").read_text())
    binary = next(item for item in artifacts if item["type"] == "Binary")
    if combined not in run(repo, str((repo / binary["path"]).resolve()), "version"):
        raise RuntimeError("Snapshot binary lost its combined source identity")
    source_archive = next((repo / "tmp/goreleaser").glob("*_source.tar.gz"))
    checksums = next((repo / "tmp/goreleaser").glob("*_checksums.txt")).read_text()
    if hashlib.sha256(source_archive.read_bytes()).hexdigest() not in checksums:
        raise RuntimeError("Prepared source is not in the release checksums")
    prepared = work / "prepared"
    prepared.mkdir()
    with tarfile.open(source_archive) as archive:
        archive.extractall(prepared, filter="data")
    prepared = next(prepared.iterdir())
    packaged = prepared / "static/app/conversation"
    actual = {str(path.relative_to(packaged)): hashlib.sha256(path.read_bytes()).hexdigest()
              for path in packaged.rglob("*") if path.is_file()}
    if actual != expected or (prepared / "web/conversation/node_modules").exists():
        raise RuntimeError(f"Prepared source differs from snapshot embed inputs: missing={sorted(expected.keys() - actual.keys())}, "
                           f"extra={sorted(actual.keys() - expected.keys())}, "
                           f"changed={[name for name in actual.keys() & expected.keys() if actual[name] != expected[name]]}")
    go_only = work / "go-only-bin"
    go_only.mkdir()
    (go_only / "go").symlink_to(Path(shutil.which("go")).resolve())
    go_env = dict(env, PATH=str(go_only), GOTOOLCHAIN="local")
    flags = (prepared / "BUILD_LDFLAGS").read_text().strip()
    installed = work / "detent"
    run(prepared, "go", "build", "-p", env["GOMAXPROCS"], "-ldflags", flags, "-o", str(installed), "./cmd/detent", command_env=go_env)
    if combined not in run(work, str(installed), "version", command_env=go_env):
        raise RuntimeError("Go-only binary lost its combined source identity")
    embedded_consumer(prepared, command_env=go_env)
    print(json.dumps({"base": base, "edit_a": edit_a, "edit_b": edit_b, "combined": combined,
                      "assets": len(expected), "source_archive": source_archive.name,
                      "clean_source_merge": True, "embedded_http": "snapshot and Go-only source build passed"}, indent=2))
