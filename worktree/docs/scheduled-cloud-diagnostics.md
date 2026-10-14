# Scheduled Cloud diagnostics

The scheduled finalizer for `digitaldrywood/detent` selects native project
`prj_6d4919bebd73446798e6cd807feda10e` through its reviewed organization MCP
endpoint. The parser and Actions evidence remain shared with GitHub-mode
reporting in other repositories. Full validation and validated release tags
remain scheduled, outside work-item implementation and merging.

The operator supplies the repository Actions secret `DETENT_API_KEY` using the
existing [API & MCP setup](api-mcp-setup.md) flow. Select an expiring Write key
for this project only. Existing tools need work reads, issue creation and comment
writes; no Admin scope, runner credential or confirmation bypass is needed.
The key's current membership, grants, scope, expiry and revocation remain the
application authority. The workflow selects the public endpoint and project;
the credential is never committed. Missing or insufficient authority fails the
finalizer without GitHub issue/comment fallback.

New source diagnostics and unknown infrastructure intake both enter configured
nondispatchable Backlog. Only concrete test/source diagnostics support source
repair; runner, setup, download and protocol failures remain instance intake.
Existing open native items and imported origin stamps in bodies/comments receive
occurrences without changing holds or lanes. Stable run/attempt/job/fingerprint
keys and durable occurrence markers prevent repeat publication after response
loss. Green runs append pinned-commit validation evidence only to items carrying
scheduled origin provenance, through the existing comment owner. Admission,
review, landing and completion remain with their existing owners.

`scheduled-reporting-evidence` retains the original jobs, publisher-failure jobs
and attempted native tool payloads for 14 days. These contain public source/run
identity and bounded diagnostic evidence, never connection credentials or raw
Cloud error responses. Retry the retained tool payload through the same current
scoped connection with its unchanged `request_id`; the application's receipt
owner handles replay. Do not route a failed Cloud publication to GitHub.

To verify a deployment, the operator/release owner must configure the scoped secret,
run the existing manual scheduled failure probe, repeat that occurrence, and
verify one native repair/occurrence and zero GitHub issue/comment writes. A later
green scheduled run must append evidence without changing native workflow state
and retain its validated tag/release evidence. Record the results in tracker
records; local fixtures do not establish production credential availability or
deployment.
