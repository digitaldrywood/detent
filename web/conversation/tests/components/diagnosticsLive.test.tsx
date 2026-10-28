import React from "react";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { HealthFindingsProvider } from "../../src/app/diagnostics/healthFindings.tsx";
import { useSidebarFindings } from "../../src/app/adapters/sidebarFindings.ts";
import { makeAccountApi } from "../../src/app/account/api.ts";
import { makeWorkHttp } from "../../src/app/work/lib/workHttp.ts";
import { DiagnosticsRoute, DiagnosticsView } from "../../src/app/diagnostics/DiagnosticsPage.tsx";
import { CapacityPanel } from "../../src/app/fleet/CapacityPanel.tsx";
import { RunnersSectionView } from "../../src/app/fleet/RunnersSection.tsx";
import { decodeFleet, type AccountProject, type UpdatesReport } from "../../src/contracts/account.ts";
import fleetFixture from "../../src/contracts/fixtures/account-fleet.json";
import { decodeDiagnostics, decodeHealthFindings } from "../../src/contracts/diagnostics.ts";
import updatesFixture from "../../src/contracts/fixtures/account-updates.json";
import report from "../../src/contracts/fixtures/diagnostics.json";
import health from "../../src/contracts/fixtures/health-findings.json";

const mocks = vi.hoisted(() => ({
  api: { appUpdates: vi.fn(), diagnostics: vi.fn(), fleet: vi.fn(), healthFindings: vi.fn() },
  bootstrap: { actor: { can_manage: false }, organization: { name: "Test organization" }, projects: [{ id: "prj_fixture", name: "Example project", profile: "native", can_write: true }] },
  http: { eventsUrl: () => "/events", listHealthFindings: vi.fn() },
}));
vi.mock("../../src/app/account/context.ts", () => ({ useAccountApi: () => mocks.api, useAccountBootstrap: () => mocks.bootstrap }));
vi.mock("../../src/app/work/lib/useWork.ts", () => ({ useWorkHttp: () => mocks.http }));
vi.mock("../../src/app/pageTitle.ts", () => ({ usePageTitle: () => {} }));
vi.mock("../../src/app/diagnostics/FindingAsk.tsx", () => ({
  FindingAskWorkspace: ({ finding, project, children }: { finding: { id: string; summary: string }; project: { name: string }; children: (controls: React.ReactNode) => React.ReactNode }) => <>
    {children(null)}
    <section aria-label="Ask about this finding">{project.name}: {finding.id}: {finding.summary}</section>
  </>,
}));
vi.mock("@tanstack/react-router", () => ({ Link: ({ to, children, ...props }: { to: string; children: React.ReactNode }) => <a href={to} {...props}>{children}</a> }));

const pendingBarriers: (() => void)[] = [];
class FindingEvents extends EventTarget {
  static current: FindingEvents;
  static instances: FindingEvents[] = [];
  close = vi.fn();
  constructor() { super(); FindingEvents.current = this; FindingEvents.instances.push(this); }
}
const healthFinding = decodeHealthFindings(health).items[0]!;
const original = { ...healthFinding, subject: { ...healthFinding.subject, project_id: "prj_fixture" } };
const next = { ...original, id: "new_finding", summary: "New finding" };
const tick = "2026-10-07T10:00:00Z";
const healthSequences = new Map<string, number>();
function healthSequence(tick: string) {
  let value = healthSequences.get(tick);
  if (value === undefined) { value = healthSequences.size + 1; healthSequences.set(tick, value); }
  return value;
}
let detectorTick = health.last_tick_at;
let open = [original];
let resolved: typeof open = [];
function emit(at = tick) {
  detectorTick = at;
  for (const source of FindingEvents.instances) source.dispatchEvent(new MessageEvent("health.findings", {
    data: JSON.stringify({ entity_id: "finding", entity_revision: healthSequence(at) }), lastEventId: String(healthSequence(at)),
  }));
}
function openCount() {
  return screen.getByText("Open findings").parentElement!.firstElementChild!.textContent;
}

beforeEach(() => {
  vi.clearAllMocks();
  FindingEvents.instances = [];
  healthSequences.clear();
  healthSequence(health.last_tick_at);
  vi.stubGlobal("EventSource", FindingEvents);
  vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
  open = [original];
  detectorTick = health.last_tick_at;
  mocks.bootstrap.projects = [{ id: "prj_fixture", name: "Example project", profile: "native", can_write: true }];
  resolved = [];
  mocks.api.appUpdates.mockRejectedValue({ status: 404 });
  mocks.api.diagnostics.mockResolvedValue(decodeDiagnostics(report));
  mocks.api.fleet.mockResolvedValue({ runners: [] });
  mocks.http.listHealthFindings.mockImplementation(async (_project, _cursor, _signal, options) => ({ items: options?.state === "all" ? [...open, ...resolved] : open, last_tick_at: detectorTick, change_sequence: healthSequence(detectorTick) }));
});
afterEach(() => { cleanup(); pendingBarriers.splice(0).forEach((release) => release()); vi.useRealTimers(); vi.restoreAllMocks(); vi.unstubAllGlobals(); });

const projects: readonly AccountProject[] = [
  { id: "prj_main", name: "Example project", profile: "native", can_write: true, can_manage_runners: true, states: [] },
  { id: "prj_optional", name: "Optional project", profile: "native", can_write: true, can_manage_runners: true, states: [] },
];
const base = decodeFleet(fleetFixture).runners[0]!;
const raw = "policy_digest git error_output ".repeat(200);
const problem = { code: "settings_invalid", project_id: "prj_optional", subject: "workflow", check: "load settings", message: raw, error_output: raw, fix_hint: raw, first_seen: "2026-10-07T10:00:00Z" };
const runners = [
  { ...base, id: "runner_one", machine_id: "host_one", display_name: "Example desktop", hostname: "desktop", state: "active", health: "online", capacity_health: "online", connection_health: "online", claim_refusal_reason: "", provider_capacity: [], host_capacity: 8, host_used: 1, capacity_limit: 8, reported_capacity: 8, problems: [problem] },
  { ...base, id: "runner_two", machine_id: "host_two", display_name: "Example laptop", hostname: "laptop", state: "active", health: "online", capacity_health: "online", connection_health: "online", claim_refusal_reason: "", provider_capacity: [], host_capacity: 4, host_used: 1, capacity_limit: 4, reported_capacity: 4, problems: [{ ...problem, code: "settings_rejected", project_id: "prj_main" }] },
  { ...base, id: "runner_three", machine_id: "host_three", display_name: "Example build host", hostname: "builder", state: "draining", health: "online", capacity_health: "online", connection_health: "online", claim_refusal_reason: "", provider_capacity: [], host_capacity: 8, host_used: 1, capacity_limit: 8, reported_capacity: 8, update: { status: "draining", desired: { version: "v1.0.0" } }, problems: [problem] },
];
const viewProps = { report: decodeDiagnostics(report), runners, findings: [], pending: false, error: null, scope: "Example organization", range: "24h" as const, projects, onRangeChange: vi.fn(), onRefresh: vi.fn() };

it("puts shared host totals and every runner before grouped typed findings without raw debug payloads", () => {
  const view = render(<DiagnosticsView {...viewProps} />);
  const live = screen.getByRole("region", { name: "Capacity right now" });
  const attention = screen.getByRole("region", { name: "Needs attention" });
  expect(live.compareDocumentPosition(attention) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  for (const name of ["Example desktop", "Example laptop", "Example build host"]) {
    expect(within(live).getByRole("link", { name }).getAttribute("href")).toBe("/settings/runners");
  }
  expect(within(live).getByText("3 of 20 slots occupied")).toBeTruthy();
  expect(within(live).getByText("10 free")).toBeTruthy();
  expect(within(live).getByText("Finishing work before updating")).toBeTruthy();
  const settings = within(attention).getByText("Project settings could not be loaded").closest("article")!;
  expect(within(settings).getByText("Optional project", { exact: false })).toBeTruthy();
  expect(within(settings).getAllByRole("link").map((link) => link.textContent)).toEqual(["Example desktop"]);
  const draining = within(attention).getByText("Not taking work because project settings could not be loaded").closest("article")!;
  expect(within(draining).getAllByRole("link").map((link) => link.textContent)).toEqual(["Example build host"]);
  expect(within(draining).getByText("This runner is draining. This runner cannot prepare work with these settings.")).toBeTruthy();
  expect(within(attention).getByText("Runner settings could not be applied")).toBeTruthy();
  expect(within(attention).getByText("Runner operator: check routing cache permissions and configured host services.")).toBeTruthy();
  expect(within(attention).getByText("Example project", { exact: false })).toBeTruthy();
  for (const text of ["policy_digest", "git error_output", "Instrumentation coverage", "Configuration that shapes these numbers", "Check local workflow"]) expect(view.container.textContent).not.toContain(text);
  view.unmount();
  render(<RunnersSectionView fleet={{ ...decodeFleet(fleetFixture), runners }} />);
  expect(screen.queryByRole("region", { name: "Capacity right now", hidden: true })).toBeNull();
  expect(screen.getAllByTestId("host-card")).toHaveLength(runners.length);
  expect(screen.queryByText(/policy_digest|git error_output/)).toBeNull();
});

const appUpdates: UpdatesReport = {
  ...updatesFixture,
  current: "v1.2.4-op.r.328.g123456789abc",
  runners: [
    { runner_id: runners[1]!.id, display_name: "Example laptop", version: "v1.2.3", behind: true, online: true },
    { runner_id: runners[0]!.id, display_name: "Example desktop", version: "v1.2.4-op.r.328.g123456789abc", behind: false, online: true },
    { runner_id: runners[2]!.id, display_name: "Example build host", version: "", behind: false, online: true },
  ],
};

it("joins reported versions by runner ID and keeps capacity and slot tones intact", () => {
  const capacityRunners = runners.map((runner, index) => ({ ...runner, version: "unused fleet version", update: index === 1 ? { status: "requested", desired: { version: appUpdates.current } } : undefined }));
  const view = render(<CapacityPanel runners={capacityRunners} appUpdates={appUpdates} />);
  const rows = screen.getAllByTestId("runner-capacity");
  expect(within(rows[0]!).getByText(appUpdates.current).getAttribute("data-version-lag")).toBe("current");
  expect(within(rows[1]!).getByLabelText(`v1.2.3 · Behind the Hub ${appUpdates.current} · Update requested`).getAttribute("data-version-lag")).toBe("patch");
  expect(within(rows[2]!).getByText("Not reported")).toBeTruthy();
  expect(screen.getByText("1 runner behind the Hub")).toBeTruthy();
  expect(screen.queryByText("unused fleet version")).toBeNull();
  const slots = [...view.container.querySelectorAll("[data-slot-state]")].map((slot) => slot.getAttribute("data-slot-state"));
  view.rerender(<CapacityPanel runners={capacityRunners} appUpdates={null} />);
  expect(screen.queryByText("v1.2.3")).toBeNull();
  expect(screen.queryByText(/behind the Hub/)).toBeNull();
  expect(screen.queryByText("Not reported")).toBeNull();
  expect(screen.getByText("3 of 20 slots occupied")).toBeTruthy();
  expect([...view.container.querySelectorAll("[data-slot-state]")].map((slot) => slot.getAttribute("data-slot-state"))).toEqual(slots);
});

it.each([
  ["v1.2.4-op.r.328.g123456789abc", false, "current"],
  ["v1.2.3", true, "patch"],
  ["v1.1.9", true, "minor"],
  ["0.9.0", true, "minor"],
  ["unparsed-build", true, "minor"],
] as const)("colors runner version %s by distance from the Hub", (version, behind, lag) => {
  render(<CapacityPanel runners={[runners[0]!]} appUpdates={{ ...appUpdates, runners: [{ runner_id: runners[0]!.id, display_name: "Example desktop", version, behind, online: true }] }} />);
  expect(screen.getByText(version).getAttribute("data-version-lag")).toBe(lag);
});

it("keeps missing runner records unreported and pluralizes the Hub warning", () => {
  render(<CapacityPanel runners={runners} appUpdates={{ ...appUpdates, runners: [], behind_count: 2 }} />);
  expect(screen.getAllByText("Not reported")).toHaveLength(3);
  expect(screen.getByText("2 runners behind the Hub")).toBeTruthy();
});

it.each([true, false])("shows the Hub build and Version column only when updates are available (%s)", async (available) => {
  mocks.api.fleet.mockResolvedValue({ runners });
  if (available) mocks.api.appUpdates.mockResolvedValue(appUpdates);
  render(<DiagnosticsRoute />, { wrapper: ({ children }) => <HealthFindingsProvider projects={mocks.bootstrap.projects}>{children}</HealthFindingsProvider> });
  await screen.findByText(original.summary);
  expect(mocks.api.appUpdates).toHaveBeenCalled();
  const hub = screen.queryByText("Hub");
  if (available) {
    expect(hub?.parentElement?.textContent).toBe(`Hub${appUpdates.current}`);
    expect(screen.getByLabelText(/^v1\.2\.3 · Behind the Hub/)).toBeTruthy();
    mocks.api.appUpdates.mockResolvedValue({ ...appUpdates, current: "v1.2.5" });
    fireEvent.click(screen.getByRole("button", { name: "Refresh diagnostics" }));
    await waitFor(() => expect(screen.getByText("Hub").parentElement?.textContent).toBe("Hubv1.2.5"));
  } else {
    expect(hub).toBeNull();
    expect(screen.queryByText("v1.2.3")).toBeNull();
    expect(screen.getByText("3 of 20 slots occupied")).toBeTruthy();
  }
});

it("keeps runner repair guidance read-only and does not group unrelated checks", () => {
  render(<DiagnosticsView {...viewProps} runners={[...runners, { ...runners[0]!, id: "runner_four", machine_id: "host_four", display_name: "Example worker", problems: [{ ...problem, check: "different check" }] }]} />);
  const attention = screen.getByRole("region", { name: "Needs attention" });
  expect(within(attention).queryByRole("link", { name: "Review policy" })).toBeNull();
  expect(within(attention).getByText("Runner operator: check routing cache permissions and configured host services.")).toBeTruthy();
  expect(within(attention).getAllByText("Project settings could not be loaded")).toHaveLength(2);
  expect(screen.queryByRole("button", { name: /Approve|Update runner|Remove runner/ })).toBeNull();
});

it.each([
  ["disabled", "online", "Not taking work because project settings could not be loaded", "This runner is disabled. "],
  ["draining", "online", "Not taking work because project settings could not be loaded", "This runner is draining. "],
  ["draining", "offline", "Not taking work because project settings could not be loaded", "This runner is offline and draining. "],
  ["active", "offline", "Not taking work because project settings could not be loaded", "This runner is offline. "],
  ["active", "unknown", "Not taking work because project settings could not be loaded", "This runner is offline. "],
  ["active", "online", "Project settings could not be loaded", ""],
] as const)("shows runner checks for %s/%s with why it is not taking work", (state, connection, summary, prefix) => {
  const runner = { ...runners[0]!, state, connection_health: connection, leases: [], host_used: 0 };
  const view = render(<DiagnosticsView {...viewProps} runners={[runner]} />);
  const attention = screen.getByRole("region", { name: "Needs attention" });
  expect(within(attention).getByText(summary)).toBeTruthy();
  expect(within(attention).getByText(`${prefix}This runner cannot prepare work with these settings.`)).toBeTruthy();
  expect(within(screen.getByRole("region", { name: "Capacity right now" })).getByText(runner.display_name)).toBeTruthy();
  view.rerender(<DiagnosticsView {...viewProps} runners={[{ ...runner, state: "active", connection_health: "online", problems: [] }]} />);
  expect(within(attention).queryByText("Project settings could not be loaded")).toBeNull();
});

it.each([null, []])("keeps unavailable capacity distinct from no enrolled runners (%j)", (empty) => {
  render(<DiagnosticsView {...viewProps} runners={empty} findings={null} detectorTick={null} report={{ ...viewProps.report, capacity: null, partial: true, busy_percent: null }} />);
  expect(screen.getByText(empty === null ? "Runner capacity is unavailable. Refresh to try again." : "No runners enrolled.")).toBeTruthy();
  expect(screen.getByText(/Findings are unavailable until/)).toBeTruthy();
  expect(screen.getByText("Hourly capacity is unavailable.")).toBeTruthy();
  expect(screen.queryByText(/0 of 0 slots/)).toBeNull();
});

it.each([20, 19])("shows active utilization without attention when %i of 20 slots are working", (used) => {
  render(<DiagnosticsView {...viewProps} runners={runners.map((runner, index) => ({ ...runner, state: "active", update: undefined, problems: [], host_used: runner.host_capacity - (index === 2 ? 20 - used : 0), leases: Array.from({ length: runner.host_capacity - (index === 2 ? 20 - used : 0) }, (_, lease) => ({ ...base.leases[0]!, lease_id: `${runner.id}-${lease}` })) }))} />);
  expect(screen.getByText(`${used} of 20 slots occupied`)).toBeTruthy();
  expect(screen.getByText(/No open findings\./)).toBeTruthy();
  expect(screen.queryByText("Needs attention", { selector: "[data-slot=badge]" })).toBeNull();
});

it.each([
  ["offline", "Offline"],
  ["unknown", "Status unavailable"],
] as const)("keeps %s runner observations honest", (health, label) => {
  render(<DiagnosticsView {...viewProps} runners={[{ ...runners[0]!, health, capacity_health: health, connection_health: health, problems: [] }]} />);
  const live = screen.getByRole("region", { name: "Capacity right now" });
  expect(within(live).getByText(label)).toBeTruthy();
  expect(within(live).getByText("1 of 8 slots occupied")).toBeTruthy();
  expect(within(live).getByText("0 free")).toBeTruthy();
  expect(within(live).queryByText("Online")).toBeNull();
});

it("removes resolved findings on live updates and restores reopened findings", async () => {
  render(<DiagnosticsRoute />, { wrapper: ({ children }) => <HealthFindingsProvider projects={mocks.bootstrap.projects}>{children}</HealthFindingsProvider> });
  await screen.findByText(original.summary);
  expect(openCount()).toBe("1");
  const headline = screen.getByRole("region", { name: "Diagnostics headline" });
  const viewport = headline.closest("[data-slot='scroll-area-viewport']")!;
  viewport.scrollTop = 120;
  open = [next];
  resolved = [{ ...original, resolved_at: tick }, { ...next, id: "historical", summary: "Historical resolved", resolved_at: tick }];
  act(() => emit());
  await screen.findByText(next.summary);
  await waitFor(() => expect(screen.queryByText(original.summary)).toBeNull());
  expect(screen.getByRole("region", { name: "Diagnostics headline" })).toBe(headline);
  expect(viewport.scrollTop).toBe(120);
  expect(screen.queryByText("Historical resolved")).toBeNull();
  expect(openCount()).toBe("1");
  expect(mocks.api.diagnostics).toHaveBeenCalledTimes(1);
  expect(mocks.api.fleet).toHaveBeenCalledTimes(1);
  open = [];
  resolved = [{ ...original, resolved_at: tick }, { ...next, resolved_at: tick }];
  act(() => emit("2026-10-07T10:00:30Z"));
  await waitFor(() => expect(openCount()).toBe("0"));
  expect(screen.queryByText("Resolved")).toBeNull();
  expect(screen.queryByText(next.summary)).toBeNull();
  open = [original, next];
  resolved = [];
  act(() => emit("2026-10-07T10:01:00Z"));
  await screen.findByText(original.summary);
  expect(openCount()).toBe("2");
  open = [next];
  resolved = [{ ...original, resolved_at: tick }];
  act(() => emit("2026-10-07T10:02:00Z"));
  await waitFor(() => expect(screen.queryByText(original.summary)).toBeNull());
  fireEvent.click(screen.getByRole("button", { name: "Refresh diagnostics" }));
  await waitFor(() => expect(screen.queryByText(original.summary)).toBeNull());
  expect(screen.getByText(next.summary)).toBeTruthy();
});

it("catches up on visibility and ignores failed or superseded live reads after unmount", async () => {
  const view = render(<DiagnosticsRoute />, { wrapper: ({ children }) => <HealthFindingsProvider projects={mocks.bootstrap.projects}>{children}</HealthFindingsProvider> });
  await screen.findByText(original.summary);
  const visibility = vi.spyOn(document, "visibilityState", "get").mockReturnValue("hidden");
  mocks.http.listHealthFindings.mockClear();
  act(() => document.dispatchEvent(new Event("visibilitychange")));
  act(() => emit());
  expect(mocks.http.listHealthFindings).not.toHaveBeenCalled();
  visibility.mockReturnValue("visible");
  open = [original, next];
  act(() => document.dispatchEvent(new Event("visibilitychange")));
  expect(mocks.http.listHealthFindings).not.toHaveBeenCalled();
  act(() => emit());
  await screen.findByText(next.summary);
  let release!: (value: unknown) => void;
  pendingBarriers.push(() => release?.({ items: [], last_tick_at: null }));
  mocks.http.listHealthFindings.mockImplementationOnce(() => new Promise((resolve) => { release = resolve; }));
  await act(async () => emit("older"));
  open = [next];
  resolved = [{ ...original, resolved_at: tick }];
  act(() => emit("newer"));
  await act(async () => release({ items: [original], last_tick_at: tick }));
  await waitFor(() => expect(screen.queryByText(original.summary)).toBeNull());
  expect(openCount()).toBe("1");
  expect(screen.queryByText("Resolved")).toBeNull();
  mocks.http.listHealthFindings.mockRejectedValueOnce(new Error("offline"));
  act(() => emit("failed"));
  await screen.findByText(/Unable to refresh findings/);
  expect(screen.queryByText("Resolved")).toBeNull();
  const source = FindingEvents.current;
  view.unmount();
  expect(source.close).toHaveBeenCalledOnce();
  mocks.http.listHealthFindings.mockClear();
  act(() => { emit("unmounted"); document.dispatchEvent(new Event("visibilitychange")); });
  expect(mocks.http.listHealthFindings).not.toHaveBeenCalled();
});

it("opens the selected finding in its project chat from every Needs attention row", async () => {
  open = [original, next];
  render(<DiagnosticsRoute />, { wrapper: ({ children }) => <HealthFindingsProvider projects={mocks.bootstrap.projects}>{children}</HealthFindingsProvider> });
  await screen.findByRole("button", { name: `Ask about ${original.summary}` });
  for (const finding of open) {
    fireEvent.click(screen.getByRole("button", { name: `Ask about ${finding.summary}` }));
    expect(screen.getByRole("region", { name: "Ask about this finding" }).textContent).toBe(`Example project: ${finding.id}: ${finding.summary}`);
  }
});

it("keeps Ask visible but unavailable when the viewer cannot write project chat", async () => {
  mocks.bootstrap.projects[0]!.can_write = false;
  render(<DiagnosticsRoute />, { wrapper: ({ children }) => <HealthFindingsProvider projects={mocks.bootstrap.projects}>{children}</HealthFindingsProvider> });
  const ask = await screen.findByRole("button", { name: `Ask about ${original.summary}` });
  expect(ask.hasAttribute("disabled")).toBe(true);
  fireEvent.click(ask);
  expect(screen.queryByRole("region", { name: "Ask about this finding" })).toBeNull();
});


it("shares one findings read per project and detector tick between Diagnostics and the sidebar", async () => {
  vi.useFakeTimers();
  const projects = ["prj_first", "prj_second", "prj_third", "prj_fourth"].map((id) => ({ id, name: `Project ${id}`, profile: "native", can_write: true }));
  mocks.bootstrap.projects = projects;
  const finding = { ...original, id: "shared_finding", severity: "attention", summary: "Shared project finding", subject: { kind: "project", id: "prj_first" }, resolved_at: null as string | null };
  let items = [finding];
  let at = health.last_tick_at;
  let unavailable = false;
  const request = vi.fn(async (url: string) => {
    if (unavailable && url.includes("prj_second/")) return Response.json({ error: "temporarily unavailable" }, { status: 503 });
    const since = new URL(url, "https://example.test").searchParams.get("since");
    const visible = items.filter((item) => item.resolved_at === null || (since !== null && item.resolved_at >= since));
    return Response.json({ items: url.includes("prj_first/") ? visible : [], last_tick_at: at, change_sequence: healthSequence(at) });
  });
  const options = { origin: "", apiBase: "/api/v2/organizations/org_fixture", csrfToken: "", fetch: request };
  const http = makeWorkHttp(options);
  mocks.api.healthFindings.mockImplementation(makeAccountApi(options).healthFindings);
  mocks.http.listHealthFindings.mockImplementation(http.listHealthFindings);
  function SidebarProbe() {
    const findings = useSidebarFindings(projects.map((project) => ({ ...project })));
    return <nav aria-label="Sidebar findings">{findings.map((item) => <a key={item.id} href={item.to}>{item.summary}</a>)}</nav>;
  }
  const view = render(<HealthFindingsProvider projects={projects}><SidebarProbe /></HealthFindingsProvider>);
  await act(async () => {});
  view.rerender(<HealthFindingsProvider projects={projects}><SidebarProbe /><DiagnosticsRoute /></HealthFindingsProvider>);
  await act(async () => {});
  const expectReads = (perProject: number) => {
    for (const project of projects) expect(request.mock.calls.filter(([url]) => url.includes(`/projects/${project.id}/health/findings`))).toHaveLength(perProject);
  };
  const sidebar = screen.getByRole("navigation", { name: "Sidebar findings" });
  expect(within(sidebar).getByRole("link", { name: finding.summary }).getAttribute("href")).toBe("/diagnostics");
  expect(openCount()).toBe("1");
  expectReads(1);
  for (let second = 0; second < 60; second++) {
    view.rerender(<HealthFindingsProvider projects={projects.map((project) => ({ ...project }))}><SidebarProbe /><DiagnosticsRoute /></HealthFindingsProvider>);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1000);
      FindingEvents.current.dispatchEvent(new Event("open"));
      FindingEvents.current.dispatchEvent(new MessageEvent("activity", { data: String(second) }));
      emit(at);
    });
    expectReads(1);
  }
  at = "2026-10-07T10:01:00Z";
  items = [{ ...finding, resolved_at: at }];
  await act(async () => emit(at));
  expectReads(2);
  expect(within(sidebar).queryByRole("link", { name: finding.summary })).toBeNull();
  expect(screen.queryByText("Resolved")).toBeNull();
  expect(screen.queryByText(finding.summary)).toBeNull();
  expect(openCount()).toBe("0");
  at = "2026-10-07T10:02:00Z";
  items = [finding, { ...finding, id: "another_finding", summary: "Another finding" }];
  await act(async () => emit(at));
  expectReads(3);
  expect(screen.queryByText("Resolved")).toBeNull();
  expect(openCount()).toBe("2");
  expect(within(sidebar).getByRole("link", { name: "Another finding" })).toBeTruthy();
  expect(request.mock.calls.some(([url]) => url.includes("since="))).toBe(false);
  unavailable = true;
  at = "2026-10-07T10:03:00Z";
  items = items.map((item) => item.id === finding.id ? { ...item, resolved_at: at } : item);
  await act(async () => emit(at));
  expectReads(4);
  expect(screen.getByText(/Unable to refresh findings/)).toBeTruthy();
  at = "2026-10-07T10:04:00Z";
  await act(async () => emit(at));
  expectReads(5);
  expect(screen.getByText(/Unable to refresh findings/)).toBeTruthy();
  unavailable = false;
  at = "2026-10-07T10:05:00Z";
  await act(async () => emit(at));
  expectReads(6);
  expect(screen.queryByText(/Unable to refresh findings/)).toBeNull();
  expect(screen.queryByText("Resolved")).toBeNull();
  expect(screen.queryByText(finding.summary)).toBeNull();
  expect(openCount()).toBe("1");
  expect(within(sidebar).queryByRole("link", { name: finding.summary })).toBeNull();
  expect(within(sidebar).getByRole("link", { name: "Another finding" })).toBeTruthy();
  expect(request.mock.calls.some(([url]) => url.includes("since="))).toBe(false);
});

it.each([
  ["subscription", 85, 30_000, "At risk"],
  ["subscription", 80, 30_000, "At risk"],
  ["subscription", 40, 30_000, null],
  ["subscription", 85, 120_000, "Stale"],
  ["subscription", 85, -1_000, "Stale"],
  ["subscription", undefined, undefined, "Unknown"],
  ["api_key", 95, 30_000, null],
  ["unknown", 95, 30_000, null],
  [undefined, 95, 30_000, null],
] as const)("shows capacity billing and quota risk: %s %s %s", (billing, percent, age, status) => {
  const now = Date.parse("2026-10-15T12:00:00Z");
  const capacity = { ...base.provider_capacity[0]!, billing, usage_windows: [{ kind: "weekly", percent_used: percent, observed_at: age === undefined ? undefined : new Date(now - age).toISOString(), reset_at: new Date(now + 3_600_000).toISOString() }] };
  render(<CapacityPanel runners={[{ ...base, provider_capacity: [capacity] }]} now={now} />);
  const panel = within(screen.getByRole("region", { name: "Capacity right now" }));
  expect(panel.getByLabelText(`${capacity.backend} · ${billing === "subscription" ? "Subscription" : billing === "api_key" ? "API key" : "Billing unknown"}`)).toBeTruthy();
  if (billing === "api_key") {
    expect(panel.getByRole("link", { name: "View runner-reported API spend in Usage" }).getAttribute("href")).toBe("/usage");
    expect(panel.queryByRole("meter")).toBeNull();
  } else {
    const meter = panel.getByRole("meter");
    expect(meter.getAttribute("aria-label")).toBe(`weekly · ${percent === undefined ? "Usage unknown" : `${percent}% used`}${status ? ` · ${status}` : ""}`);
    expect(panel.getByLabelText(/^Resets /)).toBeTruthy();
    expect(panel.getByLabelText(age === undefined ? "Observation unknown" : status === "Stale" ? /^Stale · observed/ : /^Observed/)).toBeTruthy();
  }
});

it("clears subscription risk at reset", () => {
  const now = Date.parse("2026-10-15T12:00:00Z");
  render(<CapacityPanel runners={[{ ...base, provider_capacity: [{ ...base.provider_capacity[0]!, billing: "subscription", usage_windows: [{ kind: "five_hour", percent_used: 85, observed_at: new Date(now - 30_000).toISOString(), reset_at: new Date(now).toISOString() }] }] }]} now={now} />);
  const panel = within(screen.getByRole("region", { name: "Capacity right now" }));
  expect(panel.getByRole("meter").getAttribute("aria-label")).toBe("five hour · 85% used · Stale");
});

it("renders runner capacity while the diagnostics report is pending", async () => {
  let release!: (value: ReturnType<typeof decodeDiagnostics>) => void;
  mocks.api.diagnostics.mockImplementation(() => new Promise((resolve) => { release = resolve; }));
  pendingBarriers.push(() => release(decodeDiagnostics(report)));
  mocks.api.fleet.mockResolvedValue({ runners });
  render(<DiagnosticsRoute />, { wrapper: ({ children }) => <HealthFindingsProvider projects={mocks.bootstrap.projects}>{children}</HealthFindingsProvider> });
  await screen.findByText("Capacity right now");
  expect(screen.getByText("Example desktop")).toBeTruthy();
  expect(screen.getByLabelText("Loading diagnostics")).toBeTruthy();
  await act(async () => release(decodeDiagnostics(report)));
  expect(screen.queryByLabelText("Loading diagnostics")).toBeNull();
});
