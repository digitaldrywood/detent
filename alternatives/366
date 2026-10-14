# Pi agent backend

Detent can run Pi as an external CLI through its supported
[`pi --mode rpc` JSONL protocol](https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/rpc.md).
Install and authenticate Pi on the worker host with the provider/model you intend
to use. No Node dependency is embedded in Detent. Use a CLI implementing
`get_state`, prompt disposition responses and `agent_settled`; older RPC versions
that only emit `agent_end` are unsupported. `pi --help` describes the installed
CLI's options.

Configure the backend and select it with existing routes:

```yaml
agents:
  backends:
    - id: pi-worker
      kind: pi_agent
      protocol: rpc       # default for this kind
      command: pi         # default for this kind; wrappers are supported
      provider: anthropic
      options:
        thinking_level: high
        tools: [read, bash, edit, write, grep, find, ls]
        turn_timeout_ms: 3600000
  routes:
    - name: pi-implementation
      role: code
      backend: pi-worker
      model: claude-sonnet-4-20250514 # use an exact ID from your Pi model catalog
      default: true
```

The provider field selects Pi's provider; route models and per-turn reasoning
effort use the existing model/role selection paths. Pi supports `off`, `minimal`,
`low`, `medium`, `high`, `xhigh` and `max` thinking levels where the selected model
supports them. Leave Pi out of automatic `agents.model_selection.backend_kinds`
in this initial version: the backend does not expose a Detent model catalog.

`options.tools` is an allowlist of the seven built-in tools above. Omitting it or
setting an empty list enables all seven. Pi's built-in tools are sufficient for
ordinary code workers. `session_dir` can override Pi's session storage directory;
otherwise Pi uses its native persistent session location. `shell` selects the
configured command's shell. `stall_timeout_ms` optionally limits time without
RPC records; zero disables it. The existing turn and session limits still apply.

Keep read-only roles such as validation on another configured backend.

Pi requires the operator's **native-trusted** runner isolation tier. It has no
supported OS sandbox contract. Detent refuses sandboxed and read-only requests
before launching Pi; tool selection is not a filesystem security boundary. The
adapter disables extensions, skills, prompt templates, and project trust-gated
configuration with `--no-extensions --no-skills --no-prompt-templates --no-approve`.
Pi still reads normal context files such as `AGENTS.md`; native-trusted execution
has the host permissions explicitly selected by the operator. Worker scratch
variables and toolchain caches use the existing runner environment unchanged.

Detent records the runtime provider, exact resolved model, thinking level, Pi
session ID, streamed text, tool lifecycle/output, usage and terminal outcome.
Completion waits for `agent_settled`, including automatic retries following
`agent_end`. Live usage snapshots are reported as they arrive; authoritative assistant
`message_end` usage is committed once per response without double counting; Pi cache reads/writes are included in Detent input tokens, with
cache reads also recorded as cached input. Reported reasoning tokens are already
included in output.

Initial limitations:

- Resume returns Detent's existing unsupported-resume error. Session IDs remain
  observability data until persisted session/backend/model context can be
  verified; no session is silently reopened or replaced.
- Detent dynamic tools, checkpoint/chat bridges, and live conversation control
  are not implemented. A request requiring supplemental tools is refused.
- Persisted Pi history hydration is staged follow-up work. Live activity is
  available; the history reader returns history-not-found for Pi instead of
  searching Codex logs.
- RPC does not report an authentication status or reliable provider capacity
  reset signal. Doctor checks the CLI, and runner provider-auth status stays
  pending. Provider error text is propagated without fabricated quota/auth
  classification. Launch/transport/protocol failures use the existing
  instance-owned backend-capacity outcome, including for local providers.

Offline fixtures cover RPC dispatch, correlation, strict LF framing (including
Unicode separators), text and tools, usage, retry completion, errors, callback
rejection, timeout/cancellation, and descendant cleanup. An opt-in smoke test
runs in a temporary isolated workspace with session storage inside it:

```sh
DETENT_PI_SMOKE=1 DETENT_PI_PROVIDER=anthropic \
  DETENT_PI_MODEL=claude-sonnet-4-20250514 \
  go test -p 4 ./internal/piagent -run '^TestRealPiSmoke$' -count=1
```

The smoke test requires an installed CLI and existing authentication, and can
incur provider usage. It never starts or mutates the live Detent instance.
