# Shared-origin browser evidence (September 26 run)

Captured 2026-09-26 with Chrome DevTools at 390x844, DPR 1, against `TestSharedOriginPilotPreview` (`internal/cloudentry/pilot_shared_origin_test.go`). The preview serves the real shared entry on one ephemeral loopback origin, launches each organization's tenant Hub on a private unix socket, and replaces WorkOS with a synthetic account chooser at `/__preview/authorize`. No WorkOS, Stripe, DNS or public port was used. Each browser account ran in its own isolated browser context. Every preview was stopped with `POST /__preview/stop` and exited successfully.

Reproduce:

```sh
mkdir -m 700 /tmp/shared-origin-preview
DETENT_SHARED_ORIGIN_PREVIEW=/tmp/shared-origin-preview go test ./internal/cloudentry -run '^TestSharedOriginPilotPreview$' -v -timeout 30m
# open the origin from /tmp/shared-origin-preview/shared-origin-preview.json, then POST its "stop" URL
```

| Evidence | Result |
| --- | --- |
| `01-sign-in-390.png`, `02-fixture-provider-390.png` | The single origin offers sign-in; the fixture provider lists synthetic accounts and organization-scoped sign-ins. |
| `03-new-user-chooser-390.png`, `04-create-organization-390.png` | A verified account with no organization is offered self-service creation. |
| `05-provisioning-390.png`, `06-new-organization-ready-390.png` | A real browser form submission creates the organization, shows provisioning, and after organization sign-in lands on the owner page of the new tenant. No operator YAML or registry command was involved. |
| `07-browser-created-project-issue-390.png` | Browser forms create a project and a native issue in the new organization through the shared origin. |
| `08-capacity-refusal-390.png` | With `max_tenants: 3` already used, a fourth user's request is stored as a resumable capacity refusal; ready organizations are unaffected. |
| `09-member-tab-alpha-390.png`, `10-member-tab-beta-390.png` | One member, one browser context, two tabs: after authorizing the second organization, the first tab still reads only Alpha content and the second only Beta content. |
| `browser-probes.json` | In-page probes from the member's Alpha tab: an `EventSource` on the Alpha project receives `activity`; the Beta project under the Alpha path errors; the Alpha project under the Beta path returns 403; a same-organization `fetch` mutation returns 200 and the same token against Beta returns 403. |

Real browser defect found and fixed: the first capture run (`01` to `04`, ephemeral origin 54195) reproduced a 403 "Reload the page and try again" on the create-organization submission. Chrome sent `Origin: null` because the entry sets `Referrer-Policy: no-referrer`, and the entry accepted only the exact public origin. Go tests had passed because their client set `Origin` explicitly. After the fix (the entry accepts `Origin: null` only with `Sec-Fetch-Site: same-origin`; CSRF is still required), `05` to `10` were captured on a fresh preview (origin 55115). The Go acceptance client now sends the same browser headers.

Not shown in the browser: runner enrollment and execution, entitlement grants, SSE termination on membership removal, sign-out isolation and entry restart. Those are exercised over real HTTP by `TestSharedOriginPilotAcceptance` and recorded in `../shared-origin-evidence.json`.
