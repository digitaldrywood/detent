# Monthly infrastructure usage

The Hub usage owner persists infrastructure observations in its existing SQLite
database, separately from runner AI estimates, conversation credits and hosted
plan allowances. `internal/usagecost.Observation` is the reusable source
contract. The Hub's `recordCostObservation` and `database.monthlyCosts` own source
revision storage and aggregation for all cost buckets. Sprite imports use
`sprite_infrastructure`; runner accounting can reuse the contract with its own
bucket without another schema or aggregation implementation.

## Provider feasibility

[Fly's Sprite pricing](https://fly.io/sprites/) describes cumulative CPU time
from `cpu.stat`, actual memory usage, and hot/cold storage in GB-hours. Compute
can stop while retained storage remains billable. Plan allowances, credits and
adjustments can change the invoice, which the provider identifies as authoritative.

The [public API inventory](https://docs.fly.io/llms.txt) and
[Sprite response](https://docs.fly.io/sprites/api/sprites/get-a-sprite)
document identity and lifecycle status but no per-Sprite CPU-hours, RAM/storage
GB-hours, invoice lines or billing adjustments. The filesystem/exec APIs permit
guest measurements when accessible; these do not establish billed quantities.
No undocumented metering endpoint is assumed or called. Automatic authoritative
per-Sprite billing ingestion is unavailable with this evidence. An organization
invoice total without resource attribution cannot safely be divided among projects.

There are no default rates or invented quantities. The importer supplies rate
source/effective date, currency, observation time and freshness deadline. Published
pricing supports estimates but does not prove historical or contracted rates.
The ledger does not automatically apply provider allowances, credits, taxes,
foreign exchange or subscription fees.

## Obtaining estimates

Import measurements for existing Sprites using their stable provider resource ID,
provider account and original owning project. Pool enrollment and provisioning
changes are unnecessary. Include the Sprite name when matching current inventory.

| Metric | Unit | Estimate path and limitations |
| --- | --- | --- |
| `cpu` | `CPU-hour` | Difference of cumulative `cpu.stat` usage counters divided by microseconds per hour. Sample the same cgroup and boot epoch; counter resets and gaps are unknown. Configured vCPUs and wall time are not CPU consumption. |
| `ram` | `GB-hour` | Integrate sampled actual memory over elapsed hours. Sparse samples miss peaks and sleep transitions; configured memory limits are not consumption. |
| `storage_hot` | `GB-hour` | Integrate measured hot-storage bytes over their retained interval. Guest filesystem usage cannot establish provider tier size, deduplication or checkpoint billing. |
| `storage_cold` | `GB-hour` | Integrate retained cold-storage bytes over elapsed hours, including sleep. Carrying a previously measured size forward is an estimate with an explicit freshness deadline. Guest measurements cannot establish provider cold-tier or retained checkpoint size. |

Use an already authorized running session for guest measurements. Do not wake an
idle Sprite just to measure it. Record the sampling method, boot/cgroup epoch,
byte-to-GB convention and storage assumptions in the safe evidence reference.
A constant-size storage estimate is usable for a known retention interval;
correct it when newer evidence arrives. Import components independently and
leave unavailable components unknown. This ledger adds no guest sampler, polling
loop, automatic wake, storage deletion, pool setting or spending cap.

## Import contract

`POST /api/v2/organizations/{organization}/projects/{project}/usage/sprites`
requires administrator authority and a native `idempotency_key`. The body contains
`observations`, a batch of 1–128 records that commits atomically. MCP
`import_sprite_usage` uses `project_id`, `request_id`, and `input.observations`
through the same application command. API and MCP replay recheck current project
and administrator authority.

Each observation supplies:

- `provider: fly_sprites`, `provider_account`, stable `resource_id`, optional
  `resource_name`, `bucket: sprite_infrastructure`, `metric`, `unit`, and
  `source_id` naming one canonical resource/metric stream.
- Positive `revision` and half-open `from`/`to` timestamps for observed past
  usage, at most 366 days. Split at known rate, attribution or sampling changes.
- Nullable `quantity` with `quantity_basis`: `measured`, `provider_reported`,
  `estimated` or `unknown`. Compute and both storage tiers remain separate lines.
- Nullable `amount_micros`, three-letter uppercase currency and monetary
  `basis`: `estimated`, `provider_billed` or `unknown`. A micro is a millionth of
  that currency. Provider billed amounts can include negative credits.
- Nullable integer `unit_price_micros`, `rate_source`, `rate_effective_at`,
  `evidence_source`, `observed_at`, `fresh_until`, and `coverage`: `complete`,
  `partial` or `unknown`. Missing rate metadata stays empty/null rather than
  inferred. Rate effective dates must not be later than usage start.

For a usable estimate, supply a measured or estimated quantity and unit price,
set `amount_micros` to null, and set `basis` to `estimated`. The Hub multiplies
quantity by price and rounds to the nearest micro. For example, an externally
measured retained cold-storage size multiplied by elapsed hours becomes a
`storage_cold` observation even if CPU and RAM have no observations. The importer
supplies the applicable cold-storage rate and its effective date; Detent supplies
neither a rate nor a hot/cold split.

For missing data use null quantity/amount and unknown bases/coverage. Explicit
measured zero is accepted; missing data never becomes zero. `provider_billed` is
the authorized importer's assertion about sanitized provider evidence, not an
independent invoice verification by Detent. Keep provider tokens and sensitive
invoice contents outside observations, logs and tracker evidence. Evidence
fields contain only safe identifiers or public references.

## Replay, corrections and attribution

Source-period identity is organization, provider, provider account, source ID and
exact UTC interval. Source IDs must be canonical and distinct per resource/metric
stream even if an export changes its invoice-line ID. A repeat revision with
identical normalized data adds nothing; changed content at that revision
conflicts. A higher revision replaces the period contribution. Late lower
revisions cannot resurrect it. Corrections retain original project, resource ID,
bucket, metric, unit and currency. Old revisions remain durable; reads select
only the latest revision after restart.

Overlapping periods for the same resource/bucket/metric are rejected, including
duplicate estimates with another source ID or another project. Replace an
estimate with provider evidence through the same source identity and a higher
revision. To correct interval boundaries, import a higher revision of the
original observation with `voided: true`, followed by replacement intervals in
the same atomic batch. Voided observations remain in history and contribute no
cost; retries cannot revive their earlier revisions.

Observations have no foreign keys to Sprite, pool, runner or project lifecycle
tables. Removing those records never deletes costs or changes attribution. New
nonoverlapping intervals can belong to a new owning project; original intervals
remain with their original project. Organization reads include historical
project IDs even when absent from current project inventory.

## Monthly reports and budgets

Every bucket uses UTC calendar months: midnight on the first day through,
exclusively, midnight on the next month's first day. Month lengths and leap years
are preserved. Project reads use
`GET .../projects/{project}/usage/monthly?month=YYYY-MM` with current project read
authority. Organization reads use
`GET .../organizations/{organization}/usage/monthly?month=YYYY-MM` with admin
authority and, in hosted deployments, grants to every current project. MCP
`monthly_usage_costs` accepts `month`, `scope` (`project` or `organization`), and
`project_id` for project scope. Missing month selects the current UTC month.

The existing hosted usage report and `billing_usage.hosted_usage_report` also
expose `monthly_costs`. `range=month:YYYY-MM` selects that month. Rolling AI
ranges retain their behavior and include an explicitly dated current monthly
cost report. These reads label their cost scope `readable_projects`, not full
organization usage, and include only currently granted projects.

Reports expose original source quantities, allocated quantities/amounts,
observation time, freshness deadline, stale flags, rate/evidence sources,
revision, coverage gaps and estimated/billed totals per bucket and currency.
`known_micros` is the available subtotal, not complete/fresh usage. Null subtotals
and empty totals are unknown, not zero. Stale known amounts remain visible
without claiming current coverage. Unobserved deployed pool members and
attributable Sprite runners produce component gaps. Reports do not extrapolate
compute or storage beyond imported intervals.

MCP responses retain complete monetary totals and project breakdowns, while
source and gap details are limited to 32 rows each for the existing bounded
transport. `source_count`, `gap_count` and `details_complete` identify omitted
details. The authorized HTTP monthly read provides the full source detail.

For intervals crossing months, quantities are allocated by elapsed duration.
Integer money uses differences between truncated cumulative rational amounts
at each boundary. This assigns rounding remainders deterministically and
preserves the full source amount exactly, including negative adjustments.
A prorated billed source retains its original billed basis but labels the
monthly `allocated_basis: estimated` and `allocation: duration_prorated` because
usage may not be uniform. Import provider monthly intervals to avoid that
allocation assumption.

Budget consumers query project and organization totals for the same month and
currency. `by_project` is a breakdown of `totals`, never another charge to add.
Each latest source contributes once to the organization and once to its project
breakdown. Incomplete coverage and stale evidence remain unknown portions of the
budget; the known subtotal must not be treated as a full invoice or as zero for
the unobserved portion. No visible UI is added by these API/MCP extensions.
