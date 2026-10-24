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

## Deployed state

The September 30, 2026 cutover added both A records through Namecheap, verified
the six pre-existing records were unchanged, and issued separate ACME
certificates. Both entries now use their Cloud `public_url`, and nginx serves the
new names while retaining endpoint-aware routes for the old names. Browser
navigation from an old host receives a temporary 307 to its matching Cloud host;
signed webhooks, API and runner requests continue without redirect.

The operator reported the following WorkOS settings in production and staging.
Both new callbacks are registered alongside the old callbacks. Real sign-in
completed on each new hostname, and organization boards and existing
conversations loaded. The default callback and Homepage URL settings were not
independently read from the dashboard; the application supplies its callback
explicitly during sign-in:

| WorkOS setting | Production | Staging |
| --- | --- | --- |
| Allowed redirect/callback | `https://cloud.detent.build/auth/oidc/callback` | `https://staging.cloud.detent.build/auth/oidc/callback` |
| Initiate login | `https://cloud.detent.build/auth/oidc/start` | `https://staging.cloud.detent.build/auth/oidc/start` |
| User invitation URL | `https://cloud.detent.build/invite` | `https://staging.cloud.detent.build/invite` |
| Homepage, unverified | `https://cloud.detent.build` | `https://staging.cloud.detent.build` |
| Logout destination | `https://cloud.detent.build` | `https://staging.cloud.detent.build` |

Retain both old callback allowlist entries for in-flight transactions. Audit
any application/custom-domain/SSO redirect allowlists for the old host; change
only settings belonging to that environment. The entry calls WorkOS server-side
and does not need wildcard browser CORS.

Both existing Stripe webhook endpoints use Cloud URLs; their signing secrets,
event subscriptions, live/test mode and accounts are unchanged. Signed synthetic
`detent.smoke` events were accepted at both new URLs and recorded as ignored,
with no customer or subscription changes. Staging created and immediately
expired a test Checkout session. Provider-originated delivery and a live billing
journey were not generated during cutover. WorkOS documents the distinction
between [API-key and dashboard configuration](https://workos.com/docs/cli).
Stripe's [endpoint update API](https://docs.stripe.com/api/webhook_endpoints/update)
changes an existing URL without replacing its endpoint or signing secret.

## Cutover record

1. Saved separate versioned backups of the stopped staging and production
   state, entry YAML, previous binary, ingress, and operator scripts under each
   environment's `/var/lib/detent-hub*/backups/cloud-cutover-*` directory.
2. Installed the compatibility build and final
   [staging](examples/cloud/staging.conf),
   [production](examples/cloud/production.conf), and
   [connection](examples/cloud/connection.conf) nginx configuration. Changed
   only each entry's `public_url`; the existing launcher regenerated managed
   tenant YAML. Stored tenant origin, allocation generation and organization
   identities were not rewritten.
3. Updated the staging deployment healthcheck and the hosted smoke script to
   use Cloud. The GitHub staging workflow uses the new SSH hostname with
   `HostKeyAlias=staging.hub.detent.build`, retaining the pinned host key.
4. Ran `scripts/cloud-origin-smoke.py` in each environment and the hosted smoke
   script against both Cloud hosts. Staging passed 18 checks and production 17.
   The production live price was checked read-only; no live charge was created.
5. Deployed the marketing site's new sign-in and footer links through
   `detent.build` main. Dokploy rebuilt the site, and its post-deploy
   `make smoke` passed. The final results are in the issue #3241 Codex Workpad
   and PR #3350.

Preserve the legacy routes and WorkOS callback entries while active sessions
and clients still use them. The rollback procedure below uses the versioned
backups and previous configuration if needed.

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

The first step's sign-in, board and existing conversation checks passed in
both environments on September 30. New invitation acceptance,
provider-originated billing delivery, fresh runner enrollment, and in-flight
legacy session mutation remain unexercised.

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
