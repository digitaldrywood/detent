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
      return Response.json({ ...await response.json(), checkout_repository: "example-org/example-project", checkout_verified: true });
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
  expect(within(stepper).getByText("Execution runner", { exact: true })).toBeTruthy();
  expect(within(stepper).getByText("Local validation", { exact: true })).toBeTruthy();
  expect(within(stepper).getByText("Repository configuration", { exact: true })).toBeTruthy();
  expect(within(stepper).getByText("Artifact history", { exact: true })).toBeTruthy();
  const user = userEvent.setup();
  await user.click(await screen.findByRole("button", { name: /Repository configuration/ }));
  deferRefresh = true;
  expect(poll).toBeDefined();
  act(() => poll!());
  await waitFor(() => expect(finishRefresh).toBeDefined());
  await user.click(screen.getByRole("radio", { name: "Create configuration files" }));
  await user.click(screen.getByRole("button", { name: /Artifact history/ }));
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
  await user.click(screen.getByRole("radio", { name: "Your runner (local history)" }));
  await user.click(screen.getByRole("button", { name: /Repository configuration/ }));
  expect((screen.getByRole("radio", { name: "Create configuration files" }) as HTMLInputElement).checked).toBe(true);
  await user.click(screen.getByRole("button", { name: "Save configuration choice" }));
  expect(screen.queryByRole("button", { name: "Approve settings" })).toBeNull();
  const saved = await fetch(`${client!.http.origin}${client!.http.apiBase}/projects/proj_beta/onboarding`).then((response) => response.json());
  expect(saved.progress).toMatchObject({ repository: "generate", artifacts: "local", revision: "2" });
  await user.click(screen.getByRole("button", { name: /Artifact history/ }));
  expect((screen.getByRole("radio", { name: "Your runner (local history)" }) as HTMLInputElement).checked).toBe(true);
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
  await waitFor(() => expect((screen.getByRole("radio", { name: "Your runner (local history)" }) as HTMLInputElement).checked).toBe(true));
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
        steps: onboarding.steps.map((step: { name: string; state: string }) => step.name === "Repository configuration" ? { ...step, state: approved ? "ready" : "action_required" } : step),
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
  await user.click(screen.getByRole("button", { name: /Repository configuration/ }));
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
  { ready: false, unrouted: false, capacityFull: false },
  { ready: true, unrouted: false, capacityFull: false },
  { ready: true, unrouted: true, capacityFull: false },
  { ready: true, unrouted: false, capacityFull: true },
])("uses the runner step's readiness for its primary action (ready: $ready, unrouted: $unrouted, capacity full: $capacityFull)", async ({ ready, unrouted, capacityFull }) => {
  const onNavigate = vi.fn();
  const mount = await mountSetup(onNavigate);
  const intervals = vi.spyOn(window, "setInterval");
  const clearInterval = vi.spyOn(window, "clearInterval");
  let onboardingReads = 0;
  let scopedRunnerReads = 0;
  let routedProjects: readonly string[] | undefined;
  const fetch = globalThis.fetch;
  const approval = capacityFull
    ? await fetch(`${client!.http.origin}${client!.http.apiBase}/projects/proj_alpha/policy`).then((response) => response.json())
    : undefined;
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    if (unrouted && String(input).endsWith("/runners") && (!init?.method || init.method === "GET")) {
      return Response.json({ code: "not_found", message: "Organization-wide runner access is unavailable" }, { status: 404 });
    }
    if (unrouted && String(input).endsWith("/routing") && init?.method === "PUT") {
      routedProjects = JSON.parse(String(init.body)).project_ids;
    }
    const response = await fetch(input, init);
    if (unrouted && String(input).endsWith("/routing") && (!init?.method || init.method === "GET") && response.ok) {
      scopedRunnerReads += 1;
      return Response.json({ ...await response.json(), scope: "projects", project_ids: ["proj_alpha"] });
    }
    if (ready && String(input).endsWith("/integration") && response.ok) {
      return Response.json({ ...await response.json(), checkout_repository: "example-org/example-repository", checkout_verified: true });
    }
    if (String(input).endsWith("/onboarding") && (!init?.method || init.method === "GET")) {
      const onboarding = await response.json();
      onboardingReads += 1;
      onboarding.runners = ready ? onboarding.runners : [];
      if (capacityFull) {
        onboarding.policy = approval;
        onboarding.runners = [{
          ...onboarding.runners[0],
          exclusions: [
            { code: "host_capacity", message: "Shared host capacity is full or paused" },
            { code: "runner_capacity", message: "Runner capacity is full or paused" },
          ],
        }];
      }
      if (unrouted) {
        onboarding.runners = [{
          runner: { ...onboarding.runners[0].runner, scope: "organization", project_ids: [], display_name: "Shared build runner" },
          exclusions: [{ code: "required_tag_missing", message: "Runner is missing required tag gpu" }],
        }];
      }
      onboarding.steps = onboarding.steps.map((step: { name: string; state: string }) => ({
        ...step,
        state: ready ? "ready" : step.name === "Execution runner" ? "action_required" : step.state,
      }));
      onboarding.ready = ready;
      return new Response(JSON.stringify(onboarding));
    }
    return response;
  });
  mount();
  const user = userEvent.setup();
  await screen.findByRole("button", { name: /Execution runner/ });
  for (const step of ["Execution runner", "Local validation", "Repository configuration", "Artifact history"]) {
    await user.click(screen.getByRole("button", { name: new RegExp(step) }));
    expect(screen.queryByText("Your first issue")).toBeNull();
    expect(screen.queryByLabelText("Title")).toBeNull();
    expect(screen.queryByLabelText("What needs doing")).toBeNull();
    expect(screen.queryByLabelText("Lane")).toBeNull();
    expect(screen.queryByRole("button", { name: "Create the first issue" })).toBeNull();
  }
  await user.click(screen.getByRole("button", { name: /Execution runner/ }));
  const enroll = screen.getByRole("button", { name: "Enroll a runner" });
  const forward = screen.getByRole("button", { name: ready ? "Continue" : "Skip for now" });
  expect(enroll.classList.contains("bg-primary")).toBe(!ready);
  expect(forward.classList.contains("bg-primary")).toBe(ready);
  expect(screen.queryByRole("button", { name: ready ? "Skip for now" : "Continue" })).toBeNull();
  if (unrouted) {
    expect(screen.getAllByText("Shared build runner").length).toBeGreaterThan(0);
    expect(screen.getByText("Runner is missing required tag gpu")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Route here" }).hasAttribute("disabled")).toBe(false);
    await user.click(screen.getByRole("button", { name: "Route here" }));
    await waitFor(() => expect(routedProjects).toEqual(["proj_alpha", "proj_beta"]));
    expect(scopedRunnerReads).toBe(1);
  }
  if (capacityFull) {
    expect(screen.getByText("Shared host capacity is full or paused · Runner capacity is full or paused")).toBeTruthy();
    for (const [index, [, timeout]] of intervals.mock.calls.entries()) {
      if (timeout === 5_000) {
        expect(clearInterval).toHaveBeenCalledWith(intervals.mock.results[index]?.value);
      }
    }
  }

  await user.click(enroll);
  expect(await screen.findByRole("dialog")).toBeTruthy();
  await user.click(screen.getByRole("button", { name: "Cancel" }));
  await user.click(forward);
  await waitFor(() => expect(screen.getByRole("heading", { level: 1 }).textContent).toBe("Local validation"));
  expect(screen.getByRole("button", { name: ready ? /Execution runner ready/ : /Execution runner action required/ }).textContent).toContain(ready ? "✓" : "2");
  const saved = await fetch(`${client!.http.origin}${client!.http.apiBase}/projects/proj_beta/onboarding`);
  expect((await saved.json()).progress.revision).toBe("2");

  await user.click(screen.getByRole("button", { name: "Back" }));
  expect(screen.getByRole("heading", { level: 1 }).textContent).toBe("Execution runner");
  await user.click(screen.getByRole("button", { name: /Artifact history/ }));
  if (ready) {
    await user.click(screen.getByRole("button", { name: "Finish" }));
    expect(onNavigate).toHaveBeenCalledWith("/work");
  } else {
    expect(screen.queryByRole("button", { name: "Finish" })).toBeNull();
    expect(screen.getByRole("button", { name: "Skip for now" }).hasAttribute("disabled")).toBe(false);
    expect(screen.getByRole("button", { name: "Save choice" }).hasAttribute("disabled")).toBe(true);
    await user.click(screen.getByRole("radio", { name: "Your bucket and gateway" }));
    expect(await screen.findByLabelText("Publisher token id")).toBeTruthy();
    const reads = onboardingReads;
    const poll = intervals.mock.calls.find(([, timeout]) => timeout === 5_000)?.[0];
    expect(poll).toBeTypeOf("function");
    await act(async () => {
      (poll as () => void)();
      await waitFor(() => expect(onboardingReads).toBeGreaterThan(reads));
    });
    expect(screen.getByLabelText("Publisher token id")).toBeTruthy();
    await user.click(screen.getByRole("radio", { name: "Your runner (local history)" }));
    const saveChoice = screen.getByRole("button", { name: "Save choice" });
    expect(saveChoice.classList.contains("bg-primary")).toBe(true);
    await user.click(saveChoice);
    expect(await screen.findByRole("button", { name: /Artifact history ready/ })).toBeTruthy();
    const progress = await fetch(`${client!.http.origin}${client!.http.apiBase}/projects/proj_beta/onboarding`);
    expect((await progress.json()).progress.artifacts).toBe("local");
  }
});


it.each([false, true])("leaves the last setup step only after saving with skipped steps (save fails: %s)", async (saveFails) => {
  const onNavigate = vi.fn();
  const mount = await mountSetup(onNavigate);
  const fetch = globalThis.fetch;
  const endpoint = `${client!.http.origin}${client!.http.apiBase}/projects/proj_beta/onboarding`;
  const initial = await fetch(endpoint).then((response) => response.json());
  const seeded = await fetch(endpoint, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ idempotency_key: "setup-last-step", progress: { ...initial.progress, artifacts: "local" } }),
  });
  expect(seeded.ok).toBe(true);
  const saves: Record<string, unknown>[] = [];
  let finishSave: (() => void) | undefined;
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    if (String(input) === endpoint && init?.method === "PUT") {
      saves.push(JSON.parse(String(init.body)));
      await new Promise<void>((resolve) => { finishSave = resolve; });
      if (saveFails) return Response.json({ code: "invalid_request", message: "Could not save setup." }, { status: 422 });
    }
    const response = await fetch(input, init);
    if (String(input) === endpoint && (!init?.method || init.method === "GET")) {
      const onboarding = await response.json();
      return Response.json({
        ...onboarding,
        runners: [],
        steps: onboarding.steps.map((step: { name: string; state: string }) => ({ ...step, state: step.name === "Artifact history" ? step.state : "action_required" })),
        ready: false,
      });
    }
    return response;
  });
  mount();
  const user = userEvent.setup();
  await user.click(await screen.findByRole("button", { name: /Artifact history ready/ }));
  expect(screen.queryByRole("button", { name: "Finish" })).toBeNull();
  await user.click(screen.getByRole("button", { name: "Continue" }));
  await waitFor(() => expect(finishSave).toBeDefined());
  expect(saves).toEqual([expect.objectContaining({ progress: expect.objectContaining({ artifacts: "local", repository: "", doctor: false, provider: false }) })]);
  expect(onNavigate).not.toHaveBeenCalled();
  await act(async () => finishSave!());
  if (saveFails) {
    expect(await screen.findByText("Could not save setup.")).toBeTruthy();
    expect(onNavigate).not.toHaveBeenCalled();
    expect(screen.getByRole("heading", { level: 1, name: "Artifact history" })).toBeTruthy();
  } else {
    await waitFor(() => expect(onNavigate).toHaveBeenCalledWith("/work"));
    const saved = await fetch(endpoint).then((response) => response.json());
    expect(saved.progress).toMatchObject({ artifacts: "local", revision: "3", repository: "", doctor: false, provider: false });
    expect(saved.ready).toBe(false);
  }
  for (const step of ["Execution runner", "Local validation", "Repository configuration"]) {
    await user.click(screen.getByRole("button", { name: new RegExp(`${step} action required`) }));
    expect(screen.getByRole("heading", { level: 1, name: step })).toBeTruthy();
  }
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
  await userEvent.setup().click(await screen.findByRole("button", { name: /Local validation/ }));
  expect(await screen.findByText(label)).toBeTruthy();
  if (connection_health === "offline") expect(screen.getByText(/Reconnect the runner’s computer/)).toBeTruthy();
  else expect(screen.queryByText(/Reconnect the runner’s computer/)).toBeNull();
});


it("finishes setup after the runner reports repository configuration", async () => {
  const onNavigate = vi.fn();
  const mount = await mountSetup(onNavigate);
  const fetch = globalThis.fetch;
  const descriptor = (await fetch(`${client!.http.origin}${client!.http.apiBase}/projects/proj_alpha/onboarding`).then((response) => response.json())).policy.policy;
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const response = await fetch(input, init);
    if (String(input).endsWith("/onboarding") && response.ok && (!init?.method || init.method === "GET")) {
      const onboarding = await response.json();
      const integration = await fetch(`${client!.http.origin}${client!.http.apiBase}/projects/proj_beta/integration`).then((result) => result.json());
      const bound = Boolean(integration.checkout_repository);
      const runners = bound ? onboarding.runners.slice(0, 1).map((row: { runner: object }) => ({ ...row, local_checks: { checkout: "passed", doctor: "passed", provider: "passed", observed_at: new Date().toISOString() } })) : [];
      const steps = onboarding.steps.map((step: { name: string; state: string }) => ({ ...step, state: step.name === "Execution runner" || step.name === "Local validation" || step.name === "Repository configuration" && onboarding.progress.repository !== "" ? bound ? "ready" : "action_required" : step.state }));
      return Response.json({ ...onboarding, runners, steps, observed_policies: bound && !onboarding.policy ? [{ policy: descriptor, runner_id: "runner_example", observed_at: new Date().toISOString() }] : [], ready: bound && steps.every((step: { state: string }) => step.state === "ready") });
    }
    return response;
  });
  mount();
  const user = userEvent.setup();
  expect(await screen.findByRole("heading", { level: 1, name: "Repository" })).toBeTruthy();
  expect(screen.queryByLabelText("Resolved policy descriptor")).toBeNull();
  expect(screen.queryByRole("group", { name: "Configuration files" })).toBeNull();
  fireEvent.change(screen.getByLabelText("Which repository should Detent work on?"), { target: { value: "example-org/example-repository" } });
  await user.click(screen.getByRole("button", { name: "Associate" }));
  await user.click(await screen.findByRole("button", { name: "Continue" }));
  expect(await screen.findByRole("heading", { level: 1, name: "Execution runner" })).toBeTruthy();
  await user.click(await screen.findByRole("button", { name: "Continue" }));
  expect(await screen.findByRole("heading", { level: 1, name: "Local validation" })).toBeTruthy();
  expect(screen.getAllByText("Passed")).toHaveLength(3);
  await user.click(await screen.findByRole("button", { name: "Continue" }));
  expect(await screen.findByRole("heading", { level: 1, name: "Repository configuration" })).toBeTruthy();
  await user.click(screen.getByRole("radio", { name: "Use existing configuration files" }));
  await user.click(screen.getByRole("button", { name: "Save configuration choice" }));
  expect(screen.queryByRole("button", { name: "Approve settings" })).toBeNull();
  await user.click(await screen.findByRole("button", { name: "Continue" }));
  expect(await screen.findByRole("heading", { level: 1, name: "Artifact history" })).toBeTruthy();
  await user.click(screen.getByRole("radio", { name: "Your runner (local history)" }));
  await user.click(screen.getByRole("button", { name: "Save choice" }));
  await user.click(await screen.findByRole("button", { name: "Finish" }));
  expect(onNavigate).toHaveBeenCalledWith("/work");
});
