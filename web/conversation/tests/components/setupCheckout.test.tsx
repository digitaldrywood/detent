// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import userEvent from "@testing-library/user-event";

import { startMockHub, type MockHub } from "../../dev/mock-hub.ts";
import { ClientContext } from "../../src/app/client.ts";
import { SetupRoute } from "../../src/app/account/Setup.tsx";
import { loadBootstrap, makeClient, type ConversationClient } from "../../src/runtime/bootstrap.ts";
import { fetchEventStreamTransport } from "../../src/runtime/rpc/sse.ts";

let hub: MockHub | undefined;
let client: ConversationClient | undefined;

afterEach(async () => {
  cleanup();
  client?.handles.clear();
  client = undefined;
  await hub?.close();
  hub = undefined;
  vi.restoreAllMocks();
});

async function mountSetup(onNavigate?: (to: string) => void) {
  hub = await startMockHub({ deltaDelayMs: 0, heartbeatMs: 5_000, coordinator: "hub", organization: "seeded", account: "write" });
  const bootstrap = await loadBootstrap(hub.url);
  const setupClient = makeClient({ origin: hub.url, bootstrap, transport: fetchEventStreamTransport(globalThis.fetch), heartbeatTimeoutMs: 20_000 });
  client = setupClient;
  return () => render(
    <ClientContext.Provider value={setupClient}>
      <SetupRoute projectId="proj_beta" onNavigate={onNavigate} />
    </ClientContext.Provider>,
  );
}

it("preserves unsaved setup choices and fields through refresh, then saves and switches projects", async () => {
  const mount = await mountSetup();
  const fetch = globalThis.fetch;
  let finishRefresh: (() => void) | undefined;
  let deferRefresh = false;
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const response = await fetch(input, init);
    if (String(input).endsWith("/integration") && response.ok) {
      return Response.json({ ...await response.json(), checkout_repository: String(input).includes("proj_alpha") ? "example-org/another-project" : "example-org/example-project", checkout_verified: true });
    }
    if (deferRefresh && String(input).endsWith("/onboarding") && (!init?.method || init.method === "GET")) {
      deferRefresh = false;
      await new Promise<void>((resolve) => { finishRefresh = resolve; });
    }
    return response;
  });
  let poll: (() => void) | undefined;
  const setInterval = window.setInterval;
  vi.spyOn(window, "setInterval").mockImplementation((callback, delay, ...args) => {
    const timer = setInterval(callback, delay, ...args);
    if (delay === 5_000 && typeof callback === "function") {
      poll = callback as () => void;
      window.clearInterval(timer);
    }
    return timer as unknown as ReturnType<typeof window.setInterval>;
  });
  const view = mount();
  const context = await screen.findByRole("region", { name: "Project setup context" });
  expect(within(context).getByText("Setting up a project in Mock organization")).toBeTruthy();
  expect(within(context).getByText("beta", { exact: true })).toBeTruthy();
  expect(document.title).toBe("beta setup · Detent");
  const stepper = await screen.findByRole("list", { name: "Setup steps" });
  await waitFor(() => expect(within(stepper).getByText("example-org/example-project")).toBeTruthy());
  expect(within(stepper).getByText("Runner", { exact: true })).toBeTruthy();
  expect(within(stepper).getByText("Checkout", { exact: true })).toBeTruthy();
  expect(within(stepper).getByText("Run files", { exact: true })).toBeTruthy();
  const user = userEvent.setup();
  await user.click(await screen.findByRole("button", { name: /Checkout/ }));
  deferRefresh = true;
  expect(poll).toBeDefined();
  act(() => poll!());
  await waitFor(() => expect(finishRefresh).toBeDefined());
  await user.click(screen.getByRole("radio", { name: "Create configuration files" }));
  await user.click(screen.getByRole("button", { name: /Run files/ }));
  await user.click(screen.getByRole("radio", { name: "Your bucket and gateway" }));
  const fields = [
    ["Service id", "service_example"],
    ["Gateway origin", "https://artifacts.example.com"],
    ["Publisher token id", "token_example"],
  ] as const;
  for (const [label, value] of fields) fireEvent.change(screen.getByRole("textbox", { name: label }), { target: { value } });
  await act(async () => finishRefresh!());
  expect((screen.getByRole("radio", { name: "Your bucket and gateway" }) as HTMLInputElement).checked).toBe(true);
  for (const [label, value] of fields) expect((screen.getByRole("textbox", { name: label }) as HTMLInputElement).value).toBe(value);
  expect(screen.getByRole("button", { name: "Help for Publisher token id" })).toBeTruthy();
  await user.click(screen.getByRole("radio", { name: /Keep them on/ }));
  await user.click(screen.getByRole("button", { name: /Checkout/ }));
  expect((screen.getByRole("radio", { name: "Create configuration files" }) as HTMLInputElement).checked).toBe(true);
  await user.click(screen.getByRole("button", { name: "Save configuration choice" }));
  expect(screen.queryByRole("button", { name: "Approve settings" })).toBeNull();
  const saved = await fetch(`${client!.http.origin}${client!.http.apiBase}/projects/proj_beta/onboarding`).then((response) => response.json());
  expect(saved.progress).toMatchObject({ repository: "generate", artifacts: "local", revision: "2" });
  await user.click(screen.getByRole("button", { name: /Run files/ }));
  expect((screen.getByRole("radio", { name: /Keep them on/ }) as HTMLInputElement).checked).toBe(true);
  await user.click(screen.getByRole("radio", { name: "Your bucket and gateway" }));
  const alpha = `${client!.http.origin}${client!.http.apiBase}/projects/proj_alpha/onboarding`;
  const alphaProgress = await fetch(alpha).then((response) => response.json());
  const seed = await fetch(alpha, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ idempotency_key: "setup-project-switch", progress: { ...alphaProgress.progress, repository: "generate", artifacts: "local" } }),
  });
  expect(seed.ok).toBe(true);
  view.rerender(
    <ClientContext.Provider value={client!}>
      <SetupRoute projectId="proj_alpha" />
    </ClientContext.Provider>,
  );
  expect(document.title).toBe("alpha setup · Detent");
  expect(screen.queryByText("example-org/example-project")).toBeNull();
  await waitFor(() => expect(within(screen.getByRole("list", { name: "Setup steps" })).getByText("example-org/another-project")).toBeTruthy());
  await user.click(await screen.findByRole("button", { name: /Run files/ }));
  await waitFor(() => expect((screen.getByRole("radio", { name: /Keep them on/ }) as HTMLInputElement).checked).toBe(true));
  expect((screen.getByRole("radio", { name: "Your bucket and gateway" }) as HTMLInputElement).checked).toBe(false);
  expect(screen.queryByRole("textbox", { name: "Publisher token id" })).toBeNull();
});

it.each([
  ["reported checkout", "mockorg/private", "Verified runner checkout:"],
  ["new checkout", "example-org/example-repository", "Waiting for Example runner to clone or verify checkout:"],
])("shows association progress for a %s", async (_name, repository, status) => {
  const mount = await mountSetup();
  let checkoutReported = false;
  const fetch = globalThis.fetch;
  vi.spyOn(globalThis, "fetch").mockImplementation(async (...args) => {
    const response = await fetch(...args);
    if (String(args[0]).endsWith("/onboarding") && response.ok) {
      const onboarding = await response.json();
      return Response.json({ ...onboarding, runners: onboarding.runners.map((row: { runner: { display_name: string } }) => ({ ...row, runner: { ...row.runner, display_name: "Example runner" } })) });
    }
    if (checkoutReported && String(args[0]).endsWith("/integration") && response.ok) {
      return Response.json({ ...await response.json(), checkout_verified: true });
    }
    return response;
  });
  mount();
  const user = userEvent.setup();
  await user.click(await screen.findByRole("button", { name: /Repository action required/ }));
  const field = await screen.findByLabelText("Which repository should Detent work on?");
  expect(screen.getByText(/Private repositories work too, without a GitHub integration/)).toBeTruthy();
  fireEvent.change(field, { target: { value: "https://github.com/example-org/example-repository" } });
  await user.click(screen.getByRole("button", { name: "Associate" }));
  expect(await screen.findByText("Enter the repository as owner/name, then retry.")).toBeTruthy();
  fireEvent.change(field, { target: { value: repository } });
  await user.click(screen.getByRole("button", { name: "Associate" }));
  await waitFor(() => expect(screen.getByText(/runner checkout:|clone or verify checkout:/).textContent).toContain(`${status} ${repository}`));
  await waitFor(() => expect(screen.getByRole("button", { name: "Associate" }).hasAttribute("disabled")).toBe(true));
  if (repository === "example-org/example-repository") {
    checkoutReported = true;
    await waitFor(() => expect(screen.getByText(/runner checkout:|clone or verify checkout:/).textContent).toContain(`Verified runner checkout: ${repository}`), { timeout: 6_000 });
  }
}, 10_000);

it.each(["no repository", "configuration needed", "policy pending", "policy reported", "approved", "conflict"] as const)("reveals repository setup actions in order: %s", async (state) => {
  const mount = await mountSetup();
  const fetch = globalThis.fetch;
  const policyResponse = await fetch(`${client!.http.origin}${client!.http.apiBase}/projects/proj_alpha/policy`);
  const approval = await policyResponse.json();
  const descriptor = approval.policy;
  const repository = "example-org/example-project";
  const configurationChosen = !["no repository", "configuration needed"].includes(state);
  const reported = ["policy reported", "conflict"].includes(state);
  const approveRequests: Record<string, unknown>[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    if (String(input).endsWith("/onboarding/policy") && init?.method === "PUT") {
      approveRequests.push(JSON.parse(String(init.body)));
    }
    const response = await fetch(input, init);
    if (String(input).endsWith("/onboarding") && (!init?.method || init.method === "GET") && response.ok) {
      const onboarding = await response.json();
      const approved = state === "approved" || onboarding.policy?.policy.policy_id === descriptor.policy_id;
      return Response.json({
        ...onboarding,
        progress: { ...onboarding.progress, repository: configurationChosen ? "existing" : onboarding.progress.repository },
        policy: approved ? approval : null,
        observed_policies: reported && !approved ? [{ policy: descriptor, runner_id: "runner_example", observed_at: "2026-10-12T12:00:00Z", conflict: state === "conflict" }] : [],
        steps: onboarding.steps.map((step: { name: string; state: string }) => step.name === "Checkout" ? { ...step, state: approved ? "ready" : "action_required" } : step),
      });
    }
    if (String(input).endsWith("/integration") && response.ok) {
      const integration = await response.json();
      return Response.json({ ...integration, repository: "", checkout_repository: state === "no repository" ? "" : repository, checkout_verified: state !== "no repository" });
    }
    return response;
  });
  mount();
  const user = userEvent.setup();
  await user.click(await screen.findByRole("button", { name: /^.*Repository (ready|action required)$/ }));
  expect(await screen.findByLabelText("Which repository should Detent work on?")).toBeTruthy();
  await user.click(screen.getByRole("button", { name: /Checkout/ }));
  expect(approveRequests).toEqual([]);
  expect(screen.queryByLabelText("Resolved policy descriptor")).toBeNull();
  expect(screen.queryByText(/detent hub policy inspect/)).toBeNull();

  if (state === "no repository") {
    expect(screen.queryByRole("group", { name: "Configuration files" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Approve settings" })).toBeNull();
    return;
  }
  if (state === "configuration needed") {
    expect(await screen.findByRole("group", { name: "Configuration files" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Approve settings" })).toBeNull();
    await user.click(screen.getByRole("radio", { name: "Create configuration files" }));
    await user.click(screen.getByRole("button", { name: "Save configuration choice" }));
    expect(screen.queryByRole("button", { name: "Approve settings" })).toBeNull();
    await waitFor(async () => {
      const saved = await fetch(`${client!.http.origin}${client!.http.apiBase}/projects/proj_beta/onboarding`);
      expect((await saved.json()).progress.repository).toBe("generate");
    });
    expect(approveRequests).toEqual([]);
    return;
  }
  expect(screen.queryByRole("radio")).toBeNull();
  expect(screen.queryByRole("button", { name: "Approve settings" })).toBeNull();
  expect(screen.queryByRole("button", { name: "Advanced policy details" })).toBeNull();
  expect(approveRequests).toEqual([]);
});

it.each([
  ["online", "online", "Example runner · Online"],
  ["online", "needs_attention", "Example runner · Online · Needs attention"],
  ["offline", "needs_attention", "Example runner · Offline · Needs attention"],
  [undefined, "online", "Example runner · Online"],
])("uses connection health %s with runner health %s", async (connection_health, health, label) => {
  const mount = await mountSetup();
  const fetch = globalThis.fetch;
  vi.spyOn(globalThis, "fetch").mockImplementation(async (...args) => {
    const response = await fetch(...args);
    if (String(args[0]).endsWith("/onboarding") && response.ok) {
      const onboarding = await response.json();
      return Response.json({ ...onboarding, runners: onboarding.runners.slice(0, 1).map((row: { runner: object }) => ({ ...row, local_checks: { checkout: "failed", doctor: "pending", provider: "pending", observed_at: "2026-10-12T12:00:00Z" }, runner: { ...row.runner, display_name: "Example runner", connection_health, health } })) });
    }
    return response;
  });
  mount();
  await userEvent.setup().click(await screen.findByRole("button", { name: /Checkout/ }));
  expect(await screen.findByText(label)).toBeTruthy();
  if (connection_health === "offline") expect(screen.getByText(/Reconnect the runner’s computer/)).toBeTruthy();
  else expect(screen.queryByText(/Reconnect the runner’s computer/)).toBeNull();
});

it.each([["projects", false], ["projects", true], ["organization", false]] as const)("preselects an eligible online runner and preserves routing when confirming (scope: %s, offline choice: %s)", async (scope, offline) => {
  const mount = await mountSetup();
  const fetch = globalThis.fetch;
  let routed: Record<string, unknown> | undefined;
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    if (url.endsWith("/runners/runner_selected/routing") || url.endsWith("/runners/runner_offline/routing")) {
      if (init?.method === "PUT") {
        routed = JSON.parse(String(init.body));
        return Response.json(routed);
      }
      return Response.json({ revision: 4, display_name: "Build runner", tags: ["build"], state: "active", capacity_limit: 2, scope, project_ids: scope === "organization" ? [] : ["proj_alpha"], project_ranks: { proj_alpha: 7 } });
    }
    const response = await fetch(input, init);
    if (url.endsWith("/onboarding") && (!init?.method || init.method === "GET")) {
      const onboarding = await response.json();
      const base = onboarding.runners[0];
      return Response.json({ ...onboarding, ready: false, runners: [
        { ...base, runner: { ...base.runner, runner_id: "runner_offline", display_name: "Offline runner", connection_health: "offline" } },
        { ...base, exclusions: [{ code: "required_tag_missing", message: "Needs a tag" }], runner: { ...base.runner, runner_id: "runner_unsuitable", display_name: "Unavailable runner", connection_health: "online" } },
        { ...base, exclusions: [{ code: "project_access_denied", message: "Route here first" }], other_project_count: 1, runner: { ...base.runner, runner_id: "runner_selected", display_name: "Build runner", connection_health: "online" } },
      ], steps: onboarding.steps.map((step: { name: string }) => ({ ...step, state: "action_required" })) });
    }
    return response;
  });
  mount();
  const user = userEvent.setup();
  const selected = await screen.findByRole("radio", { name: /Build runner/ });
  expect((selected as HTMLInputElement).checked).toBe(true);
  expect(selected.getAttribute("aria-describedby")).toBeTruthy();
  expect(screen.getAllByText(/works on 1 other project/).length).toBeGreaterThan(0);
  if (offline) await user.click(screen.getByRole("radio", { name: /Offline runner/ }));
  await user.click(screen.getByRole("button", { name: offline ? "Use Offline runner" : "Use Build runner" }));
  expect(await screen.findByRole("heading", { level: 1, name: "Repository" })).toBeTruthy();
  expect(routed).toMatchObject({ expected_revision: 4, project_ids: scope === "organization" ? [] : ["proj_alpha", "proj_beta"], project_ranks: { proj_alpha: 7 }, scope });
});

it("enrolls inline when there are no runners and advances when a connected runner appears", async () => {
  const mount = await mountSetup();
  const fetch = globalThis.fetch;
  let connected = false;
  let poll: (() => void) | undefined;
  const setInterval = window.setInterval;
  vi.spyOn(window, "setInterval").mockImplementation((callback, delay, ...args) => {
    const timer = setInterval(callback, delay, ...args);
    if (delay === 5_000 && typeof callback === "function") { poll = callback as () => void; window.clearInterval(timer); }
    return timer as unknown as ReturnType<typeof window.setInterval>;
  });
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const response = await fetch(input, init);
    if (String(input).endsWith("/onboarding") && (!init?.method || init.method === "GET")) {
      const onboarding = await response.json();
      return Response.json({ ...onboarding, ready: false, runners: connected ? onboarding.runners.slice(0, 1).map((entry: { runner: object }) => ({ ...entry, exclusions: [], runner: { ...entry.runner, runner_id: "runner_new", display_name: "New build runner", state: "active", connection_health: "online" } })) : [], steps: onboarding.steps.map((step: { name: string }) => ({ ...step, state: "action_required" })) });
    }
    return response;
  });
  mount();
  expect(await screen.findByRole("region", { name: "Enroll a runner" })).toBeTruthy();
  expect(screen.getByLabelText("Name")).toBeTruthy();
  expect(screen.getByRole("button", { name: "Create command" })).toBeTruthy();
  expect(screen.queryByRole("dialog")).toBeNull();
  await userEvent.setup().click(screen.getByRole("button", { name: "Use an existing runner" }));
  await userEvent.setup().click(screen.getByRole("button", { name: "Enroll a runner" }));
  connected = true;
  await act(async () => poll!());
  expect(await screen.findByRole("heading", { level: 1, name: "Repository" })).toBeTruthy();
  await userEvent.setup().click(screen.getByRole("button", { name: /Runner action required/ }));
  expect((screen.getByRole("radio", { name: /New build runner/ }) as HTMLInputElement).checked).toBe(true);
});

it.each([false, true])("saves the run-files default before finishing, preserving prior progress (save fails: %s)", async (fails) => {
  const navigate = vi.fn();
  const mount = await mountSetup(navigate);
  const fetch = globalThis.fetch;
  let saved: Record<string, unknown> | undefined;
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    if (String(input).endsWith("/onboarding") && init?.method === "PUT") {
      saved = JSON.parse(String(init.body)).progress;
      if (fails) return Response.json({ code: "invalid_request", message: "Could not save setup." }, { status: 422 });
    }
    const response = await fetch(input, init);
    if (String(input).endsWith("/onboarding") && (!init?.method || init.method === "GET")) {
      const onboarding = await response.json();
      return Response.json({ ...onboarding, progress: { ...onboarding.progress, doctor: true, provider: true } });
    }
    return response;
  });
  mount();
  const user = userEvent.setup();
  await user.click(await screen.findByRole("button", { name: /Run files/ }));
  expect((screen.getByRole("radio", { name: /Keep them on/ }) as HTMLInputElement).checked).toBe(true);
  await user.click(screen.getByRole("button", { name: "Finish setup" }));
  await waitFor(() => expect(saved).toMatchObject({ artifacts: "local", doctor: true, provider: true }));
  if (fails) {
    expect(await screen.findByText("Could not save setup.")).toBeTruthy();
    expect(navigate).not.toHaveBeenCalled();
  } else {
    await waitFor(() => expect(navigate).toHaveBeenCalledWith("/work"));
    expect((await fetch(`${client!.http.origin}${client!.http.apiBase}/projects/proj_beta/onboarding`).then((response) => response.json())).progress.artifacts).toBe("local");
  }
});

it("shows the Ready summary and opens each step from its link", async () => {
  const navigate = vi.fn();
  const mount = await mountSetup(navigate);
  const fetch = globalThis.fetch;
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const response = await fetch(input, init);
    if (String(input).endsWith("/integration")) return Response.json({ ...await response.json(), checkout_repository: "example-org/example-project", checkout_verified: true });
    if (String(input).endsWith("/onboarding") && (!init?.method || init.method === "GET")) {
      const onboarding = await response.json();
      return Response.json({ ...onboarding, ready: true, progress: { ...onboarding.progress, artifacts: "local" }, runners: onboarding.runners.slice(0, 1).map((entry: { runner: object }) => ({ ...entry, runner: { ...entry.runner, display_name: "Build runner" } })), steps: onboarding.steps.map((step: object) => ({ ...step, state: "ready" })) });
    }
    return response;
  });
  const user = userEvent.setup();
  for (const [link, heading] of [["Change Runner", "Which computer should work on beta?"], ["Change Repository", "Repository"], ["View Checkout", "Checkout"], ["Change Run files", "Where should run files go?"]]) {
    const view = mount();
    expect(await screen.findByRole("heading", { level: 1, name: "beta is ready for work" })).toBeTruthy();
    expect(screen.getAllByRole("definition").some((element) => element.textContent === "Build runner")).toBe(true);
    expect(screen.getByText("example-org/example-project", { exact: true })).toBeTruthy();
    expect(screen.getByText("Kept on Build runner")).toBeTruthy();
    await user.click(screen.getByRole("button", { name: link }));
    expect(screen.getByRole("heading", { level: 1, name: heading })).toBeTruthy();
    view.unmount();
  }
  mount();
  await user.click(await screen.findByRole("button", { name: "Open Work" }));
  expect(navigate).toHaveBeenCalledWith("/work");
});
