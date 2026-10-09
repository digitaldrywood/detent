// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, renderHook, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { RunnersSectionView } from "../../src/app/fleet/RunnersSection.tsx";
import { spriteBootstrapPin } from "../../src/app/fleet/EnrollRunner.tsx";
import type { FleetResponse, RunnerRouting } from "../../src/contracts/account.ts";
import { AccountError, makeAccountApi } from "../../src/app/account/api.ts";
import { ClientContext } from "../../src/app/client.ts";
import { resetRunnerNamesForTests, runnerDisplay, useRunnerNames } from "../../src/app/work/lib/runnerNames.ts";
import { parseRunnerWindow, serializeRunnerWindow } from "../../src/app/fleet/runnerSchedule.ts";
import emptyFixture from "../../src/contracts/fixtures/account-fleet-empty.json";
import fleetFixture from "../../src/contracts/fixtures/account-fleet.json";
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
const RUNNER = { can_edit_projects: true, ...FLEET.runners[0]!, claim_refusal_reason: "", routing: ROUTING, revision: 7 };
const PROJECTS = [{ id: "prj_known", name: "Known project" }, { id: "prj_second", name: "Second project" }];

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
    ["draining", "Waiting for active work"],
    ["refused", "Update failed"],
    ["restart_requested", "Restart requested"],
    ["running", "Updated"],
  ])("shows the enrolled update outcome %s", (status, label) => {
    renderSection({ ...FLEET, runners: [{ ...FLEET.runners[0]!, update: { status, desired: { version: "1.2.4" } } }] });
    expect(screen.getByText(`${label} · 1.2.4`)).toBeTruthy();
    if (status === "refused") {
      expect(screen.getByText(/Needs human: reinstall/)).toBeTruthy();
      expect(screen.getByText("curl -fsSL https://raw.githubusercontent.com/digitaldrywood/detent/main/install.sh | sh")).toBeTruthy();
    }
  });
  it.each(["0.117.47", "0.117.50"])("explains the one-time reinstall for %s", (version) => {
    renderSection({ ...FLEET, runners: [{ ...FLEET.runners[0]!, version, claim_refusal_reason: "Too old to take work, needs 0.117.51", update: { status: "unavailable", desired: null } }] });
    expect(screen.getByText(/One-time manual reinstall required/)).toBeTruthy();
    expect(screen.getByText("curl -fsSL https://raw.githubusercontent.com/digitaldrywood/detent/main/install.sh | sh")).toBeTruthy();
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
    fireEvent.click(screen.getByRole("button", { name: `Remove runner ${FLEET.runners[0]!.display_name}` }));
    expect(remove).not.toHaveBeenCalled();
    fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Cancel" }));
    expect(remove).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("button", { name: `Remove runner ${FLEET.runners[0]!.display_name}` }));
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
      expect(warning?.textContent).toContain(reason);
      expect(screen.getAllByText(reason)).toHaveLength(1);
    } else {
      expect(warning).toBeNull();
    }
  });

  it("shows contextual problems and filters through the segmented control", () => {
    const problem = { code: "tier_unavailable", message: "Sandbox tooling is unavailable", fix_hint: "Repair the sandbox tooling", first_seen: "2026-09-10T12:00:00Z" };
    renderSection({ ...FLEET, runners: FLEET.runners.map((runner, index) => index === 0 ? { ...runner, health: "needs_attention", problems: [problem] } : runner) });
    expect(screen.getByRole("link", { name: "Needs attention 1" })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: `Manage ${FLEET.runners[0]!.display_name}` }));
    expect(screen.getByText(problem.message)).toBeTruthy();
    expect(screen.getByText(problem.fix_hint)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Close" }));
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
    expect(within(rows[0]!).getByRole("heading", { name: "Michael's MacBook Pro" })).toBeTruthy();
    expect(rows[0]!.textContent).toContain("1 of 2 slots in use");
    expect(rows[1]!.textContent).toContain("0 of 2 slots in use");
    expect(rows[1]!.textContent).toContain("Can’t take work");
    expect(screen.getByText("1 of 4 slots running work")).toBeTruthy();
    expect(screen.queryAllByRole("heading", { level: 1 })).toHaveLength(0);
  });
});

describe("shared host capacity", () => {
  const active = { ...FLEET.runners[0]!, machine_id: "machine_shared", state: "active", health: "online", claim_refusal_reason: "", provider_capacity: [], host_capacity: 8, host_used: 1, capacity_limit: 8, reported_capacity: 8 };
  const lease = active.leases[0]!;

  it("counts a shared eight-slot host once and retains each identity's work", () => {
    renderSection({ ...FLEET, runners: [
      { ...active, capacity_limit: 4, reported_capacity: 4, host_used: 3 },
      { ...active, id: "runner_sibling", display_name: "Sibling", capacity_limit: 4, reported_capacity: 4, host_used: 3, leases: [
        { ...lease, lease_id: "lease_second", work_item_id: "wi_second" },
        { ...lease, lease_id: "lease_third", work_item_id: "wi_third" },
      ] },
    ] });
    expect(screen.getByText("3 of 8 slots running work")).toBeTruthy();
    const host = screen.getByTestId("runner-capacity");
    expect(host.querySelectorAll("[data-slot-state]")).toHaveLength(8);
    expect(host.querySelectorAll('[data-slot-state="running"]')).toHaveLength(3);
    expect(host.querySelectorAll('[data-slot-state="free"]')).toHaveLength(5);
    expect(host.querySelector('[title="#wi_second connections: custom alerts from segment membership"]')).toBeTruthy();
    expect(screen.getAllByTestId("host-card")).toHaveLength(2);
  });

  it.each([
    ["draining", { state: "draining" }, 0],
    ["disabled", { state: "disabled" }, 0],
    ["offline", { health: "offline" }, 0],
    ["outside hours", { health: "outside_hours" }, 0],
    ["needs attention", { health: "needs_attention" }, 0],
    ["claim refused", { claim_refusal_reason: "Too old to take work" }, 0],
    ["reported limit", { reported_capacity: 3 }, 2],
    ["routing limit", { capacity_limit: 2 }, 1],
  ])("preserves running work and limits free slots for %s", (_name, values, free) => {
    renderSection({ ...FLEET, runners: [{ ...active, ...values }] });
    const host = screen.getByTestId("runner-capacity");
    expect(screen.getByText("1 of 8 slots running work")).toBeTruthy();
    expect(host.querySelectorAll('[data-slot-state="running"]')).toHaveLength(1);
    expect(host.querySelectorAll('[data-slot-state="free"]')).toHaveLength(free);
    expect(host.querySelectorAll('[data-slot-state="unavailable"]')).toHaveLength(7 - free);
  });

  it("uses only an active sibling's remaining allowance while another drains", () => {
    renderSection({ ...FLEET, runners: [
      { ...active, state: "draining", host_used: 2 },
      { ...active, id: "runner_sibling", host_used: 2, capacity_limit: 2, reported_capacity: 4, leases: [{ ...lease, lease_id: "lease_second" }] },
    ] });
    const host = screen.getByTestId("runner-capacity");
    expect(host.querySelectorAll('[data-slot-state="running"]')).toHaveLength(2);
    expect(host.querySelectorAll('[data-slot-state="free"]')).toHaveLength(1);
    expect(host.querySelectorAll('[data-slot-state="unavailable"]')).toHaveLength(5);
  });

  it("keeps distinct machine identities separate despite identical names", () => {
    renderSection({ ...FLEET, runners: [active, { ...active, id: "runner_other", machine_id: "machine_other" }] });
    expect(screen.getAllByTestId("runner-capacity")).toHaveLength(2);
    expect(screen.getByText("2 of 16 slots running work")).toBeTruthy();
  });
});

describe("providers", () => {
  it("folds every runner's capacity onto one table row per provider", () => {
    renderSection();
    const providers = screen.getByRole("region", { name: "Provider accounts" });
    expect(providers.querySelectorAll("tbody tr")).toHaveLength(2);
    expect(within(providers).getByText("codex")).toBeTruthy();
    expect(within(providers).getByText("claude")).toBeTruthy();
    expect(within(providers).getByText(/gpt-6-astra/)).toBeTruthy();
  });

  it("invites enrollment and omits capacity when the fleet is empty", () => {
    renderSection(EMPTY);

    expect(screen.getByText("No runners yet. Add a runner manually or let Luna help you set one up.")).toBeTruthy();
    expect(screen.queryAllByTestId("host-card")).toHaveLength(0);
    expect(screen.queryByRole("heading", { name: "Capacity right now" })).toBeNull();
  });
});

describe("runner details", () => {
  it("round-trips allowed project IDs, including unreadable projects", async () => {
    const save = vi.fn().mockResolvedValue(undefined);
    render(<RunnersSectionView fleet={{ ...FLEET, editable: true, runners: [RUNNER] }} projects={PROJECTS} onSaveRouting={save} />);
    fireEvent.click(screen.getByRole("button", { name: "Manage Michael's MacBook Pro" }));
    const sheet = screen.getByRole("dialog");
    expect((within(sheet).getByRole("checkbox", { name: "Known project" }) as HTMLInputElement).checked).toBe(true);
    expect((within(sheet).getByRole("checkbox", { name: "prj_unknown" }) as HTMLInputElement).checked).toBe(true);
    fireEvent.click(within(sheet).getByRole("button", { name: "Save runner" }));
    await waitFor(() => expect(save).toHaveBeenCalledWith(RUNNER, ROUTING));
  });

  it("edits allowed projects and lets unknown projects be reselected", async () => {
    const save = vi.fn().mockResolvedValue(undefined);
    render(<RunnersSectionView fleet={{ ...FLEET, editable: true, runners: [RUNNER] }} projects={PROJECTS} onSaveRouting={save} />);
    fireEvent.click(screen.getByRole("button", { name: "Manage Michael's MacBook Pro" }));
    const sheet = screen.getByRole("dialog");
    expect((within(sheet).getByRole("checkbox", { name: "Second project" }) as HTMLInputElement).checked).toBe(false);
    fireEvent.click(within(sheet).getByRole("checkbox", { name: "Second project" }));
    fireEvent.click(within(sheet).getByRole("checkbox", { name: "prj_unknown" }));
    fireEvent.click(within(sheet).getByRole("checkbox", { name: "prj_unknown" }));
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
    fireEvent.click(screen.getByRole("button", { name: "Manage Michael's MacBook Pro" }));
    fireEvent.change(screen.getByLabelText("Until 1"), { target: { value: "18:00" } });
    fireEvent.change(screen.getByLabelText("Add tag"), { target: { value: "gpu,linux" } });
    fireEvent.click(screen.getByRole("button", { name: "Save runner" }));
    await waitFor(() => expect(save).toHaveBeenCalledWith(RUNNER, expect.objectContaining({
      tags: ["linux", "gpu"], availability: { ...ROUTING.availability, windows: ["Mon-Fri 09:00-18:00", "Sat-Sun 00:00-24:00"] },
    })));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    rerender(<RunnersSectionView fleet={{ ...FLEET, editable: true, runners: [RUNNER] }} projects={PROJECTS} onSaveRouting={save} />);
    fireEvent.click(screen.getByRole("button", { name: "Manage Michael's MacBook Pro" }));
    fireEvent.change(screen.getByLabelText("Availability"), { target: { value: "always" } });
    fireEvent.click(screen.getByRole("button", { name: "Save runner" }));
    await waitFor(() => expect(save).toHaveBeenLastCalledWith(RUNNER, expect.objectContaining({
      availability: { ...ROUTING.availability, windows: [], hard_deadline: "" }
    })));
  });

  it.each([false, true])("renders the same sections without controls when editing is unavailable (fleet editable: %s)", (editable) => {
    render(<RunnersSectionView fleet={{ ...FLEET, editable, runners: [RUNNER] }} projects={PROJECTS} {...(!editable ? { onSaveRouting: vi.fn() } : {})} />);
    fireEvent.click(screen.getByRole("button", { name: "Manage Michael's MacBook Pro" }));
    const sheet = screen.getByRole("dialog");
    expect(sheet.querySelectorAll("input, select, textarea")).toHaveLength(0);
    expect(sheet.querySelector('[data-slot="sheet-footer"]')).toBeNull();
    for (const name of ["Taking work", "Capacity", "Allowed projects", "Tags", "Schedule", "Agent access", "Running work", "Provider accounts"]) {
      expect(within(sheet).getByRole("heading", { name })).toBeTruthy();
    }
    expect(sheet.textContent).toContain("Known project");
    expect(sheet.textContent).toContain("prj_unknown");
    expect(sheet.textContent).toContain("Sat-Sun 00:00-24:00");
  });

  it("shows project checkout failures and the authentication command on the runner sheet", () => {
    const runner = { ...RUNNER, project_checkouts: {
      prj_known: { status: "missing", message: "Cannot clone https://github.com/acme/orders.git", fix_command: "gh auth login --hostname github.com --git-protocol https" },
      prj_unknown: { status: "ready" },
    } };
    render(<RunnersSectionView fleet={{ ...FLEET, runners: [runner] }} projects={PROJECTS} />);
    fireEvent.click(screen.getByRole("button", { name: "Manage Michael's MacBook Pro" }));
    const sheet = screen.getByRole("dialog");
    expect(within(sheet).getByText("Checkout unavailable")).toBeTruthy();
    expect(within(sheet).getByText("Cannot clone https://github.com/acme/orders.git")).toBeTruthy();
    expect(within(sheet).getByText("gh auth login --hostname github.com --git-protocol https")).toBeTruthy();
    expect(within(sheet).getByText("Checkout ready")).toBeTruthy();
    expect(within(sheet).getByText("The runner retries automatically after repository access is fixed.")).toBeTruthy();
  });

  it("does not invent settings when the fleet omits routing for a viewer", () => {
    render(<RunnersSectionView fleet={{ ...FLEET, editable: false, runners: [{ ...FLEET.runners[0]!, availability: ROUTING.availability }] }} projects={PROJECTS} />);
    fireEvent.click(screen.getByRole("button", { name: "Manage Michael's MacBook Pro" }));
    const sheet = screen.getByRole("dialog");
    expect(sheet.querySelectorAll("input, select, textarea")).toHaveLength(0);
    expect(sheet.textContent).toContain("Project access is not reported.");
    expect(sheet.textContent).toContain("Tags are not reported.");
    expect(sheet.textContent).toContain("Host services are not reported.");
    expect(sheet.textContent).toContain("Mon-Fri 09:00-17:00");
  });

  it("shows save errors and reloads the routing revision after a conflict", async () => {
    const save = vi.fn()
      .mockRejectedValueOnce(new AccountError({ status: 400, code: "invalid", message: "Invalid runner hours" }))
      .mockRejectedValueOnce(new AccountError({ status: 409, code: "conflict", message: "revision conflict" }))
      .mockResolvedValue(undefined);
    const fresh = { ...RUNNER, revision: 8, routing: { ...ROUTING, state: "draining", capacity_limit: 3 } };
    const reload = vi.fn(async () => {
      rerender(<RunnersSectionView fleet={{ ...FLEET, editable: true, runners: [fresh] }} projects={PROJECTS} onSaveRouting={save} onReloadRunner={reload} />);
    });
    const { rerender } = render(<RunnersSectionView fleet={{ ...FLEET, editable: true, runners: [RUNNER] }} projects={PROJECTS} onSaveRouting={save} onReloadRunner={reload} />);
    fireEvent.click(screen.getByRole("button", { name: "Manage Michael's MacBook Pro" }));
    fireEvent.change(screen.getByLabelText("Jobs at once"), { target: { value: "10" } });
    fireEvent.click(screen.getByRole("button", { name: "Save runner" }));
    const saveError = await screen.findByRole("alert");
    expect(saveError.textContent).toContain("Invalid runner hours");
    expect(saveError.closest('[data-slot="sheet-footer"]')).not.toBeNull();
    expect((screen.getByLabelText("Jobs at once") as HTMLInputElement).value).toBe("10");
    fireEvent.click(screen.getByRole("button", { name: "Save runner" }));
    await waitFor(() => expect(screen.getByRole("alert").textContent).toContain("settings have been reloaded"));
    expect(reload).toHaveBeenCalledOnce();
    expect((screen.getByLabelText("Jobs at once") as HTMLInputElement).value).toBe("3");
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
    fireEvent.click(screen.getByRole("button", { name: "Manage Michael's MacBook Pro" }));
    const sheet = screen.getByRole("dialog");
    expect(within(sheet).getAllByRole("alert")).toHaveLength(2);
    expect(sheet.textContent).toContain("Seen since");
    fireEvent.click(within(sheet).getByRole("button", { name: "Cancel" }));
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
