# Hub deployment examples

[Caddyfile](Caddyfile), [detent-hub.service](detent-hub.service) and the macOS
[Hub LaunchAgent](com.digitaldrywood.detent.hub.plist) are **customer-operated,
free self-hosting** examples. They serve a single Hub at the customer's chosen
origin with scoped bearer auth. The [self-hosting runbook](../../hub-self-hosting.md)
covers installation, credentials, backup and recovery. They require no Detent
subscription or billing connectivity. Changing their hostname does not create a
shared multi-tenant site.

For a Hub and runner on one machine, see the
[macOS and Linux walkthrough](../../hub-self-hosting.md#single-machine-hub-and-runner).
The Hub LaunchAgent runs in a dedicated standard account, separate from the
unprivileged runner account. It needs absolute paths customized for the Hub
account, a private token file and pre-created log directories inaccessible to
the runner. `python3 -m unittest scripts/hub_examples_test.py` parses the plist,
builds the current CLI, invokes both service commands with `--help`, and
exercises file-to-environment token loading without starting a Hub.

The rest of this page describes the shared entry (`detent cloud serve`), which
runs a public site with a shared identity/provisioning entry and metadata
registry in front of dedicated private tenant Hub processes with separate
databases. Self-hosted Detent never needs it. The public reverse proxy forwards
to the entry unchanged and never chooses a tenant with a URL rewrite; each
tenant verifies the entry's signed assertion. The [nginx examples](../cloud/)
show that ingress shape.

## Configuration surfaces

| Surface | Configuration |
| --- | --- |
| Customer Hub | `detent hub serve --database PATH --listen ADDRESS --github-disabled`; private `DETENT_HUB_ADMIN_TOKEN`, optional TLS/trusted-proxy flags |
| Hosted tenant Hub | `--hosted-config PATH`; root `organization_id`, `workos_organization_id`, `bootstrap_subject`, `public_url`, `workos`, `directory`, `staff_emails`, `support_actors`, entitlement and billing fields |
| WorkOS fields | `client_id`, `api_key_env` (default `WORKOS_API_KEY`), optional `api_url` (default `https://api.workos.com`), `issuer_url` (default `<api_url>/user_management/<client_id>`; set only for a custom auth domain) |
| Entitlements | `entitlements` plan catalog and assignments; see [allowances](../../hosted-allowances.md) |
| Shared entry and registry | `detent cloud serve --entry-config PATH`; tenant `shared_entry` block; `detent cloud registry register/list`; `detent hub migrate-shared-origin` |

Hosted and entry YAML parsers reject unknown fields.

## Shared entry

`detent cloud serve` runs the shared entry on a loopback port behind the public
TLS proxy. Its configuration:

```yaml
public_url: https://cloud.example.com
listen: 127.0.0.1:8017
state_directory: /var/lib/detent/cloud
platform:
  bootstrap_admin_email: operator@example.com
assertion:
  issuer: detent-cloud
  signing_key_env: DETENT_CLOUD_ASSERTION_KEY
workos:
  client_id: client_example
  api_key_env: WORKOS_API_KEY
```

Platform membership lives in `registry.db`, independently of customer organization
membership. `platform.bootstrap_admin_email` names the operator's verified email;
startup restores this address to `admin`, and the members API cannot remove it.
The entry requires this address or an existing registry admin to start.
`admin` manages platform members and includes support and billing privileges;
`support` can read the console and start support access; `billing` can read the
console and grant or revoke complimentary plans; `viewer` can read the console.
Complimentary plans also require `allocation.entitlement_admin_token_env`.
See [allowances](../../hosted-allowances.md#granting-from-the-platform-console).

The entry's `staff_emails`, `support_actors`, and `entitlement_administrators` are
deprecated. They seed an empty registry: remaining staff become viewers, support
actors become support members, entitlement administrators become billing members,
and addresses in both privileged lists become admins. After seeding, only registry
roles grant platform access; configured legacy lists produce one startup warning.

`detent cloud assertion-key` prints a new signing seed and public key. The seed
belongs in the entry's private environment and the public key in each tenant's
`shared_entry.public_keys`. `state_directory` holds two single-owner SQLite files:
`registry.db` (organization IDs, provider organization IDs, names, private
endpoints, generations) and the private `auth.db` (hashed session and
login-transaction references, per-organization provider sessions, content-free
audit). Neither holds collaboration content.

Tenant endpoints are Unix sockets only. Before every connection the entry checks
that the socket and its directory are owned by the entry's service user and that
the directory is private (mode 0700), so another local process cannot impersonate
a restarting tenant. The entry and tenants run as the same service user.

Tenant registration happens while the entry is stopped; it is idempotent, refuses
to reuse IDs or provider organizations, and changes an endpoint only with a higher
generation:

```sh
detent cloud registry register --registry /var/lib/detent/cloud/registry.db \
  --organization org_example --provider-organization org_workos_example \
  --name "Example" --endpoint unix:/run/detent/tenants/org_example.sock --generation 1
```

## Platform notification email

With `RESEND_API_KEY` set in the entry's environment, the entry notifies platform
admins when an identity first signs in or an organization is created. No YAML
settings are needed. Mail uses `smtp.resend.com:465` with implicit TLS and
username `resend`. The key is read only from the environment. When it is empty,
notifications are skipped and the entry logs that once at startup.

Recipients are the registry's current `admin` platform members, read when each
notification is sent. Sign-up messages contain the email address, event time and
platform user link; organization messages contain the name, creator email, plan,
event time and platform tenant link. Delivery runs in the background after the
request commits. Each event is durably claimed before sending and is attempted
at most once, including across restarts. Mail failures are logged once per event
without failing the request and are not retried. Tenant Hub health-finding mail
is independent.

## Self-service provisioning

An `allocation` block lets verified users create organizations without operator
YAML, a bootstrap user, DNS or a public port.

```yaml
allocation:
  tenant_root: /var/lib/detent/tenants
  socket_root: /run/detent/tenants
  max_tenants: 20
  max_concurrent_provisions: 1
  retry_limit: 5
  min_free_disk_bytes: 2147483648
  min_available_memory_bytes: 536870912
  allowed_domains: []
  allowed_emails: []
  entitlements: {}
  entitlement_administrator: example-operator
  entitlement_admin_token_env: DETENT_ENTITLEMENT_ADMIN_TOKEN
```

Organization creation has no per-identity count limit. Each owner has one Free
organization slot; paid, complimentary and legacy pilot organizations do not
consume it. When the slot is used, creation requires a configured paid plan and
continues to hosted checkout. Until payment succeeds, the new organization has
no project or AI execution allowance. Allocation YAML uses strict field
validation, so older files containing the retired per-identity organization
limit fail to load. `max_tenants` is the operator capacity ceiling.

`max_tenants` and the free-memory/disk floors are admission limits: a request that
does not fit is stored as a retryable `capacity` failure before any provider or
filesystem effect, and ready tenants are never disturbed. The memory floor reads
Linux `MemAvailable`; where memory cannot be measured a nonzero floor refuses
admission. `allowed_domains`/`allowed_emails` restrict sign-up; empty lists admit
any verified account. `entitlements` is the tenant's
[versioned plan catalog](../../hosted-allowances.md); its `base` plan is the free
plan a new organization starts on when its owner has an available Free slot;
no Stripe customer or card is created for that organization.
Left empty (`entitlements: {}`, an empty block, or no key at all), tenants have no
plan allowances. Any key set inside the section makes it an explicit catalog that
must be complete (`base`, `plans` and the window settings). `detent cloud serve`
generates the tenant configuration for a synthetic organization at startup and
runs the same validation a tenant Hub runs, so an incomplete catalog or
inconsistent tenant billing stops the entry with an error instead of leaving
every tenant unable to start.
`entitlement_administrator` and `entitlement_admin_token_env` are optional; when set,
each tenant accepts complimentary grants on `POST /api/v2/organizations/ORG/entitlements`
with that token (at least 32 bytes, read from the entry's environment and passed to
tenants as an environment variable, never written to `tenant.yaml`). Without them
complimentary access cannot be granted on the shared deployment.

For each organization the entry creates `tenant_root/ORG/` (mode 0700) holding the
generated `tenant.yaml` (no secrets), `hub.db` and a private per-tenant Hub admin
token, and runs `detent hub serve --hosted-config ... --listen unix:socket_root/ORG.sock`
as a supervised child (restarted with backoff; stopped when the entry stops).
The same supervisor checks the authenticated tenant health endpoint every five
seconds. A tenant that has not passed a check within 15 minutes of starting, or
that fails checks for 30 seconds after serving, is interrupted and reaped before
the existing restart backoff. The entry's `/health` returns 503 when its registry
or authentication database is unavailable or any ready tenant fails its
authenticated health check. Allocating, failed and deleted organizations are not
expected to serve yet. The entry interrupts all children together and waits for
their exit before releasing its own databases. A child has 15 seconds to exit
before the launcher kills and reaps it. Keep all tenants in the entry's systemd
cgroup: use `KillMode=mixed` with `TimeoutStopSec=45` (or longer), never
`KillMode=process`. This gives the entry time to drain requests and join tenants
while systemd retains final cgroup cleanup authority. Shared-entry tenants wait
cancellably for a previous process to release their database ownership lock;
same-process and live-sibling contention still fails immediately.

A child that exits `retry_limit` times in a row without staying up for five minutes is no
longer restarted: during setup the organization moves to `failed` with the reason
shown on the provisioning screen and the platform console, and resuming setup
starts it again. The child receives only
`PATH`/`HOME`/`TMPDIR`/`LANG`/`TZ`/`LOG_LEVEL`/`DETENT_LOG_LEVEL`, the WorkOS key
variable, the optional entitlement token variable and its own admin token. Each
tenant owns its SQLite file exclusively; never place `tenant_root` on a network
filesystem.

Deletion is owner-only (`/organizations/ORG/delete`, current provider owner, typed
name, CSRF). It marks the organization `deleting`, revokes every authorization and
tenant session, stops the tenant and records a permanent `deleted` tombstone that
cannot be routed or resurrected; an interrupted deletion resumes automatically.
Tenant data stays on disk for the operator's retention process; the entry never
erases customer data, including after a failed signup.

### Backup and recovery

`state_directory/registry.db` and every `tenant_root/ORG/hub.db` are backed up
together, with the entry and tenants quiesced. `detent hub backup` is offline
maintenance: it requires the owning tenant to be stopped even though it uses
SQLite's backup API internally, so a backup through this path interrupts every
tenant until entry and tenant health recover. Copying live database and WAL files
is unsupported; the shared entry exposes no coordinated live snapshot.
`auth.db` holds only sessions and login transactions and is not restored: a
restore starts with no sessions, and every user signs in again. To restore one
tenant, stop the entry, restore its `hub.db` with `detent hub restore`, and if
its binding moved, re-register it with a higher generation; the registry refuses
a lower generation or a reused ID, and deleted organizations stay tombstoned.

### Routing and sessions

The entry serves sign-in (`/auth/oidc/start`, `/auth/oidc/callback`), the chooser
(`/organizations`, JSON at `/api/cloud/organizations`), invitations (`/invite`,
`/invitations/join`) and sign-out, and routes `/organizations/ORG/...` and
`/api/v2/organizations/ORG/...` to the registered tenant with a signed assertion.
Browser requests need the host-only session cookie (`__Host-detent_session`,
rotated on every sign-in; the CSRF secret carries over so other tabs keep
working), a per-organization authorization obtained through the common callback,
a current provider session and active membership; mutations also need the exact
`public_url` Origin and the per-organization CSRF token. Bearer requests are
forwarded as machine requests without cookies and the tenant authenticates the
token itself. Inbound forwarding and assertion headers are stripped, tenant
`Set-Cookie` headers are dropped, and responses carry `no-store` and a restrictive
Content Security Policy.

## WorkOS and Stripe wiring

The WorkOS redirect URI is the entry origin followed by `/auth/oidc/callback`,
and the User invitation URL is the origin followed by `/invite`. Every
organization uses these values. Client ID, API key and issuer come from the same
WorkOS environment.

The entry's optional `billing` block selects `mode` (`test` by default, or
`live`), `account_id`, `api_key_env` and `webhook_secret_env`. The key and secret
variables default to `DETENT_STRIPE_TEST_KEY` / `DETENT_STRIPE_TEST_WEBHOOK_SECRET`
or `DETENT_STRIPE_LIVE_KEY` / `DETENT_STRIPE_LIVE_WEBHOOK_SECRET` for the selected
mode. The Stripe webhook is the entry origin followed by `/webhooks/stripe/test`
or `/webhooks/stripe/live`. Free sign-up creates no Stripe customer or card
requirement.

Environment references resolve only in the process that consumes them; there is
no environment-to-YAML interpolation or implicit `.envrc` loading. Missing named
secrets fail startup without logging their values.
