import React from "react";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { HealthFindingsProvider } from "../../src/app/diagnostics/healthFindings.tsx";
import { useSidebarFindings } from "../../src/app/adapters/sidebarFindings.ts";
import { makeAccountApi } from "../../src/app/account/api.ts";
import { makeWorkHttp } from "../../src/app/work/lib/workHttp.ts";
import { DiagnosticsRoute, DiagnosticsView } from "../../src/app/diagnostics/DiagnosticsPage.tsx";
import { RunnersSectionView } from "../../src/app/fleet/RunnersSection.tsx";
import { decodeFleet, type AccountProject } from "../../src/contracts/account.ts";
import fleetFixture from "../../src/contracts/fixtures/account-fleet.json";
import { decodeDiagnostics, decodeHealthFindings } from "../../src/contracts/diagnostics.ts";
import report from "../../src/contracts/fixtures/diagnostics.json";
import health from "../../src/contracts/fixtures/health-findings.json";

const mocks = vi.hoisted(() => ({
  api: { diagnostics: vi.fn(), fleet: vi.fn(), healthFindings: vi.fn() },
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

class FindingEvents extends EventTarget {
  static current: FindingEvents;
  close = vi.fn();
  constructor() { super(); FindingEvents.current = this; }
}
const healthFinding = decodeHealthFindings(health).items[0]!;
const original = { ...healthFinding, subject: { ...healthFinding.subject, project_id: "prj_fixture" } };
const next = { ...original, id: "new_finding", summary: "New finding" };
const tick = "2026-10-07T10:00:00Z";
let detectorTick = health.last_tick_at;
let open = [original];
let resolved: typeof open = [];
function emit(at = tick) {
  detectorTick = at;
  FindingEvents.current.dispatchEvent(new MessageEvent("health.findings", { data: at }));
}
function openCount() {
  return screen.getByText("Open findings").parentElement!.firstElementChild!.textContent;
}

beforeEach(() => {
  vi.clearAllMocks();
  vi.stubGlobal("EventSource", FindingEvents);
  vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
  open = [original];
  detectorTick = health.last_tick_at;
  mocks.bootstrap.projects = [{ id: "prj_fixture", name: "Example project", profile: "native", can_write: true }];
  resolved = [];
  mocks.api.diagnostics.mockResolvedValue(decodeDiagnostics(report));
  mocks.api.fleet.mockResolvedValue({ runners: [] });
  mocks.http.listHealthFindings.mockImplementation(async (_project, _cursor, _signal, options) => ({ items: options?.state === "all" ? [...open, ...resolved] : open, last_tick_at: detectorTick }));
});
afterEach(() => { cleanup(); vi.useRealTimers(); vi.restoreAllMocks(); vi.unstubAllGlobals(); });

const projects: readonly AccountProject[] = [
  { id: "prj_main", name: "Example project", profile: "native", can_write: true, can_manage_runners: true, states: [] },
  { id: "prj_optional", name: "Optional project", profile: "native", can_write: true, can_manage_runners: true, states: [] },
];
const base = decodeFleet(fleetFixture).runners[0]!;
const raw = "policy_digest git error_output ".repeat(200);
const problem = { code: "settings_invalid", project_id: "prj_optional", subject: "workflow", check: "load settings", message: raw, error_output: raw, fix_hint: raw, first_seen: "2026-10-07T10:00:00Z" };
const runners = [
  { ...base, id: "runner_one", machine_id: "host_one", display_name: "Example desktop", hostname: "desktop", state: "active", health: "online", capacity_health: "online", connection_health: "online", claim_refusal_reason: "", provider_capacity: [], host_capacity: 8, host_used: 1, capacity_limit: 8, reported_capacity: 8, problems: [problem] },
  { ...base, id: "runner_two", machine_id: "host_two", display_name: "Example laptop", hostname: "laptop", state: "active", health: "online", capacity_health: "online", connection_health: "online", claim_refusal_reason: "", provider_capacity: [], host_capacity: 4, host_used: 1, capacity_limit: 4, reported_capacity: 4, problems: [{ ...problem, code: "policy_mismatch", project_id: "prj_main" }] },
  { ...base, id: "runner_three", machine_id: "host_three", display_name: "Example build host", hostname: "builder", state: "draining", health: "online", capacity_health: "online", connection_health: "online", claim_refusal_reason: "", provider_capacity: [], host_capacity: 8, host_used: 1, capacity_limit: 8, reported_capacity: 8, update: { status: "draining", desired: { version: "v1.0.0" } }, problems: [problem] },
];
const viewProps = { report: decodeDiagnostics(report), runners, findings: [], pending: false, error: null, scope: "Example organization", range: "24h" as const, projects, onRangeChange: vi.fn(), onRefresh: vi.fn() };

it("puts shared host totals and every runner before grouped typed findings without raw debug payloads", () => {
  const view = render(<DiagnosticsView {...viewProps} canManage />);
  const live = screen.getByRole("region", { name: "Capacity right now" });
  const attention = screen.getByRole("region", { name: "Needs attention" });
  expect(live.compareDocumentPosition(attention) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  for (const name of ["Example desktop", "Example laptop", "Example build host"]) {
    expect(within(live).getByRole("link", { name }).getAttribute("href")).toBe("/settings/runners");
  }
  expect(within(live).getByText("3 of 20 slots running work")).toBeTruthy();
  expect(within(live).getByText("10 free")).toBeTruthy();
  expect(within(live).getByText("Finishing work before updating")).toBeTruthy();
  const settings = within(attention).getByText("Project settings could not be loaded").closest("article")!;
  expect(within(settings).getByText("Optional project", { exact: false })).toBeTruthy();
  expect(within(settings).getAllByRole("link").map((link) => link.textContent)).toEqual(["Example desktop", "Example build host"]);
  expect(within(attention).getByRole("link", { name: "Review policy" }).getAttribute("href")).toBe("/settings/integrations?project=prj_main");
  expect(within(attention).getByText("Example project", { exact: false })).toBeTruthy();
  for (const text of ["policy_digest", "git error_output", "Instrumentation coverage", "Configuration that shapes these numbers", "Check local workflow"]) expect(view.container.textContent).not.toContain(text);
  const totals = live.textContent;
  const slots = [...live.querySelectorAll("[data-slot-state]")].map((slot) => slot.getAttribute("data-slot-state"));
  view.unmount();
  render(<RunnersSectionView fleet={{ ...decodeFleet(fleetFixture), runners }} />);
  const settingsLive = screen.getByRole("region", { name: "Capacity right now", hidden: true });
  expect(settingsLive.textContent).toBe(totals);
  expect([...settingsLive.querySelectorAll("[data-slot-state]")].map((slot) => slot.getAttribute("data-slot-state"))).toEqual(slots);
});

it("keeps read-only policy review with its administrator and does not group unrelated checks", () => {
  render(<DiagnosticsView {...viewProps} runners={[...runners, { ...runners[0]!, id: "runner_four", machine_id: "host_four", display_name: "Example worker", problems: [{ ...problem, check: "different check" }] }]} />);
  const attention = screen.getByRole("region", { name: "Needs attention" });
  expect(within(attention).queryByRole("link", { name: "Review policy" })).toBeNull();
  expect(within(attention).getByText("Organization administrator: review the project's approved policy in Settings → Integrations.")).toBeTruthy();
  expect(within(attention).getAllByText("Project settings could not be loaded")).toHaveLength(2);
  expect(screen.queryByRole("button", { name: /Approve|Update runner|Remove runner/ })).toBeNull();
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
  expect(screen.getByText(`${used} of 20 slots running work`)).toBeTruthy();
  expect(screen.getByText("No open findings.")).toBeTruthy();
  expect(screen.queryByText("Needs attention", { selector: "[data-slot=badge]" })).toBeNull();
});

it.each([
  ["offline", "Offline"],
  ["unknown", "Status unavailable"],
] as const)("keeps %s runner observations honest", (health, label) => {
  render(<DiagnosticsView {...viewProps} runners={[{ ...runners[0]!, health, capacity_health: health, connection_health: health, problems: [] }]} />);
  const live = screen.getByRole("region", { name: "Capacity right now" });
  expect(within(live).getByText(label)).toBeTruthy();
  expect(within(live).getByText("1 of 8 slots running work")).toBeTruthy();
  expect(within(live).getByText("0 free")).toBeTruthy();
  expect(within(live).queryByText("Online")).toBeNull();
});

it("updates findings in place, retains resolutions until reload, and restores reopened findings", async () => {
  render(<DiagnosticsRoute />, { wrapper: ({ children }) => <HealthFindingsProvider projects={mocks.bootstrap.projects}>{children}</HealthFindingsProvider> });
  await screen.findByText(original.summary);
  expect(openCount()).toBe("1");
  const row = screen.getByText(original.summary).closest("article")!;
  const headline = screen.getByRole("region", { name: "Diagnostics headline" });
  const viewport = headline.closest("[data-slot='scroll-area-viewport']")!;
  viewport.scrollTop = 120;
  open = [next];
  resolved = [{ ...original, resolved_at: tick }, { ...next, id: "historical", summary: "Historical resolved", resolved_at: tick }];
  act(() => emit());
  await screen.findByText(next.summary);
  await within(row).findByText("Resolved");
  expect(row.querySelector("time")!.dateTime).toBe(tick);
  expect(screen.getByText(original.summary).closest("article")).toBe(row);
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
  expect(screen.getAllByText("Resolved")).toHaveLength(2);
  open = [original, next];
  resolved = [];
  act(() => emit("2026-10-07T10:01:00Z"));
  await waitFor(() => expect(within(row).queryByText("Resolved")).toBeNull());
  expect(openCount()).toBe("2");
  open = [next];
  resolved = [{ ...original, resolved_at: tick }];
  act(() => emit("2026-10-07T10:02:00Z"));
  await within(row).findByText("Resolved");
  fireEvent.click(screen.getByRole("button", { name: "Refresh diagnostics" }));
  await waitFor(() => expect(screen.queryByText(original.summary)).toBeNull());
  expect(screen.getByText(next.summary)).toBeTruthy();
});

it("catches up on visibility and ignores failed or superseded live reads after unmount", async () => {
  const view = render(<DiagnosticsRoute />, { wrapper: ({ children }) => <HealthFindingsProvider projects={mocks.bootstrap.projects}>{children}</HealthFindingsProvider> });
  await screen.findByText(original.summary);
  const visibility = vi.spyOn(document, "visibilityState", "get").mockReturnValue("hidden");
  mocks.http.listHealthFindings.mockClear();
  act(() => emit());
  expect(mocks.http.listHealthFindings).not.toHaveBeenCalled();
  visibility.mockReturnValue("visible");
  open = [original, next];
  act(() => document.dispatchEvent(new Event("visibilitychange")));
  await screen.findByText(next.summary);
  let release!: (value: unknown) => void;
  mocks.http.listHealthFindings.mockImplementationOnce(() => new Promise((resolve) => { release = resolve; }));
  act(() => emit("older"));
  open = [next];
  resolved = [{ ...original, resolved_at: tick }];
  act(() => emit("newer"));
  await screen.findByText("Resolved");
  await act(async () => release({ items: [original], last_tick_at: tick }));
  expect(openCount()).toBe("1");
  expect(screen.getByText("Resolved")).toBeTruthy();
  mocks.http.listHealthFindings.mockRejectedValueOnce(new Error("offline"));
  act(() => emit("failed"));
  await screen.findByText(/Unable to refresh findings/);
  expect(screen.getByText("Resolved")).toBeTruthy();
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
  const request = vi.fn(async (url: string) => Response.json({ items: url.includes("prj_first/") ? items : [], last_tick_at: at }));
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
      document.dispatchEvent(new Event("visibilitychange"));
      emit(at);
    });
    expectReads(1);
  }
  at = "2026-10-07T10:01:00Z";
  items = [{ ...finding, resolved_at: at }];
  await act(async () => emit(at));
  expectReads(2);
  expect(within(sidebar).queryByRole("link", { name: finding.summary })).toBeNull();
  expect(screen.getByText("Resolved")).toBeTruthy();
  expect(screen.getByText(finding.summary).closest("article")!.querySelector("time")!.dateTime).toBe(at);
  expect(openCount()).toBe("0");
  at = "2026-10-07T10:02:00Z";
  items = [finding, { ...finding, id: "another_finding", summary: "Another finding" }];
  await act(async () => emit(at));
  expectReads(3);
  expect(screen.queryByText("Resolved")).toBeNull();
  expect(openCount()).toBe("2");
  expect(within(sidebar).getByRole("link", { name: "Another finding" })).toBeTruthy();
  expect(request.mock.calls.filter(([url]) => url.includes("state=all&since="))).toHaveLength(8);
});
