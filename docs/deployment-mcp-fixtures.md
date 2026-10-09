# Deployment MCP fixtures

Provision these resources once in both dedicated smoke tenants to exercise
fixture-backed deployment MCP reads. Use the tenant's copied API/MCP URL and
project IDs discovered through `list_projects`; never copy production credentials or IDs into
source. The staging and production probes run the same fixture discovery.

Setup uses existing [native API](hub-api.md), MCP commands and the
[artifact service](artifacts-deployment.md). It does not start a runner process,
provider turn, workspace or GitHub operation. Do not attach a live runner to the
smoke project. Keep setup credentials separate from `DETENT_SMOKE_API_KEY`.
The deploy key needs project read/write, the conversation owner's authority,
and runner administration for the capacity read. Conversation resources are
private: create the conversation as the principal used by the deploy key.

## Replay and retention

Keep exactly one item titled `MCP smoke dependency` and one conversation titled
`MCP smoke conversation`. Discover resources before creating them. Use stable
business request IDs, such as `mcp-fixture-conversation-v1` and
`mcp-fixture-attachment-v1`, for identical setup retries; preserve the returned
receipts in the tenant's setup records. Changed inputs need new keys. Reconcile
an uncertain write by reading its existing resource before retrying.

The dependency item stays unarchived in a nondispatchable, nonterminal state.
It must contain an issue attachment reference in its body, one Change with a
current immutable version, one recorded native attempt and one artifact receipt.
The conversation must retain a message containing its attachment. Do not delete
these resources during deploy cleanup; the probe only archives its transient
per-deployment item. Retain the enrolled fixture identity even after its worker
credential expires; reads use the deploy key, not that credential. Setup should
reuse an existing attempt/version/receipt rather than publish another on rerun.

## Conversation and issue attachment

1. Discover the dependency item with `work_list`, query `MCP smoke dependency`.
   If absent, create it with `file_issue` in one of the project's nondispatchable,
   nonterminal states. Save its canonical `wi_` ID.
2. Discover the conversation with `list_project_conversations`, query
   `MCP smoke conversation`. If absent, call `create_conversation` with
   `input: {"title":"MCP smoke conversation","subject_work_item_id":"<dependency ID>"}`.
   Do not request execution or create a workspace.
3. Read `get_conversation`. If it has no attachment message, call
   `upload_conversation_attachment` with the conversation ID and
   `input: {"name":"mcp-smoke.txt","mime":"text/plain","content_base64":"TUNQIHNtb2tlIGZpeHR1cmUK"}`.
   Bind the returned `att_` ID using `post_conversation_command` with
   `input: {"kind":"message","text":"MCP smoke fixture","attachments":["<attachment ID>"]}`.
   An unbound upload expires and is not a persistent fixture. With no running
   conversation worker, the message remains recorded without a provider turn.
4. Read `work_item` for the dependency. If its body has no issue attachment,
   call `upload_attachment` with `name: mcp-smoke.txt`,
   `content_type: text/plain` and the same base64 bytes. Append the returned
   Markdown `reference` to the dependency's existing body using `edit_item`
   with its current revision. Call `reference_attachment` with the dependency
   ID and returned attachment ID to confirm the binding idempotently. Preserve
   the body and reference on subsequent setup runs.

Issue and conversation attachments use distinct resources even though both IDs
start with `att_`. The probe extracts the issue attachment from the dependency
body and the conversation attachment from its message snapshot.

## Enrolled identity, native attempt and artifact receipt

Use the existing enrollment API to enroll a fixture identity scoped only to the
smoke project with `read`, `claim`, `heartbeat`, `events` and `collaborate`
operations. Redeem the enrollment directly as documented in
[runner enrollment](hub-api.md#register-a-runner-with-one-command); do not run
the installer or start a runner service. Save the returned runner/machine IDs
and credential securely. This idle identity supplies `runner_fleet`,
`get_runner_capacity` and `get_runner_update`; unsupported updates and an expired
heartbeat remain valid observed state. An empty fleet is a missing fixture.

If `work_runs` already returns the fixture attempt, reuse it. Otherwise:

1. Have the project administrator approve a genuine repository policy through
   the existing onboarding/policy owner. Keep the smoke project's execution
   routing limited to the idle fixture identity. Temporarily place only the
   dependency item in a permitted dispatchable state through the native workflow
   owner. No live runner may claim it.
2. Using the fixture worker credential, `POST /machines/register` with its
   enrolled machine ID and capacity `1`, then `POST /claims` for the dependency
   ID, approved `policy_id`, machine ID, a unique `session_id`, `ttl_seconds: 90`,
   `protocol_major: 2` and capabilities
   `["native_issues","scoped_collaboration","fenced_run_history"]`.
   These paths are relative to
   `/api/v2/organizations/{organization}/projects/{project}`. Keep the resulting
   lease/fence and renew it during setup if necessary. Preserve any effective
   provider reservation identity returned by the claim.
3. Generate one `run_` ID and one `attempt_` ID and keep them for exact retries.
   `POST /work-items/{dependency}/events` with this shape, using the approved
   policy and returned lease/fence:

   ```json
   {
     "idempotency_key": "mcp-fixture-start-v1",
     "type": "run.started",
     "schema_version": 1,
     "data": {
       "sequence": "1",
       "identity": {"role":"implement","backend":"codex","model":"fixture"},
       "lease_id": "<lease ID>",
       "fencing_token": "<fence>",
       "policy_id": "<approved policy ID>",
       "run_id": "<run ID>",
       "attempt_id": "<attempt ID>"
     }
   }
   ```

   If the claim reserved a provider, use its exact role/backend/model instead
   of the example identity. This records the setup operation; it does not assert
   that a provider executed or validated source. Machine and runner provenance
   are derived by the Hub, not supplied in the event.
4. Before releasing the lease, record a real small fixture diff through
   `POST /attempts/{attempt}/diff`. Use the current attempt producer lease/fence,
   generation `{"source":"attempt","seq":1}`, actual base/head Git SHAs and
   a harmless fixture file/patch. The strict request shape is
   `tracker.AttemptDiffRequest` in `internal/tracker/native_diff.go`; diff producer
   fences are integers, while run event fences and sequences are decimal strings.
   Reconcile an uncertain post with `get_attempt_diff`; do not increment its
   generation simply to retry. A recorded empty diff is also valid if those two
   actual source revisions have no difference.
5. Bind the environment's artifact service through `bind_artifact_service`,
   using its existing dedicated publisher credential, then upload a small
   fixture log using the content API's reserve/parts/finalize sequence. The upload
   authority must identify this still-running fenced attempt. The service's
   publication outbox posts its verified receipt to
   `/artifact-services/{service}/receipts`. Confirm it with `artifact_references`
   on the dependency before releasing the lease. Reuse the service's identical
   upload keys on retries. Do not fabricate an available receipt or publish with
   the deploy key. The smoke reads retained immutable receipt metadata and does
   not request an artifact download grant.
6. Close the attempt with another event using the same identity and IDs,
   `type: run.finished`, `data.sequence: "2"`, `data.outcome: cancelled` and a
   distinct stable idempotency key. This setup did no implementation work and
   must not report a successful Code completion. Release the exact lease using
   `POST /leases/{lease}/release` and its current fence. Return the dependency to
   its nondispatchable state through the workflow owner. Retain the idle identity;
   do not leave any live lease or running attempt.

## Change and immutable version

Discover `list_changes` for the dependency and reuse its existing fixture Change
and current version. If absent, use `create_change` with a stable request ID and
title `MCP smoke fixture`, then `publish_change_version` with an explicitly empty
`expected_version_id`. Supply the approved policy, real fixture repository,
base/head/merge-base SHAs and the fixture's actual code reference/digest. Use
`availability: unverified` unless the artifact owner verified it. The operator
publication path may omit a Run/Attempt; do not invent worker provenance, checks,
reviews, external PRs or landing evidence. Preserve the immutable version rather
than republishing it on each deploy. An empty viewed-file list is a valid result
for `change_viewed_files` on this version.

## Verify and enable deployment

Run the existing `tools/cifailure` staging/production smoke entrypoint with the
matching environment key and copied MCP URL:

```sh
DETENT_RELEASE_ENVIRONMENT=staging go run ./tools/cifailure
```

Use `production` for the production tenant. Supply `DETENT_SMOKE_API_KEY` and
`DETENT_MCP_URL` through the environment, and leave `DETENT_RELEASE_FAILURE_PHASE`
unset. Confirm that the log calls
conversation/message/attachment/event reads, both issue attachment reads,
Change discovery/viewed-file reads, exact artifact receipt revision, attempt diff,
attempt receipt, GitHub scope timings and both runner reads.
`github_scope_timings` uses the recorded attempt's runner ID; unavailable timing
boundaries are valid because setup made no GitHub requests.

`get_project_policy` runs too: either an approved policy or the documented
`policy_mismatch` refusal beginning `No approved repository policy` is valid.
`get_change`, `get_change_version` and `get_native_run` are explicit read skips
because these operator-only detail reads are outside the dedicated project smoke
key's authority. `get_git_hub_import` and `list_git_hub_import_records` are also
explicit read skips: these native projects have no GitHub import, and fabricating
one would exercise an external tracker/cutover workflow outside the smoke's scope.

Run the probe twice to verify persistent resource reuse. Neither invocation may
create another fixture or dispatch a runner. After discovery finishes, a read
whose required fixture is absent is explicitly skipped with the missing argument and advertised schema recorded in the output.
Available fixtures are exercised. Schema errors, discovery failures and errors
from exercised tools, including inaccessible resources, still fail deployment.
The same skip policy applies to staging and production.
