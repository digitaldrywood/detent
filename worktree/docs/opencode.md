# OpenCode backend

Detent runs OpenCode v2 through ACP version 1 over a private subprocess's
stdin/stdout. It does not use Codex or Claude Code as an OpenCode proxy.
The implementation is tested against OpenCode v2.1.3.

## Authentication

Install OpenCode v2 on each runner and authenticate the runner service account
with `opencode auth login`. The ACP process uses OpenCode's provider credentials.
A browser login performed by another OS user does not authenticate the service.
Do not place credentials in issues, route definitions, or repository files.

`opencode auth list` lists connection labels without exporting secrets.
`opencode models --standalone` lists available provider/model identifiers.
For OpenCode Go, select `opencode-go/deepseek-v4.1-flash` explicitly; the
`opencode/` provider is a different route. OpenCode Console authentication is
sufficient for the Go provider; a separate DeepSeek API key is unnecessary.
DeepSeek Go models require Global regions in the workspace's Privacy settings.
Choosing that setting is an operator decision, not something Detent changes.

A Console login or model catalog entry does not prove remaining subscription
capacity. Go Plus, weekly and five-hour limits remain provider-owned. Detent
neither purchases a plan nor enables paid overage.

## Region-policy refusals

The Privacy control is in the OpenCode Console website, not local CLI settings:
select the authenticated workspace, then Settings → Privacy → Regions → Global.
If Privacy is missing from the sidebar, append `/privacy` to that workspace's
`/settings` URL. This choice allows the model's hosting region; approve it only
under the operator's data policy. Choosing another allowed model is an alternative.

The backend recognizes OpenCode's Global-region refusal, fails the current run,
and preserves an actionable settings message on the existing instance-capacity
failure path. It does not classify this as a code defect or create a tracker
Blocked lane. Existing capacity recovery/backoff still applies; this is not a
new permanent hold or an automatic change to provider privacy settings.
Doctor's binary/authentication checks cannot prove model-specific region access.
Use the explicit smoke test to verify it; catalog visibility alone is insufficient.

## Select issues for DeepSeek implementation

Use the approved project's existing backend and route settings. Preserve its
other routes, backend commands, Cloud model policy, and validation gates.
This fragment illustrates the relevant additions; it is not a replacement
for a complete project policy:

```yaml
agents:
  backends:
    - id: opencode
      kind: opencode
      protocol: acp
      command: opencode
      options:
        turn_timeout_ms: 1800000
        stall_timeout_ms: 300000
    - id: codex
      kind: codex
      command: codex app-server
  routes:
    - name: deepseek-code
      role: code
      backend: opencode
      model: opencode-go/deepseek-v4.1-flash
      selector:
        labels:
          include: [pilot:deepseek]
    - name: deepseek-rework
      role: rework
      backend: opencode
      model: opencode-go/deepseek-v4.1-flash
      selector:
        labels:
          include: [pilot:deepseek]
    - name: astra-review
      role: validator
      backend: codex
      model: gpt-6-astra
      default: true
    - name: astra-security
      role: security_audit
      backend: codex
      model: gpt-6-astra
      default: true
    - name: default-code
      role: code
      backend: codex
      default: true
```

Cloud organization/project settings own effective model selection. Keep their
Codex policy scoped to Codex; the OpenCode routes use exact provider/model IDs.
A Codex Cloud stage policy can supersede a route's Astra model, so verify its
validator and security_audit selections in the approved effective policy.
Do not put a blanket `model` in an issue's `detent-agent` block: that overrides
review stages too. No special label has an effect until its route is approved.
Removing the pilot label sends subsequent coding/rework to existing defaults.
This integration does not add an automatic correction-count fallback to Sol.

## Execution contract

- Native-trusted execution only. Sandboxed, read-only, text-only, and enforced
  subscription-auth requests fail before launch. OpenCode configuration,
  plugins, and provider credentials are trusted runner inputs, not a sandbox.
- Each turn creates a new session at its exact workspace. OpenCode's directory-
  bound persisted sessions are not resumed. Corrections use Detent's published
  source and current findings, preserving placement on any eligible runner.
- Scoped worker tools use a per-turn authenticated loopback MCP server. The
  server closes with the turn; its token travels through ACP, not command-line
  arguments or repository configuration. Tool calls retain Detent's existing
  authorization handler.
- ACP permission requests receive only an `allow_once` selection in a trusted
  coding turn. Unknown client methods are rejected. This does not override an
  OpenCode policy denial.
- Model selection is acknowledged and checked before the prompt. An unavailable
  model or unsupported explicit effort fails instead of silently using another
  model. Issue-body model/effort overrides need a backend catalog, which this
  initial adapter does not advertise; use exact route models.
- Context cancellation, configured turn duration, stream stall detection, and
  process-group cleanup bound the process lifetime. ACP does not expose a
  per-model-call counter; Detent's `MaxTurns` cannot bound internal OpenCode
  tool/model iterations. Use the existing duration/spend policies and provider
  limits; no new retry policy is introduced.
- Protocol, launch and provider request errors use the existing instance capacity
  recovery path. There is no stable ACP quota-status read. Readiness reports
  authentication as unknown until a real request verifies it.
- Live text and tool lifecycle events are recorded. Persisted OpenCode history
  hydration is unsupported; session IDs are never looked up in Codex logs.
- End-turn token usage is provider-reported experimental ACP data. Child-session
  usage is excluded by OpenCode. Detent normalizes cache reads/writes into input
  and reasoning into output, retaining cached/reasoning subset counters without
  adding them to the total again. Session context-size updates are not additional
  tokens. These values do not establish Go allowance debits or actual charges;
  no subscription cost is inferred from them. Interrupted turns may have no
  terminal usage report; missing counters must not be interpreted as zero
  provider consumption.

## Explicit smoke test

The default unit suite uses fake ACP processes and makes no provider requests.
To exercise the installed CLI, authentication, a real model and a scoped Detent
MCP tool, explicitly opt in:

```sh
DETENT_OPENCODE_SMOKE_MODEL=opencode-go/deepseek-v4.1-flash \
  go test -timeout=150s ./internal/opencode -run '^TestOpenCodeLive$' -v
```

This creates a temporary workspace and requests one tool call and a fixed
response. It consumes provider capacity but does not change live routing or
launch board work. The check does not establish real issue acceptance, review
rounds, throughput, or subscription endurance. A selected-issue pilot should
record the full implementation → Astra review → correction → verification loop,
including fallbacks, operator interventions and provider allowance snapshots.

## Protocol references

- [OpenCode ACP](https://dev.opencode.ai/v2/docs/cli/acp/)
- [OpenCode commands and authentication](https://dev.opencode.ai/v2/docs/cli/commands/)
- [OpenCode MCP servers](https://dev.opencode.ai/v2/docs/mcp-servers/)
- [ACP end-turn usage draft](https://agentclientprotocol.com/rfds/end-turn-token-usage)
