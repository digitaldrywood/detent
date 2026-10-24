# #3241 Cloud migration evidence

Run date: 2026-09-30 UTC (2026-09-29 America/Chicago).

## Repository diagnostics

Passed focused diagnostics:

```sh
go test ./internal/cloudentry -run 'TestHostedOrigin|TestSameOriginBrowserMutations|TestSharedEntry(LoginTransactions|Invitation|TwoOrganizationsOneOrigin|Boundaries)|TestSharedOriginPilotAcceptance' -count=1
go test ./internal/hubserver -run '^TestHostedDatabase(BindingRestart|CloudAliases|BootstrapBinding)$' -count=1
go test ./internal/cli -run '^TestRunnerHubTarget$' -count=1
go test . -run '^TestDeployStagingRunsOnlyFromDevelopOnHostedRunner$' -count=1
go vet ./internal/cloudorigin ./internal/cloudentry ./internal/hubserver ./internal/cli
go test ./internal/hubclient -run '^TestRunnerRequestsDoNotFollowCredentialRedirects$' -count=1
```

The initial bookmark regression used duplicate query keys and failed because
Detent already rejects them before routing. The corrected case preserves encoded
query values and multiple distinct keys without relaxing the request boundary.
The focused package tests passed after that correction (Cloud entry 6.664s,
Hub storage 7.534s, CLI 0.858s, workflow 0.420s, Hub client 0.432s).
No templates, queries or CSS inputs changed; generation was not required.

## Live results

| Observation | Production | Staging |
| --- | --- | --- |
| Authoritative DNS and Cloudflare resolver | A = `68.183.103.122`, TTL 300 | A = `68.183.103.122`, TTL 300 |
| DNS zone preservation | Six existing records unchanged | Same zone verification |
| New and old TLS (default trust and hostname validation from Jarvis) | PASS | PASS |
| New ACME certificate | Separate Cloud certificate, expires 2026-12-29 | Separate staging Cloud certificate, expires 2026-12-29 |
| WorkOS allowlisted callbacks | Both old and new registered; default remains old | Both old and new registered; default remains old |
| Stripe existing endpoint | Updated to Cloud `/webhooks/stripe/live`; enabled; live account unchanged | Updated to staging Cloud `/webhooks/stripe/test`; enabled; test account unchanged |
| Signed synthetic `detent.smoke` at both old/new HTTPS hosts | 200, no redirect | 200, no redirect |
| Same payload with tampered signature at both hosts | 400, no redirect | 400, no redirect |
| Existing runner authenticated identity read at old/new hosts | 200; same runner and organization; no redirect | Not exercised with a staging runner credential |
| Active pending ingress `nginx -t` and reload | PASS (existing unrelated nginx warnings) | PASS (same nginx) |
| Final `.next` ingress standalone `nginx -t` | PASS | PASS |
| Updated operator `.next` deploy/smoke script `bash -n` | PASS | PASS |
| Full live `cloud-origin-smoke.py` | FAIL at canonical sign-in page: new browser route intentionally returns 307 to old host | Same expected failure |
| Authenticated sign-in, invitation, navigation and new enrollment on canonical origin | NOT RUN; browser cutover pending | NOT RUN; browser cutover pending |
| Provider-originated webhook delivery and billing return journey | NOT RUN | NOT RUN |

The local worker resolver cached earlier negative DNS answers (SOA negative TTL
3601); its initial `--tls-only` invocations failed with `gaierror`. Both new records
were subsequently verified with the authoritative nameserver and `1.1.1.1`, and
both environment `--tls-only` checks passed from Jarvis. Local DNS/TLS is not
reported as a pass.

Synthetic webhook probes used each deployed environment's own signing secret in
memory, never in arguments or evidence. They inserted only ignored synthetic
events (`evt_detent_domain_3241_production` and `evt_detent_domain_3241_staging`),
with no customer, subscription, entitlement or charge changes. Updating an
existing Stripe endpoint retained its signing secret and event subscriptions.
This proves delivery/signature plumbing, not an actual Stripe-originated event.
The runner probe read an existing identity with its current credential, with
redirects disabled and TLS validated; it did not renew, rotate, enroll or modify
the running runner or its local configuration.

## Prepared operator configuration

Active: new DNS, TLS certificates, new WorkOS callback entries, updated Stripe
URLs, and `/etc/nginx/sites-enabled/detent-cloud-pending.conf`. Original browser
origins, entry YAML, binaries, tenant state and deploy/smoke scripts remain active.

Prepared on Jarvis, not activated:

- `/etc/nginx/sites-available/detent-cloud-production.conf.next`
- `/etc/nginx/sites-available/detent-cloud-staging.conf.next`
- `/etc/nginx/sites-available/detent-cloud-connection.conf.next`
- `/etc/nginx/detent-cloud-migration-check.conf` (syntax-check configuration only)
- `/opt/detent-hub-staging/deploy-from-ci-cloud.sh.next`
- `/opt/detent-hub/hosted-smoke-cloud.sh.next`

The check configuration matches `docs/examples/cloud/nginx-check.conf`. Testing
via `/dev/stdin` first failed with `no "events" section in configuration`; the
regular operator check configuration passed. No failing configuration was
activated. The `.next` script changes substitute only the hosted domain names.

## Remaining acceptance and handoff

Follow [the cutover runbook](../../../docs/cloud-domain-migration.md). Before
browser switching, a WorkOS dashboard operator must verify/update default
callback, initiate-login, invitation, homepage and configured logout destinations
for the correct production and staging clients. Keep both old callbacks.
This worker had no WorkOS dashboard session; `mcp__chrome-devtools__navigate_page`
was absent from its actual tool list. API keys added the new callback entries
but could not verify all dashboard settings.

After those settings are verified, deploy this PR's compatibility behavior,
switch staging and validate its authenticated trace; then switch production and
validate the production trace. Record real account journeys, existing-session
CSRF mutations, an in-flight legacy callback, new runner enrollment, long-poll
and reconnect, and provider-originated billing delivery evidence in the PR.
Do not treat DNS/TLS, synthetic events or missing-organization API probes as
full acceptance.

Configured gate: `true` (no validation/status, not test credit).
Current-head checks: this repository defines no PR Actions runs or required
statuses; no CI wait is needed. Review-bot findings are inspected separately.
Quiet-window wait: not applicable; the PR remains draft pending external action.
Focused test/vet batch wall time: about 91s including compilation.
Gate timing: under 1s. CI/slow-check/post-merge main-CI timings: not applicable;
no CI was started or waited on, and the branch has not merged.

Skill draft: no — this run used routine URL, provider and deployment procedures.
