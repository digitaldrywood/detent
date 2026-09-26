# Hub deployment examples and shared-site target

[Caddyfile](Caddyfile) and [detent-hub.service](detent-hub.service) are currently
supported **customer-operated, free self-hosting** examples. They serve a single
Hub at the customer's chosen origin with scoped bearer auth. Follow the
[self-hosting runbook](../../hub-self-hosting.md) for installation, credentials,
backup and recovery. They require no Detent subscription or billing connectivity.
Changing their hostname to hub.detent.build does not create the shared product.

The operator-hosted product has one public site at `https://hub.detent.build`,
a shared identity/provisioning entry and metadata registry, and dedicated private
tenant Hub processes with separate databases. All use the Detent binary; the
entry role is additional work in #2341/#2342. The public reverse proxy forwards
to that entry service, never chooses a tenant with a URL rewrite. The entry uses
[authenticated private routing](../../cloud-hub-rfc.md#shared-site-control-and-tenant-storage)
and each tenant verifies its own immutable binding. This directory installs no
shared-site service and authorizes no deployment, DNS/account change or purchase.

## Configuration availability

| Surface | Supported today | Shared-site target, not yet implemented |
| --- | --- | --- |
| Customer Hub | `detent hub serve --database PATH --listen ADDRESS --github-disabled`; private `DETENT_HUB_ADMIN_TOKEN`, optional TLS/trusted-proxy flags | Remains free; deployment mode independent of customer-selected auth |
| Reserved WorkOS tenant | `--hosted-config PATH`; root `organization_id`, `workos_organization_id`, `bootstrap_subject`, `public_url`, `workos`, `directory`, `staff_emails`, `support_actors`, entitlement/billing fields | Compatibility importer preserves IDs; no manual tenant YAML/bootstrap user/public origin at signup |
| WorkOS fields | `client_id`, `api_key_env` (default `WORKOS_API_KEY`), optional `api_url`, `issuer_url` | Same explicit provider wiring under independent `auth`; one shared callback and invitation entry |
| Pilot entitlements | Existing `entitlements` plans/assignments and separate administrator environment reference; see [allowances](../../hosted-allowances.md) | Allocator assigns a configured versioned free plan; no auth-provider inference |
| Stripe | Optional `billing.account_id`, `customer_id`, `portal_configuration_id`, `api_key_env`, `webhook_secret_env`, `grace_seconds`, `reconcile_seconds`, `prices`; test keys only | Optional `billing.mode: test/live`; registry owns customer mappings; no root per-customer configuration |
| Shared entry and registry | `detent cloud serve --entry-config PATH` (see [shared entry](#shared-entry)); tenant `shared_entry` block; `detent cloud registry register/list`; `detent hub migrate-shared-origin` | Self-service allocator and admission (#2342); billing mode (#2343) |

The current hosted YAML parser rejects unknown fields. Do not pass the proposed
site configuration further below to `--hosted-config` or `--entry-config`; its
allocation and billing blocks arrive with #2342/#2343.

## Shared entry

`detent cloud serve` runs the shared entry on a loopback port behind the public
TLS proxy (nginx or Caddy forwards `hub.detent.build` to it unchanged; it never
rewrites paths to choose a tenant). Its configuration:

```yaml
public_url: https://hub.detent.build
listen: 127.0.0.1:8017
state_directory: /var/lib/detent/cloud
staff_emails: []
support_actors: []
assertion:
  issuer: detent-cloud
  signing_key_env: DETENT_CLOUD_ASSERTION_KEY
workos:
  client_id: client_example
  api_key_env: WORKOS_API_KEY
```

`detent cloud assertion-key` prints a new signing seed and public key; put the seed
in the entry's private environment and the public key in each tenant's
`shared_entry.public_keys` (see [hosted identity](../../hosted-identity.md#shared-entry-tenant-configuration)).
`state_directory` holds two single-owner SQLite files: `registry.db` (organization
IDs, provider organization IDs, names, private endpoints, generations) and the
private `auth.db` (hashed session and login-transaction references, per-organization
provider sessions, content-free audit). Neither holds collaboration content.

Tenant endpoints are Unix sockets only. Before every connection the entry checks that the socket and its directory are owned by the entry's service user and that the directory is private (mode 0700), so another local process cannot impersonate a restarting tenant. Run the entry and tenants as the same service user.

Register each tenant while the entry is stopped; registration is idempotent,
refuses to reuse IDs or provider organizations, and changes an endpoint only with
a higher generation:

```sh
detent cloud registry register --registry /var/lib/detent/cloud/registry.db \
  --organization org_example --provider-organization org_workos_example \
  --name "Example" --endpoint unix:/run/detent/tenants/org_example.sock --generation 1
```

## Self-service provisioning

Adding an `allocation` block lets verified users create organizations without
operator YAML, a bootstrap user, DNS or a public port. It is opt-in operator
functionality; self-hosted Detent never uses it.

```yaml
allocation:
  tenant_root: /var/lib/detent/tenants
  socket_root: /run/detent/tenants
  max_tenants: 20
  max_concurrent_provisions: 1
  max_organizations_per_identity: 1
  retry_limit: 5
  min_free_disk_bytes: 2147483648
  min_available_memory_bytes: 536870912
  allowed_domains: []
  allowed_emails: []
  entitlements: {}
  entitlement_administrator: pilot-operator
  entitlement_admin_token_env: DETENT_ENTITLEMENT_ADMIN_TOKEN
```

`max_tenants` and the free-memory/disk floors are admission limits: a request that
does not fit is stored as a retryable `capacity` failure before any provider or
filesystem effect, and ready tenants are never disturbed. The memory floor reads Linux `MemAvailable`; where memory cannot be measured a configured floor refuses admission, so leave it at 0 on other platforms. Set the limits from measured
tenant usage (#2308), not guesses. `allowed_domains`/`allowed_emails` bound a pilot;
empty lists admit any verified account. `entitlements` is the tenant's
[versioned plan catalog](../../hosted-allowances.md); its `base` plan is the free
plan every new organization starts on, and no Stripe customer or card is created.
`entitlement_administrator` and `entitlement_admin_token_env` are optional; when set,
each tenant accepts complimentary grants on `POST /api/v2/organizations/ORG/entitlements`
with that token (at least 32 bytes, read from the entry's environment and passed to
tenants as an environment variable, never written to `tenant.yaml`). Without them no
operator can grant complimentary access on the shared deployment.

For each organization the entry creates `tenant_root/ORG/` (mode 0700) holding the
generated `tenant.yaml` (no secrets), `hub.db` and a private per-tenant Hub admin
token, and runs `detent hub serve --hosted-config ... --listen unix:socket_root/ORG.sock`
as a supervised child (restarted with backoff; stopped when the entry stops). The
child receives only `PATH`/`HOME`/`TMPDIR`/`LANG`/`TZ`, the WorkOS key variable, the
optional entitlement token variable and its own admin token. Each tenant owns its SQLite file exclusively; never place
`tenant_root` on a network filesystem.

Deletion is owner-only (`/organizations/ORG/delete`, current provider owner, typed
name, CSRF). It marks the organization `deleting`, revokes every authorization and
tenant session, stops the tenant and records a permanent `deleted` tombstone that
cannot be routed or resurrected; an interrupted deletion resumes automatically. Tenant data stays on disk for the operator's
published retention process; the entry never erases customer data, including after
a failed signup.

### Backup and recovery

Back up `state_directory/registry.db` and every `tenant_root/ORG/hub.db` together,
with the entry and tenants quiesced or through each owner's online backup
(`detent hub backup` for a stopped tenant). `auth.db` holds only sessions and login
transactions and is not restored: a restore starts with no sessions, and every user
signs in again. To restore one tenant, stop the entry, restore its `hub.db` with
`detent hub restore`, and if its binding moved, re-register it with a higher
generation; the registry refuses a lower generation or a reused ID, and deleted
organizations stay tombstoned.

The entry serves sign-in (`/auth/oidc/start`, `/auth/oidc/callback`), the chooser
(`/organizations`, JSON at `/api/cloud/organizations`), invitations (`/invite`,
`/invitations/join`) and sign-out, and routes `/organizations/ORG/...` and
`/api/v2/organizations/ORG/...` to the registered tenant with a signed assertion.
Browser requests need the host-only session cookie (`__Host-detent_session`, rotated on every sign-in; the CSRF secret carries over so other tabs keep working), a
per-organization authorization obtained through the common callback, a current
provider session and active membership; mutations also need the exact
`public_url` Origin and the per-organization CSRF token. Bearer requests are
forwarded as machine requests without cookies and the tenant authenticates the
token itself. Inbound forwarding and assertion headers are stripped, tenant
`Set-Cookie` headers are dropped, and responses carry `no-store` and a restrictive
Content Security Policy.

## Proposed site YAML contract

This is a reviewable schema proposal, not supported runtime configuration. Paths
and the two-tenant admission limit illustrate an isolated fixture only; production
limits require #2308 measurements and operator settings. Memory/disk byte values
must be measured positive integers before the configuration is deployable; the
placeholder strings deliberately prevent treating this as a working deployment.

```yaml
schema: 1
deployment:
  mode: operator_hosted
public_url: https://hub.detent.build
auth:
  provider: workos
  workos:
    client_id: client_example
    api_key_env: WORKOS_API_KEY
    issuer_url: https://api.workos.com
registry:
  database: /var/lib/detent-site/registry.db
allocation:
  tenant_root: /var/lib/detent-tenants
  max_tenants: 2
  max_concurrent_provisions: 1
  max_organizations_per_identity: 1
  memory_reserve_bytes: REPLACE_WITH_MEASURED_INTEGER
  tenant_memory_budget_bytes: REPLACE_WITH_MEASURED_INTEGER
  disk_reserve_bytes: REPLACE_WITH_MEASURED_INTEGER
  tenant_disk_budget_bytes: REPLACE_WITH_MEASURED_INTEGER
  retry_limit: 5
  retry_deadline_seconds: 900
proxy_auth:
  issuer: detent-site-example
  signing_key_env: DETENT_SITE_SIGNING_KEY
  mtls_certificate_file: /etc/detent-site/entry.crt
  mtls_private_key_file: /etc/detent-site/entry.key
  tenant_ca_file: /etc/detent-site/tenant-ca.crt
free_plan:
  id: pilot_free
  version: 1
```

The named free plan must exist in the configured versioned entitlement catalog;
example IDs/quantities are not approved commercial allowances. Generated tenant
configuration includes immutable organization/provider IDs, private endpoint,
database path and allocation generation, with scoped verification keys and service
identity. It contains no browser bootstrap subject or per-tenant public origin.
The allocator owns path/endpoint selection; request data cannot override them.
No wildcard DNS, certificates or public tenant ports are required. Co-located
services can use separately permissioned Unix sockets after equivalent peer
identity verification; remote services require authenticated private transport.

Proposed configuration validation must reject unknown fields, conflicting legacy
and site settings, a missing/non-HTTPS public origin outside loopback fixtures,
non-positive/unmeasured limits, untrusted paths/endpoints and mode/key mismatches.
`deployment.mode` defaults to `self_hosted` when omitted in the new schema;
`auth.provider` never changes it. `operator_hosted` explicitly enables hosted
resource policy; billing remains opt-in. Self-hosted mode rejects allocation,
registry and billing blocks before any Cloud/Stripe adapter starts. Authentication
adapters use their own validated issuer/client/secret configuration. WorkOS uses
the fields above; custom adapters must implement the supported auth interface,
not trust arbitrary proxy identity headers. Generic/local configuration remains
in its existing deployment surface; this proposal does not invent a custom-auth
protocol or imply that `--hosted-config` already separates those modes.

There is no automatic environment-to-YAML interpolation or implicit `.envrc`
loading. Environment references resolve only in the process that consumes them.
Missing named secrets fail startup without logging their values. In systemd use
a reviewed private `EnvironmentFile` or credential facility for the entry/billing
service; a developer's Mac shell does not populate a Linux service environment.
Tenant services receive only their scoped credentials, not the operator's WorkOS
or Stripe key. Never place secrets in the registry, command arguments or reports.

## WorkOS and Stripe wiring

The common WorkOS callback is exactly
`https://hub.detent.build/auth/oidc/callback`, with User invitation URL
`https://hub.detent.build/invite`. Every organization uses these values. Configure
client ID, API key and issuer from the same provider environment. See
[identity setup](../../hosted-identity.md#workos-application-setup) for invitation
verification and staging separation. Existing-origin migration must use the
[controlled procedure](../../hosted-identity.md#existing-origin-migration).

The target optional billing block keeps the pilot account/portal/price/plan fields
but replaces static `customer_id` with durable organization mappings and adds
`mode`. For example, append this **proposed** test block only with a matching paid
plan catalog and independently configured test account/portal/price objects:

```yaml
billing:
  mode: test
  account_id: acct_example
  portal_configuration_id: bpc_example
  api_key_env: DETENT_STRIPE_TEST_KEY
  webhook_secret_env: DETENT_STRIPE_TEST_WEBHOOK_SECRET
  grace_seconds: 86400
  reconcile_seconds: 120
  prices:
    - price_id: price_example
      label: Extended pilot
      plan: {id: pilot_extended, version: 1}
```

`test` is the billing default, never inferred from auth. Its exact webhook is
`https://hub.detent.build/webhooks/stripe/test`. Explicit separately authorized
`live` activation instead uses `DETENT_STRIPE_LIVE_KEY`,
`DETENT_STRIPE_LIVE_WEBHOOK_SECRET`, matching live account/price/customer/portal
bindings and `https://hub.detent.build/webhooks/stripe/live`. Each environment's
webhook secret, event livemode and account must agree; there is no fallback
between environments. Staging substitutes its separately configured origin.
See [billing lifecycle and rollback](../../hosted-billing.md#environment-separation-and-shared-webhooks).
Free signup omits billing and creates no Stripe customer or card requirement.

Before any deployment, the implementation must publish its actual CLI/schema
reference and validate this contract with synthetic two-tenant/provider fixtures,
capacity refusal and restore evidence. The design review does not install units,
change DNS/TLS, register WorkOS/Stripe endpoints, or activate live charging.
