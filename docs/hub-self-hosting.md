# Self-hosted Hub operations

Moving an existing standalone board? Start with the
[local board to local Hub migration runbook](hub-local-migration.md) for backups,
pilot cutover, import prerequisites and rollback before replacing its service.

One customer-controlled Linux host or ordinary VM, one Hub process, and local
durable storage are the supported deployment described here. Use the released
Detent binary and systemd, with a TLS reverse proxy. This is single-host operation
with explicit downtime for maintenance; there is no active-active replication,
automatic failover, or high availability guarantee. Never share a live SQLite
file over NFS/SMB, mount it into runners, or run a second owner against it.

Self-hosted Detent is free. No WorkOS, Stripe, Detent Cloud account,
`cloud.detent.build` callback, Detent subscription or purchased infrastructure is
required. Operators choose authentication, including WorkOS/custom/generic/local
options; selecting WorkOS must never enable Detent billing. An external auth
provider may have its own connectivity/account requirements. The currently
supported Hub bearer deployment below has no such dependency. Shared-site mode
and self-hosted WorkOS configuration separation are design targets documented in
[the deployment examples](examples/hub/README.md), not new flags in this runbook.
The native-only example disables GitHub transport.
Durable artifacts are optional; local-only artifact availability depends on
retaining the runner and its workspace.

## Install one host

Select a reviewed release version and architecture from the existing
[release distribution](release.md). Verify the archive/package checksum before
installation and keep that version's binary, checksum and deployment files for
recovery. Linux packages install `/usr/bin/detent`; archive installations must
install the executable at that path or adjust the example unit. Do not use an
unversioned download URL for an upgrade. The release includes this runbook and
the [Hub unit](examples/hub/detent-hub.service) and
[Caddy example](examples/hub/Caddyfile). These examples deploy a customer-operated
Hub, not the shared cloud.detent.build entry/registry. See the
[deployment boundary](examples/hub/README.md); equivalent files are in the same tagged
source checkout. Native package documentation lives under `/usr/share/doc/detent/docs`.

On a clean Linux host with systemd, install the selected Detent package and a
separately pinned Caddy package through your normal OS package process. Then:

```sh
sudo useradd --system --home-dir /var/lib/detent-hub --shell /usr/sbin/nologin detent-hub
sudo install -d -m 0700 -o detent-hub -g detent-hub /var/lib/detent-hub
sudo install -d -m 0700 -o root -g root /etc/detent-hub
sudo install -m 0644 docs/examples/hub/detent-hub.service /etc/systemd/system/detent-hub.service
sudo install -m 0600 /dev/null /etc/detent-hub/credentials.env
sudoedit /etc/detent-hub/credentials.env
```

Use the corresponding `/usr/share/doc/detent/docs/examples/hub` source path when installing
from a native package without a source checkout. In `credentials.env`, set
`DETENT_HUB_ADMIN_TOKEN=<fresh-high-entropy-token-from-your-secret-manager>`.
Generate at least 32 random bytes; the bracketed value is a placeholder, not a
credential. Keep the real value out of shell arguments/history, repositories,
screenshots, logs and issue reports. systemd reads the root-only file before
dropping privileges. Configure your real DNS name in Caddy, install its reviewed
configuration, allow inbound HTTPS, and restrict port 7777 to loopback.

```sh
sudo systemd-analyze verify /etc/systemd/system/detent-hub.service
sudo systemctl daemon-reload
sudo systemctl enable --now detent-hub
sudo systemctl status detent-hub
```

These commands operate on the example installation only. Use this dedicated
Hub unit for `hub serve`. The Hub has no source checkouts, agent login
sessions or execution workspaces on this host. Do not expose the backend port
by relying on `--trusted-proxy` alone: that flag declares trust, it does not
create TLS or configure a firewall. Alternatively use `--tls-cert` and
`--tls-key` with an explicit public listen address. Do not log Authorization
headers or request/response bodies at the proxy. Keep private keys readable
only by the TLS owner. The API continues to enforce scoped bearer auth behind
the proxy; arbitrary identity headers are not an authentication integration.

## Open the web app without WorkOS

`detent hub serve` serves the existing web app when `--hosted-config` is
omitted. No WorkOS credentials or separate app flag are needed. For local use,
start it with a private database and the administrator token environment
variable described above:

```sh
detent hub serve --database ./hub.db --github-disabled
```

The default listener is `127.0.0.1:7777`. Open
`http://127.0.0.1:7777/work` in a private browser window. In the browser's HTTP
authentication prompt, use `operator` as the username and the Hub administrator
token as the password. The app uses the database's local organization and its
projects. Board, project settings and runner enrollment use the existing app
screens and APIs. Browser access requires an unrestricted administrator token;
scoped operator and worker tokens remain API/MCP credentials. Bearer-token API
and MCP access continue to work without a browser.

Cloud billing, hosted API keys, member invitations and email notifications are
not shown on a self-hosted Hub. Manage bearer credentials through the token API
described below. There is no browser sign-out control for HTTP authentication.

The browser caches HTTP authentication for its session; close the private window
to clear it. Revoking or rotating a token takes effect on subsequent requests.
Keep tokens out of URLs. Remote access still requires TLS or a trusted TLS
reverse proxy as described above; do not transmit this password over public
HTTP. Browser writes require a matching Origin, so a reverse proxy must preserve
the public Host and provide the original HTTPS scheme when `--trusted-proxy`
is used. A Hub using `--hosted-config` retains its hosted authentication flow.

## Durable paths and outbound access

| Location | Owner and recovery treatment |
| --- | --- |
| `/var/lib/detent-hub/hub.db` and SQLite sidecars | Hub-only local disk; back up through the owner or stopped-owner command |
| `/var/lib/detent-hub/hub.db.lock` | Ownership coordination; not a backup or a lock to manually remove while running |
| `/etc/detent-hub/credentials.env` | Secret manager/root; back up separately with restricted access and revocation records |
| systemd unit, TLS configuration/certificates | Versioned operator configuration; preserve with the selected binary/checksum |
| `/var/lib/detent-hub/backups/` | Private example staging path; copy finished snapshots to encrypted off-host backup storage |
| runner identity, config and workspace roots | Private runner-local state; reenroll credentials on migration |
| artifact catalog and object bucket | Separate durable service; Hub snapshots contain references, not artifact bytes |

Native metadata service operation requires no outbound Internet connection.
TLS certificate automation may require DNS/ACME access; private PKI can avoid
that dependency. Operators installing packages need access to their package
source. Runners need HTTPS to this Hub and, when configured, the independent
artifact service; their chosen Git host, model providers and build tools need
their own outbound access. These are customer-selected endpoints, not Detent
Cloud dependencies. Artifact service outbound access is limited to the selected
Hub and object store (plus any explicitly selected credential provider).

For GitHub integration, choose one of the modes below. Allow GitHub API traffic
from whichever process owns intake and the separately selected Git transport
from runners. Follow [GitHub profiles](github-profiles.md) for import, cutover
and field ownership; Cloud comments remain private.

## GitHub modes

These modes use existing Hub flags and environment variables; no additional
configuration keys or token scopes are required. The example service keeps
GitHub disabled by default. Hub credentials and runner credentials are separate.

| Mode | Use when | GitHub credentials on the Hub |
| --- | --- | --- |
| Hub disabled, runner fallback | Native-only operation or runner-owned intake and PR landing | None |
| Hub CLI fallback | An interactive Hub or a service with an explicitly provisioned CLI login | The Hub process's existing `gh` login |
| Operator-owned GitHub App | A service needs independent GitHub access and optional webhook intake | App ID, private key and, for webhooks, a shared secret |

### Enable manual intake

Before selective intake or historical import, an administrator must enable manual
import on the project. Read
`GET /api/v2/organizations/{organization}/projects/{project}/integration`, then
`PUT` to the same URL with a fresh `idempotency_key`, `expected_revision` set to
the returned decimal-string `revision`, and `intake: "manual"`. Include the
current `projection` and `repository_enabled` values to preserve those settings.
See [GitHub integration settings](github-profiles.md) for the request contract.

Confirm `manual_import_enabled: true` in the response. A bound native project
still reports `intake: "automatic"` for webhook intake; that value alone does
not enable manual import. Enabling manual import does not disable webhooks.

### Hub disabled: runner fallback

Keep `--github-disabled` in the service's `detent hub serve` command. It bypasses
Hub credential lookup and disables Hub import, outbox, reconciliation and webhook
verification, even if App variables are present. Pure native work needs no
GitHub repository or login.

For a native project that uses GitHub, associate the runner's matching checkout,
[enable manual intake](#enable-manual-intake), and use
[selective onboarding issue intake](hub-api.md#selective-onboarding-issue-intake).
An enrolled runner with access to that repository supplies its existing `gh`
credentials when Hub transport is unavailable; `runner_id` can select it.
PR landing continues through the configured runner GitHub integration and its
existing credentials and repository protections. Disabling Hub GitHub transport
does not disable that runner integration or bypass required reviews/checks.
Intake is explicitly requested, not an idle poll for new native issues; Hub
GitHub outbox writes are unavailable in this mode.

### Hub CLI fallback

Remove `--github-disabled` and leave both `DETENT_HUB_GITHUB_APP_ID` and
`DETENT_HUB_GITHUB_APP_PRIVATE_KEY` unset. On self-hosted startup the Hub runs
`gh auth token` and uses the returned token for its import, outbox and periodic
reconciliation clients. The `gh` executable must be on the Hub process's PATH,
and its login must be accessible to that process. Failure to resolve a token
fails startup. This mode reuses an existing login; it does not require a new PAT
or additional scopes.

An interactive login as a human does not provision the `detent-hub` service
user. The example unit has `ProtectHome=true`, so credentials under a human home
directory are unavailable, and a service may also lack the interactive user's
keyring session. If using this mode in a service, provision an existing authorized
CLI credential/configuration for the service identity in its accessible private
state, without weakening `ProtectHome` or copying a human's home directory.
Otherwise choose runner fallback or the App mode. Without a webhook secret the
webhook endpoint is unavailable; outbound polling is described below.

### Operator-owned GitHub App

Register your own App using GitHub's [registration guide](https://docs.github.com/en/apps/creating-github-apps/registering-a-github-app/registering-a-github-app).
You can own it under a personal account and install it on selected repositories
you control; organization administration is not a prerequisite for self-hosting.
An organization installation remains subject to that organization's existing App
approval policy. The hosted `detent-cloud` App and its Cloud webhook remain
unchanged; a self-hosted Hub uses its own App and origin.

Use the existing Detent App repository permissions: **Issues** and **Pull
requests** read/write; **Contents**, **Checks**, **Commit statuses** and
**Metadata** read-only. No organization permissions or user OAuth token are
needed for Hub intake. Install the App on the repositories bound to the Hub's
projects, for example `example-org/example-project`. Generate an RSA private
key in the App settings and deliver these values through private service secret
storage (such as the existing root-only `credentials.env`):

| Environment variable | Value |
| --- | --- |
| `DETENT_HUB_GITHUB_APP_ID` | Numeric App ID, not its client ID or installation ID |
| `DETENT_HUB_GITHUB_APP_PRIVATE_KEY` | PEM private key contents, not a filename; literal `\n` escapes are also accepted |
| `DETENT_HUB_GITHUB_WEBHOOK_SECRET` | High-entropy shared webhook secret, identical to the App's configured secret |

Remove `--github-disabled` from the service command. Both App ID and private key
must be nonempty to select App transport; setting only one falls back to the
CLI mode. The Hub resolves repository installations and refreshes installation
tokens itself; no installation-ID variable is needed. Keep secrets out of shell
arguments, source control and logs. The Hub reads the webhook secret from
`DETENT_HUB_GITHUB_WEBHOOK_SECRET` by default; `--github-webhook-secret-env`
selects a different environment variable name, not the secret value.

In the App settings, activate webhooks, keep SSL verification enabled, set the
same secret, and subscribe to **Issues** (`issues`) and **Issue comment**
(`issue_comment`). Set the webhook URL to your reachable HTTPS origin followed
by `/api/v1/webhooks/github`, for example
`https://hub.example.com/api/v1/webhooks/github`. The self-hosted Hub verifies
the GitHub HMAC signature itself. Bound native projects receive new issues in
Triage; later source updates follow the ownership rules in GitHub profiles.

### Webhook reachability and operation without webhooks

GitHub cannot deliver to a loopback-only Hub. A public HTTPS reverse proxy or
operator-managed HTTPS tunnel must forward the webhook path to the Hub, preserving
the request body and GitHub signature/delivery/event headers. Use the existing
[Caddy example](examples/hub/Caddyfile) or equivalent; keep the backend bound with
`--listen 127.0.0.1:7777` and add `--trusted-proxy` when the proxy terminates TLS.
That flag does not create a tunnel, TLS or firewall rules. Keep the backend
private and the other API routes protected by scoped bearer authentication;
GitHub webhook requests authenticate with their signature, not a Hub API token.

To stay loopback-only, leave the App webhook inactive and omit the webhook
secret. With either Hub CLI or App credentials, compatibility projects still
receive periodic issue reconciliation: `--reconcile-interval` defaults to 10
minutes, and `--full-repair-interval` to 24 hours, with a full repair at startup.
These existing outbound reads need no inbound endpoint. Native projects skip
issue reconciliation: it does **not** discover new native GitHub issues.
Without reachable webhooks, [enable manual intake](#enable-manual-intake) before
periodically requesting the existing selective intake operation linked above
(using the Hub transport or runner fallback), or use
explicit [historical import](github-profiles.md#import-and-inspect) with Hub
credentials. There is no built-in periodic native issue-intake poll; automatic
new-issue intake requires reachable webhooks.

## Authentication, projects and runners

The bootstrap administrator is inserted once. Changing its environment value on
restart does not rotate it or resurrect a revoked token. Use the existing
`POST /api/v1/tokens`, `POST /api/v1/tokens/{id}/rotate`, and
`DELETE /api/v1/tokens/{id}` operations. Plaintext creation/rotation responses
are returned once with `Cache-Control: no-store`; persist them directly to
private secret storage. Generic worker/operator tokens do not automatically
expire; explicitly rotate/revoke them and track their owners. Issue project
grants through `/api/v2/tokens/{id}/grants`. Use individual operator credentials
so author IDs remain attributable. Generic scoped bearer authentication works
without hosted identity; do not assume an OIDC proxy can mint Hub credentials.

Create the organization/project and its workflow states as administrator before
enrolling a runner. Approve the trusted repository/workflow descriptor through
[repository policy](config.md#repository-policy-with-hub-execution). A runner
cannot grant itself tags or repository access. An exact offline host stays
queued; tags constrain routing and do not grant authority. Logical runners
sharing a host share its capacity.

### Enroll a runner without the web app

Use the released binary on the runner host. No Cloud account or Enroll dialog is
needed. Substitute your Hub URL, organization and project IDs in these examples.

1. As an instance administrator, send an authenticated
   `POST /api/v2/organizations/org_example/runner-enrollments` to your Hub with
   this exact JSON body, replacing the project ID:

   ```json
   {
     "project_ids": ["prj_example"],
     "operations": ["read", "claim", "heartbeat", "events", "collaborate"],
     "ttl_seconds": 300
   }
   ```

   Omit `runner_id` and `machine_id`: this is token-first enrollment, binding
   fresh host-generated IDs on redemption. The response contains `id`, `token`
   and `expires_at`, with `Cache-Control: no-store`. The token is usable once
   within 300 seconds; the supported TTL range is 1–900 seconds.

   Alternatively, connect an authorized MCP client to this self-hosted Hub
   using [API & MCP setup](api-mcp-setup.md), and call
   `create_runner_enrollment` with these arguments (use a fresh `request_id`
   for each new enrollment):

   ```json
   {
     "project_id": "prj_example",
     "request_id": "enroll-build-host-1",
     "enrollment": {
       "project_ids": ["prj_example"],
       "operations": ["read", "claim", "heartbeat", "events", "collaborate"],
       "ttl_seconds": 300
     }
   }
   ```

   Use a private originating connection and deliver the returned token through
   private secret storage. Keep tokens out of transcripts, logs, repositories
   and issue reports. The MCP token is returned only to its originating connection.

2. On the runner host, load that token from private secret storage into the
   environment variable `DETENT_RUNNER_ENROLLMENT_TOKEN`. Do not put its value
   in shell history or any command-line argument. Run:

   ```sh
   detent hub runner register \
     --url https://hub.example.com \
     --organization org_example \
     --name 'Build host' \
     --capacity 1 \
     --isolation-tier sandbox \
     --config "$HOME/.config/detent-runner/global.yaml" \
     --workspace-root "$HOME/detent-runner"
   unset DETENT_RUNNER_ENROLLMENT_TOKEN
   ```

   Choose an absolute private `--config` path outside repositories and execution
   workspaces. Registration stores the host identity in `identity.json` beside
   that config. Never copy it to another host. Choose a `--workspace-root` owned
   by this runner for managed checkouts; the example paths are also the defaults.
   Registration writes the Hub connection and identity configuration, and fetches
   allowed project routing from the Hub. Keep provider login on the runner; model
   selection comes from organization/project settings. Use `native-trusted`
   instead of `sandbox` only for full agent access on a dedicated machine.

3. Start the runner with the config you just created:

   ```sh
   detent --config "$HOME/.config/detent-runner/global.yaml" --headless
   ```

   For a background service, use `detent hub runner register --service` with the
   same flags instead; it installs and starts the runner service. Use only one
   running owner of this config. Keep the `runner_id` and `machine_id` printed
   by registration for inspection and any exact policy selectors.

4. From your authorized MCP client, call `runner_project_diagnostics`:

   ```json
   {"project_id":"prj_example","runner_id":"runner_example"}
   ```

   Replace `runner_example` with the returned runner ID. Inspect its connection,
   project setup and local admission observations, including observation times
   and unavailable fields. An offline runner or missing setup is not verified
   readiness; correct the reported setup or policy problem and read again.
   Selecting the runner explicitly avoids ambiguity when several runners report.

### Credential lapse and re-enrollment

Enrolled credentials last 24 hours from enrollment or renewal. The runner
renews its credential during normal operation. After a host has been offline
past that window, restart it with its existing config: renewal permits elapsed
expiry while the identity and grants remain valid. Revocation is permanent;
renewal cannot restore revoked authority.

If renewal is refused, or you are migrating to a new host, have an administrator
create a fresh one-use enrollment as above. Register with a new private config
directory (for example, `$HOME/.config/detent-runner-replacement/global.yaml`)
and an unused workspace root, supplying the token through
`DETENT_RUNNER_ENROLLMENT_TOKEN` again. New paths generate a fresh identity;
rerunning registration against the old identity cannot take over its reserved
IDs. Stop the old runner before starting its replacement, update any exact
runner/machine selectors and approve the resulting policy. Verify the new ID
with `runner_project_diagnostics`. Preserve old principal rows for history;
never reactivate them by editing SQLite.

See [Hub API enrollment](hub-api.md#scoped-runner-onboarding) for the separate
`init`/`enroll` flow when IDs must be bound before token creation, and
`detent hub runner --help` for renewal, rotation, revocation and fleet inspection.

## Model selection

Set the organization default once through `GET`/`PUT`
`/api/v2/organizations/{org}/model-selection`, or MCP
`get_organization_model_selection` / `update_organization_model_selection`,
using an organization-wide operator or administrator bearer token. Project
grants do not authorize organization settings. Project settings at
`/api/v2/organizations/{org}/projects/{prj}/model-selection` override individual
fields; setting the project `selection` to null restores organization
inheritance. See [the Hub API](hub-api.md#scoped-runner-onboarding) for request
fields and the corresponding project MCP tools.

An unconfigured self-hosted Hub uses the same seeded organization defaults as
a hosted Hub: Codex Sol (`gpt-6.1-sol`) at `high` for normal and complex work,
Codex Astra (`gpt-6-astra`) at `low` for planning and `medium` for validation,
and Astra at `medium` for `complexity:very-complex` work. Per-issue effort
remains clamped to the stage ceiling. Runners receive the effective selection
from the Hub; runner-local `global.agents.model_selection` in `global.yaml` is
ignored, and model selection does not belong in repository `detent.yaml`.

## Optional durable artifacts

Use the existing [artifact deployment](artifacts-deployment.md) and its systemd,
Caddy and S3-compatible configuration examples. It is an independently available
authorization/download process with a durable catalog/outbox and a private object
store. Execution runners may be disposable; the artifact process, catalog and
bucket are not. Keep them outside runner workspaces. No AWS compute, hosted
account or particular storage vendor is mandatory.

Run `detent artifacts --artifact-config /private/service.json verify-storage`
against the selected endpoint before enabling it. Only a passing live probe
establishes that provider's required conditional-write, integrity and optional
versioning behavior; local fixtures are not live provider certification.
Configure a dedicated scoped publisher credential, project service binding and
finite retention/capacity policy. Runners receive no bucket credentials. Omitting
durable configuration remains truthful local-only operation.

## Backup and export

`detent hub backup` is also the supported full-instance collaboration export.
It includes organizations, projects, stable issue/comment/change IDs, versions,
events, provenance, external references, identity rows, grants, approved policies,
outbox and artifact references. It is a SQLite snapshot, not a per-project JSON
merge format. Importing into an existing Hub or selectively merging tenants is
unsupported. Raw artifacts, TLS files, runner workspaces and provider credentials
are separate backups. Treat snapshots as sensitive customer data even though
Hub bearer credentials are stored as hashes.

For routine backups without an outage, an instance administrator can ask the
running self-hosted Hub to write a snapshot through its authenticated admin API.
Create a private backup directory writable by the Hub service user, then send
an absolute path on the **Hub host** (not the HTTP client's machine):

```sh
sudo -u detent-hub install -d -m 0700 /var/lib/detent-hub/backups
curl --fail-with-body --request POST 'http://127.0.0.1:7777/api/v2/admin/backup' \
  --header "Authorization: Bearer $DETENT_HUB_ADMIN_TOKEN" \
  --header 'Content-Type: application/json' \
  --data '{"output":"/var/lib/detent-hub/backups/<unique-snapshot>.db"}'
sudo -u detent-hub /usr/bin/detent hub verify --database '/var/lib/detent-hub/backups/<unique-snapshot>.db'
```

Use HTTPS when calling through the reverse proxy. Only an unscoped instance
administrator credential can create a backup; project credentials and runners
cannot. The endpoint is unavailable on hosted Cloud Hubs. Success returns
`204 No Content`; neither snapshot bytes nor the output path appear in the
response body, including errors. A lost HTTP response does not establish whether
the snapshot completed; inspect and verify the destination before repeating.

The snapshot uses SQLite's online backup API on a dedicated reader connection
with a fixed read transaction. It contains all writes committed when that read
snapshot begins; later commits remain on the live Hub. Writes continue during
the copy without holding the Hub writer connection. A long snapshot retains
its WAL read position, so allow disk space for concurrent writes. The new file
has `0600` permissions, existing destinations are never replaced, and failed or
cancelled copies remove their incomplete output. Wait for successful completion
before verifying or copying the file. No snapshot download endpoint is provided.

Offline CLI maintenance remains supported. Drain runners and stop the Hub
before using `hub backup` against its database. `backup` and `verify` acquire
the source's ownership lock and fail if another owner is running. They check
identity, integrity, foreign keys and supported schema without upgrading the
source.

```sh
sudo systemctl stop detent-hub
sudo -u detent-hub install -d -m 0700 /var/lib/detent-hub/backups
sudo -u detent-hub /usr/bin/detent hub backup --database /var/lib/detent-hub/hub.db --output '/var/lib/detent-hub/backups/<unique-snapshot>.db'
sudo -u detent-hub /usr/bin/detent hub verify --database '/var/lib/detent-hub/backups/<unique-snapshot>.db'
sudo systemctl start detent-hub
```

The running Hub uses WAL with normal SQLite locking, while its application
ownership lock prevents a second Hub from opening the database. This preserves
live WAL replication such as Cloud's Litestream setup. For routine off-host
copies, schedule the online admin operation, verify the detached snapshot,
compress it and upload that file to encrypted object storage. Offline backups
still make the Hub unavailable while checking and copying the database; restart
it even when that backup fails and choose an outage window that cannot outlast
active leases.

Replace angle-bracket placeholders before execution. Backup output must not
exist and must differ from the source. Copy only the completed snapshot to the
backup system. Never copy live DB/WAL/SHM files. Record checksum, capture time,
binary version, returned schema version, retention deadline, custody policy and
configuration revision outside the snapshot. Test an isolated restore regularly.

## Restore/import and move between Hubs

Use a trusted, unexpired snapshot and a **new** local destination. Fence the old
Hub and stop its runners before moving production traffic. Restoring does not
stop processes or revoke credentials on the source machine. A restored clone
must never run concurrently against the same external projection/workspace.

Load a fresh high-entropy `DETENT_HUB_ADMIN_TOKEN` from private secret storage in
the maintenance process environment, then run as the database owner:

```sh
detent hub verify --database '/private/backup/<snapshot>.db'
detent hub restore --database '/private/backup/<snapshot>.db' --output /private/restored/hub.db
detent hub serve --database /private/restored/hub.db --listen 127.0.0.1:7777 --github-disabled
```

Restore validates the source, copies through SQLite backup, migrates a private
staging copy, checks integrity, and publishes the new path without overwriting
anything. Before publication it revokes every copied generic/runner credential
and enrollment, releases every active lease, rotates cursor authority and creates
a fresh administrator principal. The supplied token must differ from every
source credential. JSON output gives the new administrator ID and schema, never
the token. Existing identities, grants, event ordering, policy provenance and
external references remain intact for attribution. Old pagination cursors must
be discarded. Attempts with released leases are presented as interrupted by the
existing run-history logic; they do not authorize resumed side effects.

Verify authenticated `/health`, content/history and references while isolated.
Create fresh human/operator and integration credentials, restore only currently
approved project grants, reenroll runners using new bindings, and reapprove
policies with exact ID selectors. Reconfigure review-policy principal IDs where
credentials changed. Set the service environment to the fresh administrator
credential; ordinary startup will not replace the copied revoked bootstrap row.

For artifacts, preserve catalog/service/artifact IDs and object versions, update
the trusted Hub origin, replace publisher credentials, and update the project
binding before reopening downloads. Existing grants depend on revoked principals
and must be reacquired. Keep current deletion markers independently of catalog
rollback and follow the artifact restore procedure. Hub backup alone cannot
restore object access. Keep GitHub transport disabled until an operator checks
pending outbox work against current GitHub state and resolves any already-applied
external effects; restoring a snapshot does not roll back a GitHub mutation.

## Upgrade, interruption and compatibility

For an ordinary binary deploy with an unchanged Hub schema, prepare the verified
new executable on the host before stopping the unit. Keep the previous executable
and an independently verified backup. Atomically replace the executable on the
same filesystem, then restart the Hub unit immediately. Linux keeps the old
process running from its original executable until the restart. Keep downloads,
builds, archive extraction, backup creation/verification and proxy reconfiguration
outside this restart window. Do not drain runners for this binary-only restart:
their existing lease and session continue when the Hub returns in time. Do not
restore an older database to roll back a same-schema binary; retain current run
events and lease ownership.

The restart budget is less than the configured runner renewal interval (30
seconds by default), including shutdown, database reopen and authenticated HTTPS
readiness through the existing proxy. Shutdown closes conversation streams and
control polls before waiting for HTTP handlers; the hosted activity stream exits
on its existing one-second tick. SQLite opens at the current schema without
repeating a full foreign-key scan. It still verifies ownership, identity, pragmas
and migration history; `hub verify` retains full offline integrity/foreign-key
verification. These changes do not extend a lease or accept an expired owner.

When adopting this procedure from a binary that verified foreign keys after
committing the final migration, use the existing offline `hub verify` during a
maintenance window first. A failed upgrade from that binary may already have
recorded its final schema version; unchanged-schema startup does not repeat its
data verification. Retain the verified snapshot and resolve any reported failure
before using the quick restart procedure.

A schema-changing upgrade requires a separate maintenance window. Rehearse the
selected binary's migrations against a verified snapshot before cut-over and
record the duration. The live Hub holds an exclusive SQLite connection and an
instance lock: a second binary cannot pre-migrate its live database while it
serves requests. Do not copy a migrated snapshot over the live database or open a
second owner; either loses or competes with current run events. If the measured
migration cannot fit the restart budget, let active runners finish while the old
Hub remains online, then stop the unit, create/verify the pre-upgrade backup and
start the prepared binary. Never hold a stopped Hub while waiting for active
runners to finish.

Startup applies embedded forward goose migrations before health becomes ready.
Foreign keys are checked inside the final migration transaction before recording
its version. Each migration is transactional; a crash or failed final validation
rolls back the unfinished migration. Earlier completed migrations remain applied
and the same binary retries the remainder on restart. Fix disk/permission
problems first.
Never manually advance the schema table or run the unrelated runner-store
`make db-migrate` against a Hub database.

The deployment/release owner must verify a normal production restart after
integration. Record the finalized Change head, deployed binary version, stop time
and first authenticated HTTPS-ready time; measure the entire unavailable interval
and require it to be below one renewal interval. Start a real runner session
before deployment and retain its work-item, machine, lease, fencing token, session,
run and attempt identifiers. After deployment, confirm renewal and successful
`run.finished` under those same identifiers, with one attempt and no replacement
dispatch. Keep credentials and content out of this evidence. A local reopen test
does not establish production deploy timing. The restricted staging deploy script
and production host integration are operator-owned, outside this source checkout;
their preparation/restart ordering must follow this procedure before claiming
production acceptance.

Schema 19 repairs the two formerly valid schema-18 branches. Migration 18 remains
`00018_change_viewed_files.sql`; onboarding moves to
`00019_project_onboarding.sql`. The forward-only, transactional migration creates
only missing tables. A viewed-files schema 18 retains all viewed markers and gains
onboarding; an onboarding schema 18 retains progress, revision and timestamps and
gains viewed-files storage. Hosted identity and change-version rows stay in place.
Both-table databases also retain their rows. Ordinary startup handles these
variants automatically; no schema-version edits or table deletion is required.
Use the backup procedure above before upgrading; keep the snapshot for rollback.

If restore is interrupted before publication, the requested destination is absent.
A private `.hub-restore-*` directory may remain beside it; retain it for diagnosis
or remove it only after confirming no maintenance process owns it. Retry from the
original backup to a new destination. Never serve a staging database. Once a
destination is published, it has already been migrated and old authority fenced.

A binary rejects a schema newer than it supports with `ErrUnsupportedSchema`
(`database=N supported=M`); there is no automatic downgrade. Keep the failed
database for diagnosis. Use the newer compatible binary, or restore the
pre-upgrade snapshot into a new path with a binary supporting that snapshot.
Switch the unit's database path only after validation. Never point an older binary
at the upgraded live file. Pre-upgrade backups retain the old schema because
offline backup does not migrate it. Recovery commands are available beginning
with the release that packages this runbook; keep that recovery binary too.

Prefer identical release versions across Hub and runners. Rolling overlap is
supported only where `/api/v2/capabilities` and the runner agree on protocol major
2, schema 1 and all required features. Version strings alone do not confer
compatibility. Unsupported majors/missing required features fail closed before
claim/start; older clients cannot bypass approved policy or provider reservations.
The v1 compatibility API remains for legacy configured clients and requires
GitHub compatibility configuration; it is not a fallback for a failed native
negotiation. Fixtures cover old/new feature sets, not every historical binary.

## Retention, deletion and expiration

Native collaboration currently has no automatic expiry and no supported
per-issue/organization purge command. Current content, revisions, comments,
Changes, events, idempotency responses and identity attribution remain until
whole-instance retirement. Do not promise scoped erasure or disable append-only
triggers. Choose a separate Hub per retention boundary if whole-instance deletion
is required. A finite per-record retention requirement requires further product
work before adopting this deployment for that requirement.

Operators must define a finite backup lifetime, log rotation and export expiry
in their own backup system. For example, a reviewed policy can expire snapshots
after 30 days; Detent does not schedule that expiration. Record the deadline and
refuse expired snapshots during manual restore. Do not keep indefinite copies in
download folders. Revocation records and the retirement/deletion ledger belong
outside rollbackable backups. Restore always invalidates old Hub authority, but
it does not erase previously exported content or apply a customer deletion ledger.

Whole-instance retirement means stopping/fencing Hub and runners, disabling
ingress and artifact authorization, recording the retired scope/IDs in the
external ledger, and deleting the database, sidecars, private staging directories,
exports, backups, service logs and relevant encryption keys through the storage
owner's approved erasure process. Verify expiry/deletion in every backup replica
and disallow restoring a retired snapshot. Do not reuse retired IDs. GitHub
projections and artifact objects/catalogs need their own authorized deletion and
verification; database deletion does not erase them.

Webhook raw payloads have the existing configurable retention (default 7 days);
typed collaboration/projection records are separate retained data. Artifact
policy enforces live/abandoned-upload/deletion-marker lifetimes; its backup system
owns object/catalog backup expiration. Deletion-marker retention must outlive
artifact backups plus abandoned uploads. See [artifact retention](artifacts-deployment.md#retention-and-accounting).

## Diagnostics

Use authenticated `/health`, `detent doctor`, the existing fleet/queue surfaces,
and private service logs. Supply credentials through environment/private files;
do not paste bearer headers or content into diagnostic reports.

| Symptom | Evidence and next action |
| --- | --- |
| No matching runner | Fleet/queue exclusion reason, heartbeat, tags, exact IDs, project access, drain state and host capacity; fix the intended binding or bring that host online; never widen selectors automatically |
| Expired/revoked identity | Unauthorized response and local identity expiry; renew before expiry, otherwise initialize/enroll a new binding and approve changed ID selectors |
| Policy mismatch | Hub policy descriptor/approval and locally resolved policy ID; inspect trusted repository/workflow changes and obtain explicit administrator approval |
| Storage outage | Artifact error (`storage_unreachable`, integrity/missing/expiry reason), service maintenance logs and `verify-storage`; restore configured storage access, preserve pending catalog/outbox, never advertise unverified bytes |
| GitHub compatibility/projection failure | `/api/v1/outbox/health`, `/api/v1/repositories/freshness`, `/health` and GitHub profile; distinguish disabled transport, missing permissions, rate budget, stale projection, conflict and dead-letter/operator action before retrying |
| Startup/schema failure | systemd exit status and private journal, `hub verify` while stopped; check local disk, permissions, ownership and binary/schema versions; do not remove an active lock |

## Reproduce the validation fixture

Build `tmp/detent` with `make build` and run
`python3 scripts/hub-smoke.py tmp/detent`. The fixture installs a binary into an
empty private directory, uses ephemeral loopback ports, verifies bootstrap,
restart, export/verify/import and revoked-source authentication, and cleans up
only its own child processes/files. No real credentials are used.

For Linux/container validation, cross-build the binary and Hub test executable
for the container architecture, copy them and the smoke script into a fixture
directory, then run a Python-capable Linux image with `--network none`, a read-only
fixture mount and private writable temp storage. Run the smoke script plus the
Hub recovery/routing/protocol tests there. No production deployment or paid
resources are needed. The required repository gate remains `make check`.

## Project Sprites credentials

Owners and admins can set, replace or remove a project's Sprites organization
token in project settings under Execution. The API is
`/api/v2/organizations/{organization}/projects/{project}/secrets/fly_sprites_token`:
GET returns only presence, organization slug and master key version; PUT accepts
`{"token":"..."}`; DELETE removes Detent's copy. Hosted requests require the
existing session, CSRF protection and a project grant. A self-hosted instance
administrator can use the same endpoints. Worker, runner and coordinator tools
have no plaintext read operation. PUT/DELETE use last-write-wins settings semantics;
they never persist request input or a token-bearing command receipt.

Hubs without provider secrets need no extra configuration. To enable encrypted
secrets, inject `DETENT_HUB_SECRET_KEYS` as a JSON object mapping positive integer
versions to standard base64-encoded **32-byte random master keys**, and set
`DETENT_HUB_SECRET_KEY_VERSION` to the active version. Supply these environment
variables through the same operator secret manager as `DETENT_HUB_ADMIN_TOKEN`.
Never put them in `detent.yaml`, hosted config, the database, shell history or
logs. The service refuses to start if a stored secret's key is missing or cannot
unwrap its data key.

Each value uses a separate random AES-256-GCM data key. Its data key is wrapped
with AES-256-GCM under the versioned master key. Both authenticate the
organization, project and secret kind, preventing ciphertext substitution across
rows; the wrapping also authenticates the master key version. The database
stores only ciphertext, nonce, wrapped key (including its wrapping nonce), version
and public organization metadata. Backups must keep the corresponding master
keys separately. Losing the keys loses access to stored credentials; possession
of both a database backup and its keys exposes those credentials.

For rotation, stop the Hub, add a new version to `DETENT_HUB_SECRET_KEYS` while
retaining all old versions, and select the new `DETENT_HUB_SECRET_KEY_VERSION`.
Run `detent hub secrets rotate --database /var/lib/detent/hub.db`. The existing
database ownership lock refuses rotation against a running Hub. Rotation
atomically re-wraps data keys and records the local operator identity; ciphertext
and value nonces remain unchanged. Repeating rotation at that version is a no-op.
Restart with the new active version; remove old keys only after every relevant
database is rotated, keeping them in separate archival storage for old backups.
A failed rotation rolls back every row. No master key is written to disk by this
command.

Setting a token validates `GET https://api.sprites.dev/v1/sprites` with Bearer
authentication. Redirects are refused and provider errors/bodies are never echoed.
Sprites organization tokens encode `org-slug/org-id/token-id/token-value`;
authenticated list success confirms that organization even before its first
sprite exists. Returned sprite organization fields must agree with the token's
slug. See the [Sprites list API](https://docs.fly.io/sprites/api/sprites/list-sprites)
and [Sprites authentication](https://docs.fly.io/sprites/cli/authentication).
Set, replace, remove, use (including validation attempts) and rotation events
carry actor, organization, project, kind and key version in `project_secret_audit`,
never a value. Successful secret mutations and their audit events commit together.
Internal use decrypts only for a provider call and clears mutable plaintext buffers;
Go strings and HTTP header allocations are transient, garbage-collected memory,
so this is not a guarantee of physical memory erasure. Credentials are never
passed to workers, Luna or sprite processes.

Use a dedicated Fly organization containing nothing else and configure a spend
alert. A Sprites organization token can create and drive sprites in that
organization. Removing it from Detent does not revoke it at Fly; revoke it at
[your Sprites account](https://sprites.dev/account) to invalidate it.
