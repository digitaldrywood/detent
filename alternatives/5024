---
name: configuration-map
description: "Find Cloud settings, their application owners, required permissions and interacting capacity limits; propose authorized changes through chat confirmation."
when_to_use: "Use for any question or requested change about runner capacity, routing, providers, model selection, policy, integrations or project setup."
---

# Cloud configuration map

Read the current project's fleet before answering about runners, workers or
capacity. Include enrolled self-hosted runners first and Sprites when the
project uses them. When both serve the project and a change is ambiguous, ask
which kind. An empty project can use either self-hosted runners or Sprites.
A denied read is a named permission refusal, never zero runners.

The paths below are Cloud settings routes. Select this conversation's project.
API paths use `:organization`, `:project` and `:runner` route parameters.
The tools use the same application owners and current viewer authorization.

| Setting and screen path | Cloud route | Read tool | Change owner | Required change permission | API change route |
| --- | --- | --- | --- | --- | --- |
| Runner capacity: Settings > Providers & runners > runner > Capacity > Jobs at once | /settings/runners | get_runner_capacity | update_runner_capacity | Runner administration; current manage_runner grants | PUT /api/v2/organizations/:organization/runners/:runner/capacity |
| Runner routing: Settings > Providers & runners > runner > Routing | /settings/runners | get_project_runners | update_runner_routing | Runner administration; only runner owner may change allowed projects | PUT /api/v2/organizations/:organization/runners/:runner/routing |
| Host ceiling: Settings > Providers & runners > host | /settings/runners | get_project_runners | update_runner_host | Runner administration | PUT /api/v2/organizations/:organization/machines/:machine/routing |
| Project model selection: Settings > Projects > project > Model selection | /settings/projects | get_project_model_selection | update_project_model_selection | Organization owner or admin | PUT /api/v2/organizations/:organization/projects/:project/model-selection |
| Organization model defaults: Settings > Organization > Model selection | /settings/organization | get_organization_model_selection | update_organization_model_selection | Organization owner or admin; use the organization settings screen | PUT /api/v2/organizations/:organization/model-selection |
| Repository binding: Settings > Projects > project > Setup | /settings/projects | get_onboarding | bind_native_repository | Organization owner or admin | POST /api/v2/organizations/:organization/projects/:project/onboarding/repository |
| Policy approval: Settings > Projects > project > Setup | /settings/projects | get_project_policy | approve_project_policy | Organization owner or admin; exact selected repository policy | PUT /api/v2/organizations/:organization/projects/:project/onboarding/policy |
| Policy revocation: Settings > Projects > project > Setup | /settings/projects | get_project_policy | revoke_project_policy | Organization owner or admin | DELETE /api/v2/organizations/:organization/projects/:project/policy |
| Tracker and GitHub integration: Settings > Projects > project > Setup | /settings/projects | get_project_integration | update_project_integration | Organization owner or admin; transport entitlement | PUT /api/v2/organizations/:organization/projects/:project/onboarding/integration |
| Onboarding progress: Settings > Projects > project > Setup | /settings/projects | get_onboarding | save_onboarding | Project write access | PUT /api/v2/organizations/:organization/projects/:project/onboarding |
| Organization Sprites pool floor and ceiling: Settings > Integrations > Sprites | /settings/integrations | get_sprite_pool | set_sprite_pool | Organization owner or admin | PUT /api/v2/organizations/:organization/sprite-pool |
| Project Sprites token: Settings > Integrations > Sprites | /settings/integrations | project_secret_metadata | remove_project_secret | Organization owner or admin | DELETE /api/v2/organizations/:organization/projects/:project/secrets/:kind |

For "where do I set how many runners run at once?", show every routed runner's
current, reported and effective capacity and the Jobs at once path. Offer to
change selected runners. For "raise it to N", read `get_runner_capacity` for
each selected runner, then propose one `update_runner_capacity` card per runner.
Pass the read's runner `revision`, `applied.revision` as
`expected_config_revision`, the desired `capacity`, and the selected `backend`
in `change`. If configuration evidence is absent, explain the reported
constraint instead of inventing a revision. The client controls confirmation
and submission. After confirmation, read the result and current settings;
accepted intent may still be waiting for the existing heartbeat/reload.

Cloud's desired runner ceiling and the runner's reported
`global.max_concurrent_agents` / `client.capacity` govern its configuration
ceiling. The lower applicable limit wins. Jobs at once requests the existing
configuration owner to save these values and reload; desired, applied and
effective observations can differ while it runs. A host ceiling still limits
the shared host. Declared provider concurrency and account/model reservations can impose a
lower limit. Subscription quota usage and freshness are diagnostics only. External provider
reports are changed at their producer, not by editing generated reports.
Project throughput combines the eligible capacity of routed runners and also
depends on project/pool/lane limits, approved policy and plan admission.
Shared host/provider limits mean summed runner ceilings are not guaranteed
concurrent jobs. Project-scoped in-use counts cannot establish global free
capacity. Sprites `min_runners` and `max_runners` govern pool size, not jobs per
self-hosted runner.

Routing changes preserve other projects' assignments and ranks. Read the
current routing and runner revision first. A shared runner requires access to
all assignments involved; if unavailable, name the permission and direct an
administrator to Providers & runners. Never expose an ungranted project.

Project model overrides inherit organization defaults when cleared. Stage
selection is delivered by the Hub; do not recommend global.yaml or detent.yaml
for model selection. Approve the entire selected repository policy unchanged;
integration toggles do not alter its landing policy. A missing GitHub
transport cannot be fixed by enabling repository_enabled.

Read secret metadata only. To set or replace a Sprites token, use
`set_sprites_token` for the secure connector link; the human enters it there.
Never request, repeat, store or put secret values in chat arguments. Other
provider sign-in and host/provider producer configuration use their existing
settings owners outside chat. For a denied change, name the required
permission and screen path so the user can ask an administrator.
