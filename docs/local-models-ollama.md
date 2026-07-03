# Local Models With Codex And Ollama

Detent can run against a local model without Go code changes. Keep the Codex
backend, point Codex at an Ollama-compatible provider, and route only the
Detent roles you want to the local model.

This guide uses a dedicated `CODEX_HOME` so the Detent host does not have to
mutate your personal `~/.codex/config.toml`.

## Validated Setup

This path was validated on July 2, 2026 with:

- Codex CLI `0.142.5`
- Ollama `0.23.0`
- macOS arm64 with 128 GB unified memory
- `qwen3-coder:30b` through an explicit Codex `model_providers` entry

The validation ran both a direct `codex exec` file-edit smoke and an isolated
Detent memory-tracker dispatch. Detent created a worktree, dispatched
`qwen3-coder:30b` through `codex app-server`, completed repeated sessions, and
the local gate passed after the model appended the expected line to `SMOKE.md`.
`ollama ps` reported a loaded context of `262144` for the validated run.

Expected caveats:

- Codex warns that model metadata for `qwen3-coder:30b` is not in its catalog.
  Set `model_context_window = 65536` and expect Detent pricing warnings for the
  local model.
- The local `devstral:latest` available during validation transported through
  Detent but did not complete the file-edit smoke. Treat Devstral Small 24B as
  the lighter model to try when qwen3-coder is too large, but validate it
  against your own workflow before routing important work to it.
- Local `qwen2.5-coder:32b`, `qwen3:32b`, and older agent aliases available on
  the validation machine produced plain-text or unsupported tool calls. For
  reliable Detent work, use `qwen3-coder:30b` as the current baseline.

## Hardware And Model Floor

Use `qwen3-coder:30b` as the baseline local model for agentic tool-calling. Its
Q4 Ollama artifact is about 18-19 GB. Plan on at least:

- 32 GB unified-memory Mac
- 24 GB VRAM Linux box

Below roughly 14B parameters, tool-call reliability drops sharply for Detent's
worktree, shell, patch, and validation loop.

## Configure Ollama Context First

The most common failure is context truncation. Ollama models often default to a
4k context. Detent's Codex prompt needs at least 64k, or the agent can lose
instructions and behave inconsistently.

Pull the baseline model:

```sh
ollama pull qwen3-coder:30b
```

Check whether your model already declares a large enough context:

```sh
ollama show qwen3-coder:30b --modelfile | rg 'PARAMETER num_ctx'
```

If the model has no `num_ctx`, or it is lower than `65536`, create a local alias
with a 64k context:

```sh
mkdir -p ~/.ollama-detent
cat > ~/.ollama-detent/qwen3-coder-64k.Modelfile <<'EOF'
FROM qwen3-coder:30b
PARAMETER num_ctx 65536
EOF

ollama create detent-qwen3-coder:30b -f ~/.ollama-detent/qwen3-coder-64k.Modelfile
```

After the first Codex run, verify the actual loaded context:

```sh
ollama ps
```

The `CONTEXT` column for the Detent model must be `65536` or higher.

## Configure Codex

Do not use Codex's `--oss` shortcut for Detent. Use an explicit provider so the
Ollama `base_url` is deterministic, including remote Ollama hosts.

Use a custom provider id such as `local_ollama`. Current Codex reserves the
built-in ids `openai`, `ollama`, and `lmstudio`, so do not define
`[model_providers.ollama]`.

```sh
mkdir -p ~/.codex-detent-ollama
cat > ~/.codex-detent-ollama/config.toml <<'EOF'
approval_policy = "never"
model_provider = "local_ollama"
model = "detent-qwen3-coder:30b"
model_context_window = 65536

[model_providers.local_ollama]
name = "Ollama"
base_url = "http://127.0.0.1:11434/v1"
wire_api = "responses"
EOF
```

Change `base_url` when Ollama is remote. Keep the `/v1` suffix.

Codex profile syntax changed in recent releases. In Codex `0.142.5`,
`codex exec --profile name` overlays `$CODEX_HOME/name.config.toml`, but
`codex app-server --profile name` was rejected. For Detent, launch
`codex app-server` with a dedicated `CODEX_HOME` as shown below. One-off
`-c key=value` overrides are also viable, but `CODEX_HOME` keeps long-running
Detent config easier to inspect.

## Direct Codex Smoke

Before routing Detent work to the model, make Codex prove it can execute tools:

```sh
mkdir -p /tmp/detent-local-model-smoke
cd /tmp/detent-local-model-smoke
printf '# Smoke\n' > SMOKE.md

CODEX_HOME="$HOME/.codex-detent-ollama" \
  codex exec --skip-git-repo-check --ephemeral --sandbox workspace-write \
  --ignore-rules 'Append a line containing exactly local codex smoke passed to SMOKE.md. Do not change any other file.'

rg 'local codex smoke passed' SMOKE.md
ollama ps
```

Do not proceed until the file changed and `ollama ps` shows `CONTEXT` at
`65536` or higher.

## Detent Route Snippet

Keep the rest of your fleet on the normal Codex backend and pin a low-stakes
role to the local model first. This example routes only the validator role to
Ollama; code-agent work keeps using the default backend.

When you add `agents.backends`, define every backend that routes reference.
If your workflow already has a top-level `codex` block, copy that command and
options into a standard explicit backend before adding the local one.

```yaml
gate:
  kind: command
  run: make check
  require_automated_review: true
  required_status_checks: []
  validator:
    enabled: true
    min_score: 0.8
    block_on:
      - p1

agents:
  backends:
    - id: codex-standard
      kind: codex
      protocol: app-server
      command: codex app-server
      options:
        approval_policy: never
        thread_sandbox: workspace-write
        turn_sandbox_policy:
          type: workspaceWrite
          networkAccess: true
    - id: codex-local-ollama
      kind: codex
      protocol: app-server
      command: env CODEX_HOME=/Users/you/.codex-detent-ollama codex app-server
      options:
        approval_policy: never
        thread_sandbox: workspace-write
        turn_sandbox_policy:
          type: workspaceWrite
          networkAccess: true
        turn_timeout_ms: 900000
        read_timeout_ms: 900000
        stall_timeout_ms: 300000
  routes:
    - name: local-validator
      role: validator
      backend: codex-local-ollama
      model: detent-qwen3-coder:30b
    - name: default
      backend: codex-standard
      model: gpt-5-codex
      default: true
```

If you skipped the Ollama alias because `qwen3-coder:30b` already loads with a
large enough context on your host, use `model: qwen3-coder:30b` in both Codex
and Detent.

Leave `gate.validator.model` unset when you want the validator route's `model`
to control the local pin. If `gate.validator.model` is non-empty, it overrides
the route model.

Run `detent doctor --port 0` before starting the service. For the first real
dispatch, use a trivial issue and verify the completed Codex session, worktree
diff, validation gate, and `ollama ps` context before routing more work to the
local backend.
