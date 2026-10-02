// @vitest-environment jsdom
//
// The Enroll dialog asks for a name, a capacity and projects, creates a
// token-first enrollment and shows the one command a host runs. Nobody types a
// runner or machine ID.
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createMemoryHistory, createRootRoute, createRouter, RouterProvider } from "@tanstack/react-router";
import { afterEach, describe, expect, it, vi } from "vitest";

import { startMockHub, type MockHub } from "../../dev/mock-hub.ts";
import { ClientContext } from "../../src/app/client.ts";
import { EnrollRunnerDialog, type PendingEnrollment } from "../../src/app/fleet/EnrollRunner.tsx";
import { RunnersSettings } from "../../src/app/fleet/RunnersSection.tsx";
import { loadBootstrap, makeClient, type ConversationClient } from "../../src/runtime/bootstrap.ts";
import { fetchEventStreamTransport } from "../../src/runtime/rpc/sse.ts";

let hub: MockHub | undefined;
let client: ConversationClient | undefined;

afterEach(async () => {
  cleanup();
  client?.handles.clear();
  client = undefined;
  vi.unstubAllGlobals();
  await hub?.close();
  hub = undefined;
});

function projectBox(id: string): HTMLElement {
  const box = document.getElementById(`enroll-project-${id}`);
  if (box === null) throw new Error(`no checkbox for ${id}`);
  return box;
}

const sameRealmFetch: typeof globalThis.fetch = (input, init) => {
  const { signal: _abort, ...rest } = (init ?? {}) as RequestInit;
  return globalThis.fetch(input as string, rest);
};

async function mountDialog(projectIds?: readonly string[], settings = false) {
  if (settings) {
    vi.stubGlobal("fetch", vi.fn(async (input: string) => {
      if (String(input).endsWith("/runner-enrollments")) {
        return new Response(JSON.stringify({ id: "enrollment_build", token: "det_enroll_secret", expires_at: "2026-10-02T20:00:00Z" }), { status: 201 });
      }
      return new Response(JSON.stringify({ runners: [], usage: { window_ends_at: "", allowances: {} }, spend: null }), { status: 200 });
    }));
    client = {
      account: {
        organization: { id: "org_build" },
        organizations: [{ current: true, public_url: "https://hub.example.test" }],
        actor: { can_manage_runners: true },
        projects: [{ id: "project_build", name: "Build", can_manage_runners: true }],
      },
      http: { origin: "", apiBase: "/api/v2/organizations/org_build", csrfToken: "csrf" },
      handles: new Map(),
    } as unknown as ConversationClient;
  } else {
    hub = await startMockHub({ deltaDelayMs: 0, heartbeatMs: 5_000, coordinator: "hub", organization: "seeded", account: "write" });
    const bootstrap = await loadBootstrap(hub.url);
    client = makeClient({ origin: hub.url, bootstrap, transport: fetchEventStreamTransport(sameRealmFetch), heartbeatTimeoutMs: 20_000 });
  }
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
        <EnrollRunnerDialog open onOpenChange={() => undefined} onEnrolled={onEnrolled} {...(projectIds === undefined ? {} : { projectIds })} />
      )}
    </ClientContext.Provider>,
  );
  return { onEnrolled, projects: client.account?.projects ?? [] };
}

describe("the Enroll dialog", () => {
  it("asks for no host IDs and shows one register command", async () => {
    const { onEnrolled, projects } = await mountDialog();
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
    const command = copy.parentElement?.querySelector("code")?.textContent ?? "";
    expect(command).toMatch(/^detent hub runner register --url \S+ (--organization org_\w+ )?--token det_enroll_\w+ --name 'Build host' --capacity 2 --service$/);
    expect(command).not.toMatch(/runner_|machine_|init|enroll --organization/);

    const sent = hub!.lastEnrollment();
    expect(sent?.runner_id).toBe("");
    expect(sent?.machine_id).toBe("");
    expect(sent?.project_ids).toEqual(projects.filter((_, index) => index !== 1).map((project) => project.id));
    await waitFor(() => expect(onEnrolled).toHaveBeenCalledTimes(1));
    expect(onEnrolled.mock.calls[0]![0]).toEqual({
      id: expect.any(String),
      expiresAt: expect.any(String),
      name: "Build host",
    });
  });

  it.each(["Done", "Escape", "Close"])("discards the token and command from the settings page after %s", async (close) => {
    const user = userEvent.setup();
    await mountDialog(undefined, true);
    await user.click(await screen.findByRole("button", { name: "Enroll a runner" }));
    await user.type(screen.getByLabelText("Name"), "Build host");
    await user.click(screen.getByRole("button", { name: "Create command" }));
    const copy = await screen.findByRole("button", { name: "Copy the register command" });
    const command = copy.parentElement!.querySelector("code")!.textContent!;
    const token = command.match(/--token (\S+)/)![1]!;
    expect(document.body.innerHTML).toContain(token);

    if (close === "Escape") await user.keyboard("{Escape}");
    else await user.click(screen.getByRole("button", { name: close }));

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

  it("preselects only the projects it was given and can skip the service", async () => {
    const seeded = await mountDialog();
    cleanup();
    await hub?.close();
    const only = seeded.projects[0]!.id;
    await mountDialog([only]);
    fireEvent.click(document.getElementById("enroll-runner-service")!);
    fireEvent.click(screen.getByRole("button", { name: "Create command" }));
    const copy = await screen.findByRole("button", { name: "Copy the register command" });
    const command = copy.parentElement?.querySelector("code")?.textContent ?? "";
    expect(command).not.toContain("--service");
    expect(command).not.toContain("--name");
    expect(hub!.lastEnrollment()?.project_ids).toEqual([only]);
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
