# Bootstrap On A New Machine (Humans And AI Agents)

[Back to README](../README.md#documentation)

A complete, ordered runbook to take a bare machine to a running Detent. Every
step has a verification command — do not proceed until it passes. An AI agent
can execute these steps top to bottom; replace each `<...>` placeholder. The
[`detent-orchestration`](https://github.com/digitaldrywood/detent-orchestration)
repo is a real, working instance of this setup to copy from.

1. **Install Detent.** `brew install digitaldrywood/tap/detent` (macOS/Linux),
   `go install github.com/digitaldrywood/detent/cmd/detent@latest`, or a
   platform installer from [Install](../README.md#install). Verify: `detent version`.

2. **Install and authenticate the GitHub CLI.** Install
   [`gh`](https://cli.github.com), then choose scopes for the tracker mode:

   ```sh
   # ProjectV2-backed board mode.
   gh auth login --scopes "repo,read:org,read:project,project"
   # For existing auth:
   gh auth refresh -h github.com --scopes "repo,read:org,read:project,project"

   # Boardless issue-field mode.
   gh auth login --scopes "repo,read:org"

   # Boardless label mode.
   gh auth login --scopes "repo"
   ```

   Verify the required classic PAT scopes independently. Boardless issue-field
   and label modes do not require `read:project` or `project`; label mode also
   does not require `read:org` unless another workflow setting needs it.

   ```sh
   gh auth status 2>&1 | rg '\brepo\b'
   gh auth status 2>&1 | rg '\bread:org\b'
   gh auth status 2>&1 | rg '\bread:project\b'
   gh auth status 2>&1 | rg "(^|[[:space:],'\"])project([[:space:],'\"]|$)"
   ```

   Use `github_token: gh` in `global.yaml` so Detent resolves this token at
   startup.

3. **Install and sign in to the Codex CLI.** Install the
   [OpenAI Codex CLI](https://github.com/openai/codex) and sign in. Detent
   dispatches every agent through `codex app-server`. Verify: `codex --version`.

4. **Choose the GitHub status source.** For the current/default compatibility
   path, choose the GitHub ProjectV2 board Detent will drive and get its node
   id (starts with `PVT_`):

   ```sh
   gh project list --owner <org-or-user> --format json --limit 50
   ```

   This verifies the token can read ProjectV2 boards. The write `project` scope
   is verified when Detent first performs an intentional board mutation. The
   board only needs to exist — Detent auto-provisions missing `Status` and
   `Priority` options on first run. The option names must match your
   `WORKFLOW.md` states, and Detent keeps known `Status` options in canonical
   board order.

   For boardless issue-field mode, skip ProjectV2 board creation and instead
   confirm the repository's organization has a single-select issue field named
   `Status`:

   ```sh
   gh api /orgs/<org>/issue-fields --jq '.[] | select(.name == "Status")'
   ```

   For boardless label mode, skip ProjectV2 board creation and organization
   issue-field setup, then confirm the repository has status labels with the
   configured prefix:

   ```sh
   gh api repos/<owner>/<repo>/labels --paginate --jq '.[].name' | rg '^detent:'
   ```

5. **Clone the repository you want Detent to work on** (its checkout becomes
   `workspace.source_root`):

   ```sh
   git clone <repo-url> <source-root>
   ```

6. **Author the project contract.** Copy the mode-specific template as a
   starting point, then edit it. For from-zero board creation, interview
   questions, issue intake, and the first-dispatch smoke test, follow
   [Project Onboarding](ONBOARDING.md):

   ```sh
   GITHUB_MODE="${GITHUB_MODE:?set GITHUB_MODE to project_v2, issue_field, or label}"
   curl -fsSL "https://raw.githubusercontent.com/digitaldrywood/detent/main/docs/templates/WORKFLOW.${GITHUB_MODE}.md" \
     -o <source-root>/WORKFLOW.md
   ```

   The maintained templates are
   [`WORKFLOW.project_v2.md`](templates/WORKFLOW.project_v2.md),
   [`WORKFLOW.issue_field.md`](templates/WORKFLOW.issue_field.md),
   [`WORKFLOW.label.md`](templates/WORKFLOW.label.md), and the CLI-only
   local-status template
   [`WORKFLOW.github_local.md`](templates/WORKFLOW.github_local.md).
   Non-code artifact workflows can start from
   [`WORKFLOW.non_code_artifact.md`](templates/WORKFLOW.non_code_artifact.md).
   A runnable local example with seeded work items, artifact metadata, API
   payloads, and artifact-gate transitions lives in
   [`docs/examples/non-code-artifact`](examples/non-code-artifact).
   Existing GitHub-backed `WORKFLOW.md` copies must be migrated manually:
   require an open pull request that is marked ready for review and is not a
   draft before declaring `status: complete`, use `gh pr ready <number>` as the
   remedy, and verify `gh pr view <number> --json isDraft --jq '.isDraft'`
   returns `false`. Known deployed copies requiring this audit include
   `digitaldrywood/video-studio` (tracked by
   [video-studio#57](https://github.com/digitaldrywood/video-studio/issues/57)),
   `digitaldrywood/ghostreel`, and the `hostlet.click` workflow.
   They set `server.kanban.mode: integration` for trusted project boards;
   change that to `read_only` only for an observer or shared dashboard,
   explicit no-writes choice, or failed post-authorization write probes. They
   also include a `## Required Execution Flow` with `For Todo`, `For In
   Progress`, `For Rework`, and `For Merging` sections so merge workers have a
   terminal instruction: invoke `$go-workflow:ship`, merge and move the issue to
   `Done`, move it to `Rework` with an actionable defect, or leave it in
   `Merging` with a concrete external blocker recorded. Before dispatching
   `Merging`, confirm the Detent host's Codex environment exposes
   `$go-workflow:ship`; otherwise install or enable that workflow, or replace
   the `For Merging` section with equivalent project-local merge instructions.

   For ProjectV2 mode, set `tracker.project_slug` (your `PVT_` id). For
   boardless issue-field mode, set `tracker.github_status_source:
   issue_field`, `tracker.repository: <repo-owner>/<repo-name>`, and optionally
   `tracker.status_field`. For boardless label mode, set
   `tracker.github_status_source: label`, `tracker.repository:
   <repo-owner>/<repo-name>`, and `tracker.status_label_prefix`. For
   externally owned or open-source repositories where Detent must not pollute
   issues, labels, fields, Projects, or issue comments, use
   `tracker.kind: github_local` from `WORKFLOW.github_local.md`; do not set
   `tracker.github_status_source`, configure `tracker.local_sqlite.path`, and
   import explicit issue numbers with
   `detent github-local import <project-id> <issue-number> --state Todo`. In
   every mode, set `workspace.source_root` (`<source-root>`), `workspace.root`
   (a worktrees directory), `write_probe_issue` for ProjectV2 or issue-field
   status-write proof, and the prompt body. In label mode, set
   `write_probe_issue` only when using legacy/deep issue-object probes.
   If the repository already has a `WORKFLOW.md`, audit its prompt body for the
   same Required Execution Flow and `Current Detent status: {{ issue.state }}`
   line before dispatching Detent against it.
   Registered projects can use `github_token: gh` in `global.yaml`; leave
   `tracker.api_key` out of the workflow unless you are intentionally using a
   workflow-local token. The full field reference is in [Quick Start](getting-started.md#quick-start).

   Interactive alternative: when Detent starts without a resolved `global.yaml`
   and without a `WORKFLOW.md` in the current directory, it serves the
   `/onboarding` web wizard. Open `http://localhost:<port>/onboarding` to walk
   through tracker, credentials, project, agent, and write steps for generating
   `WORKFLOW.md`. The first step asks for ProjectV2 board, organization issue
   field, or repository labels; label-mode users should choose **Repository
   labels**, then enter the repository and status label prefix such as
   `detent:`. The wizard hides ProjectV2 fields for boardless modes. Then
   return to the runbook for board creation, global registration, issue intake,
   and the smoke test.

7. **Create global config and register the project:**

   ```sh
   detent init
   detent add-project --id <id> \
     --workflow <source-root>/WORKFLOW.md \
     --workdir <source-root>
   ```

   Edit the resolved `global.yaml` and set `github_token: gh` with any desired
   `env`, `log_level`, and `port` overrides.

8. **Verify everything:**

   ```sh
   detent doctor --allow-write-probes
   ```

   Every check must pass before starting Detent. If Detent is already running on
   the configured port, the server-port check may fail because the live service
   owns the port. In that case, validate the rest of the setup without the port
   collision, then verify the running service:

   ```sh
   detent doctor --port 0 --allow-write-probes
   curl -fsS http://127.0.0.1:4000/health | jq -e '.status == "ok" and .mode == "running"'
   ```

9. **Start Detent and confirm the dashboard:**

   ```sh
   detent --host 127.0.0.1 --port 4000
   ss -ltnp | rg ':4000|detent'
   curl -fsS http://127.0.0.1:4000/api/v1/state
   ```

   Keep `127.0.0.1` for SSH tunnels. For VPN access, use the selected private
   or Tailscale IP instead and verify it from another machine:

   ```sh
   detent --host <tailscale-or-private-ip> --port 4000
   curl -fsS http://<tailscale-or-private-ip>:4000/api/v1/state
   ```

   Use `--host 0.0.0.0` only when every host interface is trusted for dashboard
   access; it is not limited to Tailscale.

10. **Dispatch work.** Move an issue to `Todo` through the configured status
    source: ProjectV2 `Status`, issue-field `Status`, the `detent:todo` status
    label, or the local SQLite state used by `github_local`. Detent claims it,
    creates an isolated worktree, dispatches an agent, and the issue appears
    under Running on the dashboard. Drive the rest through the configured
    status source (`Todo` → `In Progress` → `Human Review` → `Merging` →
    `Done`).
