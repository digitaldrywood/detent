# Detent Cloud domain migration

The canonical hosted product is **Detent Cloud** at
`https://cloud.detent.build`. Staging uses `https://staging.cloud.detent.build`.
Hub still names the self-hosted server, tenant API and `detent hub` command.
Historical [pilot evidence](cloud-pilot-evidence.md) and validation artifacts keep
the origins they actually exercised.

## Environment bindings

| Setting | Production | Staging |
| --- | --- | --- |
| New hostname | `cloud.detent.build` | `staging.cloud.detent.build` |
| Legacy hostname | `hub.detent.build` | `staging.hub.detent.build` |
| A record | `68.183.103.122`, TTL 300 | `68.183.103.122`, TTL 300 |
| Entry unit | `detent-cloud.service` | `detent-cloud-staging.service` |
| Entry config | `/etc/detent-hub/cloud.yaml` | `/etc/detent-hub-staging/cloud.yaml` |
| Private listener | `127.0.0.1:8017` | `127.0.0.1:8018` |
| Managed tenant root | `/var/lib/detent-hub/tenants` | `/var/lib/detent-hub-staging/tenants` |
| WorkOS client | `client_01M216SRGWJVNB4J1NW7BD4VE6` | `client_01M216SR50DRDQ8HQP19Y81J2Q` |
| Stripe account | `acct_1UKgghIxceXB54ID` (live) | `acct_1UKggoIugGyH6lT8` (test) |
| Stripe endpoint | `we_1UKokHIxceXB54ID2bMCP9yt` | `we_1UKgxdIugGyH6lT8UWHgeuWy` |
| Stripe URL | `https://cloud.detent.build/webhooks/stripe/live` | `https://staging.cloud.detent.build/webhooks/stripe/test` |

These are operator-owned environment bindings, not product defaults. Preserve
all existing state roots, assertion keys, WorkOS issuer/client/key bindings,
Stripe signing secrets, account/mode/price bindings and tenant generations.

## Provisioned state and remaining work

The September 30, 2026 UTC issue run added both A records through Namecheap,
verified the six pre-existing records were unchanged, issued separate ACME
certificates, and installed `/etc/nginx/sites-enabled/detent-cloud-pending.conf`
on Jarvis. Both new names serve valid TLS. The pending ingress sends browser
navigation back to the matching old origin while proxying `/webhooks/`, `/api/`
and organization API requests directly to the existing entry. It does not
redirect signed webhook requests. Both entry `public_url` values are still old.

Both new WorkOS callbacks are registered alongside their old callbacks. Both
existing Stripe webhook endpoints now use Cloud URLs; their signing secrets,
event subscriptions, live/test mode and accounts are unchanged. Signed synthetic
`detent.smoke` events were accepted at all four URLs and recorded as ignored,
with no customer or subscription changes. This is signature/routing evidence,
not provider-originated delivery or an authenticated billing journey.

Reviewed final ingress files are staged on Jarvis as
`/etc/nginx/sites-available/detent-cloud-{production,staging,connection}.conf.next`.
The standalone `/etc/nginx/detent-cloud-migration-check.conf` passed `nginx -t`.
The domain-only operator script updates are prepared as
`/opt/detent-hub-staging/deploy-from-ci-cloud.sh.next` and
`/opt/detent-hub/hosted-smoke-cloud.sh.next`; both passed `bash -n` and are inactive.
Compare them with the current operator files again before installation.

**Browser cutover is pending.** An operator with a WorkOS dashboard session must
verify each environment's settings before it:

| WorkOS setting | Production | Staging |
| --- | --- | --- |
| Default redirect/callback | `https://cloud.detent.build/auth/oidc/callback` | `https://staging.cloud.detent.build/auth/oidc/callback` |
| Initiate login | `https://cloud.detent.build/auth/oidc/start` | `https://staging.cloud.detent.build/auth/oidc/start` |
| User invitation URL | `https://cloud.detent.build/invite` | `https://staging.cloud.detent.build/invite` |
| Homepage | `https://cloud.detent.build` | `https://staging.cloud.detent.build` |
| Logout destination, if configured | `https://cloud.detent.build` | `https://staging.cloud.detent.build` |

Retain both old callback allowlist entries for in-flight transactions. Audit
any application/custom-domain/SSO redirect allowlists for the old host; change
only settings belonging to that environment. The entry calls WorkOS server-side
and does not need wildcard browser CORS. The available API keys registered the
new callbacks, but no dashboard session or browser MCP was available to this
worker to verify the remaining settings. WorkOS documents the distinction
between [API-key and dashboard configuration](https://workos.com/docs/cli).
Stripe's [endpoint update API](https://docs.stripe.com/api/webhook_endpoints/update)
changes an existing URL without replacing its endpoint or signing secret.

## Cutover order

1. Verify the WorkOS dashboard settings above and preserve the old callbacks.
   Verify Stripe's configured URLs, account/mode and recent delivery history.
2. Back up each environment through the existing operational backup procedure.
   Save the current binary, entry YAML, ingress and deployment scripts for rollback.
3. Install a binary containing this issue's compatibility behavior in staging
   first. Stop its Cloud entry using the existing service procedure, change only
   `public_url` in `/etc/detent-hub-staging/cloud.yaml` to
   `https://staging.cloud.detent.build`, then install the final ingress in the next step before restarting the entry.
   Its existing
   launcher regenerates every managed `tenant.yaml` with that origin. Inspect
   tenant startup before continuing. Shared database bindings recognize only the
   environment's approved old/new pair; the stored origin remains immutable and
   no allocation generation or organization identity changes.
4. Install [connection.conf](examples/cloud/connection.conf) once in nginx's
   `http` context and replace the old staging site with
   [staging.conf](examples/cloud/staging.conf). Remove the staging blocks from
   the pending site in the same reload, avoiding duplicate `server_name` routes.
   Keep both TLS certificates. Test with `nginx -t` before reloading, then
   start the entry and inspect all tenant startups.
5. Change only `DOMAIN=staging.hub.detent.build` to
   `DOMAIN=staging.cloud.detent.build` in the operator-owned
   `/opt/detent-hub-staging/deploy-from-ci.sh`. The GitHub workflow uses the new
   SSH hostname with `HostKeyAlias=staging.hub.detent.build`, retaining the
   existing pinned host key. Do not regenerate it from an untrusted connection.
6. Run `python3 scripts/cloud-origin-smoke.py --environment staging`, then the
   authenticated acceptance trace below. Run the existing hosted smoke script
   with its Cloud base override; review its actions first because staging smoke
   may create and expire test Checkout sessions.
7. After staging passes, repeat for production with `detent-cloud.service`,
   `/etc/detent-hub/cloud.yaml`, and
   [production.conf](examples/cloud/production.conf). Remove the remaining
   pending site. Update the operator-owned `/opt/detent-hub/hosted-smoke.sh`
   production/staging `DOMAIN` values to Cloud. Run the production origin smoke
   and authenticated acceptance trace. Production uses live keys; do not create
   a charge as a diagnostic.
8. Record exact results in the PR and Workpad. Preserve the legacy routes and
   callback entries while active sessions and clients still use them. Audit
   current marketing links in the product-site repository; this repository's
   README now links to Detent Cloud.

## Legacy request behavior after cutover

TLS ingress forwards both hostnames to the same environment's entry without
changing Host, path, query or signed bodies. HTTP uses 308 to its HTTPS host.
The entry makes endpoint-aware migration decisions:

- GET/HEAD navigation without an existing legacy session receives a temporary
  307 to Cloud with the original path/query. Existing strict path and query
  validation still applies.
- Login initiation and invitations move to Cloud before a login transaction is
  created, so the host-only state/PKCE cookie and callback share an origin.
- Old callbacks stay at the old host and consume the original transaction
  cookie. Existing legacy account sessions remain usable through expiry or
  sign-out. Cookies are never copied or widened across hosts.
- Mutations, API requests, bearer-authenticated requests, WebSockets and signed
  webhooks are forwarded without a host redirect. Legacy browser mutations
  accept their own exact Origin only on their matching legacy Host; staging,
  production, arbitrary origins and customer-operated Hub aliases stay separate.

## Authenticated acceptance trace

Run separately in production and staging with approved test accounts and an
organization in that environment:

1. Sign in from Cloud, finish WorkOS state/PKCE callback, and verify the selected
   organization, Work view and organization switcher stay at the new origin.
2. Send an invitation to an approved recipient, open the received link, sign in
   as that recipient and join the correct organization. Verify wrong-account
   and cross-environment invitations fail.
3. Open an old bookmark containing path/query. Start sign-in at the old origin;
   verify the transaction starts at Cloud. Complete a transaction begun before
   cutover at the old callback and confirm its legacy session can navigate and
   mutate with CSRF validation intact.
4. Verify existing billing/portal return URLs use Cloud and a provider-originated
   Stripe delivery reaches the enabled environment-specific webhook with the
   existing signing secret. Exercise staging test billing; inspect approved
   production delivery history without initiating a customer charge.
5. Enroll a disposable runner through the Cloud enrollment dialog and verify
   its generated Hub URL names Cloud. Verify authenticated reads, heartbeat,
   long-poll/streaming and reconnect through the old configured URL as well.
   The smoke script's missing-organization request is only a routing check.

`--tls-only` reports DNS/TLS separately and explicitly skips migration and
account journeys. Neither that option nor synthetic signatures count as full
acceptance. See [recorded results](../.detent/validation/3241/README.md).

## Rollback

Keep DNS and both certificates/routes available. Restore the previous entry
`public_url` and binary for the affected environment using the existing service
procedure, and restore its old ingress and healthcheck domain. Managed tenants
regenerate their YAML; the original database binding still recognizes the same
service. Restore WorkOS defaults/invitation/homepage settings and the existing
Stripe endpoint URL if reverting provider configuration. Preserve signing
secrets, organization/provider IDs, generations and all databases. In-flight
callbacks and legacy runner/webhook requests continue on their original host.
