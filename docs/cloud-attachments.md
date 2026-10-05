# Cloud attachments

Cloud entry owns the private attachment store. Each environment needs its own
dedicated DigitalOcean Spaces bucket and a key limited to that bucket. Disable
public ACLs and anonymous bucket policies. Do not configure a CDN. Entry serves
authenticated bytes itself; clients never receive bucket URLs, credentials or
presigned URLs. Tenant Hubs store metadata in `attachments`, with no object
bytes or Spaces settings in their database, configuration or environment.

Configure the entry's `cloud.yaml` after the operator creates the bucket:

```yaml
attachments:
  endpoint: https://nyc3.digitaloceanspaces.com
  region: nyc3
  bucket: detent-production-attachments
```

Set `DETENT_ATTACHMENTS_ACCESS_KEY_ID` and
`DETENT_ATTACHMENTS_SECRET_ACCESS_KEY` in the entry service's `cloud.env`.
Credentials cannot be supplied in YAML. Omit the block to disable attachments;
uploads then return `503` with `attachments_not_configured`. With the block
present, startup must successfully write, read and delete a random `probe/`
object. An anonymous GET must return 401, 403 or 404; an accessible object or
an inconclusive response prevents startup.

Upload with `POST /organizations/:org/api/v2/projects/:project/attachments`.
Send one multipart `file` part, or send the file body with `Content-Type` and
`X-Attachment-Name`. Browser mutations need the organization CSRF token in
`X-CSRF-Token` and the same origin. Scoped API keys use `Authorization: Bearer`;
read keys cannot upload or delete. The equivalent
`/api/v2/organizations/:org/projects/:project/attachments` routes support API
clients. A successful upload returns attachment metadata with a random 128-bit `att_`
identifier and a `reference` field. Embed that markdown in an issue body or
comment body. Raster references use `![name](path)`; other files use
`[name](path)`. Native issue/comment creation and editing bind uploads in the
same transaction. Path references retain their existing binding to one issue
body or comment. Reads still require a session or a scoped token.

Native issue agents can call `attach_evidence` with a workspace-relative `path`
and a short, single-line `caption`. The runner reads the file on the worker that
owns the attempt, including Sprite workers, and uploads through the existing
entry attachment owner using its current lease and fencing token. The existing
runner `events` grant authorizes this path; no new credential or grant is needed.
Absolute paths, traversal outside the workspace, escaping symlinks, nonregular
files, unsupported types and files over 20 MiB are refused. Images and small
text, Markdown, CSV, JSON and log files are supported, with at most ten files
per attempt. The Hub records the attachment references and captions on the
attempt and publishes them with its completion response, or in an evidence-only
comment when the attempt ends without a response. Use ignored workspace output
or remove uploaded files before staging. Browser screenshots and runtime logs
belong on the issue, never in committed repository files.

The hosted MCP `upload_attachment` tool accepts `project_id`, `name`,
`content_base64` and optional `content_type`. It returns the same metadata and
reference. Embed the reference in `file_issue.description` (the native
create-issue operation) or `add_comment.body`. The existing 64 KiB MCP argument
limit permits up to 60,000 base64 characters; use the upload API or CLI for
larger files. The tool never reads a path on the remote server.

Shared entry also exposes these typed attachment tools. Dedicated Hubs and local
daemons omit them because they do not own this storage service.

| Tool | Input and result |
| --- | --- |
| `read_attachment_metadata` | `project_id`, `attachment_id`; the authenticated metadata used by Cloud, including recorded type, size, SHA-256, dimensions and `referenced_by` |
| `read_attachment` | The same selector, optional `offset` and `length`; `metadata`, `content_base64`, `offset`, `returned_bytes`, `eof` and `max_content_bytes` |
| `reference_attachment` | The same selector, `request_id`, `work_item_id` and optional `comment_id`; the exact project/item/comment binding receipt |
| `delete_attachment` | The same selector and `request_id`; the existing connection-bound action preview, approval destination and receipt |

Reads require current project read authority. Content defaults to 32 KiB and
accepts lengths from 1 to 32,768 bytes. Offsets range from zero through the
recorded size, up to the existing 20 MiB file limit. The last chunk may be shorter;
an offset equal to the recorded size returns an empty chunk with `eof: true`.
Missing stored bytes return an unavailable error rather than a successful
partial chunk. The SHA-256 describes the complete stored file, not an individual
chunk. No content result grants access to another request. Metadata and content
results are limited to 256 KiB; oversized metadata returns an unavailable result.
Modern MCP returns the same JSON as text and structured content; legacy protocol
versions return text only. Reads go through entry's authenticated metadata and
storage owners, with no bucket URL, storage credential or internal authorization
principal in the result.

Binding and deletion require current project write authority. A binding resolves
the item and optional comment within that project and reuses the existing
reference upsert, so replay adds no duplicate relation. Deletion always requires
the existing real human approval or human-selected connection YOLO. The preview
binds the recorded metadata and references; a change before execution refuses
the stale action. Reusing its `request_id` with different inputs is denied.
Approval rechecks the originating credential as well as the approving browser.
Read-only, revoked and foreign-project credentials confer no write authority.

After approval, metadata is hidden before entry removes the object. Poll
`action_result` with `action_id` (or replay `delete_attachment`) for the authorized
receipt and `object_deletion`: `pending` or `confirmed`. Receipt delivery through
entry finishes pending object deletion through the existing deletion owner. If
delivery or storage is unavailable, the existing maintenance cycle retains that
work. Quota stays charged until object deletion is confirmed. Resolved receipts
omit `approval_url`; confirmed replays do not repeat storage deletion. Internal
maintenance callbacks remain service-only and are never MCP tools.

`detent attach screenshot.png --project prj_example` uses the configured
Cloud client and prints just the markdown reference. Configured project names
also work. An explicit entry URL, organization and scoped token can be selected:

```sh
detent attach screenshot.png --project prj_example \
  --hub-url https://cloud.detent.build/organizations/org_example \
  --organization org_example --token-env DETENT_HUB_TOKEN
```

Native coding/rework workers collect up to ten screenshots from
`.detent/validation/` before workspace cleanup and publish an inline validation
evidence comment through the existing execution owner. SSH workers send file
bytes through their execution callback; credentials stay on the owner. Symlinks
are skipped and filesystem reads stay within the assigned worktree.

Cloud issue bodies, comments and the new-issue form accept paste, drop and an
attach button. Upload placeholders are replaced with
`![name](attachment:att_<id>)` for images or `[name](attachment:att_<id>)` for
files; failures remove the placeholder and show an inline error. Saving waits
for uploads to finish.

Entry verifies the browser's organization authorization, then makes a small
signed request to the tenant Hub to verify current membership, token authority
and project grants. It repeats the authorization and quota check before the
object write. Object keys are built solely from that organization and the
server-generated identifier: `orgs/<organization_id>/attachments/<id>`.
Filenames never enter object keys. Private Hub metadata endpoints cannot be
reached through the public proxy. The Hub verifies entry's signature, audience, generation,
method, path and body digest on every metadata request.

Files are limited to 20 MiB, independently of the signed proxy's 2 MiB body cap.
Entry spools bounded input to `TMPDIR`, `TMP` or `TEMP` when supplied, otherwise
its private state directory, checks the content and streams the prepared file
to Spaces. Scratch files are private and removed when the request ends.
Raster input accepts PNG,
JPEG, GIF and WebP by magic bytes. Entry checks the 16-megapixel limit before
decoding and re-encodes the pixels to remove EXIF/GPS and trailing polyglot
content. JPEG and PNG retain their format; GIF and WebP become PNG (the decoded
frame, without animation). The normalized file also must fit the size cap.
PDF, UTF-8 plain text, Markdown, CSV and valid JSON are downloads. SVG, HTML,
binary text mismatches and malformed images are refused. Metadata records the
stored content type, bytes, SHA-256, dimensions, original sanitized filename,
project, uploader, creation time, work item/comment references and deletion
time.

Read with `GET .../attachments/:id`. Entry checks the current organization and
project authority and the attachment's metadata before reading Spaces. Reads
without a session or a valid scoped token, from another organization or from
another project return 404. A copied link grants no access. Responses carry:

| Header | Value |
| --- | --- |
| `X-Content-Type-Options` | `nosniff` |
| `Content-Security-Policy` | `default-src 'none'; sandbox` |
| `Cache-Control` | `private, max-age=300` |
| `Cross-Origin-Resource-Policy` | `same-origin` |
| `Content-Disposition` | `inline` for normalized rasters; `attachment` otherwise |

Cloud first reads `GET .../attachments/:id/metadata` through entry, under the
same current organization and project authorization. This read returns no
storage URL or internal authorization principal and does not fetch object bytes.
It uses `Cache-Control: private, no-store`. Missing or foreign references render
as “Attachment unavailable”. Authorized images render at natural size up to the
column width and open the existing expanded image dialog; files show their name,
size and a download link. The Cloud image CSP remains `img-src 'self' data:`.

Stored bytes count toward the organization's existing `collaboration_bytes`
allowance. Metadata insertion checks the allowance transactionally; concurrent
uploads cannot exceed it. A quota refusal uses `allowance_exhausted`. Deleted
bytes remain counted until object deletion is confirmed.

Use `POST .../attachments/:id/reference` with `work_item_id` and optional
`comment_id` to bind an upload to an existing item/comment in the same project.
Issue and comment saves also record their markdown references in the same
transaction. Metadata exposes all sources in `referenced_by`; an attachment may
belong to multiple items or comments in its project. Repeating a binding is safe.
Editing out the last reference makes it an orphan. Archiving retains it.
Physical deletion hides an attachment only when no other source references it
and leaves its metadata pending object cleanup.
Explicit `DELETE .../attachments/:id` also hides it before deleting the object.

The existing entry maintenance cycle expires unreferenced metadata after seven
days, deletes pending objects in batches, and removes old objects left by
interrupted uploads that have no metadata. Referenced attachments do not expire.
Maintenance checks hourly; unavailable storage or tenant metadata leaves work
for the next cycle. An uncertain metadata write is read back before cleanup;
entry never deletes an object whose metadata commit is unresolved. Organization
deprovisioning removes the entire `orgs/<organization_id>/` prefix before marking
the tenant deleted, serialized with active object writes. Every upload,
read and delete attempt has an entry hosted audit record; maintenance uses the
entry actor. Bucket creation and production credentials are operator work.
