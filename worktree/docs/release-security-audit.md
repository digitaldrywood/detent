# Release security audit stage

Release workflows use a controller built from a reviewed, pinned Detent revision.
The controller is independent of the target checkout. Invoke its `cifailure`
binary with `DETENT_RELEASE_AUDIT=true` and a JSON request on standard input:

```json
{
  "identity": {
    "project_id": "prj_example",
    "repository": "example-org/example-project",
    "commit": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "release_series": "release/v1.0",
    "run_id": "123",
    "attempt": "1",
    "job_id": "security-audit",
    "run_url": "https://example.test/runs/123"
  },
  "repository_directory": "/job/source",
  "model": "selected-by-the-project",
  "allowance": {
    "invocations": 40,
    "usage": {
      "input_tokens": 200000,
      "output_tokens": 50000,
      "cached_input_tokens": 150000
    }
  }
}
```

The identity must match the scheduled job's `DETENT_PROJECT_ID`,
`GITHUB_REPOSITORY`, `CI_DEVELOP_SHA`, `CI_RELEASE_BRANCH`, `GITHUB_RUN_ID`
and `GITHUB_RUN_ATTEMPT`. The controller uses the project-scoped
`DETENT_MCP_URL` and `DETENT_API_KEY` through the existing native reporting
owner. The job supplies `TMPDIR`, `TMP` or `TEMP`, Node, Codex and its provider
credentials. Provider sessions, the skill bundle, validators and fixture
artifacts stay in job scratch. Model selection comes from the project's
effective configuration; the stage does not select a model or modify policy.

The embedded Cloudflare bundle is pinned by commit and SHA-256, includes its
MIT license, and is verified before any provider invocation. Validators always
come from that bundle. Source reads use Git blobs at the full pinned commit,
including when the working tree has changed. Reconnaissance, hunters,
candidate disprovers and final record verifiers use fresh provider sessions.
Every prompt treats source, issue metadata, findings and prior evidence as
untrusted data. Native tools, configured MCP servers, hooks, skills, memories
and provider delegation are disabled; only controller-owned observation tools
are available.

Target execution requires Linux Bubblewrap, prlimit, and a delegated cgroup v2
memory controller under the controller process's current cgroup. Each command
starts atomically inside a dedicated cgroup with a 1 GiB aggregate memory limit,
no swap, and group-wide OOM termination. The controller kills the fixture's
process tree and removes that cgroup after execution; an OOM event
invalidates the observation. Missing controls or cgroup attachment failures
never fall back to unrestricted execution. The controller
materializes the pinned Git tree in scratch and mounts it read-only in a
networkless namespace, with an allowlisted environment, resource limits and
one size-limited, disposable scratch filesystem per command. Target commands never
mount provider state or retained evidence; the controller captures bounded output. When those controls are unavailable,
execution-dependent candidates remain `needs_validation`. Those records and
`rejected` records are audit evidence and create no repair issue.

Invocations are reserved before provider calls. Usage updates are cumulative
within each fresh session; input, output and cached input are retained
separately. An allowance cancels further provider work and publishes
`incomplete`, retaining candidates and coverage. Missing accounting, reused
sessions, malformed output or validator failures produce infrastructure
records. An incomplete audit never asserts a clean result.

Project-scoped checkpoints use the existing Hub attachment service for bounded
UTF-8 documents. Parts are at most 32768 bytes and retain their order, offsets,
individual hashes and the complete document hash. Typed checkpoint and part
rows retain source, bundle, run and usage identity. The native operator API
provides `GET` and `POST /release-audits` and
`GET /release-audits/:checkpoint` under the existing project base path.
Worker credentials cannot publish or read this operator reporting state.
Referenced audit attachments remain retained by the existing attachment
retention owner. Another eligible job hydrates prior project checkpoints
through the authenticated Hub API; it uses no home directory or repository
output as memory.

Confirmed records go through `cifailure` and its existing release-series
stabilization owner. The project and source-derived root-cause fingerprint
form the stable repair key. Occurrences retain pinned SHA, job identity,
run URL and bundle provenance. Reporting reuses open work and preserves its
holds, active state and stronger priority. Infrastructure failures are
separate from vulnerability verdicts. The stage reports repairs without
waiting for them.

Adoption belongs to each project's release workflow. Each adoption work item
depends on the shared stage and names the workflow file, release-only job or
step, trusted controller revision, pinned target commit input, scoped reporting
owner, allowances and retained artifacts. The caller supplies its selected
project identity; the controller has no project allowlist or runner affinity.
Keep existing jobs, govulncheck/gosec commands, artifact retention and the
fail-closed tagging rule. The per-change security audit and its Rework
disposition are independent and unchanged.

Detent's `ci.yml` invokes the controller after Security on release branches.
The `audit-controller` checkout pins its trusted revision independently of
the `audit-source` checkout at the preflight release SHA. Configure the
repository variable `DETENT_RELEASE_AUDIT_MODEL` with the project's selected
model, and supply `OPENAI_API_KEY` and the existing scoped `DETENT_API_KEY`
as Actions secrets. Credentials are supplied only to the audit invocation.
The job allows 40 invocations, 200000 input tokens, 50000 output tokens and
150000 cached input tokens. Its request, result and diagnostics are retained
for 14 days. Allowance exhaustion is advisory and retains incomplete coverage;
infrastructure errors fail the job and use the existing scheduled reporter.
Develop and pull request runs execute no audit steps.

When replacing an existing reviewer, retire its model review engine in the
same workflow change. Preserve its last-reviewed-ancestor cursor and native
deduplication rather than running both engines. The workflow sequence is:

1. Read the previous cursor and verify that it is an ancestor of the pinned
   release commit, retaining the existing first-run behavior.
2. Invoke the trusted controller against that commit. It hydrates project
   evidence from the Hub, publishes each completed finding before reporting
   it, and returns only after final checkpoint publication and reporting.
3. Retain the result and artifacts. Advance the cursor to the pinned commit
   only after the controller succeeds with completed coverage. A
   `needs_validation` result alone does not fail the stage or wait for a
   repair. `incomplete` and `failed` results never advance the cursor or
   represent clean coverage, even if some findings were already published.
4. Let the existing release finalizer apply its green-tag rule. Audit reporting
   never waits for the repair issues it creates.

The adopting workflow's fixtures must exercise its own cursor reads and
writes: first run, reviewed ancestor, invalid ancestry, failed publication or
reporting, and successful publish-before-cursor ordering. Keep its existing
untrusted-metadata, native-deduplication and human-hold regressions. The shared
stage fixtures establish the controller's publication, reporting and
project-isolation boundaries; they do not establish another repository's
workflow integration or the existence of its tracker work items.
