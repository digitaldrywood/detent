# Runner capacity acceptance

Use the existing hosted runner-settings journey for integrated capacity acceptance.
It serves the real Cloud application and API with fake authentication, two projects,
two enrolled runners on separate machines, and an isolated SQLite database. Its test
plan permits sixteen concurrent jobs so the runner ceilings can be observed. No
provider process executes and no production system or fleet setting is accessed.

From the workspace root, with Node 24, Go 1.26 and worker scratch configured:

```sh
make app
npm ci --ignore-scripts
go test -p 4 -timeout=120s ./internal/hubserver -run '^(TestRunnerProjectCapacityClaims|TestRunnerProjectCapacityAdministration|TestRunnerCapacityApplication|TestHubMCPFleetBoundary)$' -count=1 -v
go test -p 4 -c -o "$TMPDIR/runner-capacity-hub.test" ./internal/hubserver
DETENT_HOSTED_PREVIEW_BINARY="$TMPDIR/runner-capacity-hub.test" node node_modules/@playwright/test/cli.js test tests/visual/runner-settings.spec.js --project=chromium --workers=1 --retries=0 --reporter=line --output "$TMPDIR/runner-capacity-playwright"
```

The browser journey needs the matching Playwright Chromium installation. Its Go
preview runs with an explicit 25-minute process deadline and stops through its
existing preview lifecycle. All listeners bind ephemeral ports. Each test creates
its own data; runner capacity setup is enabled only for the capacity journey.

| Boundary | Observable assertion |
| --- | --- |
| Manage, save and reload | Total 8 and Browser collaboration maximum 4 survive reload on the runners surface. |
| Reconnect | Authenticated heartbeat returns the same total and project maximum; refreshed UI reports effective total 8. |
| Admission | Four claims for each project fill the first runner; a fifth capped-project claim and a ninth total claim fail. |
| Independent runner | The second runner saves maximum 1 for the same project and admits one claim while the first runner is full; a second claim fails. |
| Project reduction | Reducing maximum to 3 preserves four active project jobs, refuses another, and permits the other project to replace a released job. |
| Total reduction | Reducing total to 3 refuses additional starts while all eight existing leases renew. |
| Increase | Restoring total 8 / maximum 4 permits a replacement claim when a slot opens. |
| Honest status | Real heartbeat evidence distinguishes pending local configuration, an effective provider ceiling of 3, and refused configuration from applied total 8. |
| Validation and errors | Invalid maximum 10001 is rejected; a deliberately injected 503 preserves the draft and shows an error without save success. |
| Offline rendering | A deliberately injected offline fleet response reports offline and unknown effective capacity. This is presentation evidence only. |

`TestRunnerProjectCapacityClaims` additionally covers concurrent claims, two runner
identities sharing one host, restart persistence, explicit zero, inherited maxima,
and an expired-lease renewal racing a new claim. Administration and capacity
application fixtures preserve denied cross-organization writes, stale revisions,
active leases, access grants, isolation and provider ceilings. The MCP fleet fixture
preserves bounded routing receipts, including a full eight-lease runner.

The browser writes light/dark screenshots at 1440px and 390px for list, editor,
saved values, saved list, validation, pending, constrained, refused, injected error,
injected offline and reduced-limit states under
`$TMPDIR/runner-capacity-acceptance`. It also writes `claim-observations.txt` with
asserted counts read from the real routing API. Screenshot capture never uses pixel
comparisons. Inspect screenshots visually before publishing them.

Bind evidence to the tested Git head, candidate tree, relevant source hashes,
Node/Go versions and exact commands with exit results. Preserve failed diagnostic
output separately from passing candidate evidence. Attach screenshots, their
screen/state index and complete bounded command logs through the supported tracker
artifact owner; never commit runtime evidence. The generic fixture project stands
in for the operator's named project. Injected presentation states do not establish
admission; the authenticated claims and Go fixtures establish that behavior.

This acceptance adds no capacity owner or product UI. It preserves INV-1, INV-3,
INV-5, INV-15, INV-16, INV-17, INV-19 and INV-22. Host finalization owns the landing
gate, independent review, security audit and Change publication.
