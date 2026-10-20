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
import { useAccountApi } from "../../src/app/account/context.ts";
import { useResource } from "../../src/app/account/useResource.ts";
import type { ConversationClient } from "../../src/runtime/bootstrap.ts";
import type { FleetRunner } from "../../src/contracts/account.ts";
import fleetFixture from "../../src/contracts/fixtures/account-fleet.json";

let client: ConversationClient | undefined;

if (typeof globalThis.PointerEvent === "undefined") {
  globalThis.PointerEvent = globalThis.MouseEvent as unknown as typeof PointerEvent;
}

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

async function mountDialog(projectIds?: readonly string[], settings = false, initialRunners: readonly FleetRunner[] = [], initialFleetRead?: Promise<Response>, spritesPresent = false, version = "v1.2.3", selectTier = true) {
  let runners = initialRunners;
  const fleetReads = vi.fn();
  const enrollmentRequests = vi.fn();
  vi.stubGlobal("fetch", vi.fn(async (input: string, init?: RequestInit) => {
    if (String(input).endsWith("/secrets/fly_sprites_token")) {
      return new Response(JSON.stringify({ kind: "fly_sprites_token", present: spritesPresent, organization_slug: "preview" }));
    }
    if (String(input).endsWith("/runner-enrollments")) {
      enrollmentRequests(JSON.parse(String(init?.body)));
      return new Response(JSON.stringify({ id: "enrollment_build", token: "det_enroll_secret", expires_at: "2026-10-02T20:00:00Z" }), { status: 201 });
    }
    if (String(input).endsWith("/capacity")) return new Response(JSON.stringify({ error: { code: "not_found", message: "Not found" } }), { status: 404 });
    fleetReads();
    if (fleetReads.mock.calls.length === 1 && initialFleetRead !== undefined) return initialFleetRead;
    return new Response(JSON.stringify({ current: version, runners, usage: { window_ends_at: "", allowances: {} }, spend: null }), { status: 200 });
  }));
  client = {
    account: {
      version: "v1.2.3",
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
  const onConnected = vi.fn<(entry: PendingEnrollment) => void>();
  const router = createRouter({
    routeTree: createRootRoute({ component: RunnersSettings }),
    history: createMemoryHistory({ initialEntries: ["/"] }),
  });
  function DirectDialog() {
    const api = useAccountApi();
    const fleet = useResource(() => api.fleet(), [api]);
    return <EnrollRunnerDialog open onOpenChange={() => undefined} onEnrolled={onEnrolled} onConnected={onConnected} fleet={fleet} {...(projectIds === undefined ? {} : { projectIds })} />;
  }
  render(
    <ClientContext.Provider value={client}>
      {settings ? (
        <RouterProvider router={router as never} />
      ) : (
        <DirectDialog />
      )}
    </ClientContext.Provider>,
  );
  if (!settings && selectTier) fireEvent.click(await screen.findByRole("radio", { name: "Sandbox" }));
  if (!settings && initialFleetRead === undefined && selectTier) {
    await waitFor(() => expect((screen.getByRole("button", { name: "Create command" }) as HTMLButtonElement).disabled).toBe(false));
  }
  return { onEnrolled, onConnected, enrollmentRequests, projects: client.account?.projects ?? [], fleetReads, setRunners: (next: readonly FleetRunner[]) => { runners = next; } };
}

describe("the Enroll dialog", () => {
  it.each([false, true])("warns when a connected runner reports no providers (providers: %s)", async (hasProviders) => {
    const { setRunners } = await mountDialog();
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Build host" } });
    vi.useFakeTimers();
    await act(async () => fireEvent.click(screen.getByRole("button", { name: "Create command" })));
    const runner = { ...fleetFixture.runners[0]!, id: "rnr_new", display_name: "Build host", provider_capacity: hasProviders ? fleetFixture.runners[0]!.provider_capacity : [] } as FleetRunner;
    setRunners([runner]);
    await act(() => vi.advanceTimersByTimeAsync(2_000));
    expect(screen.getByText("Build host is connected")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Next" }));
    if (hasProviders) {
      expect(screen.queryByRole("alert")).toBeNull();
    } else {
      const warning = screen.getByRole("alert");
      expect(warning.textContent).toContain("No providers reported");
      expect(warning.textContent).toContain("service PATH");
      expect(warning.textContent).toContain("runner service logs");
    }
  });

  it("waits for a delayed initial fleet baseline before matching a new runner", async () => {
    let release!: (response: Response) => void;
    const initial = new Promise<Response>((resolve) => { release = resolve; });
    const existing = { ...fleetFixture.runners[0]!, display_name: "Build host" } as FleetRunner;
    const { enrollmentRequests, setRunners } = await mountDialog(undefined, false, [existing], initial);
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Build host" } });
    const create = screen.getByRole("button", { name: "Create command" }) as HTMLButtonElement;
    expect(create.disabled).toBe(true);
    expect(screen.getByRole("status").textContent).toContain("Reading runners");
    fireEvent.click(create);
    expect(enrollmentRequests).not.toHaveBeenCalled();
    await act(async () => release(new Response(JSON.stringify({ runners: [existing], usage: { window_ends_at: "", allowances: {} }, spend: null }), { status: 200 })));
    await waitFor(() => expect(create.disabled).toBe(false));
    vi.useFakeTimers();
    await act(async () => fireEvent.click(create));
    expect(screen.getByRole("button", { name: "Copy the register command" })).toBeTruthy();
    expect(enrollmentRequests).toHaveBeenCalledOnce();
    await act(() => vi.advanceTimersByTimeAsync(2_000));
    expect(screen.queryByText("Build host is connected")).toBeNull();
    setRunners([existing, { ...existing, id: "rnr_new", has_heartbeat: true }]);
    await act(() => vi.advanceTimersByTimeAsync(2_000));
    expect(screen.getByText("Build host is connected")).toBeTruthy();
  });

  it("waits for a heartbeat and Next, preserves navigation, and never re-shows a redeemed token", async () => {
    userEvent.setup();
    const writeText = vi.spyOn(navigator.clipboard, "writeText").mockResolvedValue();
    const { setRunners, onEnrolled, onConnected, enrollmentRequests } = await mountDialog();
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Build host" } });
    vi.useFakeTimers();
    await act(async () => fireEvent.click(screen.getByRole("button", { name: "Create command" })));
    fireEvent.click(screen.getByRole("button", { name: "Show token" }));
    expect(document.body.textContent).toContain("det_enroll_secret");
    const enrolled = { ...fleetFixture.runners[0]!, id: "rnr_new", display_name: "Build host", has_heartbeat: false } as FleetRunner;
    setRunners([enrolled]);
    await act(() => vi.advanceTimersByTimeAsync(2_000));
    expect(screen.getByRole("status").textContent).toContain("Build host is enrolled, waiting for the runner to start");
    expect(screen.getByRole("list").querySelector('[aria-current="step"]')?.textContent).toContain("Run the command");
    expect((screen.getByRole("button", { name: "Next" }) as HTMLButtonElement).disabled).toBe(true);
    expect(onEnrolled).toHaveBeenCalledOnce();
    expect(onConnected).not.toHaveBeenCalled();
    expect(screen.queryByRole("button", { name: "Show token" })).toBeNull();
    expect(document.body.textContent).not.toContain("det_enroll_secret");
    fireEvent.click(screen.getByRole("button", { name: "Copy the register command" }));
    expect(writeText).toHaveBeenLastCalledWith(expect.stringContaining("--token 'detent_••••••••'"));
    expect(writeText.mock.calls.at(-1)![0]).not.toContain("det_enroll_secret");
    fireEvent.click(screen.getByRole("button", { name: "Back" }));
    expect((screen.getByLabelText("Name") as HTMLInputElement).matches(":disabled")).toBe(true);
    expect(screen.queryByRole("button", { name: "Create command" })).toBeNull();
    setRunners([{ ...enrolled, has_heartbeat: true, last_heartbeat_at: "2026-10-12T16:00:01Z" }]);
    await act(() => vi.advanceTimersByTimeAsync(2_000));
    expect(screen.getByRole("list").querySelector('[aria-current="step"]')?.textContent).toContain("Set it up");
    expect(onConnected).toHaveBeenCalledOnce();
    expect(onConnected).toHaveBeenCalledWith(onEnrolled.mock.calls[0]![0]);
    fireEvent.click(screen.getByRole("button", { name: "Next" }));
    expect(screen.getByRole("status").textContent).toBe("Build host is connected");
    expect(screen.getByRole("button", { name: "Copy the register command" })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Next" }));
    expect(screen.getByRole("list").querySelector('[aria-current="step"]')?.textContent).toContain("Connected");
    fireEvent.click(screen.getByRole("button", { name: "Back" }));
    expect(screen.getByRole("button", { name: "Copy the register command" })).toBeTruthy();
    expect(document.body.textContent).not.toContain("det_enroll_secret");
    expect(screen.queryByRole("button", { name: "Show token" })).toBeNull();
    expect(enrollmentRequests).toHaveBeenCalledOnce();
    await act(() => vi.advanceTimersByTimeAsync(10_000));
    expect(onConnected).toHaveBeenCalledOnce();
  });

  it.each([400, 503])("keeps creation unavailable after an initial fleet error (%s) and retries the existing read", async (status) => {
    const initial = Promise.resolve(new Response(JSON.stringify({ code: "unavailable", message: "Fleet unavailable" }), { status }));
    const { enrollmentRequests, fleetReads } = await mountDialog(undefined, false, [], initial);
    const create = screen.getByRole("button", { name: "Create command" }) as HTMLButtonElement;
    await screen.findByText("unavailable: Fleet unavailable");
    expect(create.disabled).toBe(true);
    fireEvent.click(create);
    expect(enrollmentRequests).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("button", { name: "Retry runner read" }));
    await waitFor(() => expect(create.disabled).toBe(false));
    expect(fleetReads).toHaveBeenCalledTimes(2);
    expect(screen.queryByText("unavailable: Fleet unavailable")).toBeNull();
    fireEvent.click(create);
    await screen.findByRole("button", { name: "Copy the register command" });
    expect(enrollmentRequests).toHaveBeenCalledOnce();
  });

  it.each([
    [false, "v1.2.3", "v1.2.3"],
    [true, "v1.2.3", "v1.2.3"],
    [true, "operator-landed-3c51987c563b", "3c51987c563b"],
    [true, "", "${detent_release}"],
  ] as const)("guides Sprite setup with token presence %s and Hub version %s, keeping credentials masked", async (present, version, ref) => {
    userEvent.setup();
    const writeText = vi.spyOn(navigator.clipboard, "writeText").mockResolvedValue();
    await mountDialog(["project_build"], false, [], undefined, present, version);
    fireEvent.click(screen.getByLabelText("A Fly Sprite"));
    expect(screen.getByRole("radio", { name: "Full access" }).getAttribute("aria-checked")).toBe("true");
    expect(screen.getByRole("radio", { name: "Sandbox" }).getAttribute("aria-disabled")).toBe("true");
    expect(screen.getByText("Sandbox on a Fly Sprite needs a passing isolation probe on that runner: update the runner, then switch it to Sandbox once it reports sandbox support.")).toBeTruthy();
    expect((screen.getByLabelText("Name") as HTMLInputElement).value).toMatch(/^detent-[a-z0-9]+$/);
    expect(screen.getByText(/The Sprite gets the same name/)).toBeTruthy();
    await waitFor(() => expect(screen.queryByText("Checking Sprites tokens…")).toBeNull());
    if (!present) {
      expect(screen.getByText(/No Sprites token is set/)).toBeTruthy();
      expect(screen.getByText(/No Sprites token is set/).textContent).toContain("for Build");
      expect(screen.getByText(/No Sprites token is set/).textContent).toContain("uncheck the project");
      expect(screen.getByRole("link", { name: "Set a Sprites token" }).getAttribute("href")).toBe("/settings/integrations?project=project_build#sprites");
    } else {
      expect(screen.queryByRole("link", { name: "Set a Sprites token" })).toBeNull();
    }
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Build host" } });
    expect((screen.getByRole("button", { name: "Create command" }) as HTMLButtonElement).disabled).toBe(true);
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "build-host" } });
    expect((screen.getByRole("button", { name: "Create command" }) as HTMLButtonElement).disabled).toBe(false);
    expect(screen.queryByLabelText("Install it as a background service")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Create command" }));
    const instructions = await screen.findByRole("list", { name: "Sprite setup instructions" });
    const organization = present ? "preview" : '"${SPRITE_ORG:?Set SPRITE_ORG to your Fly organization}"';
    expect(instructions.textContent).toContain(`sprite list -o ${organization} && sprite create -o ${organization} build-host`);
    expect(instructions.textContent).toContain(`sprite console -o ${organization} -s build-host`);
    expect(instructions.textContent).toContain(`/${ref}/scripts/sprite-runner-bootstrap.sh`);
    if (version === "v1.2.3") {
      expect(instructions.textContent).toContain("bash sprite-runner-bootstrap.sh --version v1.2.3");
      expect(instructions.textContent).not.toContain("releases/latest");
    } else {
      expect(instructions.textContent).toContain("https://github.com/digitaldrywood/detent/releases/latest");
      expect(instructions.textContent).toContain('bash sprite-runner-bootstrap.sh --version "$detent_release"');
      expect(instructions.textContent).toContain(`Upgrade the installed runner to ${version || "the Hub’s build"} afterwards.`);
    }
    expect(instructions.textContent).toContain("claude auth login");
    expect(instructions.textContent).toContain("codex login --device-auth");
    expect(instructions.textContent).toContain("reported repository configuration");
    expect(instructions.textContent).toContain("gh auth login --hostname github.com --git-protocol https --web");
    expect(instructions.textContent).toContain("hooks.runner_setup");
    expect(instructions.textContent).toContain("installation checkpoint does not prove readiness");
    expect(document.body.innerHTML).not.toContain("det_enroll_secret");
    expect(screen.queryByRole("button", { name: "Show token" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Copy the register command" }));
    expect(writeText).toHaveBeenCalledWith(expect.stringContaining("--token det_enroll_secret --name build-host"));
    expect(writeText.mock.calls[0]![0]).toContain("--isolation-tier native-trusted");
    expect(writeText.mock.calls[0]![0]).not.toContain("--service");
    expect(document.body.innerHTML).not.toContain("det_enroll_secret");
  });
  it.each([null, "Sandbox", "Full access"])("restores the machine access choice %s after choosing a Sprite", async (choice) => {
    await mountDialog(undefined, false, [], undefined, true, "v1.2.3", false);
    if (choice !== null) fireEvent.click(screen.getByRole("radio", { name: choice }));
    fireEvent.click(screen.getByRole("radio", { name: "A Fly Sprite" }));
    expect(screen.getByRole("radio", { name: "Full access" }).getAttribute("aria-checked")).toBe("true");
    fireEvent.click(screen.getByRole("radio", { name: "A machine I run" }));
    for (const label of ["Sandbox", "Full access"]) {
      expect(screen.getByRole("radio", { name: label }).getAttribute("aria-checked")).toBe(String(choice === label));
    }
    const sandbox = screen.getByRole("radio", { name: "Sandbox" }) as HTMLInputElement;
    expect(sandbox.getAttribute("aria-disabled")).not.toBe("true");
    expect(screen.getByRole("radio", { name: "Full access" }).getAttribute("aria-disabled")).not.toBe("true");
    fireEvent.click(sandbox);
    expect(sandbox.getAttribute("aria-checked")).toBe("true");
    expect(screen.queryByText("Sandbox on a Fly Sprite needs a passing isolation probe on that runner: update the runner, then switch it to Sandbox once it reports sandbox support.")).toBeNull();
  });

  it.each([["Sandbox", "sandbox"], ["Full access", "native-trusted"]] as const)("requires an explicit %s selection and carries it into the register command", async (label, tier) => {
    userEvent.setup();
    const writeText = vi.spyOn(navigator.clipboard, "writeText").mockResolvedValue();
    const { onEnrolled, enrollmentRequests, projects } = await mountDialog(undefined, false, [], undefined, false, "v1.2.3", false);
    const create = await screen.findByRole("button", { name: "Create command" }) as HTMLButtonElement;
    expect(create.disabled).toBe(true);
    expect(screen.getByText("Choose Sandbox or Full access to create a command.")).toBeTruthy();
    expect(screen.getByRole("radio", { name: "Sandbox" }).getAttribute("aria-disabled")).not.toBe("true");
    expect(screen.getByRole("radio", { name: "Full access" }).getAttribute("aria-disabled")).not.toBe("true");
    expect(screen.getByRole("radio", { name: "Sandbox" }).getAttribute("aria-checked")).toBe("false");
    expect(screen.getByRole("radio", { name: "Full access" }).getAttribute("aria-checked")).toBe("false");
    fireEvent.click(create);
    expect(enrollmentRequests).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("radio", { name: label }));
    await waitFor(() => expect(create.disabled).toBe(false));
    expect(screen.queryByText("Choose Sandbox or Full access to create a command.")).toBeNull();
    expect(projects.length).toBeGreaterThan(1);
    expect((screen.getByLabelText("Concurrency") as HTMLInputElement).value).toBe("1");
    expect(screen.getByText(/each with its own workspace and agent/)).toBeDefined();
    expect(screen.queryByLabelText("Runner id")).toBeNull();
    expect(screen.queryByLabelText("Machine id")).toBeNull();

    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Build host" } });
    fireEvent.change(screen.getByLabelText("Concurrency"), { target: { value: "2" } });
    expect((screen.getByLabelText("Runner scope") as HTMLSelectElement).value).toBe("organization");
    fireEvent.change(screen.getByLabelText("Runner scope"), { target: { value: "projects" } });
    fireEvent.click(projectBox(projects[1]!.id));
    fireEvent.click(screen.getByRole("button", { name: "Create command" }));

    const copy = await screen.findByRole("button", { name: "Copy the register command" });
    const code = copy.parentElement!.querySelector("code")!;
    expect(code.textContent).toContain("detent_••••••••");
    expect(document.body.innerHTML).not.toContain("det_enroll_");
    fireEvent.click(screen.getByRole("button", { name: "Show token" }));
    const command = code.textContent ?? "";
    expect(command).toMatch(/^detent hub runner register --url \S+ (--organization org_\w+ )?--token det_enroll_\w+ --name 'Build host' --capacity 2 --isolation-tier (sandbox|native-trusted) --service$/);
    expect(command).toContain(`--isolation-tier ${tier}`);
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
    await user.click(await screen.findByRole("button", { name: "Add runner" }));
    await user.click(await screen.findByRole("menuitem", { name: "Manual" }));
    await user.click(screen.getByRole("radio", { name: "Sandbox" }));
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

    await user.click(screen.getByRole("button", { name: "Make a new command" }));
    expect((screen.getByLabelText("Name") as HTMLInputElement).value).toBe("Build host");
    expect((screen.getByRole("button", { name: "Create command" }) as HTMLButtonElement).disabled).toBe(true);
    expect(document.body.innerHTML).not.toContain(token);
    expect(document.body.innerHTML).not.toContain("detent hub runner register");
    await user.keyboard("{Escape}");
    await user.click(screen.getByRole("button", { name: "Add runner" }));
    await user.click(await screen.findByRole("menuitem", { name: "Manual" }));
    await user.click(screen.getByRole("radio", { name: "Sandbox" }));
    expect((screen.getByLabelText("Name") as HTMLInputElement).value).toBe("");
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
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: "Add runner" }));
    await user.click(await screen.findByRole("menuitem", { name: "Manual" }));
    await user.click(screen.getByRole("radio", { name: "Sandbox" }));
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: name } });
    vi.useFakeTimers();
    await act(async () => fireEvent.click(screen.getByRole("button", { name: "Create command" })));
    const dialog = within(screen.getByRole("dialog"));
    expect(dialog.getByRole("list").querySelector('[aria-current="step"]')?.textContent).toContain("Run the command");
    expect(dialog.getByRole("status").textContent).toContain(`Waiting for ${displayName} to check in`);
    await act(() => vi.advanceTimersByTimeAsync(2_000));
    expect(dialog.queryByText(`${displayName} is connected`)).toBeNull();

    if (finish === "connected") {
      setRunners([existing, { ...existing, id: "rnr_new", has_heartbeat: true }]);
      const before = fleetReads.mock.calls.length;
      await act(() => vi.advanceTimersByTimeAsync(2_000));
      expect(fleetReads).toHaveBeenCalledTimes(before + 1);
      expect(dialog.getByText(`${displayName} is connected`)).toBeTruthy();
      expect(dialog.getByRole("button", { name: "Copy the register command" })).toBeTruthy();
      expect(dialog.getByRole("list").querySelector('[aria-current="step"]')?.textContent).toContain("Run the command");
      fireEvent.click(dialog.getByRole("button", { name: "Next" }));
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
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Build host" } });
    fireEvent.click(document.getElementById("enroll-runner-service")!);
    fireEvent.click(screen.getByRole("button", { name: "Create command" }));
    const copy = await screen.findByRole("button", { name: "Copy the register command" });
    const command = copy.parentElement?.querySelector("code")?.textContent ?? "";
    expect(command).not.toContain("--service");
    expect(command).toContain("--name 'Build host'");
    expect(command).toContain("--isolation-tier sandbox");
    expect(enrollmentRequests.mock.calls[0]![0].scope).toBe("projects");
    expect(enrollmentRequests.mock.calls[0]![0].project_ids).toEqual([only]);
  });

  it.each([
    { body: { code: "invalid_request", message: "Enrollment projects must be unique and belong to the organization" }, status: 422, code: "invalid_request" },
    { body: { message: "Invalid enrollment request" }, status: 400, code: "invalid" },
  ])("shows the server reason when enrollment fails with $status", async ({ body, status, code }) => {
    const { onEnrolled } = await mountDialog(["project_build"]);
    vi.mocked(globalThis.fetch).mockResolvedValueOnce(new Response(JSON.stringify(body), { status }));
    fireEvent.click(screen.getByRole("button", { name: "Create command" }));
    expect((await screen.findByRole("alert")).textContent).toContain(`${code}: ${body.message}`);
    expect(onEnrolled).not.toHaveBeenCalled();
    expect(screen.queryByRole("button", { name: "Copy the register command" })).toBeNull();
  });

  it("falls back to SPRITE_ORG while the token check for a new selection is pending", async () => {
    await mountDialog(["project_build"], false, [], undefined, true);
    fireEvent.click(screen.getByLabelText("A Fly Sprite"));
    await waitFor(() => expect(screen.queryByText("Checking Sprites tokens…")).toBeNull());
    const respond = vi.mocked(globalThis.fetch).getMockImplementation()!;
    vi.mocked(globalThis.fetch).mockImplementation((input, init) =>
      String(input).endsWith("/secrets/fly_sprites_token") ? new Promise<Response>(() => undefined) : respond(input, init));
    fireEvent.click(projectBox("project_release"));
    fireEvent.click(projectBox("project_build"));
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "build-host" } });
    fireEvent.click(screen.getByRole("button", { name: "Create command" }));
    const instructions = await screen.findByRole("list", { name: "Sprite setup instructions" });
    const organization = '"${SPRITE_ORG:?Set SPRITE_ORG to your Fly organization}"';
    expect(instructions.textContent).toContain(`sprite create -o ${organization} build-host`);
    expect(instructions.textContent).not.toContain("-o preview");
  });

  it("shows the server reason when a Sprites token check fails", async () => {
    await mountDialog(["project_build"]);
    vi.mocked(globalThis.fetch).mockResolvedValueOnce(new Response(JSON.stringify({ code: "secret_unavailable", message: "The Sprites token could not be read" }), { status: 400 }));
    fireEvent.click(screen.getByRole("radio", { name: "A Fly Sprite" }));
    expect((await screen.findByText(/secret_unavailable: The Sprites token could not be read/)).textContent).toContain("Could not check the Sprites token");
  });

  it("reports a failed fleet refresh without clearing the enrollment command", async () => {
    await mountDialog(["project_build"]);
    vi.useFakeTimers();
    await act(async () => fireEvent.click(screen.getByRole("button", { name: "Create command" })));
    vi.mocked(globalThis.fetch).mockResolvedValueOnce(new Response(JSON.stringify({ code: "unavailable", message: "Fleet refresh failed" }), { status: 400 }));
    await act(() => vi.advanceTimersByTimeAsync(2_000));
    expect(screen.getByRole("alert").textContent).toContain("unavailable: Fleet refresh failed");
    expect(screen.getByRole("button", { name: "Copy the register command" })).toBeTruthy();
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
    fireEvent.change(screen.getByLabelText("Runner scope"), { target: { value: "projects" } });
    for (const project of projects) fireEvent.click(projectBox(project.id));
    expect(create.disabled).toBe(true);
  });
});
