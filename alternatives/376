# Manual scale soak

Run this tool by hand for INV-21 evidence. It is never invoked by a merge,
release, scheduled workflow, Make target, or production service. It changes no
Hub or runner configuration. The control process launches isolated Go test
worker processes using the current checkout's real Hub and Cloud Entry code.
No production Hub, provider, credentials, S3 bucket, or running runner is used.

From the repository root, use Go 1.26 and Node 24, prepare the embedded app,
and choose an exact checkout commit:

```sh
make app
go run ./tools/scalesoak -pin "$(git rev-parse HEAD)" \
  -projects 2 -items 100 -attempts 1000 -runners 10 \
  -duration 10m -attachment-objects 16000 -sweep-passes 3
```

Set `TMPDIR`, `TMP`, or `TEMP` to an isolated scratch directory first if none
is set. All configuration, SQLite databases, compressed objects and reports
are written beneath that directory. The command prints `scale_soak_output`,
`fixture_root`, and JSON reports. Outputs remain there for inspection until
the scratch owner cleans them up. Detent workers must preserve their supplied
scratch location. Never commit those outputs.

For a large tenant, choose the dimensions independently:

```sh
go run ./tools/scalesoak -pin "$(git rev-parse HEAD)" \
  -projects 100 -items 10000 -attempts 1000000 -runners 1000 \
  -detail-bytes 8192 -duration 8h -attachment-objects 16000
```

Start small and budget disk space and seeding time before the overnight run.
Each historical attempt owns a released lease, a compressed detail object,
an attachment record and an archive reference. Projects, items and runners
are created through the existing authenticated fixture API. History is inserted
in batches of at most 100; object I/O happens outside write transactions.
`-seed-only` produces the requested tenant fixture and report without running
the fleet or entry-plane sweep. Item count must be at least both project and
runner counts, so each active runner gets a distinct item in its assigned
project. Detail bytes control the synthetic execution output per attempt.

The fleet uses real loopback HTTP against an ephemeral Hub listener. Each
runner enrolls with a separate synthetic identity and provider account,
registers, heartbeats, claims its item and starts a run. Active sessions then
heartbeat, renew their 90-second lease, and publish bounded batches of run
observations every 30 seconds with 20% jitter. Startup is spread across one
heartbeat interval. Sessions remain active for the observation window; this
measures a stationary active fleet, with claims during startup. A short run
may end before every runner's first renewal; use at least 70 seconds when
checking the whole lifecycle at production cadence.

The tenant report includes the requested dimensions, checkout commit, tracked
source diff digest, dirty flag, Go/platform identity, seeding time, request
counts, error rates, p50/p95/p99/max request latency, and writer hold per
request. Percentiles use fixed power-of-two microsecond histogram buckets and
are upper bounds; max is measured directly. Memory for timing samples is
constant per operation. Context cancellation at the end of the observation
window can appear in the error count and is retained in the first-error list.
A dirty report is candidate evidence, not evidence for the clean pinned commit.
Use a clean checkout for commit comparisons. Untracked files make a report
dirty but are not included in the tracked diff digest.

Health evaluation, board refresh, and the conversation attachment sweep run as
explicit maintenance passes concurrently with the fleet, once per minute with
jitter. The report labels these as explicit passes, rather than claiming they
are observations of the scheduled ticker. The background Hub workers remain
enabled. Board refresh uses its existing bounded maintenance budget; the
conversation sweep may have no expired uploads. These measurements exercise
one tenant database and do not establish total-fleet capacity at 10,000 tenants
or 10,000 runners. Repeat isolated tenant runs for aggregate host pressure;
this tool does not claim to reproduce the complete hosted deployment.

A second worker process measures the actual Cloud Entry organization attachment
retention/orphan sweep against isolated tenant Hubs and an in-memory S3
transport. It reseeds the requested number of abandoned objects before each
pass, reports duration percentiles and object-walk reader-lock observations,
and verifies the objects were removed. It runs after the fleet measurement,
so this report isolates entry-plane sweep cost rather than claiming sweep and
fleet contention were measured together. It excludes network/object-store
latency. Hub detail archives use actual compressed files on local disk. Neither
storage mode predicts remote S3 latency.

Keep the reports with the tested commit and machine description when comparing
runs. Higher latency, writer hold, request errors, or maintenance duration are
observations for diagnosis; there are no pass/fail performance thresholds and
no release or merge dependency. Protocol and fixture errors still fail the
manual command. Interrupt the control process to cancel a long run.

The small regression command checks seed counts and archive readability across
two projects, then drives two concurrent runners through heartbeat, claim,
renewal and ordered event publication against an in-process Hub:

```sh
go test -timeout=120s ./internal/hubserver -run '^TestScaleSoak(Seed|Fleet)$'
```

The manual workers are opt-in tests selected only by the control tool through
`DETENT_SCALE_SOAK_CONFIG`. Ordinary test runs skip the long soak. The existing
`TestAttachmentOrphanSweepAndDeprovision` covers the sweep's isolation and lock
behavior at small scale. No safety-critical dispatch implementation is changed;
INV-5 and INV-17 remain unchanged.
