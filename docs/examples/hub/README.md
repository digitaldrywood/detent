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

The shared Cloud deployment runs one `detent cloud serve` process behind the public reverse proxy. The entry authenticates the caller and routes the signed request to a Hub handler in the same process. Organizations share one PostgreSQL runtime pool. The platform registry and authentication store use a separate role and pool. The [nginx examples](../cloud/) show the ingress configuration.

## Configuration surfaces

| Surface | Configuration |
| --- | --- |
| Customer Hub | `detent hub serve --database PATH --listen ADDRESS`; private `DETENT_HUB_ADMIN_TOKEN`, Hub CLI login or GitHub App credentials, optional TLS/trusted-proxy flags |
| Hosted tenant Hub | `--hosted-config PATH`; root `organization_id`, `workos_organization_id`, `bootstrap_subject`, `public_url`, `workos`, `directory`, `staff_emails`, `support_actors`, entitlement and billing fields |
| WorkOS fields | `client_id`, `api_key_env` (default `WORKOS_API_KEY`), optional `api_url` (default `https://api.workos.com`), `issuer_url` (default `<api_url>/user_management/<client_id>`; set only for a custom auth domain) |
| Entitlements | `entitlements` plan catalog and assignments; see [allowances](../../hosted-allowances.md) |
| Shared entry and registry | `detent cloud serve --entry-config PATH` and `detent hub migrate-shared-origin` |

Hosted and entry YAML parsers reject unknown fields.

The customer Hub service examples require an existing `gh` login accessible to
the Hub service identity or both `DETENT_HUB_GITHUB_APP_ID` and
`DETENT_HUB_GITHUB_APP_PRIVATE_KEY` in its private environment. A hosted Hub
started with `--hosted-config` skips CLI credential lookup and runs without Hub
GitHub transport when either App value is missing. See
[GitHub modes](../../hub-self-hosting.md#github-modes) for credential setup and
runner fallback.

## Shared entry

`detent cloud serve` runs the shared entry on a loopback port behind the public
TLS proxy. These fields identify the public site. Use the existing platform, object storage and runtime database settings for the installation.

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

PostgreSQL stores platform membership separately from organization membership. `platform.bootstrap_admin_email` names the operator's verified email;
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
`shared_entry.public_keys`. PostgreSQL owns the platform registry,
authentication sessions, login transactions and content-free audit records.
Install directories contain disposable runtime files, never application state.

### Upgrading a registered organization

Configure `tenant_postgres` on the entry for both self-service and registration-only deployments. Point it at the existing Hub database with its restricted runtime login. Keep `storage.postgres_dsn_env` on the platform login. Preserve the signing key, encryption keyring, public URL, organization and provider IDs, allocation generations and registered endpoints. The entry retains endpoint values for binding checks and routes traffic in process.

Fence the previous entry and all tenant Hub writers before applying migration `20261118020844_github_project_receipts.sql`. It adds the accepted project to the webhook receipt primary key. Earlier writers use the old conflict target. Apply the migrations with the migration role, then start the shared entry. After this schema change, recovery requires a compatible binary or restoration of the verified pre-migration backup.

On first activation the shared Hub imports the registered organization's settings from its original `tenant.yaml`, including the bootstrap subject, conversation settings, usage prices and workspace policy. For a managed organization the source is `allocation.tenant_root/ORG/tenant.yaml`. For registration-only operation place the original file at `state_directory/tenants/ORG/tenant.yaml`. The entry checks the organization and provider IDs, validates the settings and records the configuration and workspace object references together in PostgreSQL. Later activations load the imported settings. Keep the original file available until import succeeds. An older registration or a workspace policy alone still needs the original hosted settings.

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
limit fail to load.

The entry enforces host memory, disk and concurrent provisioning limits. A request that exceeds a limit records a retryable `capacity` failure before it creates a provider organization. The memory floor reads
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
with that token (at least 32 bytes, read from the entry's environment). Without them
complimentary access cannot be granted on the shared deployment.

Provisioning creates the organization's database binding and cursor keys in a tenant transaction. Ready organizations reopen their existing bindings. Closing an organization releases its service view. The host owns the physical pool until shutdown.

An owner requests deletion at `/organizations/ORG/delete` with a current provider session, CSRF token and the organization's name. Deletion marks the matching generation as deleting, revokes access, joins tenant work and drains admitted archive publishers. The entry then erases the organization's object-storage bindings and records the deletion tombstone. An interrupted erasure retains its receipt and resumes through the allocator.

### Backup and recovery

Back up the shared PostgreSQL database with verified PostgreSQL tooling,
including platform registry and authentication records and tenant data. Keep
object storage backups and install credentials separately. Test an isolated
restore, preserve tenant bindings and generations, and fence the old deployment
before routing production traffic to the restored Hub. Runners retain no local
application database.

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
