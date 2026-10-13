// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
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
  let reported = ["policy reported", "conflict"].includes(state);
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
    expect(await screen.findByRole("button", { name: "Approve settings" })).toBeTruthy();
    const saved = await fetch(`${client!.http.origin}${client!.http.apiBase}/projects/proj_beta/onboarding`);
    expect((await saved.json()).progress.repository).toBe("generate");
    expect(approveRequests).toEqual([]);
    return;
  }
  expect(screen.queryByRole("radio")).toBeNull();
  if (state === "approved") {
    expect(await screen.findByText(`Approved the settings in detent.yaml and WORKFLOW.md from ${repository}.`)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Approve settings" })).toBeNull();
    return;
  }
  const approve = await screen.findByRole("button", { name: "Approve settings" });
  expect(approve.hasAttribute("disabled")).toBe(state !== "policy reported");
  if (state === "policy pending") {
    expect(screen.getByText(`Waiting for the runner to report the settings in detent.yaml and WORKFLOW.md from ${repository}.`)).toBeTruthy();
    await user.click(screen.getByRole("button", { name: "Advanced policy details" }));
    fireEvent.change(screen.getByLabelText("Resolved policy descriptor"), { target: { value: JSON.stringify(descriptor) } });
    expect(approve.hasAttribute("disabled")).toBe(true);
    reported = true;
    await waitFor(() => expect(approve.hasAttribute("disabled")).toBe(false), { timeout: 6_000 });
    expect(approveRequests).toEqual([]);
    return;
  }
  expect(screen.getByText(`Approve the settings in detent.yaml and WORKFLOW.md from ${repository}.`)).toBeTruthy();
  if (state === "conflict") {
    expect(screen.getByRole("alert").textContent).toContain("Runners reported different settings");
    await user.click(screen.getByRole("button", { name: "Advanced policy details" }));
    fireEvent.change(screen.getByLabelText("Resolved policy descriptor"), { target: { value: "not JSON" } });
    await user.click(approve);
    expect(await screen.findByText(/The pasted descriptor is not JSON/)).toBeTruthy();
    expect(approveRequests).toEqual([]);
    fireEvent.change(screen.getByLabelText("Resolved policy descriptor"), { target: { value: JSON.stringify(descriptor) } });
  }
  await user.click(approve);
  expect(await screen.findByText(`Approved the settings in detent.yaml and WORKFLOW.md from ${repository}.`)).toBeTruthy();
  expect(approveRequests).toEqual([{ expected_policy_id: "", policy: descriptor }]);
});

it.each([
  { ready: false, unrouted: false },
  { ready: true, unrouted: false },
  { ready: true, unrouted: true },
])("uses the runner step's readiness for its primary action (ready: $ready, unrouted: $unrouted)", async ({ ready, unrouted }) => {
  const onNavigate = vi.fn();
  const mount = await mountSetup(onNavigate);
  const fetch = globalThis.fetch;
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const response = await fetch(input, init);
    if (ready && String(input).endsWith("/integration") && response.ok) {
      return Response.json({ ...await response.json(), checkout_repository: "example-org/example-repository", checkout_verified: true });
    }
    if (String(input).endsWith("/onboarding") && (!init?.method || init.method === "GET")) {
      const onboarding = await response.json();
      onboarding.runners = ready ? onboarding.runners : [];
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
    await user.click(screen.getByRole("radio", { name: "Your runner (local history)" }));
    const saveChoice = screen.getByRole("button", { name: "Save choice" });
    expect(saveChoice.classList.contains("bg-primary")).toBe(true);
    await user.click(saveChoice);
    expect(await screen.findByRole("button", { name: /Artifact history ready/ })).toBeTruthy();
    const progress = await fetch(`${client!.http.origin}${client!.http.apiBase}/projects/proj_beta/onboarding`);
    expect((await progress.json()).progress.artifacts).toBe("local");
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


it("finishes setup by moving forward from repository binding through policy approval", async () => {
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
      const steps = onboarding.steps.map((step: { name: string; state: string }) => ({ ...step, state: step.name === "Execution runner" || step.name === "Local validation" ? bound ? "ready" : "action_required" : step.state }));
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
  await user.click(await screen.findByRole("button", { name: "Approve settings" }));
  await user.click(await screen.findByRole("button", { name: "Continue" }));
  expect(await screen.findByRole("heading", { level: 1, name: "Artifact history" })).toBeTruthy();
  await user.click(screen.getByRole("radio", { name: "Your runner (local history)" }));
  await user.click(screen.getByRole("button", { name: "Save choice" }));
  await user.click(await screen.findByRole("button", { name: "Finish" }));
  expect(onNavigate).toHaveBeenCalledWith("/work");
});
