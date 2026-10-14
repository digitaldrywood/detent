import React from "react";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
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
  http: { eventsUrl: () => "/events" },
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
let open = [original];
let resolved: typeof open = [];
function emit(at = tick) {
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
  mocks.bootstrap.projects[0]!.can_write = true;
  resolved = [];
  mocks.api.appUpdates.mockRejectedValue({ status: 404 });
  mocks.api.diagnostics.mockResolvedValue(decodeDiagnostics(report));
  mocks.api.fleet.mockResolvedValue({ runners: [] });
  mocks.api.healthFindings.mockImplementation(async (_projects, options) => ({ items: options?.state === "resolved" ? resolved : open, last_tick_at: options ? tick : health.last_tick_at }));
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); });

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
  const totals = within(live).getAllByTestId("runner-capacity").map((row) => row.textContent);
  const slots = [...live.querySelectorAll("[data-slot-state]")].map((slot) => slot.getAttribute("data-slot-state"));
  view.unmount();
  render(<RunnersSectionView fleet={{ ...decodeFleet(fleetFixture), runners }} />);
  const settingsLive = screen.getByRole("region", { name: "Capacity right now", hidden: true });
  expect(within(settingsLive).getAllByTestId("runner-capacity").map((row) => row.textContent)).toEqual(totals);
  expect([...settingsLive.querySelectorAll("[data-slot-state]")].map((slot) => slot.getAttribute("data-slot-state"))).toEqual(slots);
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
  expect(screen.getByText("Runner")).toBeTruthy();
  expect(screen.getByText("Version")).toBeTruthy();
  expect(screen.getByText("Slots")).toBeTruthy();
  const rows = screen.getAllByTestId("runner-capacity");
  expect(within(rows[0]!).getByText(appUpdates.current)).toBeTruthy();
  expect(within(rows[0]!).getByText("Same as the Hub")).toBeTruthy();
  expect(within(rows[1]!).getByText("v1.2.3")).toBeTruthy();
  expect(within(rows[1]!).getByText("Behind the Hub · Update requested").classList.contains("text-warning-foreground")).toBe(true);
  expect(within(rows[2]!).getByText("Not reported")).toBeTruthy();
  expect(screen.getByText("1 runner behind the Hub")).toBeTruthy();
  expect(screen.queryByText("unused fleet version")).toBeNull();
  const slots = [...view.container.querySelectorAll("[data-slot-state]")].map((slot) => slot.getAttribute("data-slot-state"));
  view.rerender(<CapacityPanel runners={capacityRunners} appUpdates={null} />);
  expect(screen.queryByText("Version")).toBeNull();
  expect(screen.queryByText(/behind the Hub/)).toBeNull();
  expect(screen.queryByText("Not reported")).toBeNull();
  expect(screen.getByText("3 of 20 slots running work")).toBeTruthy();
  expect([...view.container.querySelectorAll("[data-slot-state]")].map((slot) => slot.getAttribute("data-slot-state"))).toEqual(slots);
});

it("keeps missing runner records unreported and pluralizes the Hub warning", () => {
  render(<CapacityPanel runners={runners} appUpdates={{ ...appUpdates, runners: [], behind_count: 2 }} />);
  expect(screen.getAllByText("Not reported")).toHaveLength(3);
  expect(screen.getByText("2 runners behind the Hub")).toBeTruthy();
  expect(screen.queryByText("Same as the Hub")).toBeNull();
});

it.each([true, false])("shows the Hub build and Version column only when updates are available (%s)", async (available) => {
  mocks.api.fleet.mockResolvedValue({ runners });
  if (available) mocks.api.appUpdates.mockResolvedValue(appUpdates);
  render(<DiagnosticsRoute />);
  await screen.findByText(original.summary);
  expect(mocks.api.appUpdates).toHaveBeenCalled();
  const hub = screen.queryByText("Hub");
  if (available) {
    expect(hub?.parentElement?.textContent).toBe(`Hub${appUpdates.current}`);
    expect(screen.getByText("Version")).toBeTruthy();
    expect(screen.getByText(/^Behind the Hub/)).toBeTruthy();
    mocks.api.appUpdates.mockResolvedValue({ ...appUpdates, current: "v1.2.5" });
    fireEvent.click(screen.getByRole("button", { name: "Refresh diagnostics" }));
    await waitFor(() => expect(screen.getByText("Hub").parentElement?.textContent).toBe("Hubv1.2.5"));
  } else {
    expect(hub).toBeNull();
    expect(screen.queryByText("Version")).toBeNull();
    expect(screen.getByText("3 of 20 slots running work")).toBeTruthy();
  }
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
  expect(within(live).getByText("1 of 8 slots running work")).toBeTruthy();
  expect(within(live).getByText("0 free")).toBeTruthy();
  expect(within(live).queryByText("Online")).toBeNull();
});

it("updates findings in place, retains resolutions until reload, and restores reopened findings", async () => {
  render(<DiagnosticsRoute />);
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
  const view = render(<DiagnosticsRoute />);
  await screen.findByText(original.summary);
  const visibility = vi.spyOn(document, "visibilityState", "get").mockReturnValue("hidden");
  mocks.api.healthFindings.mockClear();
  act(() => emit());
  expect(mocks.api.healthFindings).not.toHaveBeenCalled();
  visibility.mockReturnValue("visible");
  open = [original, next];
  act(() => document.dispatchEvent(new Event("visibilitychange")));
  await screen.findByText(next.summary);
  let release!: (value: unknown) => void;
  mocks.api.healthFindings.mockImplementationOnce(() => new Promise((resolve) => { release = resolve; }));
  act(() => emit("older"));
  open = [next];
  resolved = [{ ...original, resolved_at: tick }];
  act(() => emit("newer"));
  await screen.findByText("Resolved");
  await act(async () => release({ items: [original], last_tick_at: tick }));
  expect(openCount()).toBe("1");
  expect(screen.getByText("Resolved")).toBeTruthy();
  mocks.api.healthFindings.mockRejectedValueOnce(new Error("offline"));
  act(() => emit("failed"));
  await screen.findByText(/Unable to refresh findings/);
  expect(screen.getByText("Resolved")).toBeTruthy();
  const source = FindingEvents.current;
  view.unmount();
  expect(source.close).toHaveBeenCalledOnce();
  mocks.api.healthFindings.mockClear();
  act(() => { emit("unmounted"); document.dispatchEvent(new Event("visibilitychange")); });
  expect(mocks.api.healthFindings).not.toHaveBeenCalled();
});

it("opens the selected finding in its project chat from every Needs attention row", async () => {
  open = [original, next];
  render(<DiagnosticsRoute />);
  await screen.findByRole("button", { name: `Ask about ${original.summary}` });
  for (const finding of open) {
    fireEvent.click(screen.getByRole("button", { name: `Ask about ${finding.summary}` }));
    expect(screen.getByRole("region", { name: "Ask about this finding" }).textContent).toBe(`Example project: ${finding.id}: ${finding.summary}`);
  }
});

it("keeps Ask visible but unavailable when the viewer cannot write project chat", async () => {
  mocks.bootstrap.projects[0]!.can_write = false;
  render(<DiagnosticsRoute />);
  const ask = await screen.findByRole("button", { name: `Ask about ${original.summary}` });
  expect(ask.hasAttribute("disabled")).toBe(true);
  fireEvent.click(ask);
  expect(screen.queryByRole("region", { name: "Ask about this finding" })).toBeNull();
});
