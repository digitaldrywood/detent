# Configure the Detent operator project's workflow

This procedure is for native Cloud project
`prj_6d4919bebd73446798e6cd807feda10e`, after this change is integrated and the
Cloud release owner deploys it. The workflow is selected by that project's
owner or administrator through the existing project settings authority. Other
projects keep their chosen workflows and repository policies.

Open the project's settings and review the workflow definition in
[the migration example](examples/detent-cloud-migration-workflow.json). Paste
the array into **Workflow definition** and select **Save workflow**. The first
configured state is the default lane in native issue creation forms. Backlog is
first, nondispatchable and operator-only; Rework is active. Blocked and Human
Review remain nondispatchable and accessible to legitimate orchestrator
transitions. Cancelled is terminal and operator-only. Existing default
transitions are retained alongside the migration lanes.

The same definition can be supplied as `states` to the existing
`PUT /api/v2/organizations/{organization}/projects/{project}/onboarding/integration`
or `/integration` settings command. Read current settings first and send its
`revision` as `expected_revision`, a fresh `idempotency_key`, and the current
`intake`, `projection` and `repository_enabled` values. Omitting `states`
preserves the selected workflow. Hosted project creation also accepts optional
`states`; omission retains the existing hosted default. Workflow writes reuse
NativeState validation, existing idle requirements, current administrator and
project grants, and the native transaction/receipt owner. A stale revision or
removal of a state used by an active or archived item is refused atomically.

Before any bulk migration, the project owner must confirm all nine states in
both an authenticated project GET and the settings UI after reloading. Check
that Backlog is initial and nondispatchable, Rework is dispatchable, and existing
items and prerequisites remain intact. Source Todo imports must explicitly
target Backlog. Preserve source Blocked, Rework and human holds when mapping
other items; imported historical discussion does not authorize dispatch or
clear native holds. No import or live workflow mutation is part of this source
change.

Keep the operator's repository configuration at `gate.run: true` (the no-op
command) and `gate.required_status_checks: []`. Workflow configuration neither
modifies that configuration nor approves a new policy. Existing approved policy,
native version review and check requirements remain authoritative. Projects with
other validation commands or required checks keep those policies.

Live API/UI confirmation and staged migration remain pending with the existing
Cloud release and project configuration owners. Workers commit source changes;
they do not deploy, edit live workflow state or import the source backlog.
