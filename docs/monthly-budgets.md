# Monthly organization and project budgets

Monthly budgets constrain detected USD costs through the Hub's existing claim,
placement, conversation and Sprite lifecycle owners. Both organization and
project constraints apply. Organization exhaustion takes precedence in diagnostics;
project headroom cannot override it. These constraints filter eligibility before
capacity acquisition and preserve the existing ranking and dependency rules.
Ready Todo supplies new issue admissions. Backlog is never promoted by a budget.

## Settings and units

Read or update `/api/v2/organizations/{organization}/monthly-budget` or
`/api/v2/organizations/{organization}/projects/{project}/monthly-budget`.
Organization settings require organization administration; project writes require
an owner or admin with current project authority. Project workers can read their
settings but cannot change them. Hosted requests use the existing authenticated
settings boundary and CSRF protection. Non-hosted organization settings require
an instance administrator.

GET returns `revision` and `policy`. PUT accepts `idempotency_key`, the current
`expected_revision` as a decimal string, and `policy`. The command owner rejects
stale revisions and retains each saved policy version, actor and timestamp.
Neither setting changes nor budget stops change runner grants or issue lanes.
For example, this policy sets independent $20 Luna, $100 runner API and $50 Sprite
caps, plus an optional $150 total safety net:

```json
{
  "idempotency_key": "set-monthly-caps-1",
  "expected_revision": "0",
  "policy": {
    "enabled": true,
    "mode": "drain_issue",
    "luna_api_micros": 20000000,
    "runner_api_micros": 100000000,
    "sprite_infrastructure_micros": 50000000,
    "total_micros": 150000000
  }
}
```

Amounts are integer USD millionths. An omitted cap is unconstrained; an explicit
zero cap closes that bucket immediately when enabled. `enabled: false` disables
all caps in that policy. Omitted policies are disabled. Negative caps and unknown
modes are invalid. Omitted mode selects `drain_issue`; `hard_stop` is the alternative.
Policies do not inherit or replace one another: disabling a project policy leaves
an enabled organization policy in force.

## Accounting and exposure

The [shared monthly ledger](sprite-usage.md#monthly-reports-and-budgets) owns UTC
calendar periods, latest revisions, corrections and cross-month allocation.
Enforcement uses its known USD subtotals, including recorded estimates, without
adding estimated reservations. The optional total sums Luna, runner API and Sprite
subtotals once; organization totals and their project breakdown are separate
constraints, never additional charges. Negative bucket subtotals after credits
are treated as zero for enforcement and do not replenish another bucket.

Luna uses existing conversation usage. Runner API exposure uses the approved
project's billing mode; an older policy without billing configuration is treated
as potentially paid. Local subscription-agent activity has no runner API exposure
and local infrastructure is free. A local metered project remains exposed to the
runner API and total caps. Sprite execution adds infrastructure exposure to either
billing mode. Workspace-only leases expose infrastructure on Sprites but run no
model. Luna exhaustion does not make unrelated runner activity ineligible.

The existing monthly usage HTTP and MCP reports remain the accounting diagnostic
owner. They expose unknown/null costs, unreported attempts, active attempts,
coverage gaps, stale observations, estimates and reported/billed amounts. A known
subtotal below a cap is not proof of remaining actual billing headroom. Unknown,
non-USD and not-yet-reported costs are not silently presented as free; they remain
uncovered in those reports and cannot be compared to a USD cap until attributable
USD evidence arrives. Late reports and corrections affect the month of their
source intervals, not their receipt month. Current admission diagnostics use the
existing provider-capacity refusal and scheduling records with scope, bucket,
mode, detected amount and cap.

## Drain and hard stop

The default drain closes new affected admissions at detected exhaustion. A fenced
issue claim persists its admission cohort independently of its current attempt,
month or machine connection. The cohort permits required continuation through
validation, review, Rework, conflict repair and Merging. It does not bypass workflow,
provider capacity, placement, dependencies, operator stops or landing readiness.
Done, Blocked, Cancelled, archival or returning to Backlog removes the cohort;
subsequent Todo work needs a new admission. Existing admitted native issues are
carried into the cohort on upgrade. Pool members holding the latest lease of a
budget-managed admitted issue retain their workspace across idle lifecycle stages.
Terminal cleanup permits ordinary pool cleanup again.

Sprite exhaustion closes new provisioning and wake work. Drain permits required
Sprite continuation for claimable cohort work, without provisioning an idle floor
solely to admit fresh work after exhaustion. Eligible local execution remains
available. A retained Sprite may still accrue storage costs.

Hard stop rejects affected queued continuation claims and run starts, and refuses
renewal of affected current leases through the existing execution-authority owner.
The worker then cancels its attempt using its existing authority-loss behavior;
its issue, prior evidence and checkpoint records are preserved. The lease is not
rewritten as a successful answer or an operator hold. Detection and cancellation
can lag until the next renewal, and an outstanding lease expires normally.
Affected Luna turns use the existing coordinator cancellation owner, retaining
partial responses and saving their user messages for retry. Current Sprite
provision/wake lifecycle contexts are cancelled through their existing owner,
and pending reprovision requests are cleared. External provider operations may
already have taken effect before cancellation; ordinary provisioning cleanup
retains its existing responsibility.

Recovery is automatic. Claims and renewals reevaluate current totals and caps.
The settings command notifies existing dispatch and pool owners, and wakes saved
conversation work. UTC month rollover uses existing dispatch, pool and conversation
maintenance owners. Raising or disabling the exhausted cap also restores eligibility,
as can a correction lowering the detected subtotal. Normal policy, grants and
operator stops still apply. No new budget polling or reconciliation loop exists.

## Practical financial limits

These are detected-spend admission controls, not exact provider dollar ceilings.
Runner keys and billing remain customer owned, runner costs normally arrive when
attempts finish, and infrastructure measurements may be delayed or absent.
Already admitted issues can spend through their full lifecycle under drain, even
after admissions close. Hard stop still has detection, renewal, cancellation and
external-operation lag; retained storage can keep costing money. Parallel work,
retries and incomplete observations can produce substantial overrun. There is
no guaranteed small or numerically bounded excess and no reservation or extra
runaway-spend safeguard. This behavior is the accepted policy; operators must
use the existing usage evidence to assess the exposure. No Fly-side billing limit
is assumed. See [Sprite compute and storage pricing](https://fly.io/sprites/) and
[GitLab autoscaler capacity guidance](https://docs.gitlab.com/runner/executors/docker_autoscaler/)
for the distinction between spending, active capacity and warm capacity.
