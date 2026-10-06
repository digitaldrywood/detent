# Runner AI usage accounting

Runner costs use the Hub's existing native attempt ingestion and the shared
[monthly usage ledger](sprite-usage.md#monthly-reports-and-budgets). The
`runner_api`, `luna_api` and `sprite_infrastructure` buckets are distinct charges.
No provider credentials, account invoices or private provider payloads are
collected. Hosted-plan allowances and Luna credit balances are separate contracts.

## Finish reports

`run.finished` carries `data.usage`, attributed by the authenticated attempt's
original organization, project and work item. Entries name a provider/model,
token counts, currency and `billing_mode`: `metered`, `subscription` or `unknown`.
An omitted billing mode is unknown. Runners forward the explicitly configured
billing mode, without using budget-enforcement defaults to infer billing.

`reported_cost_micros` is nullable: omission means unavailable, while an explicit
zero is a reported zero. `cost_source` is `backend_result` or `runner_report`.
This is the runner's assertion about paid API use in metered mode, not Detent's
verification of a provider invoice. Claude result cost is forwarded when present;
backends that expose only tokens leave reported cost unavailable. Subscription
provider result amounts can describe API-equivalent activity and are preserved
as evidence, never counted as paid per-run spend.

The runner accumulates per-turn tokens, token-derived `cost_estimate` and reported
cost separately. If any turn lacks cost, the known reported subtotal remains
visible with `cost_coverage: partial`. Missing turns are not priced as actual
charges. A wholly unavailable reported subtotal remains null. Reports without
`cost_coverage` and with a reported amount assert complete cost coverage.

Finish reports carry the execution interval (`from`, `to`), `reported_at` and a
positive `revision`. The standard runner uses the attempt start and finish
instants. Older runners fall back to the Hub's recorded attempt start/finish
interval. The ledger records both the report's observation and the Hub's
`received_at`; delayed ingestion does not move usage into the arrival month.

The current native protocol still requires the authenticated current lease and
ordered execution events. Accounting does not relax those requirements to admit
reports after release or expiration. The original completed outcome, completion
evidence and finish timestamp are immutable during accounting corrections.

## Cumulative, incremental and corrected reports

An omitted `usage_kind` means `cumulative`: one running total per provider/model,
with a canonical source derived from attempt/model identity. The standard runner
sums turn increments into this cumulative report before sending it. Checkpoints
retain the existing hourly token-estimate ingestion; monthly monetary facts are
recorded at finish. Hourly reports remain API-equivalent token estimates, including
subscription activity, and must not be treated as paid invoices or added to the
monthly monetary ledger.

`usage_kind: incremental` requires a bounded `source_id`, positive revision and
explicit nonoverlapping interval. Each segment has its own source identity.
Incremental token details are exposed in monthly sources; they are not sent
through the older cumulative hourly token-delta path. Repeating a segment adds
nothing. Overlapping cumulative and incremental intervals for the same
attempt/provider/model are rejected rather than counted twice.

Source revisions use the shared ledger's durable replay and correction rules.
An identical revision is idempotent, changed content at that revision conflicts,
and lower revisions cannot resurrect superseded spend. Higher revisions replace
the contribution while retaining older facts. Corrections use the original exact
source interval, currency and attribution. A usage-only next `run.finished`
event can supply corrections while the original lease remains current; it cannot
change the outcome or completion data. Changing interval boundaries is not
supported by this runner report contract. The infrastructure import contract's
void-and-replace operation remains infrastructure-specific.

## Monthly costs and coverage

For metered runners, a reported cost takes precedence over token-derived cost
for the same source. If no reported amount is available, a known token estimate
contributes only as estimated spend. Without either, the amount is unknown.
Unknown billing modes contribute no known money even when estimates or raw
provider result amounts exist. Subscription sources retain tokens and provenance
but contribute no monetary total. These rules preserve reported and estimated
amounts as separate facts without summing both representations of the same use.

Sources disclose original token counts, `estimated_amount_micros`,
`estimate_source`, nullable `reported_amount_micros`, `reported_cost_source`, billing mode, attempt and
work-item IDs, runner/machine identity, placement, observation time, receive time,
coverage and allocated basis. Historical finalized attempt costs do not expire
merely because another month has elapsed. Freshness for infrastructure remains
bounded by the importer's observation deadline. Missing monetary evidence and
partial runner cost coverage produce gaps over the reported execution interval.

The monthly API/MCP and hosted usage report expose `runner_reporting: finish_time`,
`active_attempts`, `unreported_attempts` and `active_work_complete` for the selected
scope and month. Active work can spend money before its finish report exists.
A reported attempt can still have unknown billing or missing cost; totals disclose
`unknown_observations` and `unreported_cost_observations`. These counters describe
known native attempts, not independently verified provider completeness or
real-time metering. Older unledgered reports are not silently backfilled as paid
charges. Readable-project reports do not imply organization-wide coverage.

Luna costs derive from the existing durable conversation usage and price records,
with one source per turn and their original project attribution. They remain
token-priced estimates, separate from runner reports and from credit purchases.
Missing Luna prices remain unknown.

Every bucket uses UTC calendar months and the shared duration allocation rule.
Cross-month reported costs retain their original reported basis, while prorated
monthly allocations are estimates. Integer rounding preserves the original
amount across months. Consumers sum `known_micros` once per bucket/currency, or
select its `estimated_micros`, `reported_micros` and `billed_micros` breakdown;
they never sum a subtotal and its breakdown together. `by_project` is another
breakdown of organization totals, not additional spend. Local infrastructure
contributes no infrastructure charge to placement budgeting; paid local runner
AI still contributes `runner_api`. Sprite AI and infrastructure can both
contribute because they represent separate charges. Unknown coverage remains
unknown rather than becoming a zero or an exact provider invoice.
