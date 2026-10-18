# Sprites Cloud Runners Spike

Status: spike, 2026-09-30. The output of this document is a viability verdict and
a cost model, not code to keep. Everything built for it is throwaway and is
listed at the end.

## Question

Can Detent Cloud replace the "enroll a machine and keep it polling" runner
model with [Fly.io Sprites](https://fly.io/sprites/): the Hub creates a Linux VM
per work attempt from a project-registered image, runs the agent in it, lands the
result, and lets the VM disappear, so a project scales to zero between issues and
to its peak during a burst without anyone owning a host. What would that cost for
Detent's own workload, and is it viable today?

## Recommendation

Viable now, with one platform caveat. Build it as a per-project pool of named,
sleeping sprites that the Hub wakes by `exec`, resets with an in-place checkpoint
restore between issues, and lets pause again. Do not wait for a "create sprite
from image" primitive; it does not exist in the public API today, and the pool
gives the same instant-up, work, instant-down behavior at near-zero idle cost.

Compute for Detent's busiest month on record is between roughly $90 and $270,
which is below the Hub's Starter tier and one to three percent of the provider
spend for the same work. The customer brings their own Sprites organization token
and their own model access, either a subscription login captured in the project's
checkpoint or an API key behind a Sprites connector, so Detent resells nothing.

## What Sprites is (verified)

Sources: [Working with Sprites](https://docs.fly.io/sprites/working-with-sprites/),
[Lifecycle and persistence](https://docs.fly.io/sprites/concepts/lifecycle.md),
[Checkpoints](https://docs.fly.io/sprites/concepts/checkpoints.md),
[Connectors](https://docs.fly.io/sprites/concepts/connectors.md),
[Keeping a Sprite running](https://docs.fly.io/sprites/keeping-sprites-running.md),
[Fly's sandbox pricing comparison](https://fly.io/learn/ai-sandbox-pricing/),
[Fly pricing](https://fly.io/pricing/),
[Go SDK](https://pkg.go.dev/github.com/superfly/sprites-go),
[Design and implementation](https://fly.io/blog/design-and-implementation/).

- A sprite is a Firecracker VM (Ubuntu 26.04, 8 vCPU, memory autoscaled by the
  platform, 100 GB disk) created by `POST /v1/sprites` with only a name. CPU,
  memory and region are not user-settable in the documented API.
- It pauses about 30 seconds after the last command, session, open connection or
  service. Wake is 100 to 500 ms from a warm pause and 1 to 2 s from cold.
- Billing is consumed CPU seconds from `cpu.stat` and consumed memory GB-hours,
  not allocation: $0.07 per CPU-hour, $0.04375 per GB-hour, $0.50 per GB-month for
  written blocks while hot, $0.02 per GB-month while cold. A paused sprite is
  billed for storage only. Bandwidth is not metered. Nothing is charged per sprite.
- Checkpoints are copy-on-write filesystem snapshots. Restore is in place, into
  the same sprite. A checkpoint cannot be restored into a different sprite, so
  there is no image or template primitive yet. A third party hit the same wall on
  2026-09-28 and filed it as a platform request
  ([fountain#2530](https://github.com/managoat/fountain/issues/2530)).
- Connectors hold provider credentials in the organization. Processes inside a
  sprite call `https://api.sprites.dev/v1/gateway/<provider>/...` and the gateway
  injects the key based on the calling sprite's signed identity. Anthropic and
  OpenAI are bring-your-own-key connectors; the gateway is the documented way for
  Claude Code inside a sprite to reach a model without a key on disk.
- Claude Code and Codex CLIs, `gh`, Go 1.25, Node, Python and the usual toolchains
  are preinstalled. A project bootstrap still needs its own Go toolchain
  (Detent targets Go 1.26 and above) and `make setup`.
- The Go SDK mirrors `os/exec`: `sprite.CommandContext(ctx, name, args...)` with
  `StdinPipe`, `StdoutPipe`, `CombinedOutput` and `ExitCode`. The filesystem API
  reads and writes files without a shell. A Tasks API keeps a sprite awake for a
  named unit of work with a one-hour lease and heartbeat refresh.
- The same API is reimplemented by [wisp](https://github.com/jhgaylor/wisp) on a
  single Linux host with KVM, and the official CLI and SDKs run against it
  unmodified, so a Sprites backend in Detent is not Fly-only.

## Measured on 2026-09-30

Go SDK probe, one fresh sprite, all steps sequential, from a laptop on the
public internet:

| Step | Seconds |
| --- | ---: |
| Create sprite | 0.50 |
| First exec (`uname`, `nproc` = 8) | 0.27 |
| Exec returning exit code 7 (propagated) | 0.30 |
| Shallow clone of detent (95 MB) | 3.27 |
| Checkpoint create | 0.67 |
| Checkpoint restore (in place, dirty tree back to clean) | 14.59 |
| Status after 50 s idle | `warm` (paused) |
| First exec after pause | 0.19 |
| Delete sprite | 0.39 |

Through the CLI (`sprite exec`), stdin lines, streamed stdout and non-zero exit
codes all pass through unchanged, so a process factory that runs
`sprite exec -s NAME -- claude ...` behaves like a local `exec.Cmd`.

Project bootstrap in the sprite (cold caches):

| Step | Seconds |
| --- | ---: |
| Download and unpack the latest Go toolchain | 3 |
| Shallow clone | 3 |
| `go mod download` | 13 |
| `go build ./...` | 54 |
| `make setup` (Go 1.26 toolchain, golangci-lint, templ, sqlc, goose, Node deps) | 358 |
| `make check-fast` through invariants, migrations, generated files, the Vite app build and tests, and lint | 845 |

CPU actually consumed, read from the sprite's cgroup `cpu.stat` the way Fly
bills it: 674 CPU-seconds for `make setup` and 1,476 CPU-seconds for
`check-fast` up to lint, with a 5.8 GB memory peak. Lint stopped the run on a
pre-existing staticcheck finding in `develop`
(`internal/hubserver/hosted_storage.go:41`, QF1001, filed as [#3391](https://github.com/digitaldrywood/detent/issues/3391)), so `test-fast` was run
separately: 17 packages ok, 0 failed, 47 s wall, 177 CPU-seconds. At $0.07 per CPU-hour and $0.04375 per GB-hour the
complete gate (`check-fast` plus `test-fast`) costs about 6 to 7 cents of compute (1,653 CPU-seconds plus roughly 0.7 GB-hours)
per run, and the one-time bootstrap about $0.02.

Restore, second look. A restore first snapshots the current state as an
automatic `pre-restore` checkpoint, then swaps the filesystem and restarts the
environment. On a fresh sprite with a 2 GB tree, checkpoint took 0.5 s and an
immediate restore succeeded. On the bootstrapped `detent-spike` sprite (about
7 GB written, one checkpoint and one restore already done), a checkpoint created
in 1.0 s was followed by a restore that failed with

```
BackupActiveCheckpoint failed: JuiceFS rename clone: rename
/dev/fly_vol/juicefs/data/checkpoints/v3.in-progress
/dev/fly_vol/juicefs/data/checkpoints/v3: file exists
```

and after that every checkpoint create and restore on that sprite fails with the
same error; the environment upgrade endpoint had nothing to publish for its
release lane. Exec, files and the agents keep working, so the sprite is usable
but can no longer be reset by checkpoint. This is a platform bug to report with
the sprite id and timestamps. For the pool design it means a member whose reset
fails is deleted and re-bootstrapped rather than repaired; the pool already has
to tolerate that for any other failure, and a `git checkout -- . && git clean
-fd` reset covers the common case without touching checkpoints at all.

Landing a change without any repository credential in the VM: a commit made
inside the sprite was exported with `git bundle`, read out through
`GET /v1/sprites/NAME/fs/read?path=...` (477 bytes, HTTP 200) and verified
against the host's `develop` with `git bundle verify`. The Hub, which already
holds the customer's repository authority, applies it; the sprite never sees a
GitHub token.

Agent turns inside the sprite, non-interactive, each asked to append a line to
a file and commit it:

| Agent | Auth | Wall seconds | Result |
| --- | --- | ---: | --- |
| Claude Code 2.1.251 (preinstalled) | Claude subscription OAuth credential copied from the operator's machine | 11.3 (3 turns) | commit `ad99ba1` |
| Codex CLI 0.159.2 (`npm i -g @openai/codex`, 8 s) | ChatGPT subscription `auth.json` copied from the operator's machine | 34.6 (3 commands) | commit `88d3b97` |

Both subscription logins worked from a new host, so the "device key" login
(`claude auth login`, `codex login`) can be done once when a pool member is
bootstrapped and captured in the project's clean checkpoint. An API-key
connector is the alternative for customers who meter through a provider account.
Neither path needs Detent to hold a model key.

Sprite image gap found on the way: the preinstalled Codex (0.151.0) fails every
tool call with `failed to spawn code-mode host
~/.local/share/sprite-agents/codex/codex-code-mode-host: No such file or
directory`. The stock npm build works. Worth reporting to the Sprites team.

## Detent's own workload, September 2026

Source: the dogfood instance's `work_attempts` table, 2026-09-01 through
2026-09-30 13:30 UTC (708 hours).

| Project | Attempts | Attempt-hours | Mean minutes |
| --- | ---: | ---: | ---: |
| detent | 2,045 | 856 | 25 |
| parable | 414 | 265 | 38 |
| gopher-ai | 181 | 77 | 25 |
| video-studio | 312 | 59 | 11 |
| three smaller projects | 141 | 43 | 17 |
| fleet | 3,093 | 1,299 | 25 |

Detent agent attempt durations: p50 20 minutes, p90 86, p99 205. Peak
simultaneous attempts: 13. In 346 of the 708 hours nothing was running at all,
so a fixed runner host sits idle 49 percent of the month while its peak need is
13 VMs. That shape is exactly what scale-to-zero compute is for.

## Cost model

Per attempt-hour, using Fly's published rates. Fly's own worked example for a
Claude Code session is 30 percent of two CPUs and 1.5 GB.

| Profile | CPU-hours per hour | GB | USD per attempt-hour |
| --- | ---: | ---: | ---: |
| Light (Fly's Claude Code average) | 0.6 | 1.5 | 0.108 |
| Heavy (two CPUs pegged, 4 GB) | 2.0 | 4.0 | 0.315 |
| Gate run (all 8 CPUs pegged, 8 GB) | 8.0 | 8.0 | 0.910 |

Applied to September:

| Scope | Attempt-hours | Light | Heavy | All gate |
| --- | ---: | ---: | ---: | ---: |
| detent project | 856 | $92 | $270 | $779 |
| whole fleet | 1,299 | $140 | $409 | $1,182 |

Real attempts are mostly agent think time (light) with short gate bursts
(heavy or all-CPU), so the expected bill sits between the first two columns.
Per 25-minute attempt that is 5 to 13 cents. The same attempts consumed the
equivalent of about $3.80 each in provider-metered tokens, so compute adds one
to three percent on top of model spend.

Storage: a pool of 13 sprites holding a clone, module cache and build cache of
about 7 GB each (measured: 0.5 GB clone, 2.8 GB module cache, 3.3 GB build cache) is under $3 per month cold and under $35 per
month if every one of them were hot for the entire month. Checkpoints only store
changed blocks.

Against the hosted catalog (Starter $49, Growth $149, Scale $399 per
organization per month), the compute for the busiest project Detent runs fits
inside Starter at the light profile and inside Growth at the heavy profile. The
customer pays Fly directly on their own organization; Detent's plan price does
not need to carry it.

What is not in the model: model spend (unchanged, still the customer's provider
account), and Fly's per-organization sprite concurrency limits, which are not
published and need an answer from Fly before promising a peak above 13.

## Integration design (per-attempt sprites, deferred)

This is the per-attempt shape the spike set out to test. It is sound, and it is
deferred behind the runner-in-sprite path in "Path to production" below because
the code trace showed the runner host also owns git, landing and merge
validation, all of which would move too. Detent already has the seams. Verified
in the tree at `develop`:

- `internal/claudecode` takes `Options.CommandFactory func(ctx) *exec.Cmd`, and
  `internal/codex` takes the same shape through `NewLocalTransportFactory`. For a
  spike, a factory that returns `exec.CommandContext(ctx, "sprite", "exec",
  "-s", name, "--", "claude", ...)` needs no other change; stdin, stdout and exit
  codes were verified above. For production, a small `Cmd` interface satisfied by
  both `*exec.Cmd` and the SDK's `*sprites.Cmd` removes the CLI hop.
- `internal/workspace.Backend` is `Create`, `Cleanup`, `BeforeRun`, `AfterRun`,
  `DiffStat`. A `SpriteWorkspace` backend maps one to one: `Create` wakes or
  creates the project's sprite from the pool, `BeforeRun` restores the project's
  clean checkpoint and fetches the branch, `DiffStat` runs `git diff --stat` by
  exec, `AfterRun` exports the bundle through the filesystem API, `Cleanup`
  restores again and lets the sprite pause.
- The Hub already owns claims and leases (`internal/hubclient/scheduler.go`) and
  a scoped worker API (`internal/hubserver/api_worker.go`). With sprites, the
  runner that polls the Hub disappears: the Hub's dispatcher holds the
  organization's Sprites token and drives the SDK directly. The Tasks API hold,
  refreshed on the lease heartbeat, replaces the runner's liveness.

Per-project configuration is three values: the customer's Sprites organization
token (stored as a Hub secret), a bootstrap script that turns a fresh sprite into
the project's clean checkpoint (toolchain, `make setup`, module download,
connectors to allow), and a pool ceiling. "Registering an image" is running that
bootstrap once per pool member and checkpointing; the clean checkpoint id is the
image. When Fly ships create-from-checkpoint the pool becomes optional and the
same checkpoint becomes a real template.

Mechanism moratorium note: this replaces the enrolled-runner path for Cloud
projects rather than adding a brake or lease. It reuses the existing claim,
lease and gate flow.

## Per-attempt compute cost attribution

Detent already records provider tokens and a notional USD per attempt in
`usage_events`, and `work_attempts.metrics_json` is the per-attempt sink. The
public Sprites API has no per-sprite usage endpoint (checked the documented
endpoints, the sprite object, and the in-sprite management socket), but Fly
states that CPU is billed from the cgroup's `cpu.stat` and memory from actual
usage. Those counters are readable inside the sprite at
`/sys/fs/cgroup/cpu.stat` (`usage_usec`) and `/sys/fs/cgroup/memory.current`,
so a runner can meter itself on exactly the quantities Fly invoices.

Shape: before the agent process starts, read `usage_usec`; while it runs, sample
`memory.current` every second; after it exits, read `usage_usec` again. CPU
seconds and average GB times wall hours, at the published rates, give the
attempt's compute USD. Written to `metrics_json` next to the token counts, it
rolls up per PR, per issue and per project like tokens do today, and the fleet
report gains a "compute per merged PR" column next to "tokens per merged PR".

Measured with that meter in a fresh sprite:

| Work | Wall s | CPU s | Avg GB | Compute USD | Provider USD |
| --- | ---: | ---: | ---: | ---: | ---: |
| One Claude Code turn (read a doc, answer; 41,741 tokens in, 170 out) | 10.1 | 3.4 | 0.43 | 0.00012 | 0.2255 |
| Cold `go build ./...` of detent, 8 vCPU | 45.7 | 204.9 | 2.33 | 0.00528 | n/a |

For a think-heavy agent turn the compute is one two-thousandth of the provider
charge. Gate runs are where the compute goes, and even a cold full build is half
a cent.

Implementation (#3409) meters the worker's unified cgroup around each agent
turn, samples memory every second, and integrates sampled bytes over elapsed
time (including the final partial interval). Memory prices use decimal GB.
`worker.compute_rates` overrides `cpu_hour_usd` and `memory_gb_hour_usd` per
host (`local` or the configured SSH destination); omitted prices inherit
[Sprites public prices](https://fly.io/sprites/). Explicit zero is accepted.
No counters, malformed counters, or a reset during a turn leave compute
unavailable. Historical and macOS attempts are not backfilled with estimates.

Counters measure the whole worker cgroup, including agent tools. For attributable
per-attempt values the host must dedicate that cgroup to the worker; overlapping
attempts in a shared cgroup include each other's consumption. This change does
not create cgroups or alter concurrency/dispatch. Report totals include measured
sessions only and display their count; token budgets remain token-only. Compute
is a rate-based resource estimate, excluding storage, allowances and invoice
adjustments, not an invoice reconciliation.

Storage is not per attempt; charge the pool's written blocks to the project at
month end. Monthly reconciliation against the Fly invoice catches drift in the
published rates. A per-sprite usage endpoint from Fly would make that
reconciliation exact and is worth asking for.

## Path to production

Tracing the runner at `develop` changes the shortest path. Today the runner
host does everything: `internal/cli/runner.go` builds the Codex and Claude
commands with `cmd.Dir` set to the worktree, `workspace.LocalGit` owns the git
worktrees, the agent itself runs `git push` and the gate command inside its
turn, native Cloud projects deliver diffs to the Hub over
`POST .../attempts/:attempt/diff`, and `LocalGit.LandChange` pushes the reviewed
head from the runner's own checkout. Nothing in the tree executes remotely, and
the Hub never runs git. Moving each of those seams into a remote VM is real
work. Moving the whole runner into the VM is not: the spike already ran
`make setup`, `make check-fast`, both agents and git inside a sprite as an
ordinary Linux host.

So the production shape is a sprite-hosted runner, provisioned and scaled by the
Hub, rather than a per-attempt remote-exec rewrite:

1. Enroll a Detent runner inside a sprite with the documented `detent hub
   runner register` flow and keep it alive as a sprite Service. The runner
   polls the Hub as it does on any host. Idle cost is the runner process
   itself: measured on the operator's Cloud runner at about 180 MB resident and
   0.1 percent of a core, which at Sprites' consumed-resource rates is under $6
   a month per runner, so "scale to zero" is a few dollars, not zero, and the
   pool floor can be zero anyway.
   Work: [#3407](https://github.com/digitaldrywood/detent/issues/3407).
2. Store the customer's Fly Sprites organization token in the Hub as a
   write-only per-project secret.
   Work: [#3408](https://github.com/digitaldrywood/detent/issues/3408).
3. Let the Hub create, bootstrap, enroll and delete those sprite runners between
   a floor and a ceiling from the project's Todo depth and free capacity. A
   sprite runner is an ordinary runner with a label; the existing identity,
   lease and routing tables carry it. No new brake or reason code: a failed
   bootstrap marks the sprite for deletion and the next scale-up retries.
   Work: [#3410](https://github.com/digitaldrywood/detent/issues/3410).
4. Meter compute per attempt from the cgroup counters and show it next to
   tokens. This works on any Linux runner today, sprite or not.
   Work: [#3409](https://github.com/digitaldrywood/detent/issues/3409).
5. Luna-guided onboarding: an admin tells Luna to run the project on Sprites,
   Luna collects the token through the secret store, sets floor and ceiling,
   triggers the first sprite, watches the bootstrap and reports which provider
   login is still needed inside it. The rough edges hit by hand in this spike
   (Go version, the broken preinstalled Codex, the Service requirement, the
   provider login) are the script for that conversation.
   Work: [#3411](https://github.com/digitaldrywood/detent/issues/3411).
6. Per-attempt sprites through a remote execution seam stay in Backlog until
   the pool is in use and shows a need for per-attempt isolation.
   Work: [#3412](https://github.com/digitaldrywood/detent/issues/3412).

With 1 and 2 done, a project with a Fly token runs on Sprites for real, by hand.
With 3 it scales itself. With 5 a new organization gets there through a chat.

Runner credential note: the runner identity renews only while running
([#3382](https://github.com/digitaldrywood/detent/issues/3382)), so a paused
sprite runner that sleeps past a day must be re-enrolled; the Service keeps it
awake, and the pool deletes rather than pauses idle members above the floor.


## Dots versus Detent on Sprites

OpenAI announced Dots at DevDay on 2026-09-29: always-on agents inside ChatGPT,
each with "its own cloud computer and browser", rolling out to Pro and Business
Premium with the first dot included and further dots priced later. Sources:
[TechCrunch](https://techcrunch.com/2026/09/29/openai-launches-dots-its-bubbly-agentic-avatar/),
[SiliconANGLE](https://siliconangle.com/2026/09/29/openai-launches-dots-always-on-ai-agents-in-chatgpt-with-their-own-cloud-computers/),
[DataCamp](https://www.datacamp.com/blog/openai-dots).

| | Dots | Detent on Sprites |
| --- | --- | --- |
| Model | GPT-6 Astra only | Any backend Detent has: Codex, Claude Code, or any OpenAI-compatible endpoint through the Genkit backend; switch per project or per issue |
| Computer | OpenAI-owned, spec undisclosed, no BYO compute | Customer's Fly organization, or a self-hosted wisp box; plain Ubuntu the customer can inspect, checkpoint and restore |
| Keys | OpenAI account | Customer's own subscription login or API key behind a Sprites connector |
| Repo and tools | OpenAI plugin ecosystem; Codex cloud environments for code | Customer's git repository, `make check-fast`, their own gates and merge policy |
| API | None announced | Sprites REST and SDKs; Detent Hub API |
| Work model | Chat-assigned tasks, agent decides what to do next | Issue tracker is the queue; scheduler, leases, gates and merge train decide |
| Cost | Bundled with a ChatGPT plan, extra dots TBD | Metered compute on the customer's Fly bill plus their own model spend |

The one-line version: a dot is a model with a computer attached, and the model is
the product. Detent on Sprites is a computer with whichever model the customer
chooses attached, and the workflow is the product. Swapping GPT-6 for Claude in
Detent is a config change; leaving Dots means leaving ChatGPT.

## Open questions for Fly

- Create-from-checkpoint or a template primitive. Without it every pool member
  is bootstrapped once; with it the pool is unnecessary.
- Per-organization concurrency ceiling for sprites, and whether region can be
  pinned for customers with data-residency needs.
- Whether the connector gateway can front git over HTTPS for GitHub, so a sprite
  can push without a token. The bundle path above works without it.
- A per-sprite usage endpoint (CPU seconds, GB-hours, blocks written) so the
  self-metered numbers above can be reconciled against the invoice per sprite.
- The preinstalled Codex build is missing its code-mode host binary (see above).
- The wedged checkpoint store on `detent-spike` (`v3.in-progress` rename
  collision) and whether a sprite can recover from it without deletion.

Not a Fly question, but open: subscription logins for Claude Code and Codex copied
into a sprite worked today. Whether each provider keeps allowing a subscription
session from a cloud host is provider policy; the API-key connector path does not
depend on it.

## Throwaway artifacts

- `spritespike` Go program in the session scratchpad: create, exec, checkpoint,
  restore, idle, wake, delete, with timings. Not kept.
- Sprite `detent-spike` with the bootstrapped detent tree (its checkpoint
  store is wedged, see above). Destroyed after the demo.
- Issues filed from this spike: #3391 (lint on develop), #3407 to #3412 (the
  path above).
