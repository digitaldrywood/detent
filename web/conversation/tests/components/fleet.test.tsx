// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { RunnersSectionView } from "../../src/app/fleet/RunnersSection.tsx";
import type { FleetResponse, RunnerRouting } from "../../src/contracts/account.ts";
import { AccountError } from "../../src/app/account/api.ts";
import { parseRunnerWindow, serializeRunnerWindow } from "../../src/app/fleet/runnerSchedule.ts";
import emptyFixture from "../../src/contracts/fixtures/account-fleet-empty.json";
import fleetFixture from "../../src/contracts/fixtures/account-fleet.json";

afterEach(() => { cleanup(); window.history.replaceState(null, "", "/"); });

const FLEET = fleetFixture as unknown as FleetResponse;
const EMPTY = emptyFixture as unknown as FleetResponse;
const NOW = Date.parse("2026-09-10T12:09:31Z");
const ROUTING: RunnerRouting = {
  display_name: FLEET.runners[0]!.display_name, state: "active", capacity_limit: 6,
  project_ids: ["prj_known", "prj_unknown"], home_project_ids: ["prj_unknown"], tags: ["linux"],
  isolation_tier: "sandbox", host_services: ["tcp:127.0.0.1:8080"],
  availability: { timezone: "America/Chicago", windows: ["Mon-Fri 09:00-17:00", "Sat-Sun 00:00-24:00"], hard_deadline: "30m" },
  spillover: { mode: "after", after_minutes: 5 },
};
const RUNNER = { ...FLEET.runners[0]!, routing: ROUTING, revision: 7 };
const PROJECTS = [{ id: "prj_known", name: "Known project" }, { id: "prj_second", name: "Second project" }];

function renderSection(fleet: FleetResponse = FLEET) {
  render(<RunnersSectionView fleet={fleet} now={NOW} />);
}

describe("runner rows", () => {
  it.each([
    ["v1.2.3", "Too old to take work, needs v1.2.4"],
    ["v1.2.4", ""],
    ["v1.3.0", ""],
    ["dev", ""],
  ])("renders the Hub admission refusal for %s", (version, reason) => {
    renderSection({ ...FLEET, current: "v1.2.4", minimum_runner_version: "v1.2.4", runners: [{ ...FLEET.runners[0]!, version, claim_refusal_reason: reason }] });
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
    expect(screen.getByText("Needs attention")).toBeTruthy();
    expect(screen.getByText(problem.message)).toBeTruthy();
    expect(screen.getByText(problem.fix_hint)).toBeTruthy();
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
    expect(rows[1]!.textContent).toContain("stale");
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
    expect(screen.getByText("No provider capacity reported")).toBeTruthy();
    expect(screen.getByText("No runners yet. Enroll a machine to start taking work.")).toBeTruthy();
    expect(screen.queryAllByTestId("host-card")).toHaveLength(0);
    expect(screen.queryByRole("heading", { name: "Capacity right now" })).toBeNull();
  });
});

describe("runner details", () => {
  it("round-trips project and home IDs, including unreadable projects", async () => {
    const save = vi.fn().mockResolvedValue(undefined);
    render(<RunnersSectionView fleet={{ ...FLEET, editable: true, runners: [RUNNER] }} projects={PROJECTS} onSaveRouting={save} />);
    fireEvent.click(screen.getByRole("button", { name: "Manage Michael's MacBook Pro" }));
    const sheet = screen.getByRole("dialog");
    expect((within(sheet).getByRole("checkbox", { name: "Known project" }) as HTMLInputElement).checked).toBe(true);
    expect((within(sheet).getByRole("checkbox", { name: "prj_unknown" }) as HTMLInputElement).checked).toBe(true);
    expect((within(sheet).getByRole("checkbox", { name: "Home prj_unknown" }) as HTMLInputElement).checked).toBe(true);
    fireEvent.click(within(sheet).getByRole("button", { name: "Save runner" }));
    await waitFor(() => expect(save).toHaveBeenCalledWith(RUNNER, ROUTING));
  });

  it("keeps home projects within selected access and lets unknown projects be reselected", async () => {
    const save = vi.fn().mockResolvedValue(undefined);
    render(<RunnersSectionView fleet={{ ...FLEET, editable: true, runners: [RUNNER] }} projects={PROJECTS} onSaveRouting={save} />);
    fireEvent.click(screen.getByRole("button", { name: "Manage Michael's MacBook Pro" }));
    const sheet = screen.getByRole("dialog");
    fireEvent.click(within(sheet).getByRole("checkbox", { name: "Home Second project" }));
    expect((within(sheet).getByRole("checkbox", { name: "Second project" }) as HTMLInputElement).checked).toBe(true);
    fireEvent.click(within(sheet).getByRole("checkbox", { name: "prj_unknown" }));
    expect((within(sheet).getByRole("checkbox", { name: "Home prj_unknown" }) as HTMLInputElement).checked).toBe(false);
    fireEvent.click(within(sheet).getByRole("checkbox", { name: "prj_unknown" }));
    fireEvent.click(within(sheet).getByRole("button", { name: "Save runner" }));
    await waitFor(() => expect(save).toHaveBeenCalledWith(RUNNER, expect.objectContaining({
      project_ids: ["prj_known", "prj_second", "prj_unknown"], home_project_ids: ["prj_second"],
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
    fireEvent.change(screen.getByLabelText("Queued work spillover"), { target: { value: "never" } });
    fireEvent.click(screen.getByRole("button", { name: "Save runner" }));
    await waitFor(() => expect(save).toHaveBeenLastCalledWith(RUNNER, expect.objectContaining({
      availability: { ...ROUTING.availability, windows: [], hard_deadline: "" }, spillover: { mode: "never", after_minutes: 0 },
    })));
  });

  it.each([false, true])("renders the same sections without controls when editing is unavailable (fleet editable: %s)", (editable) => {
    render(<RunnersSectionView fleet={{ ...FLEET, editable, runners: [RUNNER] }} projects={PROJECTS} {...(!editable ? { onSaveRouting: vi.fn() } : {})} />);
    fireEvent.click(screen.getByRole("button", { name: "Manage Michael's MacBook Pro" }));
    const sheet = screen.getByRole("dialog");
    expect(sheet.querySelectorAll("input, select, textarea")).toHaveLength(0);
    expect(sheet.querySelector('[data-slot="sheet-footer"]')).toBeNull();
    for (const name of ["Taking work", "Capacity", "Projects", "Tags", "Schedule", "Isolation", "Running work", "Provider accounts"]) {
      expect(within(sheet).getByRole("heading", { name })).toBeTruthy();
    }
    expect(sheet.textContent).toContain("Known project");
    expect(sheet.textContent).toContain("prj_unknown");
    expect(sheet.textContent).toContain("Sat-Sun 00:00-24:00");
  });

  it("does not invent settings when the fleet omits routing for a viewer", () => {
    render(<RunnersSectionView fleet={{ ...FLEET, editable: false, runners: [{ ...FLEET.runners[0]!, availability: ROUTING.availability }] }} projects={PROJECTS} />);
    fireEvent.click(screen.getByRole("button", { name: "Manage Michael's MacBook Pro" }));
    const sheet = screen.getByRole("dialog");
    expect(sheet.querySelectorAll("input, select, textarea")).toHaveLength(0);
    expect(sheet.textContent).toContain("Project access is not reported.");
    expect(sheet.textContent).toContain("Tags are not reported.");
    expect(sheet.textContent).toContain("Host services are not reported.");
    expect(sheet.textContent).toContain("Queued work spillover is not reported.");
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
    fireEvent.click(within(screen.getByTestId("runner-attention")).getByRole("button", { name: "Open runner Michael's MacBook Pro" }));
    const sheet = screen.getByRole("dialog");
    expect(within(sheet).getAllByRole("alert")).toHaveLength(2);
    expect(sheet.textContent).toContain("Seen since");
    fireEvent.click(within(sheet).getByRole("button", { name: "Cancel" }));
    expect(save).not.toHaveBeenCalled();
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it.each(["Spilled over", "Waiting for home work (5m)"])("keeps %s out of the list and in Manage", (status) => {
    renderSection({ ...FLEET, runners: [{ ...FLEET.runners[0]!, home_project_ids: ["prj_home"], home_status: status }] });
    const row = screen.getByTestId("host-card");
    expect(within(row).queryByText("Home projects: prj_home")).toBeNull();
    fireEvent.click(within(row).getByRole("button", { name: "Manage Michael's MacBook Pro" }));
    const details = screen.getByRole("dialog");
    expect(within(details).getByText("Home projects: prj_home")).toBeTruthy();
    expect(within(details).getByText(status)).toBeTruthy();
  });

  it("shows outside hours as a plain runner status", () => {
    renderSection({ ...FLEET, runners: [{ ...FLEET.runners[0]!, health: "outside_hours", claim_refusal_reason: "" }] });
    const row = screen.getByTestId("host-card");
    expect(within(row).getByText("Outside hours")).toBeTruthy();
    expect(screen.queryByRole("alert")).toBeNull();
  });
});
