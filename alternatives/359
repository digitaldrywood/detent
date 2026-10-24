# Key rotation and compromise response

## Two definitions of done

**Re-keyed:** no stored value depends on the old key, and the old key is gone
from every running environment. Keep retired keys separately for restoring old
backups; they must not remain in live environment files or adjacent backup copies
of those files.

**Replaced:** every credential that could have been read is revoked or regenerated
at its source. Routine encryption-key rotation needs only re-keying. Compromise
needs both. Re-encrypting a database does not protect an already leaked copy:
anyone holding that copy and its old key can still read its credentials.

`detent hub secrets status` checks the stored-data part of re-keying in the
databases selected for that invocation. It cannot prove credential replacement,
inspect other running environments, or inspect archived backups and browser-held
state. Require both its successful result and deployment/environment checks
before declaring re-keying complete. Status never revokes a provider credential.

## Key inventory

Entry means the shared Cloud entry process. Tenant passthrough means the entry
copies a value into tenant Hub environments; changing the entry environment alone
does not change an already running tenant. Standalone Hubs read their own
environment. All source links below identify the reader or credential lookup;
procedure links give ordered steps and verification details.

| Key or credential | Protects | Lives in | Stored-data dependency | Rotation procedure | Blast radius if wrong | Verification and code reader |
| --- | --- | --- | --- | --- | --- | --- |
| `DETENT_HUB_SECRET_KEYS` / `DETENT_HUB_SECRET_KEY_VERSION` | Sprites organization/project tokens, Slack webhooks; entry WorkOS access/refresh tokens and OAuth consent/registration seals | Hub, entry, tenant passthrough | Versioned encrypted organization/project secrets and entry authorizations; browser-held seals also reference versions | [Hub keyring](#hub-keyring) | Missing versions lose credential access; premature retirement breaks pending OAuth flows | Status over all Hub and auth databases, then environment audit; [environment reader](../internal/hubsecrets/envelope.go), [entry reader](../internal/cloudentry/service.go), [entry seals](../internal/cloudentry/sessions.go), [provider-secret readers](../internal/hubserver/project_secrets.go), [Slack reader](../internal/hubserver/slack_settings.go) |
| `DETENT_CLOUD_ASSERTION_KEY` (default `assertion.signing_key_env`) | Entry-to-tenant signed assertions; legacy entry sealing | Entry private key; tenant configuration gets public key | Legacy WorkOS tokens and legacy OAuth seals depend on it until migrated or abandoned | [Assertion key](#assertion-key) | Tenant assertion rejection; unreadable legacy tokens if changed before reseal | Auth status, client-backfill counts and authenticated tenant requests; [env reader and tenant config](../internal/cli/cloud.go), [legacy seal reader](../internal/cloudentry/sessions.go), [client reader](../internal/cloudentry/oauth_clients.go) |
| `DETENT_HUB_GITHUB_APP_PRIVATE_KEY` | GitHub App authentication and installation-token minting | Entry, Hub, tenant passthrough | None encrypted under it | [GitHub App key](#github-app-key-and-id) | GitHub intake and App operations fail across installations | Successful App-authenticated operation; [entry reader](../internal/cli/cloud.go), [Hub reader](../internal/cli/hub.go) |
| `DETENT_HUB_GITHUB_APP_ID` | Selects the App identity; this ID is not a secret | Entry, Hub, tenant passthrough | App installations belong to this identity | [GitHub App ID](#github-app-key-and-id) | ID/key mismatch prevents authentication; changing App changes installation authority | Confirm same App ID with replacement private key; [readers](../internal/cli/cloud.go), [Hub reader](../internal/cli/hub.go) |
| `DETENT_HUB_GITHUB_WEBHOOK_SECRET` | GitHub webhook authenticity | Entry, Hub, tenant passthrough | None | [GitHub webhook](#github-webhook-secret) | Legitimate events refused, or forged events accepted if exposed | Accepted signed delivery and replay logs; [entry reader](../internal/cli/cloud.go), [Hub reader](../internal/cli/hub.go) |
| `DETENT_STRIPE_TEST_KEY` / `DETENT_STRIPE_LIVE_KEY` | Stripe API access in the selected mode | Entry; tenant passthrough when allocation billing is configured | Billing references persist, but are not encrypted with this key | [Stripe API key](#stripe-api-and-webhook-keys) | Checkout, billing synchronization and payment operations fail in that mode | API access for the same mode/account; [default names and reader](../internal/cli/cloud.go), [tenant reader](../internal/cli/hub_hosted.go) |
| `DETENT_STRIPE_TEST_WEBHOOK_SECRET` / `DETENT_STRIPE_LIVE_WEBHOOK_SECRET` | Stripe webhook authenticity in the selected mode | Entry; tenant passthrough when allocation billing is configured | None | [Stripe webhook](#stripe-api-and-webhook-keys) | Billing events refused or forged | Accepted delivery for the configured mode; [env reader](../internal/cli/cloud.go), [tenant reader](../internal/cli/hub_hosted.go), [verifier](../internal/billing/webhook.go) |
| `DETENT_ATTACHMENTS_ACCESS_KEY_ID` / `DETENT_ATTACHMENTS_SECRET_ACCESS_KEY` | Object-store access for attachments | Entry; explicitly excluded from tenant passthrough | Objects and references remain, but are not encrypted by this credential | [Object-store credentials](#object-store-credentials) | Attachment uploads, downloads or deletion fail; excessive scope exposes stored objects | Upload and read through the attachment owner; [reader and passthrough exclusion](../internal/cli/cloud.go) |
| `WORKOS_API_KEY` (default `workos.api_key_env`) | WorkOS authentication/session and organization operations | Entry, hosted Hub, tenant passthrough | Provider sessions remain external; stored provider tokens use the Hub keyring | [WorkOS API key](#workos-and-openai-api-keys) | Sign-in, session validation and organization operations fail | Sign-in and session revalidation; [entry reader](../internal/cli/cloud.go), [Hub reader](../internal/cli/hub_hosted.go) |
| `OPENAI_API_KEY` | OpenAI conversation backend access | Hub, tenant passthrough | No stored values encrypted with it | [OpenAI API key](#workos-and-openai-api-keys) | Conversation/model requests fail; exposed key permits provider usage | Configured backend request succeeds; [reader](../internal/cli/hub_conversation.go), [passthrough](../internal/cli/cloud.go) |
| `DETENT_HUB_ADMIN_TOKEN` | Initial Hub administrator bearer authentication | Hub environment; Cloud launcher loads each tenant's private `admin-token` file | Hub stores a hash; startup inserts only a missing bootstrap row | [Admin token](#hub-admin-token) | Administrator lockout; stale file does not repair a rotated/revoked DB token | New token authenticates, old token fails; [env reader](../internal/cli/hub.go), [file reader](../internal/cloudentry/launcher.go), [bootstrap lookup](../internal/hubserver/api_auth.go) |
| Detent API keys (no fixed server env) | Scoped Hub API or Cloud personal/service access | Caller secret storage; Hub or entry stores hashes only | No decryptable key material; independent of encryption/assertion keys | [Detent API keys](#detent-api-keys) | Callers lose access; exposure grants the key's scope | Replacement works within scope and old key fails; [Hub lookup](../internal/hubserver/api_auth.go), [Cloud lookup](../internal/cloudentry/access_keys.go) |
| Runner credentials (no fixed server env) | Enrolled runner authentication and scoped execution authority | Runner private identity file; Hub stores hash only | Identity/leases persist; credential is renewable, not encrypted with a master key | [Runner credentials](#runner-credentials) | Runner cannot claim, renew or report work | Identity validation and renewal succeed; [Hub lookup and rotation](../internal/hubserver/runner_identity.go), [CLI reader](../internal/cli/hub_runner.go) |
| MCP OAuth grants (no fixed server env) | Delegated MCP access | Connector holds bearer tokens; entry stores access and refresh hashes only | Grant, client metadata and redirect URIs persist independently of assertion sealing | [MCP OAuth grants](#mcp-oauth-grants) | Connector loses access, or retains stolen authority until revoked | Refresh/access succeed for new grant, revoked grant fails; [token lookup](../internal/cloudentry/oauth_tokens.go), [client lookup](../internal/cloudentry/oauth_clients.go), [revocation](../internal/cloudentry/oauth_grants.go) |

Stripe defaults follow `DETENT_STRIPE_<MODE>_KEY` and
`DETENT_STRIPE_<MODE>_WEBHOOK_SECRET`, with `MODE` expanded to `TEST` or `LIVE`.
Configured `api_key_env` / `webhook_secret_env` names override those defaults;
tenant billing reads its configured names. Rotate every configured consumer,
including any custom environment names.

## Routine rotation procedures

Use the operator's secret manager to generate and deliver replacements. Never
put secret values in commands, shell history, logs, tickets or this repository.
Take owner-supported database backups and preserve their corresponding keys
separately before encryption-key changes. Plan for service restart interruption;
these procedures do not provide high availability. After a passthrough change,
restart the entry and affected tenant Hubs with the intended environment.

### Hub keyring

1. Generate a new random 32-byte master key, standard-base64 encode it, and add
   it under a new canonical positive integer version in `DETENT_HUB_SECRET_KEYS`.
   Keep every old version. Never change the material of an existing version,
   duplicate version names, or reuse identical material across versions.
2. Prepare a candidate environment with the expanded keyring and the **new**
   `DETENT_HUB_SECRET_KEY_VERSION`. Leave the deployed environment unchanged.
   Run status with this proposed environment, not the currently deployed one:

   ```sh
   detent hub secrets status --tenant-root "$TENANT_ROOT" --auth-database "$AUTH_DATABASE"
   ```

   These shell variables denote operator-selected paths, not secret values.
   `--tenant-root` scans each tenant directory's `hub.db`. For one Hub substitute
   `--database "$HUB_DATABASE"` for `--tenant-root "$TENANT_ROOT"`. Omit
   `--auth-database` for deployments without an entry, or use it alone to inspect
   only entry state. Do not combine `--database` and `--tenant-root`.
3. Expect exit **3** if existing values use an older version or legacy entry
   sealing. Exit **0** is also safe when there are no such values. Exit **2**
   means preflight failed: resolve invalid configuration, missing keys,
   `unwrap_failed`, or unreadable databases/tables before deploying. Legacy
   entry counts alone do not prove that the current assertion key can open them;
   retain that key and inspect reseal completion before changing it.
4. Deploy the candidate environment, making the new version active, and restart
   the entry and all Hubs. Each serving Hub runs one background rewrap pass;
   the entry runs a background authorization reseal pass. Concurrent credential
   updates win over a pass. Investigate logged failures; after correcting their
   cause, a restart runs another pass. Do not remove old versions while work
   remains. See [Hub rewrap](../internal/hubserver/secret_rewrap.go) and
   [entry reseal](../internal/cloudentry/auth_secrets.go).
5. Re-run status across the entire tenant root **and** entry auth database until
   exit **0**. Require the retiring version to appear in `retirable` across the
   full selected inventory. Include inactive tenant databases and every other
   Hub using the keyring; one database's result cannot authorize fleet retirement.
   Status reads live SQLite WAL state without a writer lock or migrations.
   Its checks authenticate wrapped data keys, not provider validity or successful
   decryption of every secret payload.
6. Check `auth_database.oauth_clients_without_redirect_uris` before retiring keys
   used for entry sealing. Allow startup client backfill to finish; investigate
   a nonzero count. Status exit 0 does not assert client backfill completion.
   Pending registrations not yet persisted and consent cookies are outside its
   inventory: let their flows finish/expire or have users redo them after old
   sealing-key retirement. New flows use the active key.
7. Remove the old version from every live environment and restart its consumers.
   Confirm status remains at exit 0 and no running process retains the old
   environment. Keep all writers on the same active version so they cannot
   reintroduce an old dependency. Remove old material from environment backup
   copies beside the live file. Archive it separately, access-controlled and
   associated with the backups/replicas that still need it. Do not discard it
   until those retained copies expire or are replaced.

Status emits one JSON document. Hub problems include database, organization,
project, kind, key version and `updated_at`; counts group table/kind/version.
Entry auth status reports format/version/kind counts and problems, plus client
backfill counts. It does not identify affected users. `retirable` concerns
references in scanned data only; it is withheld on incomplete database scans.
See [CLI flags and exit codes](../internal/cli/hub_secrets.go),
[Hub report](../internal/hubserver/secret_status.go) and
[entry report](../internal/cloudentry/auth_secret_status.go).

### Assertion key

1. With the **current** assertion key still installed, configure the Hub keyring
   and restart the entry. Complete the keyring procedure, including auth status
   exit 0 with no legacy provider-token values and completion of client redirect
   URI backfill. This migrates WorkOS tokens off the assertion-derived seal.
2. Generate a replacement Ed25519 signing key using the existing
   `detent cloud assertion-key` command, capturing its sensitive output directly
   into private secret storage. Keep the Hub keyring unchanged. Coordinate the
   entry private key with each tenant's trusted public keys: generated tenant
   configuration receives the public key from the entry; externally managed
   tenants must trust the replacement before it signs their requests.
3. Deploy the private key through the configured signing-key environment and
   restart the entry and managed tenants. Verify authenticated tenant requests,
   an existing browser session, and an existing connector's access/refresh flow.
   Remove old assertion trust from any externally managed tenant after cutover.
4. Existing browser sessions, resealed WorkOS authorizations, and consented MCP
   connectors survive. Legacy assertion-sealed in-flight consent pages and
   never-consented registrations must be redone. New keyring-sealed pages and
   registrations survive assertion-key rotation while their keyring versions
   remain available; retiring those versions requires redoing pending flows.

The legacy fallback still exists when no Hub keyring is configured. Rotating the
assertion key in that mode loses access to legacy provider tokens; installing
the newer binary alone is not a completed migration. See
[entry initialization](../internal/cloudentry/service.go),
[seal selection](../internal/cloudentry/sessions.go),
[OAuth seals](../internal/cloudentry/oauth.go) and
[client backfill](../internal/cloudentry/oauth_clients.go).

### GitHub App key and ID

1. Generate another private key for the **same** GitHub App. Keep its App ID and
   existing private key valid during routine cutover. The App ID is an identity,
   not a secret to periodically regenerate.
2. Replace the private-key environment value in the entry and all consuming
   Hubs, then restart them. Verify App-authenticated installation access.
3. Delete the old private key at GitHub after all consumers have switched.
   Replacing the App itself requires separately installing/authorizing the new
   App; swapping only its ID is not private-key rotation.

### GitHub webhook secret

1. Prepare a new shared secret and coordinate the entry and Hub environment
   changes. GitHub offers no old/new webhook-secret overlap window.
2. Change the secret in the GitHub App settings, update the consuming service
   environments, and restart promptly. Deliveries during the mismatch can fail.
3. Check accepted deliveries and the entry's startup replay logs. Startup reads
   receipts from ready tenants and chooses their **latest** non-null receipt as
   the start, with entry startup time as the exclusive end. An unreadable tenant
   receipt aborts window discovery; no receipt means no replay. Replay requires
   both App credentials and a webhook secret.
4. Replay groups deliveries by GUID, selects the latest attempt in the window,
   and requests redelivery only for failed (non-2xx) `issues` and `issue_comment`
   attempts. It stops on an API error and logs the requested count. It does not
   recover other event types, successful deliveries, older gaps, or deliveries
   unavailable in GitHub's retained history. Inspect remaining failed deliveries
   and request redelivery at GitHub as necessary. Standalone Hubs have no entry
   startup replay; inspect their missed deliveries separately.

See the implemented [startup replay](../internal/cloudentry/github_replay.go)
and [entry webhook receiver](../internal/cloudentry/github_webhook.go).

### Stripe API and webhook keys

1. Select the correct Stripe account, mode and endpoint. For the API key, create
   a replacement with the required permissions, deliver it through the configured
   environment, and restart every consumer. Verify API access before expiring
   the old key during routine rotation. Do not change account or mode as part of
   a key replacement.
2. For the webhook secret, use Stripe's **Roll secret** action and select an
   expiry overlap long enough to update every consumer. Update that mode's
   webhook-secret environment and restart inside the overlap, then verify
   accepted deliveries before the old secret expires.
3. Detent configures one secret per verifier, but
   [the verifier](../internal/billing/webhook.go) accepts **any matching `v1`
   signature** in the header, with timestamp and mode validation. During Stripe's
   overlap, either deployed secret can match its corresponding signature; Detent
   does not need a comma-separated list of secret values. Confirm the old API
   key and webhook secret are expired after cutover.

### Object-store credentials

1. Create a replacement access-key pair with access to the same bucket and
   required object operations. Keep the old pair valid during routine cutover.
2. Deliver both values to the entry and restart it. Verify attachment upload and
   subsequent read through the attachment service, including retained objects.
   No object or database re-encryption is required for an access-key replacement.
3. Disable/delete the old pair at the object-store provider. Entry attachment
   credentials are not passed to tenant Hubs. If archive access uses separately
   referenced environment credentials, rotate those consumers too; see the
   [archive credential reader](../internal/attachment/binding.go).

### WorkOS and OpenAI API keys

1. Create the replacement in the same WorkOS environment or OpenAI project with
   the required permissions. Keep the old key valid for routine cutover where
   the provider permits it.
2. Update the configured environment value in all consumers and restart them.
   For WorkOS, verify sign-in and existing-session revalidation; for OpenAI,
   verify a request through the configured conversation backend.
3. Revoke the old key at its provider. API-key rotation does not itself revoke
   WorkOS user sessions or replace their stored access/refresh tokens.

### Hub admin token

1. Preserve an independently usable administrator credential for recovery.
   Use `POST /api/v1/tokens/{id}/rotate` for the existing bootstrap administrator
   row (`bootstrap-admin`). Capture the returned plaintext securely; rotation
   immediately invalidates the old bearer token.
2. Replace the standalone Hub's admin-token environment source, or the Cloud
   tenant's private `admin-token` file, with the **returned** token. Coordinate
   the database mutation and file replacement to minimize interruption; restart
   the consumer so it receives the new environment.
3. Verify the new token works and the old token is refused. Restart alone never
   overwrites an existing bootstrap hash. Deleting the tenant file and letting
   the launcher generate an unrelated token does not rotate the database token.
   If administrator access is already lost, use the existing
   [Hub recovery procedure](hub-self-hosting.md#restoreimport-and-move-between-hubs)
   rather than editing live tables.

### Detent API keys

1. Inventory callers and scope. For Hub tokens, use
   `POST /api/v1/tokens/{id}/rotate`; for Cloud personal keys, use
   `POST /api/cloud/account/api-keys/{key}/rotate`. Rotation replaces the hash
   immediately and returns the replacement plaintext once.
2. Deliver the replacement privately to callers, then verify authorized access
   and refusal of the old token. For continuity, create a separate appropriately
   scoped key, switch callers, then revoke the old key instead of rotating in
   place. Cloud service keys use this create/switch/revoke sequence; they have
   no personal-key rotation endpoint.
3. Use the existing revoke operations for unused keys. No Hub encryption or
   assertion-key change rotates these credentials. See
   [Hub token operations](../internal/hubserver/credential_commands.go) and
   [Cloud key routes](../internal/cloudentry/access_keys.go) and
   [personal-key rotation](../internal/cloudentry/key_controls.go).

### Runner credentials

1. Use the enrolled runner's private identity file and existing
   `detent hub runner renew` operation for routine expiry renewal. Renewal
   extends validity without changing the credential; it is not replacement.
2. For replacement, use `detent hub runner rotate` with that identity file.
   The runner generates and persists replacement material through the supported
   client flow; do not generate a server environment variable or edit Hub rows.
3. Verify runner identity validation and renewal with the persisted replacement.
   Revoke an exposed runner through the administrator runner-revocation owner
   and re-enroll from a clean host. Revocation is not undone by renewal or
   generic API-token rotation. Follow
   [runner renewal, rotation and revocation](hub-api.md#runner-renewal-rotation-and-revocation).

### MCP OAuth grants

1. Routine refresh uses the existing OAuth token endpoint: the connector swaps
   its refresh token for new access/refresh tokens and saves the replacement.
   Both server-side token representations are hashes; assertion-key rotation
   does not require decrypting either token.
2. To replace delegated authority, revoke the existing grant using the account's
   grant-revocation operation, then reconnect and consent to a new grant.
   The existing routes and MCP operations are in
   [OAuth grant operations](../internal/cloudentry/oauth_grants.go) and
   [route registration](../internal/cloudentry/oauth.go).
3. Verify the old grant cannot access or refresh, and the newly consented
   connector can. Refresh alone is not compromise containment: a stolen current
   bearer token may still be usable until invalidation. Retrying a spent refresh
   token triggers grant revocation; do not replay it as a rotation check.

## Compromise response

1. **Contain access.** Restrict the exposed host/database and revoke unauthorized
   access. Rotate the exposed key and any exposed database, host, deployment or
   secret-manager credentials. Use clean administration paths. For compromise,
   revoke exposed provider credentials promptly rather than preserving a routine
   overlap for availability. Remove old assertion trust from tenants if the
   signing key leaked: it could forge entry assertions. Preserve necessary
   evidence privately through the incident owner, never in repository files.
2. **Determine the exposure set.** A database plus a usable wrapping/sealing key
   exposes its stored credentials even after the live database is re-keyed.
   Treat an exposed host as exposing the credentials it could read, including
   passthrough values and private token files. Hash-only API, runner and OAuth
   token storage does not reveal bearer plaintext from a database copy alone;
   host/client compromise may still reveal it.
3. **List organizations and notify through the incident owner.** Before rewrap
   removes old-version problem rows, run status with the proposed new active
   version and retain its report privately. Use Hub problem-row organization,
   project, kind and `updated_at` metadata to scope affected organizations.
   This is not a complete credential inventory: active-version rows produce no
   problem entry, entry auth output has no user/organization mapping, and deleted
   or previously replaced values may exist only in the exposed snapshot/audit
   history. Combine the report with authorized incident-time inventory and
   credential-change records. Hub rewrap leaves `updated_at` unchanged. A value
   newly replaced **after the exposure ended** is not in the earlier exposed
   copy; its predecessor still needs revocation. A later timestamp alone does
   not clear an ongoing exposure or prove provider revocation.
4. **Replace credentials at their sources**, not only in Detent:

   | Exposed credential | Required replacement |
   | --- | --- |
   | Sprites organization/project token | Revoke at Fly, issue a new token, then set it in Detent. Removing a token from Detent does **not** revoke it at Fly. Include organization defaults and project overrides. |
   | Slack webhook | Revoke/regenerate in Slack and store the replacement through the existing Slack integration owner. |
   | WorkOS access/refresh tokens or user session | Revoke affected sessions through WorkOS; require new sign-in. Re-keying stored tokens does not invalidate their leaked copies. Rotate the WorkOS API key too if exposed. |
   | MCP OAuth grant | Use existing grant revocation, then reconnect/consent. Replace an exposed confidential client's registration secret by registering a new client as needed. |
   | GitHub App private key | Generate a replacement and delete the exposed key at GitHub; assess and revoke access tokens minted during exposure. Rotate an exposed webhook secret separately. |
   | Stripe API key or webhook secret | Regenerate/roll at Stripe in each affected mode and expire the exposed credential. Review provider actions taken during exposure. |
   | Attachment access-key pair | Disable/delete at the object-store provider, issue a replacement, and assess access to stored objects; changing Detent's env does not revoke the old pair. |
   | OpenAI key | Revoke at OpenAI, issue a replacement, and assess unauthorized usage. |
   | Hub admin token, Detent API key or runner credential | Rotate/revoke through its existing Detent owner; update private consumer storage or re-enroll a clean runner. Replacing a hash via the supported operation invalidates the old bearer. |

5. **Prove both outcomes.** Complete the keyring status and environment checks
   for re-keying, and record source revocation/replacement for every potentially
   readable credential through the incident owner. Neither a green status report
   nor a service restart proves replacement. Secure retained backups, replicas
   and archived keys; never restore exposed credentials into service from them.

## Rollback

While both master-key versions remain configured, set the previous
`DETENT_HUB_SECRET_KEY_VERSION` active and restart all writers. The same Hub
rewrap and entry reseal passes move stored values back. Run full-scope status
until exit 0 before retiring either version. Preserve keys needed by pending
browser flows and retained backups. This rolls back key selection, not schema
migrations; do not assume an older binary understands newer entry seal formats.

An assertion-key rollback also requires matching tenant public-key trust and
all keys still needed for legacy reads. Never restore a compromised key to live
use to recover availability.

Replacing a credential at its source only moves forward. A revoked provider
key, API token, runner credential, WorkOS session or OAuth grant cannot be
recovered by restoring an environment file or database backup. Obtain a fresh
credential/session/grant and update its consumers instead.
