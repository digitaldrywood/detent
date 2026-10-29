# Antigravity backend

Detent can run the Antigravity CLI (`agy`) as an external worker through its
print-mode stream protocol. Install `agy` on the worker host and sign in by
running it once interactively. Detent launches the unmodified CLI, which keeps
its own sign-in; Detent never reads or forwards Antigravity credentials. Use of
an Antigravity subscription is governed by Google's Antigravity terms.

Configure the backend and select it with existing routes:

```yaml
agents:
  backends:
    - id: antigravity-worker
      kind: antigravity
      protocol: stream-json # default for this kind
      command: agy          # default for this kind; wrappers are supported
      provider: google
      options:
        effort: high
        turn_timeout_ms: 3600000
  routes:
    - name: antigravity-implementation
      role: code
      backend: antigravity-worker
      model: gemini-4-argon-high # use an exact ID from `agy models`
      default: true
```

Each turn runs `agy --input-format stream-json --output-format stream-json
--dangerously-skip-permissions --disable-slash-commands --print=` in the issue
workspace, with `--model` and `--effort` from the route and options. The prompt
is written to standard input as one `user` message, so prompt size is not
limited by the command line. `provider` labels the runtime identity only.

`effort` accepts `low`, `medium`, `high`, `xhigh` and `max`; a role's reasoning
effort overrides it. `shell` selects the configured command's shell.
`stall_timeout_ms` optionally limits time without stream records; zero disables
it. The existing turn and session limits still apply, and the turn limit counts
model responses that report usage.

Antigravity requires the operator's **native-trusted** runner isolation tier.
Detent refuses sandboxed and read-only requests before launching `agy`, so
read-only roles such as validation need another configured backend. Detent
dynamic tools, image attachments and resume are not supported: Antigravity
conversations are machine-local CLI state, so corrections start fresh from
published source and findings.

Detent records the conversation ID, requested model, effort, streamed text,
tool lifecycle and output, per-response usage and the terminal outcome. Input
tokens include cache reads, and output tokens include thinking tokens. A result
other than `SUCCESS` fails the turn with the CLI's error. Launch failures,
malformed or truncated streams, and exits before a result are attributed to the
backend instance.

An opt-in smoke test runs one turn through an installed, signed-in CLI:

```sh
DETENT_ANTIGRAVITY_SMOKE=1 DETENT_ANTIGRAVITY_MODEL=gemini-3.8-flash-low \
  go test ./internal/antigravity -run TestRealAntigravitySmoke -count=1
```
