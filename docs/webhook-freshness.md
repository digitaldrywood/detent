# GitHub Webhook Freshness

[Back to README](../README.md#documentation)

Each GitHub-backed project can opt into near-real-time external updates by
setting a webhook secret in its `WORKFLOW.md`:

```yaml
tracker:
  kind: github
  github_status_source: label
  repository: digitaldrywood/detent
  github_webhook_secret: $DETENT_GITHUB_WEBHOOK_SECRET
polling:
  interval_ms: 60000
  conditional: true
```

Set `DETENT_GITHUB_WEBHOOK_SECRET` in the Detent process environment. Configure
the repository webhook with:

- Payload URL: `https://<detent-host>/api/v1/webhooks/github`
- Content type: `application/json`
- Secret: the same high-entropy value
- Events: Issues, Pull requests, Check suites, and Labels

Detent verifies `X-Hub-Signature-256` with HMAC-SHA256, routes the delivery only
to projects whose configured repository and secret match, and queues a fetch for
the affected issue. Pull-request and check-suite deliveries resolve Detent's
issue from its generated branch name. A repository-level label event has no
single issue target, so it queues a normal conditional refresh. Unknown
repositories never trigger a fleet-wide fallback. See GitHub's
[webhook signature validation](https://docs.github.com/en/webhooks/using-webhooks/validating-webhook-deliveries)
and [event payload reference](https://docs.github.com/en/webhooks/webhook-events-and-payloads).

For a ProjectV2 tracker spanning multiple repositories, omit `tracker.repository`
to let the connector verify project membership after signature validation. Set
`tracker.repository` when the project is repository-scoped and strict routing is
preferred.

GitHub must be able to reach the payload URL. For local testing, GitHub documents
[forwarding deliveries with smee.io](https://docs.github.com/en/webhooks/using-webhooks/handling-webhook-deliveries).
For a persistent host without public ingress, an outbound tunnel such as a
[Cloudflare Tunnel published application](https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/get-started/create-remote-tunnel/#2a-publish-an-application)
can route a public HTTPS hostname to Detent. Configure relays and reverse proxies
to preserve the raw request body and GitHub signature headers; modifying either
causes signature verification to fail.

## Hub reconciliation

`detent hub serve` repairs deliveries that never reach the webhook endpoint. It
runs a full repository repair at startup, incremental repairs every 10 minutes,
and a full repair every 24 hours by default. Configure the intervals with
`--reconcile-interval` and `--full-repair-interval`. Incremental issue reads use
the repository update cursor, while repository and pull-request reads reuse
GitHub ETags through conditional requests. A pending targeted hydration request
rewinds the next incremental cursor so a delayed partial webhook cannot be
skipped.

A full repair compares complete issue and pull-request inventories. It updates
labels and repository transfers, imports items missed during a webhook gap, and
marks source records absent from GitHub as deleted so they stop participating in
scheduling.

Query `GET /api/v1/repositories/freshness` for each repository's latest
successful sync, webhook receipt, reconciliation, full repair, and most recent
webhook/reconciliation errors. `GET /health` returns `status: degraded` with
fresh, stale, and error repository counts whenever any mirror is older than two
incremental intervals or has an unresolved sync error. The Hub remains available
for local reads while degraded.
