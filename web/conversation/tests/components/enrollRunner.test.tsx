// @vitest-environment jsdom
//
// The Enroll dialog asks for a name, a capacity and projects, creates a
// token-first enrollment and shows the one command a host runs. Nobody types a
// runner or machine ID.
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createMemoryHistory, createRootRoute, createRouter, RouterProvider } from "@tanstack/react-router";
import { afterEach, describe, expect, it, vi } from "vitest";

import { ClientContext } from "../../src/app/client.ts";
import { EnrollRunnerDialog, type PendingEnrollment } from "../../src/app/fleet/EnrollRunner.tsx";
import { RunnersSettings } from "../../src/app/fleet/RunnersSection.tsx";
import type { ConversationClient } from "../../src/runtime/bootstrap.ts";
import type { FleetRunner } from "../../src/contracts/account.ts";
import fleetFixture from "../../src/contracts/fixtures/account-fleet.json";

let client: ConversationClient | undefined;

afterEach(() => {
  cleanup();
  client?.handles.clear();
  client = undefined;
  vi.useRealTimers();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

function projectBox(id: string): HTMLElement {
  const box = document.getElementById(`enroll-project-${id}`);
  if (box === null) throw new Error(`no checkbox for ${id}`);
  return box;
}

async function mountDialog(projectIds?: readonly string[], settings = false, initialRunners: readonly FleetRunner[] = []) {
  let runners = initialRunners;
  const fleetReads = vi.fn();
  const enrollmentRequests = vi.fn();
  vi.stubGlobal("fetch", vi.fn(async (input: string, init?: RequestInit) => {
    if (String(input).endsWith("/runner-enrollments")) {
      enrollmentRequests(JSON.parse(String(init?.body)));
      return new Response(JSON.stringify({ id: "enrollment_build", token: "det_enroll_secret", expires_at: "2026-10-02T20:00:00Z" }), { status: 201 });
    }
    fleetReads();
    return new Response(JSON.stringify({ runners, usage: { window_ends_at: "", allowances: {} }, spend: null }), { status: 200 });
  }));
  client = {
    account: {
      organization: { id: "org_build" },
      organizations: [{ current: true, public_url: "https://hub.example.test" }],
      actor: { can_manage_runners: true },
      projects: [
        { id: "project_build", name: "Build", can_manage_runners: true },
        { id: "project_release", name: "Release", can_manage_runners: true },
      ],
    },
    http: { origin: "", apiBase: "/api/v2/organizations/org_build", csrfToken: "csrf" },
    handles: new Map(),
  } as unknown as ConversationClient;
  const onEnrolled = vi.fn<(entry: PendingEnrollment) => void>();
  const router = createRouter({
    routeTree: createRootRoute({ component: RunnersSettings }),
    history: createMemoryHistory({ initialEntries: ["/"] }),
  });
  render(
    <ClientContext.Provider value={client}>
      {settings ? (
        <RouterProvider router={router as never} />
      ) : (
        <EnrollRunnerDialog open onOpenChange={() => undefined} onEnrolled={onEnrolled} fleet={{ value: undefined, refresh: async () => undefined }} {...(projectIds === undefined ? {} : { projectIds })} />
      )}
    </ClientContext.Provider>,
  );
  return { onEnrolled, enrollmentRequests, projects: client.account?.projects ?? [], fleetReads, setRunners: (next: readonly FleetRunner[]) => { runners = next; } };
}

describe("the Enroll dialog", () => {
  it("asks for no host IDs and shows one register command", async () => {
    userEvent.setup();
    const writeText = vi.spyOn(navigator.clipboard, "writeText").mockResolvedValue();
    const { onEnrolled, enrollmentRequests, projects } = await mountDialog();
    expect(projects.length).toBeGreaterThan(1);
    expect((screen.getByLabelText("Concurrency") as HTMLInputElement).value).toBe("1");
    expect(screen.getByText(/each with its own workspace and agent/)).toBeDefined();
    expect(screen.queryByLabelText("Runner id")).toBeNull();
    expect(screen.queryByLabelText("Machine id")).toBeNull();

    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Build host" } });
    fireEvent.change(screen.getByLabelText("Concurrency"), { target: { value: "2" } });
    fireEvent.click(projectBox(projects[1]!.id));
    fireEvent.click(screen.getByRole("button", { name: "Create command" }));

    const copy = await screen.findByRole("button", { name: "Copy the register command" });
    const code = copy.parentElement!.querySelector("code")!;
    expect(code.textContent).toContain("detent_••••••••");
    expect(document.body.innerHTML).not.toContain("det_enroll_");
    fireEvent.click(screen.getByRole("button", { name: "Show token" }));
    const command = code.textContent ?? "";
    expect(command).toMatch(/^detent hub runner register --url \S+ (--organization org_\w+ )?--token det_enroll_\w+ --name 'Build host' --capacity 2 --service$/);
    expect(command).not.toMatch(/runner_|machine_|init|enroll --organization/);
    fireEvent.click(screen.getByRole("button", { name: "Hide token" }));
    expect(code.textContent).toContain("detent_••••••••");
    fireEvent.click(copy);
    expect(writeText).toHaveBeenCalledWith(command);
    expect(code.textContent).not.toContain(command.match(/--token (\S+)/)![1]!);

    const sent = enrollmentRequests.mock.calls[0]![0];
    expect(sent.runner_id).toBeUndefined();
    expect(sent.machine_id).toBeUndefined();
    expect(sent?.project_ids).toEqual(projects.filter((_, index) => index !== 1).map((project) => project.id));
    await waitFor(() => expect(onEnrolled).toHaveBeenCalledTimes(1));
    expect(onEnrolled.mock.calls[0]![0]).toEqual({
      id: expect.any(String),
      expiresAt: expect.any(String),
      name: "Build host",
    });
  });

  it.each(["footer", "Escape", "Close"])("discards the token and command from the settings page after %s", async (close) => {
    const user = userEvent.setup();
    await mountDialog(undefined, true);
    await user.click(await screen.findByRole("button", { name: "Enroll a runner" }));
    await user.type(screen.getByLabelText("Name"), "Build host");
    await user.click(screen.getByRole("button", { name: "Create command" }));
    const copy = await screen.findByRole("button", { name: "Copy the register command" });
    await user.click(screen.getByRole("button", { name: "Show token" }));
    const command = copy.parentElement!.querySelector("code")!.textContent!;
    const token = command.match(/--token (\S+)/)![1]!;
    expect(document.body.innerHTML).toContain(token);

    if (close === "Escape") await user.keyboard("{Escape}");
    else await user.click(screen.getAllByRole("button", { name: "Close" })[close === "footer" ? 0 : 1]!);

    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(document.body.innerHTML).not.toContain(token);
    expect(document.body.innerHTML).not.toContain("detent hub runner register");
    expect(screen.getByRole("heading", { name: "Waiting to connect" })).toBeDefined();
    expect(screen.getByRole("heading", { name: "Build host" })).toBeDefined();
    expect(screen.getByText(/^No check-in yet · expires /)).toBeDefined();

    await user.click(screen.getByRole("button", { name: "Enroll a runner" }));
    expect(screen.getByLabelText("Name")).toBeDefined();
    expect(document.body.innerHTML).not.toContain(token);
    expect(document.body.innerHTML).not.toContain("detent hub runner register");
  });

  it.each([
    ["connected", " Build host "],
    ["connected", ""],
    ["closed", "Build host"],
    ["unmounted", "Build host"],
  ])("stops fleet refreshes when %s (name: %j)", async (finish, name) => {
    const displayName = name!.trim() || "Unnamed runner";
    const existing = { ...fleetFixture.runners[0]!, display_name: displayName } as FleetRunner;
    const { setRunners, fleetReads } = await mountDialog(undefined, true, [existing]);
    fireEvent.click(await screen.findByRole("button", { name: "Enroll a runner" }));
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: name } });
    vi.useFakeTimers();
    await act(async () => fireEvent.click(screen.getByRole("button", { name: "Create command" })));
    const dialog = within(screen.getByRole("dialog"));
    expect(dialog.getByRole("list").querySelector('[aria-current="step"]')?.textContent).toContain("Run the command");
    expect(dialog.getByRole("status").textContent).toContain(`Waiting for ${displayName} to check in`);
    await act(() => vi.advanceTimersByTimeAsync(2_000));
    expect(dialog.queryByText(`${displayName} is connected`)).toBeNull();

    if (finish === "connected") {
      setRunners([existing, { ...existing, id: "rnr_new" }]);
      const before = fleetReads.mock.calls.length;
      await act(() => vi.advanceTimersByTimeAsync(2_000));
      expect(fleetReads).toHaveBeenCalledTimes(before + 1);
      expect(dialog.getByText(`${displayName} is connected`)).toBeTruthy();
      expect(dialog.getByText(existing.hostname)).toBeTruthy();
      expect(dialog.getByText(`${existing.os} / ${existing.architecture}`)).toBeTruthy();
      expect(dialog.getByText(String(existing.reported_capacity))).toBeTruthy();
      expect(dialog.getByText("codex, claude")).toBeTruthy();
      expect(dialog.getByRole("button", { name: "Done" })).toBeTruthy();
      expect(dialog.queryByRole("button", { name: "Copy the register command" })).toBeNull();
      expect(screen.queryByRole("heading", { name: "Waiting to connect" })).toBeNull();
    } else if (finish === "closed") {
      await act(async () => fireEvent.click(dialog.getAllByRole("button", { name: "Close" })[0]!));
    } else {
      cleanup();
    }
    const stoppedAt = fleetReads.mock.calls.length;
    await act(() => vi.advanceTimersByTimeAsync(10_000));
    expect(fleetReads).toHaveBeenCalledTimes(stoppedAt);
  });

  it("preselects only the projects it was given and can skip the service", async () => {
    const seeded = await mountDialog();
    cleanup();
    const only = seeded.projects[0]!.id;
    const { enrollmentRequests } = await mountDialog([only]);
    fireEvent.click(document.getElementById("enroll-runner-service")!);
    fireEvent.click(screen.getByRole("button", { name: "Create command" }));
    const copy = await screen.findByRole("button", { name: "Copy the register command" });
    const command = copy.parentElement?.querySelector("code")?.textContent ?? "";
    expect(command).not.toContain("--service");
    expect(command).toContain("--name 'Unnamed runner'");
    expect(enrollmentRequests.mock.calls[0]![0].project_ids).toEqual([only]);
  });

  it("refuses to create a command with no project or an impossible capacity", async () => {
    const { projects } = await mountDialog();
    const create = screen.getByRole("button", { name: "Create command" }) as HTMLButtonElement;
    fireEvent.change(screen.getByLabelText("Concurrency"), { target: { value: "0" } });
    expect(create.disabled).toBe(true);
    fireEvent.change(screen.getByLabelText("Concurrency"), { target: { value: "2.9" } });
    expect(create.disabled).toBe(true);
    expect(screen.getByText("A whole number from 1 to 16")).toBeTruthy();
    fireEvent.change(screen.getByLabelText("Concurrency"), { target: { value: "1" } });
    expect(create.disabled).toBe(false);
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "é".repeat(101) } });
    expect(create.disabled).toBe(true);
    expect(screen.getByText("That name is too long; shorten it.")).toBeTruthy();
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "é".repeat(100) } });
    expect(create.disabled).toBe(false);
    for (const project of projects) fireEvent.click(projectBox(project.id));
    expect(create.disabled).toBe(true);
  });
});
