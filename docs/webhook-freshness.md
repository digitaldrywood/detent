# GitHub Webhook Freshness

[Back to README](../README.md#documentation)

A Hub with GitHub compatibility enabled accepts signed GitHub webhook
deliveries at `POST /api/v1/webhooks/github`. Detent Cloud receives them
through the product GitHub App; a self-hosted `detent hub serve` uses the same
path on its own origin and verifies the signature itself with the secret named
by `--github-webhook-secret-env` (default `DETENT_HUB_GITHUB_WEBHOOK_SECRET`).
Without that secret the endpoint answers `webhook_unavailable`. See
[GitHub profiles](github-profiles.md) for the App, its event subscriptions, and
repository binding.

Detent verifies `X-Hub-Signature-256` with HMAC-SHA256 against the raw request
body. See GitHub's
[webhook signature validation](https://docs.github.com/en/webhooks/using-webhooks/validating-webhook-deliveries)
and [event payload reference](https://docs.github.com/en/webhooks/webhook-events-and-payloads).

GitHub must be able to reach the payload URL. For local testing, GitHub documents
[forwarding deliveries with smee.io](https://docs.github.com/en/webhooks/using-webhooks/handling-webhook-deliveries).
For a persistent host without public ingress, an outbound tunnel such as a
[Cloudflare Tunnel published application](https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/get-started/create-remote-tunnel/#2a-publish-an-application)
can route a public HTTPS hostname to the Hub. Configure relays and reverse
proxies to preserve the raw request body and GitHub signature headers; modifying
either causes signature verification to fail.

## Cloud entry startup replay

The shared entry uses `DETENT_HUB_GITHUB_APP_ID` and
`DETENT_HUB_GITHUB_APP_PRIVATE_KEY` to request App webhook redeliveries once
per start. Before serving, it snapshots the newest accepted delivery timestamp
across ready tenant inboxes. After serving begins, it requests redelivery of
failed `issues` and `issue_comment` deliveries between that receipt and startup.
It follows delivery pagination, uses the latest attempt per delivery GUID, and
leaves successful deliveries alone. Tenant inboxes deduplicate redeliveries
using `X-GitHub-Delivery`.

The pass has a five-minute deadline and logs the number of requests accepted.
GitHub or tenant receipt read failures are logged without stopping the entry.
There is no periodic replay. Without an accepted receipt, configured App
credentials, or webhook verification secret, no historical replay is attempted.
Deliveries older than the newest accepted receipt or outside GitHub's retained
history require operator recovery.

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
scheduling. It also refreshes mergeability, checks, statuses, and reviews for
every open pull request. Incremental passes fetch those details only for the
exact webhook hydration requests captured when the pass starts; requests added
or coalesced during the fetch remain pending for the next pass.

Query `GET /api/v1/repositories/freshness` for each repository's latest
successful sync, webhook receipt, reconciliation, full repair, and most recent
webhook/reconciliation errors. `GET /health` returns `status: degraded` with
fresh, stale, and error repository counts whenever any mirror is older than two
incremental intervals or has an unresolved sync error. The Hub remains available
for local reads while degraded.
