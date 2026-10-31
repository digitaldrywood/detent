// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, renderHook, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { RunnerProblems } from "../../src/app/fleet/RunnerProblems.tsx";
import React from "react";
import { RunnerDetailPage } from "../../src/app/fleet/RunnerDetailPage.tsx";
import { CapacityPanel } from "../../src/app/fleet/CapacityPanel.tsx";
import { ProviderSummary } from "../../src/app/usage/ProviderSummary.tsx";
import { RunnersSectionView as RunnersList } from "../../src/app/fleet/RunnersSection.tsx";
import { spriteBootstrapPin } from "../../src/app/fleet/EnrollRunner.tsx";
import type { FleetResponse, RunnerRouting } from "../../src/contracts/account.ts";
import { decodeFleet, type SuggestedLimit } from "../../src/contracts/account.ts";
import { useRunnerSuggestions } from "../../src/app/fleet/runnerSuggestions.ts";
import { AccountError, makeAccountApi } from "../../src/app/account/api.ts";
import { ClientContext } from "../../src/app/client.ts";
import { resetRunnerNamesForTests, runnerDisplay, useRunnerNames } from "../../src/app/work/lib/runnerNames.ts";
import { parseRunnerWindow, serializeRunnerWindow } from "../../src/app/fleet/runnerSchedule.ts";
import emptyFixture from "../../src/contracts/fixtures/account-fleet-empty.json";
import fleetFixture from "../../src/contracts/fixtures/account-fleet.json";
import capacityFixture from "../../src/contracts/fixtures/account-runner-capacity.json";
import bootstrapFixture from "../../src/contracts/fixtures/account-bootstrap.json";

afterEach(() => { cleanup(); vi.useRealTimers(); vi.unstubAllGlobals(); resetRunnerNamesForTests(); window.history.replaceState(null, "", "/"); });

const FLEET = fleetFixture as unknown as FleetResponse;
const EMPTY = emptyFixture as unknown as FleetResponse;
const NOW = Date.parse("2026-09-10T12:09:31Z");
const ROUTING: RunnerRouting = {
  display_name: FLEET.runners[0]!.display_name, state: "active", capacity_limit: 6,
  project_ids: ["prj_known", "prj_unknown"], tags: ["linux"],
  isolation_tier: "sandbox", host_services: ["tcp:127.0.0.1:8080"],
  availability: { timezone: "America/Chicago", windows: ["Mon-Fri 09:00-17:00", "Sat-Sun 00:00-24:00"], hard_deadline: "30m" },
};
const RUNNER = { can_edit_projects: true, ...FLEET.runners[0]!, editable: true, claim_refusal_reason: "", routing: ROUTING, revision: 7 };
const PROJECTS = [{ id: "prj_known", name: "Known project" }, { id: "prj_second", name: "Second project" }];

function RunnersSectionView(props: React.ComponentProps<typeof RunnersList> & {
  onSaveRouting?: React.ComponentProps<typeof RunnerDetailPage>["onSave"];
  onReloadRunner?: () => Promise<void>;
  suggestions?: Record<string, SuggestedLimit>;
  loadHostHistory?: React.ComponentProps<typeof RunnerDetailPage>["loadHostHistory"];
}) {
  const [id, setId] = React.useState<string | null>(null);
  const runner = props.fleet.runners.find((entry) => entry.id === id);
  return runner ? <RunnerDetailPage runner={runner} projects={props.projects ?? []} editable={(runner.editable ?? props.fleet.editable) !== false} onSave={props.onSaveRouting} onReload={props.onReloadRunner} suggestion={props.suggestions?.[runner.id]} loadHostHistory={props.loadHostHistory} now={props.now} onClose={() => setId(null)} /> : <RunnersList {...props} onOpenRunner={(entry) => setId(entry.id)} />;
}

function renderSection(fleet: FleetResponse = FLEET) {
  render(<RunnersSectionView fleet={fleet} now={NOW} />);
}

describe("Sprite bootstrap pin", () => {
  it.each([
    ["v1.2.3", { ref: "v1.2.3", release: "v1.2.3" }],
    ["1.2.3", { ref: "v1.2.3", release: "v1.2.3" }],
    ["operator-landed-3c51987c563b", { ref: "3c51987c563b", release: null }],
    ["3c51987c563b", { ref: "3c51987c563b", release: null }],
    ["", { ref: "latest", release: null }],
    ["dev", { ref: "latest", release: null }],
  ])("resolves %s", (version, expected) => {
    expect(spriteBootstrapPin(version)).toEqual(expected);
  });
});

describe("runner rows", () => {
  it.each([
    ["subscription", "Subscription"], ["api_key", "API key"], ["unknown", "Billing unknown"], [undefined, "Billing unknown"],
  ] as const)("shows backend billing in details: %s", (billing, label) => {
    const capacity = { ...RUNNER.provider_capacity[0]!, billing, usage_windows: [{ kind: "weekly", percent_used: 95, observed_at: new Date(NOW - 30_000).toISOString() }] };
    renderSection({ ...FLEET, runners: [{ ...RUNNER, provider_capacity: [capacity] }] });
    expect(within(screen.getByTestId("host-card")).queryByText(new RegExp(`${capacity.backend}.*${label}`))).toBeNull();
    if (billing === "api_key") expect(screen.queryByText(/95% used|At risk/)).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: `Manage ${RUNNER.display_name}` }));
    expect(within(screen.getByRole("article")).getByText(new RegExp(`${capacity.backend}.*${label}`))).toBeTruthy();
  });

  it.each([
    ["at risk", 85, 30_000, "At risk"],
    ["threshold", 80, 30_000, "At risk"],
    ["below threshold", 79.9, 30_000, null],
    ["weekly", 40, 30_000, null],
    ["zero", 0, 30_000, null],
    ["stale", 85, 120_000, "Stale"],
    ["future", 85, -1_000, "Stale"],
    ["unknown", undefined, undefined, "Unknown"],
  ])("shows subscription usage only in details: %s", (_name, percent, age, status) => {
    const window = { kind: "weekly", percent_used: percent, reset_at: new Date(NOW + 3_600_000).toISOString(), observed_at: age === undefined ? undefined : new Date(NOW - age).toISOString() };
    const capacity = { ...RUNNER.provider_capacity[0]!, billing: "subscription" as const, usage_windows: [window] };
    renderSection({ ...FLEET, runners: [{ ...RUNNER, provider_capacity: [capacity] }] });
    expect(screen.queryByRole("list", { name: `${capacity.backend} subscription usage` })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: `Manage ${RUNNER.display_name}` }));
    const row = within(screen.getByRole("list", { name: `${capacity.backend} subscription usage` }));
    expect(row.getByText(percent === undefined ? /Usage unknown/ : new RegExp(`${percent}% used`))).toBeTruthy();
    expect(row.getByText(/Resets/)).toBeTruthy();
    expect(row.getByText(age === undefined ? "Observation unknown" : /Observed/)).toBeTruthy();
    if (status) expect(row.getByText(status)).toBeTruthy();
    else expect(row.queryByText("At risk")).toBeNull();
    expect(row.queryByText("Healthy")).toBeNull();
    const detail = within(screen.getByRole("article"));
    expect(detail.getByRole("list", { name: `${capacity.backend} subscription usage` })).toBeTruthy();
    if (status) expect(detail.getByText(status)).toBeTruthy();
  });

  it("preserves decoded unknown usage and old-runner presentation", () => {
    const capacity = { ...RUNNER.provider_capacity[0]!, usage_windows: [{ kind: "five_hour" }] };
    const decoded = decodeFleet({ ...FLEET, runners: [{ ...RUNNER, provider_capacity: [capacity] }] });
    expect(decoded.runners[0]!.provider_capacity[0]!.usage_windows).toEqual([{ kind: "five_hour" }]);
    const { rerender } = render(<RunnersSectionView fleet={decoded} now={NOW} />);
    fireEvent.click(screen.getByRole("button", { name: `Manage ${RUNNER.display_name}` }));
    expect(screen.getByText("Reset unknown")).toBeTruthy();
    expect(screen.getByText("Observation unknown")).toBeTruthy();
    rerender(<RunnersSectionView fleet={{ ...FLEET, runners: [RUNNER] }} now={NOW} />);
    expect(screen.queryByText(/Usage unknown|Reset unknown|Observation unknown|At risk|Stale/)).toBeNull();
  });

  it.each([
    ["healthy", "healthy", "active", undefined, "Healthy", null],
    ["attention", "needs_attention", "active", undefined, "Needs attention", "Needs attention"],
    ["draining", "healthy", "draining", undefined, "Draining", "Finishing running work"],
    ["offline", "offline", "active", undefined, "Offline", "Offline"],
    ["failed update", "healthy", "active", "refused", "Needs attention", "Update failed · reinstall needed"],
  ])("renders a compact %s row without diagnostics", (_name, health, state, update, label, reason) => {
    const onOpenRunner = vi.fn();
    const runner = { ...RUNNER, health: health!, state: state!, version: "1.2.3", ...(update ? { update: { status: update, desired: { version: "1.2.4" } } } : {}) };
    render(<RunnersList fleet={{ ...FLEET, runners: [runner] }} projects={PROJECTS} onOpenRunner={onOpenRunner} />);
    const row = within(screen.getByTestId("host-card"));
    expect(row.getByRole("img", { name: label! })).toBeTruthy();
    if (reason) expect(row.getByText(reason)).toBeTruthy();
    else expect(row.queryByText("Healthy")).toBeNull();
    expect(row.getByText("1.2.3")).toBeTruthy();
    expect(row.getByText(`Requested total: ${runner.capacity_limit} slots`)).toBeTruthy();
    expect(row.getByText(`Shared machine: ${runner.host_used} of ${runner.host_capacity} slots in use`)).toBeTruthy();
    expect(row.queryByRole("alert")).toBeNull();
    expect(row.queryByRole("list", { name: /subscription usage/ })).toBeNull();
    fireEvent.click(row.getByRole("button", { name: `Manage ${runner.display_name}` }));
    expect(onOpenRunner).toHaveBeenCalledWith(runner);
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("keeps raw problem and update output out of the list", () => {
    renderSection({ ...FLEET, runners: [{ ...RUNNER, health: "needs_attention", problems: [{ code: "backend_probe_failed", message: "raw probe failure", error_output: "raw stack trace", check: "raw probe command", fix_hint: "Check provider setup", fix_command: "provider login", first_seen: "2026-09-10T12:00:00Z" }] }] });
    const row = within(screen.getByTestId("host-card"));
    expect(row.getByText("Backend probe failed")).toBeTruthy();
    expect(row.queryByText(/raw|provider login/)).toBeNull();
    expect(screen.queryByRole("heading", { name: "Capacity right now" })).toBeNull();
    expect(screen.queryByRole("region", { name: "Provider accounts" })).toBeNull();
  });

  it.each([undefined, "", "1.2.3"])("shows only a reported version badge (%s)", (version) => {
    renderSection({ ...FLEET, runners: [{ ...RUNNER, version, has_heartbeat: false }] });
    expect(within(screen.getByTestId("host-card")).getByText("Version unknown")).toBeTruthy();
    expect(within(screen.getByTestId("host-card")).queryByText("1.2.3")).toBeNull();
  });

  it("refreshes active and historical names through the scoped names projection", async () => {
    vi.useFakeTimers();
    const id = FLEET.runners[0]!.id;
    const active = FLEET.runners[1]!;
    const names = { [id]: { display_name: "Retired Mac", hostname: "retired.local" }, [active.id]: { display_name: active.display_name, hostname: active.hostname } };
    const fetch = vi.fn(async () => new Response(JSON.stringify({ runner_names: names })));
    vi.stubGlobal("fetch", fetch);
    const client = { bootstrap: bootstrapFixture, http: { origin: "https://hub.test", apiBase: "/api/v2/organizations/org_test", csrfToken: "test" } } as unknown as React.ContextType<typeof ClientContext>;
    const { result } = renderHook(() => useRunnerNames(), { wrapper: ({ children }) => <ClientContext.Provider value={client}>{children}</ClientContext.Provider> });
    await act(async () => {});
    expect(runnerDisplay(result.current, id)).toBe("Retired Mac");
    expect(result.current.get(active.id)).toEqual({ display: active.display_name, host: active.hostname });
    expect(result.current.get(id)?.host).toBe("retired.local");
    expect(runnerDisplay(result.current, "runner_0000000000000000000000000000dead")).toBe("runner_00000000");
    expect(fetch).toHaveBeenCalledWith("https://hub.test/api/v2/organizations/org_test/fleet?include=names", expect.objectContaining({ method: "GET", credentials: "same-origin" }));
    const shared = renderHook(() => useRunnerNames(), { wrapper: ({ children }) => <ClientContext.Provider value={client}>{children}</ClientContext.Provider> });
    expect(fetch).toHaveBeenCalledTimes(1);
    names[active.id]!.display_name = "Renamed host";
    await act(async () => { await vi.advanceTimersByTimeAsync(60_000); });
    expect(fetch).toHaveBeenCalledTimes(2);
    expect(runnerDisplay(result.current, active.id)).toBe("Renamed host");
    expect(runnerDisplay(shared.result.current, active.id)).toBe("Renamed host");
    fetch.mockImplementation(async () => new Response("", { status: 403 }));
    await act(async () => { await vi.advanceTimersByTimeAsync(60_000); });
    expect(runnerDisplay(result.current, active.id)).toBe("Renamed host");
    shared.unmount();
    cleanup();
    const otherClient = { ...client!, bootstrap: { ...bootstrapFixture, actor: { ...bootstrapFixture.actor, principal_id: "other-account" } } };
    const other = renderHook(() => useRunnerNames(), { wrapper: ({ children }) => <ClientContext.Provider value={otherClient}>{children}</ClientContext.Provider> });
    await act(async () => {});
    expect(other.result.current.size).toBe(0);
    expect(runnerDisplay(other.result.current, id)).toBe(id);
  });

  it("reads the suggestion and host history through the existing runner and fleet routes", async () => {
    const fetch = vi.fn(async (_input: string, _init?: RequestInit) => new Response(JSON.stringify([]), { status: 200, headers: { "content-type": "application/json" } }));
    const api = makeAccountApi({ origin: "https://hub.test", apiBase: "/api/v2/organizations/org_test", csrfToken: "test", fetch });
    await api.runnerHostHistory("runner/a", "2026-09-03T13:00:00Z", "2026-09-10T13:00:00Z");
    expect(fetch.mock.calls[0]![0]).toBe("https://hub.test/api/v2/organizations/org_test/fleet?include=host_metrics&runner_id=runner%2Fa&from=2026-09-03T13%3A00%3A00Z&to=2026-09-10T13%3A00%3A00Z");
    fetch.mockResolvedValueOnce(new Response(JSON.stringify({ recommendation: { suggested_limit: { limit: 4, current: 6, direction: "lower", hours_covered: 30, saturated_hours: 5, reasons: [] } } }), { status: 200, headers: { "content-type": "application/json" } }));
    expect((await api.runnerCapacity("runner/a")).recommendation?.suggested_limit?.limit).toBe(4);
    expect(fetch.mock.calls[1]![0]).toBe("https://hub.test/api/v2/organizations/org_test/runners/runner%2Fa/capacity");
    fetch.mockResolvedValueOnce(new Response(JSON.stringify({ recommendation: { suggested_limit: { limit: 4, current: 6, direction: "shrink", hours_covered: 30, saturated_hours: 5, reasons: [] } } }), { status: 200, headers: { "content-type": "application/json" } }));
    await expect(api.runnerCapacity("runner/a")).rejects.toThrow();
  });

  it("reads capacity only for runner managers and again only for a runner whose revision changed", async () => {
    const read = vi.fn(async (id: string) => ({ recommendation: { suggested_limit: { limit: 1, current: 2, direction: "lower" as const, hours_covered: 30, saturated_hours: 5, reasons: [id] } } }));
    const first = [{ id: "a", revision: 1 }, { id: "b", revision: 1 }];
    const viewer = renderHook(() => useRunnerSuggestions(read, first, false));
    await act(async () => undefined);
    expect(read).not.toHaveBeenCalled();
    expect(viewer.result.current).toEqual({});
    viewer.unmount();
    let release!: () => void;
    const { result, rerender } = renderHook(({ runners }) => useRunnerSuggestions(read, runners, true), { initialProps: { runners: first } });
    await waitFor(() => expect(Object.keys(result.current).toSorted()).toEqual(["a", "b"]));
    expect(read.mock.calls.map(([id]) => id)).toEqual(["a", "b"]);
    read.mockImplementationOnce((id: string) => new Promise((resolve) => { release = () => resolve({ recommendation: { suggested_limit: { limit: 1, current: 2, direction: "lower", hours_covered: 31, saturated_hours: 5, reasons: [id] } } }); }));
    rerender({ runners: [{ id: "a", revision: 1 }, { id: "b", revision: 2 }] });
    await waitFor(() => expect(result.current.b).toBeUndefined());
    expect(result.current.a?.hours_covered).toBe(30);
    expect(read.mock.calls.map(([id]) => id)).toEqual(["a", "b", "b"]);
    await act(async () => release());
    await waitFor(() => expect(result.current.b?.hours_covered).toBe(31));
  });

  it("removes through the existing DELETE API and accepts its empty response", async () => {
    const fetch = vi.fn(async () => new Response(null, { status: 204 }));
    const api = makeAccountApi({ origin: "https://hub.test", apiBase: "/api/v2/organizations/org_test", csrfToken: "test", fetch });
    await api.removeRunner("runner/a");
    expect(fetch.mock.calls[0]).toEqual([
      "https://hub.test/api/v2/organizations/org_test/runners/runner%2Fa",
      expect.objectContaining({ method: "DELETE" }),
    ]);
  });
  it.each([false, true])("confirms removal and keeps refusal visible (refused=%s)", async (refused) => {
    const remove = vi.fn(async () => {
      if (refused) throw new Error("This runner has active work. Drain it first.");
    });
    render(<RunnersSectionView fleet={{ ...FLEET, editable: true }} onRemoveRunner={remove} />);
    fireEvent.click(screen.getByRole("button", { name: `More actions for ${FLEET.runners[0]!.display_name}` }));
    fireEvent.click(await screen.findByRole("menuitem", { name: "Remove runner" }));
    expect(remove).not.toHaveBeenCalled();
    fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Cancel" }));
    expect(remove).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("button", { name: `More actions for ${FLEET.runners[0]!.display_name}` }));
    fireEvent.click(await screen.findByRole("menuitem", { name: "Remove runner" }));
    fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Remove runner" }));
    await waitFor(() => expect(remove).toHaveBeenCalledWith(FLEET.runners[0]));
    if (refused) {
      await waitFor(() => expect(screen.getByRole("alert").textContent).toContain("active work"));
      expect(screen.getAllByTestId("host-card")).toHaveLength(FLEET.runners.length);
    } else {
      await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    }
  });

  it("hides removal from a read-only fleet", () => {
    render(<RunnersSectionView fleet={{ ...FLEET, editable: false }} onRemoveRunner={vi.fn()} />);
    expect(screen.queryByRole("button", { name: /Remove runner/ })).toBeNull();
  });
  it("shows sleeping Sprites without capacity or attention warnings", () => {
    renderSection({ ...FLEET, runners: [{ ...FLEET.runners[1]!, state: "active", reported_capacity: 2, health: "asleep", claim_refusal_reason: "", problems: [], sprite: { name: "build-host", status: "warm", can_wake: true, wake_failed: false } }] });
    expect(screen.getByText("Asleep, wakes on new work")).toBeTruthy();
    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.queryByText(/slots can't take work/)).toBeNull();
    expect(screen.getByRole("link", { name: "Needs attention 0" })).toBeTruthy();
    expect(document.querySelectorAll('[data-slot-state="unavailable"]')).toHaveLength(0);
  });
  it.each([
    ["v1.2.3", "Too old to take work, needs v1.2.4"],
    ["v1.2.4", ""],
    ["v1.3.0", ""],
    ["dev", ""],
  ])("renders the Hub admission refusal for %s", (version, reason) => {
    renderSection({ ...FLEET, current: "v1.2.4", minimum_runner_version: "v1.2.4", runners: [{ ...FLEET.runners[0]!, version, claim_refusal_reason: reason }] });
    fireEvent.click(screen.getByRole("button", { name: `Manage ${FLEET.runners[0]!.display_name}` }));
    const warning = screen.queryByTestId("host-update");
    if (reason !== "") {
      expect(warning?.textContent).toContain("Runner update needed");
      expect(screen.queryByText(reason)).toBeNull();
    } else {
      expect(warning).toBeNull();
    }
  });

  it.each([
    { tier: "sandbox", probe: ["native-trusted"], allowed: false },
    { tier: "native-trusted", probe: ["native-trusted"], allowed: false },
    { tier: "native-trusted", probe: ["sandbox", "native-trusted"], allowed: true },
  ])("gates Sandbox for a Sprite with saved tier $tier on its isolation probe $probe", ({ tier, probe, allowed }) => {
    const runner = { ...RUNNER, isolation_tier: tier, routing: { ...ROUTING, isolation_tier: tier }, backend_isolation: { codex: probe }, problems: [{ code: "tier_unavailable", message: "Access unavailable", fix_hint: "Choose supported access", first_seen: "2026-09-10T12:00:00Z" }], sprite: { name: "build-host", status: "warm", can_wake: true, wake_failed: false } };
    render(<RunnersSectionView fleet={{ ...FLEET, editable: true, runners: [runner] }} projects={PROJECTS} onSaveRouting={vi.fn()} />);
    fireEvent.click(screen.getByRole("button", { name: "Manage Example laptop" }));
    const sheet = within(screen.getByRole("article"));
    expect(sheet.getByRole("radio", { name: "Sandbox" }).getAttribute("aria-disabled") === "true").toBe(!allowed);
    expect(sheet.getByRole("radio", { name: tier === "sandbox" ? "Sandbox" : "Full access" }).getAttribute("aria-checked")).toBe("true");
    expect(sheet.queryByText("Sandbox on a Fly Sprite needs a passing isolation probe on that runner: update the runner, then switch it to Sandbox once it reports sandbox support.") !== null).toBe(!allowed);
    expect(sheet.queryByRole("button", { name: "Switch to Sandbox" }) !== null).toBe(allowed);
    expect(sheet.getByRole("radio", { name: "Full access" }).getAttribute("aria-disabled")).not.toBe("true");
  });

  it("shows contextual problems and filters through the segmented control", () => {
    const problem = { code: "tier_unavailable", message: "Sandbox tooling is unavailable", fix_hint: "Repair the sandbox tooling", first_seen: "2026-09-10T12:00:00Z" };
    renderSection({ ...FLEET, runners: FLEET.runners.map((runner, index) => index === 0 ? { ...runner, health: "needs_attention", problems: [problem] } : runner) });
    expect(screen.getByRole("link", { name: "Needs attention 1" })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: `Manage ${FLEET.runners[0]!.display_name}` }));
    const sheet = within(screen.getByRole("article"));
    expect(sheet.getByText("Agent access unavailable")).toBeTruthy();
    expect(sheet.getByText(problem.fix_hint)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Back to Runners" }));
    fireEvent.click(screen.getByRole("link", { name: "Needs attention 1" }));
    expect(window.location.search).toBe("?health=needs_attention");
    expect(screen.getAllByTestId("host-card")).toHaveLength(1);
    fireEvent.click(screen.getByRole("link", { name: "All 2" }));
    expect(window.location.search).toBe("");
    expect(screen.getAllByTestId("host-card")).toHaveLength(FLEET.runners.length);
  });

  it("draws one row per runner with leases and host capacity", () => {
    renderSection();
    const rows = screen.getAllByTestId("host-card");
    expect(rows).toHaveLength(FLEET.runners.length);
    expect(within(rows[0]!).getByRole("heading", { name: "Example laptop" })).toBeTruthy();
    expect(rows[0]!.textContent).toContain(`Shared machine: ${FLEET.runners[0]!.host_used} of ${FLEET.runners[0]!.host_capacity} slots in use`);
    expect(rows[1]!.textContent).toContain(`Shared machine: ${FLEET.runners[1]!.host_used} of ${FLEET.runners[1]!.host_capacity} slots in use`);
    expect(rows[1]!.textContent).toContain("Runner update needed");
    expect(screen.queryByTestId("runner-capacity")).toBeNull();
    expect(screen.queryAllByRole("heading", { level: 1 })).toHaveLength(0);
  });
});

describe("shared host capacity", () => {
  function renderCapacity(fleet: FleetResponse) { render(<CapacityPanel runners={fleet.runners} onOpen={vi.fn()} />); }
  const active = { ...FLEET.runners[0]!, machine_id: "machine_shared", state: "active", health: "online", claim_refusal_reason: "", provider_capacity: [], host_capacity: 8, host_used: 1, capacity_limit: 8, reported_capacity: 8 };
  const lease = active.leases[0]!;

  it("counts a shared eight-slot host once and retains each identity's work", () => {
    renderCapacity({ ...FLEET, runners: [
      { ...active, capacity_limit: 4, reported_capacity: 4, host_used: 3 },
      { ...active, id: "runner_sibling", display_name: "Sibling", capacity_limit: 4, reported_capacity: 4, host_used: 3, leases: [
        { ...lease, lease_id: "lease_second", work_item_id: "wi_second" },
        { ...lease, lease_id: "lease_third", work_item_id: "wi_third" },
      ] },
    ] });
    expect(screen.getByText("3 of 8 slots occupied")).toBeTruthy();
    const host = screen.getByTestId("runner-capacity");
    expect(host.querySelectorAll("[data-slot-state]")).toHaveLength(8);
    expect(host.querySelectorAll('[data-slot-state="running"]')).toHaveLength(3);
    expect(host.querySelectorAll('[data-slot-state="free"]')).toHaveLength(5);
    expect(host.querySelector('[title="#wi_second connections: custom alerts from segment membership"]')).toBeTruthy();

  });

  it.each([
    ["draining", { state: "draining" }, 0],
    ["disabled", { state: "disabled" }, 0],
    ["offline", { health: "offline" }, 0],
    ["outside hours", { health: "outside_hours" }, 0],
    ["needs attention", { health: "needs_attention" }, 0],
    ["uncertain update on an online runner", { health: "needs_attention", capacity_health: "online", update: { status: "uncertain", desired: { version: "1.2.4" } } }, 7],
    ["refused update on an online runner", { health: "needs_attention", capacity_health: "online", update: { status: "refused", desired: { version: "1.2.4" } } }, 7],
    ["uncertain update with claim refused", { health: "needs_attention", capacity_health: "online", claim_refusal_reason: "Too old to take work", update: { status: "uncertain", desired: { version: "1.2.4" } } }, 0],
    ["uncertain update on an offline runner", { health: "needs_attention", capacity_health: "offline", update: { status: "uncertain", desired: { version: "1.2.4" } } }, 0],
    ["uncertain update outside hours", { health: "needs_attention", capacity_health: "outside_hours", update: { status: "uncertain", desired: { version: "1.2.4" } } }, 0],
    ["uncertain update with other problems", { health: "needs_attention", capacity_health: "needs_attention", update: { status: "uncertain", desired: { version: "1.2.4" } } }, 0],
    ["uncertain update with provider exhausted", { health: "needs_attention", capacity_health: "online", update: { status: "uncertain", desired: { version: "1.2.4" } }, provider_capacity: [{ ...FLEET.runners[0]!.provider_capacity[0]!, state: "exhausted" }] }, 0],
    ["uncertain update with default provider report", { health: "needs_attention", capacity_health: "online", update: { status: "uncertain", desired: { version: "1.2.4" } }, provider_capacity: [{ ...FLEET.runners[0]!.provider_capacity[0]!, max_concurrent: undefined, state: "unknown", used: 12 }] }, 7],
    ["uncertain update with zero account limit", { health: "needs_attention", capacity_health: "online", update: { status: "uncertain", desired: { version: "1.2.4" } }, provider_capacity: [{ ...FLEET.runners[0]!.provider_capacity[0]!, max_concurrent: 0, state: "unknown" }] }, 7],
    ["claim refused", { claim_refusal_reason: "Too old to take work" }, 0],
    ["reported limit", { reported_capacity: 3 }, 2],
    ["default provider report", { provider_capacity: [{ ...FLEET.runners[0]!.provider_capacity[0]!, max_concurrent: undefined, state: "unknown", used: 12 }] }, 7],
    ["zero account limit", { provider_capacity: [{ ...FLEET.runners[0]!.provider_capacity[0]!, max_concurrent: 0, state: "unknown" }] }, 7],
    ["bounded provider full", { provider_capacity: [{ ...FLEET.runners[0]!.provider_capacity[0]!, max_concurrent: 2, used: 2 }] }, 0],
  ])("preserves running work and limits free slots for %s", (_name, values, free) => {
    renderCapacity(decodeFleet({ ...FLEET, runners: [{ ...active, ...values }] }));
    const host = screen.getByTestId("runner-capacity");
    expect(screen.getByText("1 of 8 slots occupied")).toBeTruthy();
    expect(host.querySelectorAll('[data-slot-state="running"]')).toHaveLength(1);
    expect(host.querySelectorAll('[data-slot-state="free"]')).toHaveLength(free);
    expect(host.querySelectorAll('[data-slot-state="unavailable"]')).toHaveLength(7 - free);
    if (free < 7) {
      expect(screen.getByText(`${7 - free} slots can't take work`)).toBeTruthy();
    } else {
      expect(screen.queryByText(/slots can't take work/)).toBeNull();
    }
  });

  it("counts a runner limit below the host ceiling as its slots, not as slots that can't take work", () => {
    renderCapacity({ ...FLEET, runners: [{ ...active, capacity_limit: 2 }] });
    const host = screen.getByTestId("runner-capacity");
    expect(screen.getByText("1 of 2 slots occupied")).toBeTruthy();
    expect(screen.queryByText(/slots can't take work/)).toBeNull();
    expect(host.querySelectorAll('[data-slot-state="free"]')).toHaveLength(1);
    expect(host.querySelectorAll('[data-slot-state="unavailable"]')).toHaveLength(0);
  });

  it("counts only offline runner slots as unable to take work across a mixed fleet", () => {
    renderCapacity({ ...FLEET, runners: [
      { ...active, capacity_limit: 1, reported_capacity: 1 },
      { ...active, id: "runner_mini", machine_id: "machine_mini", host_capacity: 4, host_used: 0, capacity_limit: 2, reported_capacity: 2, leases: [] },
      { ...active, id: "runner_hosted", machine_id: "machine_hosted", state: "draining", health: "offline", connection_health: "offline", host_capacity: 1, host_used: 0, capacity_limit: 1, reported_capacity: 1, leases: [] },
    ] });
    expect(screen.getByText("1 of 4 slots occupied")).toBeTruthy();
    expect(screen.getByText("2 free")).toBeTruthy();
    expect(screen.getByText("1 slots can't take work")).toBeTruthy();
  });

  it("uses only an active sibling's remaining allowance while another drains", () => {
    renderCapacity({ ...FLEET, runners: [
      { ...active, state: "draining", host_used: 2 },
      { ...active, id: "runner_sibling", host_used: 2, capacity_limit: 2, reported_capacity: 4, leases: [{ ...lease, lease_id: "lease_second" }] },
    ] });
    const host = screen.getByTestId("runner-capacity");
    expect(host.querySelectorAll('[data-slot-state="running"]')).toHaveLength(2);
    expect(host.querySelectorAll('[data-slot-state="free"]')).toHaveLength(1);
    expect(host.querySelectorAll('[data-slot-state="unavailable"]')).toHaveLength(5);
  });

  it("keeps distinct machine identities separate despite identical names", () => {
    renderCapacity({ ...FLEET, runners: [active, { ...active, id: "runner_other", machine_id: "machine_other" }] });
    expect(screen.getAllByTestId("runner-capacity")).toHaveLength(2);
    expect(screen.getByText("2 of 16 slots occupied")).toBeTruthy();
  });
});

describe("providers", () => {
  it("folds every runner's capacity onto one table row per provider", () => {
    render(<ProviderSummary runners={FLEET.runners} />);
    const providers = screen.getByRole("region", { name: "Provider accounts" });
    expect(providers.querySelectorAll("tbody tr")).toHaveLength(2);
    expect(within(providers).getByText("codex")).toBeTruthy();
    expect(within(providers).getByText("claude")).toBeTruthy();
    expect(within(providers).getAllByText(/models/).length).toBeGreaterThan(0);
    expect(within(providers).queryByText(/gpt-6-astra/)).toBeNull();
  });

  it("invites enrollment and omits capacity when the fleet is empty", () => {
    renderSection(EMPTY);

    expect(screen.getByText("No runners yet. Add a runner manually or let Luna help you set one up.")).toBeTruthy();
    expect(screen.queryAllByTestId("host-card")).toHaveLength(0);
    expect(screen.queryByRole("heading", { name: "Capacity right now" })).toBeNull();
  });
});

describe("runner details", () => {
  const LOWER = { limit: 4, current: 6, direction: "lower" as const, hours_covered: 91, saturated_hours: 19, reasons: ["CPU stayed at 90% or more for 2+ hours running in 11 of 91 busy hours."] };
  const HOUR = {
    hour: "2026-09-10T10:00:00Z", logical_cores: 16, memory_total_bytes: 128 * 2 ** 30, memory_available_min_bytes: 24 * 2 ** 30, memory_sample_count: 120,
    swap_used_max_bytes: 0, pressure_warn_count: 3, pressure_critical_count: 0, cpu_sample_count: 120, cpu_busy_average_percent: 95, load1_max: 32, load_sample_count: 120,
    disk_free_min_bytes: 50e9, disk_total_min_bytes: 2000 * 2 ** 30, disk_sample_count: 120,
  };

  it.each([["v1.2.3", "1.2.3", "same as the Hub"], ["1.2.2", "1.2.3", "Hub is 1.2.3"]])("compares the reported version %s with Hub %s", (version, currentVersion, label) => {
    render(<RunnerDetailPage runner={{ ...RUNNER, version }} currentVersion={currentVersion} projects={PROJECTS} editable onSave={vi.fn()} onClose={vi.fn()} />);
    expect(screen.getByText(label!)).toBeTruthy();
    expect(screen.getByText(`Shared machine: ${RUNNER.host_used} of ${RUNNER.host_capacity} slots in use across runner identities`)).toBeTruthy();
  });

  it("fills only the draft from the suggestion until Save runner", async () => {
    const save = vi.fn().mockResolvedValue(undefined);
    render(<RunnersSectionView fleet={{ ...FLEET, editable: true, runners: [RUNNER] }} projects={PROJECTS} onSaveRouting={save} suggestions={{ [RUNNER.id]: LOWER }} />);
    fireEvent.click(screen.getByRole("button", { name: "Manage Example laptop" }));
    const box = within(screen.getByTestId("suggested-limit-box"));
    expect(box.getByText("Suggested: 4 jobs at once")).toBeTruthy();
    expect(box.getByText(LOWER.reasons[0]!)).toBeTruthy();
    fireEvent.click(box.getByRole("button", { name: "Use 4" }));
    expect((screen.getByRole("spinbutton", { name: "Total runner slots" }) as HTMLInputElement).value).toBe("4");
    expect(box.queryByRole("button")).toBeNull();
    expect(box.queryByText(/fills Total runner slots/)).toBeNull();
    expect(save).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("button", { name: "Save runner" }));
    await waitFor(() => expect(save).toHaveBeenCalledWith(RUNNER, { ...ROUTING, capacity_limit: 4 }));
  });

  it.each([["already applied", { ...LOWER, limit: 6 }, "Suggested: 6 jobs at once"], ["unknown direction", { ...LOWER, direction: "shrink" as SuggestedLimit["direction"] }, null]] as const)("offers no apply when the suggestion is %s", (_case, suggestion, heading) => {
    render(<RunnersSectionView fleet={{ ...FLEET, editable: true, runners: [RUNNER] }} projects={PROJECTS} onSaveRouting={vi.fn()} suggestions={{ [RUNNER.id]: suggestion }} />);
    fireEvent.click(screen.getByRole("button", { name: "Manage Example laptop" }));
    expect(screen.queryByTestId("suggested-limit-box")?.querySelector("p")?.textContent ?? null).toBe(heading);
    expect(screen.queryByRole("button", { name: /^Use / })).toBeNull();
  });

  it("shows thin evidence without a number or an apply button", () => {
    render(<RunnersSectionView fleet={{ ...FLEET, editable: true, runners: [RUNNER] }} projects={PROJECTS} onSaveRouting={vi.fn()} suggestions={{ [RUNNER.id]: { current: 6, direction: "insufficient_evidence", hours_covered: 9, saturated_hours: 0, reasons: [] } }} />);
    fireEvent.click(screen.getByRole("button", { name: "Manage Example laptop" }));
    const box = within(screen.getByTestId("suggested-limit-box"));
    expect(box.getByText("Not enough data yet")).toBeTruthy();
    expect(box.queryByRole("button")).toBeNull();
  });

  it("charts host load for the selected range and notes low disk", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(NOW);
    const load = vi.fn().mockResolvedValue([HOUR, { ...HOUR, hour: "2026-09-10T11:00:00Z", cpu_busy_average_percent: 40, load1_max: 8, pressure_warn_count: 0, disk_free_min_bytes: 120e9 }]);
    render(<RunnersSectionView fleet={{ ...FLEET, runners: [RUNNER] }} now={NOW} loadHostHistory={load} />);
    fireEvent.click(screen.getByRole("button", { name: "Manage Example laptop" }));
    const section = within(await screen.findByTestId("host-load"));
    await waitFor(() => expect(section.getByText("Hours ≥ 90% CPU")).toBeTruthy());
    expect(section.getByText("95%")).toBeTruthy();
    expect(section.getByText("2")).toBeTruthy();
    expect(section.getByText("24 GB")).toBeTruthy();
    expect(section.getByText(/Free disk fell to 50\.0 GB on Sep 10\./)).toBeTruthy();
    const memory = within(section.getByRole("region", { name: "Memory" }));
    const ticks = [...memory.getByRole("img", { name: "Free memory and swap by hour" }).querySelectorAll("text")].map((text) => text.textContent);
    expect(ticks).not.toContain("128 GB");
    expect(Math.max(...ticks.filter((text) => text?.endsWith(" GB")).map((text) => Number.parseInt(text!, 10)))).toBeLessThan(64);
    fireEvent.keyDown(memory.getByRole("img", { name: "Free memory and swap by hour" }), { key: "ArrowRight" });
    const readout = memory.getByRole("status");
    expect(readout.textContent).toContain("Pressure warning");
    expect(readout.textContent).not.toContain("128 GB");
    expect(load).toHaveBeenCalledWith(RUNNER.id, "2026-09-03T13:00:00.000Z", "2026-09-10T13:00:00.000Z");
    fireEvent.click(section.getByRole("button", { name: "24h" }));
    await waitFor(() => expect(load).toHaveBeenLastCalledWith(RUNNER.id, "2026-09-09T13:00:00.000Z", "2026-09-10T13:00:00.000Z"));
  });

  it.each([403, 404])("hides host load without an error when the read returns %s", async (status) => {
    const load = vi.fn().mockRejectedValue(new AccountError({ status, code: "not_found", message: "Not found" }));
    render(<RunnersSectionView fleet={{ ...FLEET, runners: [RUNNER] }} now={NOW} loadHostHistory={load} />);
    fireEvent.click(screen.getByRole("button", { name: "Manage Example laptop" }));
    await waitFor(() => expect(load).toHaveBeenCalled());
    await waitFor(() => expect(screen.queryByTestId("host-load")).toBeNull());
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it.each([["prj_known", "prj_unknown"], ["prj_unknown"]])("preserves unreadable project grants without displaying raw IDs: %s", async (...projectIds) => {
    const save = vi.fn().mockResolvedValue(undefined);
    const runner = { ...RUNNER, routing: { ...ROUTING, project_ids: projectIds } };
    render(<RunnersSectionView fleet={{ ...FLEET, editable: true, runners: [runner] }} projects={PROJECTS} onSaveRouting={save} />);
    const row = within(screen.getByTestId("host-card"));
    expect(row.getByText(/Unavailable project/)).toBeTruthy();
    expect(row.queryByText("No projects reported")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Manage Example laptop" }));
    const sheet = screen.getByRole("article");
    expect((within(sheet).getByRole("checkbox", { name: "Known project" }) as HTMLInputElement).checked).toBe(projectIds.includes("prj_known"));
    expect((within(sheet).getByRole("checkbox", { name: "Unavailable project" }) as HTMLInputElement).checked).toBe(true);
    expect(sheet.textContent).not.toContain("prj_unknown");
    fireEvent.click(within(sheet).getByRole("button", { name: "Save runner" }));
    await waitFor(() => expect(save).toHaveBeenCalledWith(runner, runner.routing));
  });

  it("saves a permitted runner when unrelated projects prevent org-wide editing", async () => {
    const save = vi.fn().mockResolvedValue(undefined);
    const runner = { ...RUNNER, editable: true, can_edit_projects: false, routing: { ...ROUTING, state: "draining" } };
    render(<RunnersSectionView fleet={{ ...FLEET, editable: false, runners: [runner] }} projects={PROJECTS} onSaveRouting={save} />);
    fireEvent.click(screen.getByRole("button", { name: "Manage Example laptop" }));
    expect((screen.getByRole("checkbox", { name: "Known project" }) as HTMLInputElement).disabled).toBe(true);
    fireEvent.click(screen.getByRole("radio", { name: "Active Takes new work" }));
    fireEvent.click(screen.getByRole("button", { name: "Decrease total runner slots" }));
    fireEvent.click(screen.getByRole("button", { name: "Save runner" }));
    await waitFor(() => expect(save).toHaveBeenCalledWith(runner, expect.objectContaining({ state: "active", capacity_limit: 5, project_ids: ROUTING.project_ids })));
  });

  it("edits allowed projects and lets unknown projects be reselected", async () => {
    const save = vi.fn().mockResolvedValue(undefined);
    render(<RunnersSectionView fleet={{ ...FLEET, editable: true, runners: [RUNNER] }} projects={PROJECTS} onSaveRouting={save} />);
    fireEvent.click(screen.getByRole("button", { name: "Manage Example laptop" }));
    const sheet = screen.getByRole("article");
    expect((within(sheet).getByRole("checkbox", { name: "Second project" }) as HTMLInputElement).checked).toBe(false);
    fireEvent.click(within(sheet).getByRole("checkbox", { name: "Second project" }));
    fireEvent.click(within(sheet).getByRole("checkbox", { name: "Unavailable project" }));
    fireEvent.click(within(sheet).getByRole("checkbox", { name: "Unavailable project" }));
    fireEvent.click(within(sheet).getByRole("button", { name: "Save runner" }));
    await waitFor(() => expect(save).toHaveBeenCalledWith(RUNNER, expect.objectContaining({
      project_ids: ["prj_known", "prj_second", "prj_unknown"],
    })));
  });

  it.each(["Mon-Fri 09:00-17:00", "Sat-Sun 00:00-24:00", "Fri-Mon 22:00-06:00", "Tue-Tue 08:15-12:45", " Mon-Fri  09:00-17:00 "])("preserves the existing hours string %s", (window) => {
    expect(serializeRunnerWindow(parseRunnerWindow(window))).toBe(window);
    expect(serializeRunnerWindow({ ...parseRunnerWindow(window), until: "13:00" })).toBe(`${parseRunnerWindow(window).days} ${parseRunnerWindow(window).from}-13:00`);
  });

  it("serializes edited hours and chips and clears deadlines for always available", async () => {
    const save = vi.fn().mockResolvedValue(undefined);
    const { rerender } = render(<RunnersSectionView fleet={{ ...FLEET, editable: true, runners: [RUNNER] }} projects={PROJECTS} onSaveRouting={save} />);
    fireEvent.click(screen.getByRole("button", { name: "Manage Example laptop" }));
    fireEvent.change(screen.getByLabelText("Until 1"), { target: { value: "18:00" } });
    fireEvent.change(screen.getByLabelText("Add tag"), { target: { value: "gpu,linux" } });
    fireEvent.click(screen.getByRole("button", { name: "Save runner" }));
    await waitFor(() => expect(save).toHaveBeenCalledWith(RUNNER, expect.objectContaining({
      tags: ["linux", "gpu"], availability: { ...ROUTING.availability, windows: ["Mon-Fri 09:00-18:00", "Sat-Sun 00:00-24:00"] },
    })));
    await screen.findByText("Runner saved");
    fireEvent.click(screen.getByRole("button", { name: "Back to Runners" }));
    rerender(<RunnersSectionView fleet={{ ...FLEET, editable: true, runners: [RUNNER] }} projects={PROJECTS} onSaveRouting={save} />);
    fireEvent.click(screen.getByRole("button", { name: "Manage Example laptop" }));
    fireEvent.change(screen.getByLabelText("Availability"), { target: { value: "always" } });
    fireEvent.click(screen.getByRole("button", { name: "Save runner" }));
    await waitFor(() => expect(save).toHaveBeenLastCalledWith(RUNNER, expect.objectContaining({
      availability: { ...ROUTING.availability, windows: [], hard_deadline: "" }
    })));
  });

  it.each([false, true])("shows routing and missing grants when this runner is read-only (fleet editable: %s)", (editable) => {
    const reason = 'Project "Second project" lacks a manage_runner grant; ask an owner or admin.';
    render(<RunnersSectionView fleet={{ ...FLEET, editable, runners: [{ ...RUNNER, editable: false, edit_refusal_reason: reason }] }} projects={PROJECTS} onSaveRouting={vi.fn()} />);
    fireEvent.click(screen.getByRole("button", { name: "Manage Example laptop" }));
    const sheet = screen.getByRole("article");
    expect(sheet.querySelectorAll("input, select, textarea")).toHaveLength(0);
    expect(sheet.querySelector('[data-slot="sheet-footer"]')).toBeNull();
    for (const name of ["Taking work", "Capacity", "Allowed projects", "Tags", "Schedule", "Agent access", "Running work", "Provider accounts"]) {
      expect(within(sheet).getByRole("heading", { name })).toBeTruthy();
    }
    expect(within(sheet).getByRole("status").textContent).toContain(reason);
    expect(sheet.textContent).toContain("Known project");
    expect(sheet.textContent).toContain("Unavailable project");
    expect(sheet.textContent).not.toContain("prj_unknown");
    expect(sheet.textContent).toContain("linux");
    expect(sheet.textContent).toContain("tcp:127.0.0.1:8080");
    expect(sheet.textContent).toContain("Sat-Sun 00:00-24:00");
    expect(sheet.textContent).not.toContain("Tags are not reported");
  });

  it("shows project checkout failures and the authentication command on the runner page", () => {
    const runner = { ...RUNNER, project_checkouts: {
      prj_known: { status: "missing", message: "Cannot clone https://github.com/example-org/example-project.git", fix_command: "gh auth login --hostname github.com --git-protocol https" },
      prj_unknown: { status: "ready" },
    } };
    render(<RunnersSectionView fleet={{ ...FLEET, runners: [runner] }} projects={PROJECTS} />);
    fireEvent.click(screen.getByRole("button", { name: "Manage Example laptop" }));
    const sheet = screen.getByRole("article");
    expect(within(sheet).getByText("Checkout unavailable")).toBeTruthy();
    expect(within(sheet).queryByText("Cannot clone https://github.com/example-org/example-project.git")).toBeNull();
    expect(within(sheet).getByText("gh auth login --hostname github.com --git-protocol https")).toBeTruthy();
    expect(within(sheet).getByText("Checkout ready")).toBeTruthy();
    expect(within(sheet).getByText("The runner retries automatically after repository access is fixed.")).toBeTruthy();
  });

  it("does not invent settings when the fleet omits routing for a viewer", () => {
    render(<RunnersSectionView fleet={{ ...FLEET, editable: false, runners: [{ ...FLEET.runners[0]!, availability: ROUTING.availability }] }} projects={PROJECTS} />);
    fireEvent.click(screen.getByRole("button", { name: "Manage Example laptop" }));
    const sheet = screen.getByRole("article");
    expect(sheet.querySelectorAll("input, select, textarea")).toHaveLength(0);
    expect(sheet.textContent).toContain("Project access is not reported.");
    expect(sheet.textContent).toContain("Tags are not reported.");
    expect(sheet.textContent).toContain("Host services are not reported.");
    expect(sheet.textContent).toContain("Mon-Fri 09:00-17:00");
  });

  it.each([false, true])("shows save errors and reviews the routing revision after a conflict with reload failure %s", async (reloadFails) => {
    const save = vi.fn()
      .mockRejectedValueOnce(new AccountError({ status: 400, code: "invalid", message: "Invalid runner hours" }))
      .mockRejectedValueOnce(new AccountError({ status: 409, code: "conflict", message: "revision conflict" }))
      .mockResolvedValue(undefined);
    const fresh = { ...RUNNER, revision: 8, routing: { ...ROUTING, state: "draining", capacity_limit: 3, project_capacity_limits: { prj_known: 0 } } };
    const reload = vi.fn(async () => {
      if (reloadFails) throw new Error("Fleet unavailable");
      rerender(<RunnersSectionView fleet={{ ...FLEET, editable: true, runners: [fresh] }} projects={PROJECTS} onSaveRouting={save} onReloadRunner={reload} />);
    });
    const { rerender } = render(<RunnersSectionView fleet={{ ...FLEET, editable: true, runners: [RUNNER] }} projects={PROJECTS} onSaveRouting={save} onReloadRunner={reload} />);
    fireEvent.click(screen.getByRole("button", { name: "Manage Example laptop" }));
    fireEvent.change(screen.getByLabelText("Total runner slots"), { target: { value: "10" } });
    fireEvent.change(screen.getByLabelText("Known project maximum slots"), { target: { value: "4" } });
    fireEvent.change(screen.getByLabelText("Add tag"), { target: { value: "gpu" } });
    fireEvent.click(screen.getByRole("button", { name: "Save runner" }));
    const saveError = await screen.findByRole("alert");
    expect(saveError.textContent).toContain("Invalid runner hours");
    expect(saveError.closest("form")).not.toBeNull();
    expect((screen.getByLabelText("Total runner slots") as HTMLInputElement).value).toBe("10");
    expect((screen.getByLabelText("Known project maximum slots") as HTMLInputElement).value).toBe("4");
    expect(screen.getByRole("button", { name: "Remove tag gpu" })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Save runner" }));
    await waitFor(() => expect(screen.getByRole("alert").textContent).toContain(reloadFails ? "Could not reload it: Fleet unavailable" : "settings have been reloaded"));
    expect(reload).toHaveBeenCalledOnce();
    if (reloadFails) {
      expect((screen.getByLabelText("Total runner slots") as HTMLInputElement).value).toBe("10");
      expect((screen.getByLabelText("Known project maximum slots") as HTMLInputElement).value).toBe("4");
      expect(screen.getByRole("button", { name: "Remove tag gpu" })).toBeTruthy();
      rerender(<RunnersSectionView fleet={{ ...FLEET, editable: true, runners: [fresh] }} projects={PROJECTS} onSaveRouting={save} onReloadRunner={reload} />);
      expect((screen.getByLabelText("Total runner slots") as HTMLInputElement).value).toBe("10");
      expect((screen.getByLabelText("Known project maximum slots") as HTMLInputElement).value).toBe("4");
      expect(screen.getByRole("button", { name: "Remove tag gpu" })).toBeTruthy();
      fireEvent.click(screen.getByRole("button", { name: "Save runner" }));
      expect(save).toHaveBeenCalledTimes(2);
      fireEvent.click(screen.getByRole("button", { name: "Review latest settings" }));
    }
    expect((screen.getByLabelText("Total runner slots") as HTMLInputElement).value).toBe("3");
    expect((screen.getByLabelText("Known project maximum slots") as HTMLInputElement).value).toBe("0");
    expect(screen.queryByRole("button", { name: "Remove tag gpu" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Save runner" }));
    await waitFor(() => expect(save).toHaveBeenLastCalledWith(fresh, fresh.routing));
  });

  it("opens every problem from attention and cancels without saving", () => {
    const save = vi.fn();
    const problems = [
      { code: "sandbox", message: "Sandbox unavailable", fix_hint: "Repair sandbox", first_seen: "2026-09-10T12:00:00Z" },
      { code: "provider", message: "Provider unavailable", fix_hint: "Sign in", first_seen: "2026-09-10T12:01:00Z" },
    ];
    render(<RunnersSectionView fleet={{ ...FLEET, editable: true, runners: [{ ...RUNNER, health: "needs_attention", problems }] }} projects={PROJECTS} onSaveRouting={save} />);
    fireEvent.click(screen.getByRole("button", { name: "Manage Example laptop" }));
    const sheet = screen.getByRole("article");
    expect(within(sheet).getAllByRole("alert")).toHaveLength(2);
    expect(sheet.textContent).not.toContain("raw stack trace");
    fireEvent.click(within(sheet).getByRole("button", { name: "Back to Runners" }));
    expect(save).not.toHaveBeenCalled();
    expect(screen.queryByRole("dialog")).toBeNull();
  });


  it("shows outside hours as a plain runner status", () => {
    renderSection({ ...FLEET, runners: [{ ...FLEET.runners[0]!, health: "outside_hours", claim_refusal_reason: "" }] });
    const row = screen.getByTestId("host-card");
    expect(within(row).getByText("Outside hours")).toBeTruthy();
    expect(screen.queryByRole("alert")).toBeNull();
  });
});

describe("runner problem reports", () => {
  it("shows each problem with its fix action and excludes raw diagnostics", () => {
    render(<RunnerProblems runner={{ ...RUNNER, problems: [
      { code: "backend_missing", message: "raw error message", check: "raw probe", error_output: "raw stack trace", fix_command: "codex login --device-auth", fix_hint: "Sign in", first_seen: "2026-09-10T12:00:00Z", subject: "codex" },
      { code: "policy_mismatch", project_id: "prj_second", message: "raw policy digest", fix_hint: "Approve the project policy", first_seen: "2026-09-10T12:00:00Z" },
    ] }} />);
    expect(screen.getByText("Provider sign-in or setup needed")).toBeTruthy();
    expect(screen.getByText("Project policy needs approval")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Copy codex login --device-auth" })).toBeTruthy();
    expect(screen.getByText("Approve the project policy")).toBeTruthy();
    expect(screen.queryByText(/raw/)).toBeNull();
  });
});


describe("runner slot settings", () => {
  const routing: RunnerRouting = { ...ROUTING, project_ids: PROJECTS.map((project) => project.id), project_capacity_limits: { prj_second: 0 }, project_ranks: { prj_known: 10 }, availability: { ...ROUTING.availability, window_slots: { "Mon-Fri 09:00-17:00": 3 } } };
  const runner = decodeFleet({ ...FLEET, runners: [{ ...RUNNER, state: "draining", host_capacity: 8, host_used: 3, routing: { ...routing, state: "draining" }, capacity_configuration: capacityFixture }] }).runners[0]!;

  it.each(["prj_known", "prj_second", null])("prunes unselected maxima when converting organization scope to projects: %s", async (selected) => {
    const save = vi.fn().mockResolvedValue(undefined);
    const organizationRunner = { ...runner, routing: { ...runner.routing!, scope: "organization" as const, project_ids: [], project_capacity_limits: { prj_known: 4, prj_second: 0 } } };
    render(<RunnersSectionView fleet={{ ...FLEET, runners: [organizationRunner] }} projects={PROJECTS} onSaveRouting={save} />);
    fireEvent.click(screen.getByRole("button", { name: "Manage Example laptop" }));
    fireEvent.change(screen.getByLabelText("Runner scope"), { target: { value: "projects" } });
    if (selected) fireEvent.click(screen.getByRole("checkbox", { name: selected === "prj_known" ? "Known project" : "Second project" }));
    fireEvent.click(screen.getByRole("button", { name: "Save runner" }));
    await waitFor(() => expect(save).toHaveBeenCalledWith(organizationRunner, {
      ...organizationRunner.routing, scope: "projects", project_ids: selected ? [selected] : [],
      project_capacity_limits: selected === "prj_known" ? { prj_known: 4 } : selected === "prj_second" ? { prj_second: 0 } : {},
    }));
  });

  it.each(["draining", "disabled"].flatMap((state) => ["total", "projects", "both"].map((setting) => ({ state, setting }))))("saves $setting capacity settings for a $state runner, reloads them, and leaves sibling settings and grants intact", async ({ state, setting }) => {
    const edited = { ...runner, state, routing: { ...runner.routing!, state } };
    let fleet: FleetResponse = { ...FLEET, runners: [edited, { ...RUNNER, id: "runner_second", display_name: "Second runner", state: "disabled" }] };
    const sibling = fleet.runners[1];
    const save = vi.fn(async (saved, next) => {
      fleet = { ...fleet, runners: fleet.runners.map((entry) => entry.id === saved.id ? { ...entry, revision: 8, routing: next, capacity_limit: next.capacity_limit } : entry) };
      rerender(view());
    });
    const view = () => <RunnersSectionView fleet={fleet} projects={PROJECTS} onSaveRouting={save} />;
    const { rerender, unmount } = render(view());
    fireEvent.click(screen.getByRole("button", { name: "Manage Example laptop" }));
    expect(screen.getByText("2 slots in use · effective maximum 6")).toBeTruthy();
    expect(screen.getByText("1 slots in use · effective maximum 0")).toBeTruthy();
    if (setting !== "projects") fireEvent.change(screen.getByLabelText("Total runner slots"), { target: { value: "8" } });
    if (setting !== "total") {
      fireEvent.change(screen.getByLabelText("Known project maximum slots"), { target: { value: "4" } });
      fireEvent.change(screen.getByLabelText("Second project maximum slots"), { target: { value: "" } });
    }
    fireEvent.change(screen.getByLabelText("Add tag"), { target: { value: "gpu" } });
    fireEvent.click(screen.getByRole("button", { name: "Save runner" }));
    await screen.findByText("Runner saved", { exact: true });
    expect(save).toHaveBeenCalledWith(edited, { ...edited.routing, capacity_limit: setting === "projects" ? 6 : 8, project_capacity_limits: setting === "total" ? { prj_second: 0 } : { prj_known: 4 }, tags: ["linux", "gpu"] });
    expect(fleet.runners[1]).toBe(sibling);
    fireEvent.click(screen.getByRole("button", { name: "Save runner" }));
    await waitFor(() => expect(save).toHaveBeenLastCalledWith(expect.objectContaining({ revision: 8 }), expect.objectContaining({ capacity_limit: setting === "projects" ? 6 : 8, project_capacity_limits: setting === "total" ? { prj_second: 0 } : { prj_known: 4 } })));
    unmount();
    render(view());
    fireEvent.click(screen.getByRole("button", { name: "Manage Example laptop" }));
    expect((screen.getByLabelText("Total runner slots") as HTMLInputElement).value).toBe(setting === "projects" ? "6" : "8");
    expect((screen.getByLabelText("Known project maximum slots") as HTMLInputElement).value).toBe(setting === "total" ? "" : "4");
    expect((screen.getByLabelText("Second project maximum slots") as HTMLInputElement).value).toBe(setting === "total" ? "0" : "");
  });

  it.each([
    ...["", "-1", "1.5", "10001", "1e2"].map((value) => ({ field: "Total runner slots", value })),
    ...["-1", "1.5", "1e2"].map((value) => ({ field: "Known project maximum slots", value })),
  ])("refuses invalid $field value '$value' without saving", async ({ field, value }) => {
    const save = vi.fn();
    render(<RunnersSectionView fleet={{ ...FLEET, runners: [runner] }} projects={PROJECTS} onSaveRouting={save} />);
    fireEvent.click(screen.getByRole("button", { name: "Manage Example laptop" }));
    fireEvent.change(screen.getByLabelText(field), { target: { value } });
    fireEvent.submit(screen.getByRole("button", { name: "Save runner" }).closest("form")!);
    expect(screen.getByRole("alert").textContent).toContain("whole numbers from 0 to 10000");
    expect(save).not.toHaveBeenCalled();
  });

  it.each(["draining", "disabled"])("accepts explicit zero without reactivating a %s runner and validates project maxima independently", async (state) => {
    const edited = { ...runner, state, routing: { ...runner.routing!, state } };
    const save = vi.fn().mockResolvedValue(undefined);
    render(<RunnersSectionView fleet={{ ...FLEET, runners: [edited] }} projects={PROJECTS} onSaveRouting={save} />);
    fireEvent.click(screen.getByRole("button", { name: "Manage Example laptop" }));
    fireEvent.change(screen.getByLabelText("Total runner slots"), { target: { value: "0" } });
    fireEvent.change(screen.getByLabelText("Known project maximum slots"), { target: { value: "10001" } });
    fireEvent.submit(screen.getByRole("button", { name: "Save runner" }).closest("form")!);
    expect(save).not.toHaveBeenCalled();
    fireEvent.change(screen.getByLabelText("Known project maximum slots"), { target: { value: "0" } });
    fireEvent.click(screen.getByRole("button", { name: "Save runner" }));
    await waitFor(() => expect(save).toHaveBeenCalledWith(edited, expect.objectContaining({ capacity_limit: 0, project_capacity_limits: { prj_known: 0, prj_second: 0 }, state })));
  });

  it("retains drafts when a background refresh changes the revision and requires review", async () => {
    const save = vi.fn();
    const view = (revision: number) => <RunnersSectionView fleet={{ ...FLEET, runners: [{ ...runner, revision }] }} projects={PROJECTS} onSaveRouting={save} />;
    const { rerender } = render(view(7));
    fireEvent.click(screen.getByRole("button", { name: "Manage Example laptop" }));
    fireEvent.change(screen.getByLabelText("Known project maximum slots"), { target: { value: "4" } });
    rerender(view(8));
    expect((screen.getByLabelText("Known project maximum slots") as HTMLInputElement).value).toBe("4");
    fireEvent.click(screen.getByRole("button", { name: "Save runner" }));
    expect(save).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("button", { name: "Review latest settings" }));
    expect((screen.getByLabelText("Known project maximum slots") as HTMLInputElement).value).toBe("");
  });

  it.each([false, true])("shows named scoped read-only maxima for organization scope %s", async (organization) => {
    const scoped = { ...runner, editable: false, routing: { ...routing, scope: organization ? "organization" as const : "projects" as const }, capacity_configuration: { ...runner.capacity_configuration!, projects: { ...runner.capacity_configuration!.projects, hidden: { desired: 9, applied: 9, effective: 6, used: 1 } } } };
    const { rerender } = render(<RunnersSectionView fleet={{ ...FLEET, runners: [scoped] }} projects={PROJECTS} onSaveRouting={vi.fn()} />);
    fireEvent.click(screen.getByRole("button", { name: "Manage Example laptop" }));
    const sheet = screen.getByRole("article");
    expect(within(sheet).getByText("Known project maximum slots: Inherited total")).toBeTruthy();
    expect(within(sheet).getByText("Second project maximum slots: 0")).toBeTruthy();
    expect(sheet.querySelectorAll("input,select,textarea")).toHaveLength(0);
    expect(sheet.textContent).not.toContain("hidden");
    rerender(<RunnersSectionView fleet={{ ...FLEET, runners: [{ ...scoped, routing: undefined }] }} projects={PROJECTS} onSaveRouting={vi.fn()} />);
    expect(within(sheet).getByText("Project access is not reported.")).toBeTruthy();
    expect(sheet.textContent).not.toContain("Known project maximum slots:");
    expect(sheet.textContent).not.toContain("Second project maximum slots:");
    rerender(<RunnersSectionView fleet={{ ...FLEET, runners: [scoped] }} projects={PROJECTS} onSaveRouting={vi.fn()} />);
    expect(within(sheet).getByText("Known project maximum slots: Inherited total")).toBeTruthy();
    expect(within(sheet).getByText("Second project maximum slots: 0")).toBeTruthy();
    rerender(<RunnersSectionView fleet={{ ...FLEET, runners: [{ ...scoped, revision: 8, routing: { ...scoped.routing, capacity_limit: 8, project_capacity_limits: { prj_known: 4 } } }] }} projects={PROJECTS} onSaveRouting={vi.fn()} />);
    expect(within(sheet).getByText("Total runner slots: 8")).toBeTruthy();
    expect(within(sheet).getByText("Known project maximum slots: 4")).toBeTruthy();
    expect(within(sheet).getByText("Second project maximum slots: Inherited total")).toBeTruthy();
    const save = vi.fn().mockResolvedValue(undefined);
    const writable = { ...scoped, editable: true, revision: 9, routing: { ...scoped.routing, capacity_limit: 8, project_capacity_limits: { prj_known: 4 } } };
    rerender(<RunnersSectionView fleet={{ ...FLEET, runners: [{ ...writable, routing: undefined }] }} projects={PROJECTS} onSaveRouting={save} />);
    expect(within(sheet).getByText("Project access is not reported.")).toBeTruthy();
    rerender(<RunnersSectionView fleet={{ ...FLEET, runners: [writable] }} projects={PROJECTS} onSaveRouting={save} />);
    expect((screen.getByLabelText("Total runner slots") as HTMLInputElement).value).toBe("8");
    expect((screen.getByLabelText("Known project maximum slots") as HTMLInputElement).value).toBe("4");
    fireEvent.click(screen.getByRole("button", { name: "Save runner" }));
    await waitFor(() => expect(save).toHaveBeenCalledWith(writable, writable.routing));
  });

  it.each([null, 3])("never treats a requested increase as effective with evidence %s", (effective) => {
    const capacity = { ...capacityFixture, desired: 8, effective, status: "partially_applied", limits: [...capacityFixture.limits, { owner: "provider:codex", limit: 3, constraint: "Unmanaged provider ceiling" }] };
    render(<RunnersSectionView fleet={decodeFleet({ ...FLEET, runners: [{ ...runner, capacity_configuration: capacity }] })} projects={PROJECTS} />);
    const row = within(screen.getByTestId("host-card"));
    expect(row.getByText("Requested total: 8 slots")).toBeTruthy();
    expect(row.getByText(new RegExp(`Effective total: ${effective ?? "unknown"}.*Pending or constrained`))).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Manage Example laptop" }));
    expect(within(screen.getByRole("article")).getByText(/Unmanaged provider ceiling/)).toBeTruthy();
    expect(row.queryByText("Effective total: 8 slots")).toBeNull();
  });

  it("sends maxima and total to routing with revision and idempotency, preserving unrelated settings", async () => {
    const fetch = vi.fn(async () => new Response("{}", { status: 200 }));
    const api = makeAccountApi({ origin: "https://hub.test", apiBase: "/api/v2/organizations/org_test", csrfToken: "test", fetch });
    await api.setRunnerRouting({ runner: runner.id, revision: 7, displayName: runner.display_name, tags: routing.tags, state: "draining", capacityLimit: 8, projectIds: routing.project_ids, projectCapacityLimits: { prj_known: 4, prj_second: 0 }, projectRanks: routing.project_ranks, availability: routing.availability, isolationTier: routing.isolation_tier, hostServices: routing.host_services });
    expect(fetch).toHaveBeenCalledOnce();
    const [url, request] = fetch.mock.calls[0]! as unknown as [string, RequestInit];
    expect(url).toBe(`https://hub.test/api/v2/organizations/org_test/runners/${runner.id}/routing`);
    expect(request.method).toBe("PUT");
    expect(JSON.parse(request.body as string)).toEqual({ expected_revision: 7, display_name: runner.display_name, tags: routing.tags, state: "draining", capacity_limit: 8, project_ids: routing.project_ids, project_capacity_limits: { prj_known: 4, prj_second: 0 }, project_ranks: routing.project_ranks, availability: routing.availability, isolation_tier: routing.isolation_tier, host_services: routing.host_services, idempotency_key: expect.any(String) });
  });
});
