# Antigravity backend

Detent can run the Antigravity CLI (`agy`) as an external worker through its
print-mode stream protocol. Detent launches the unmodified CLI, which keeps its
own sign-in; Detent never reads or forwards Antigravity credentials. Use of an
Antigravity subscription is governed by Google's Antigravity terms.

Workers never use the operator's own Antigravity home. Create a dedicated
directory for the runner and sign in once with that directory as `HOME`:

```sh
mkdir -p /srv/detent/agy-home
HOME=/srv/detent/agy-home agy
```

Configure the backend with that directory and select it with existing routes:

```yaml
agents:
  backends:
    - id: antigravity-worker
      kind: antigravity
      protocol: stream-json # default for this kind
      command: agy          # default for this kind; wrappers are supported
      provider: google
      options:
        home: /srv/detent/agy-home
        effort: high
        turn_timeout_ms: 3600000
  routes:
    - name: antigravity-implementation
      role: code
      backend: antigravity-worker
      model: gemini-4-argon-high # use an exact ID from `agy models`
      default: true
```

`home` is required and must be absolute. Every turn runs with `HOME` set to it,
so the operator's Antigravity instructions, MCP servers and transcripts are
never read or written. Concurrent workers share the dedicated home.

Each turn runs `agy --input-format stream-json --output-format stream-json
--dangerously-skip-permissions --disable-slash-commands --print=` in the issue
workspace, with `--model` and `--effort` from the route and options. The prompt
is written to standard input as one `user` message, so prompt size is not
limited by the command line. `provider` labels the runtime identity only.

Detent worker tools are served from a per-turn loopback MCP server with its own
bearer token. Detent writes that server to `.agents/mcp_config.json` in a
private directory under the worker scratch directory, passes the directory with
`--add-dir`, and removes it when the turn ends. The repository worktree and the
dedicated home's MCP configuration are not modified.

`effort` accepts `low`, `medium`, `high`, `xhigh` and `max`; a role's reasoning
effort overrides it. `shell` selects the configured command's shell.
`stall_timeout_ms` optionally limits time without stream records; zero disables
it. The existing turn and session limits still apply, and the turn limit counts
model responses that report usage.

Antigravity requires the operator's **native-trusted** runner isolation tier.
Detent refuses sandboxed and read-only requests before launching `agy`, so
read-only roles such as validation need another configured backend. Image
attachments and resume are not supported; corrections start fresh from
published source and findings.

Detent records the conversation ID, requested model, effort, streamed text,
tool lifecycle and output, per-response usage and the terminal outcome. Input
tokens include cache reads, and output tokens include thinking tokens. Launch
failures, malformed or truncated streams, exits before a result, and results
other than `SUCCESS`, such as a missing sign-in or an unknown model, are
attributed to the backend instance.
