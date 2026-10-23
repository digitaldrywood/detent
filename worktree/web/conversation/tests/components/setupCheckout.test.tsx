// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import userEvent from "@testing-library/user-event";

import { startMockHub, type MockHub } from "../../dev/mock-hub.ts";
import { ClientContext } from "../../src/app/client.ts";
import { SetupRoute } from "../../src/app/account/Setup.tsx";
import { RepositoryPicker } from "../../src/app/account/RepositoryPicker.tsx";
import { Onboarding } from "../../src/contracts/account.ts";
import onboardingFixture from "../../src/contracts/fixtures/account-onboarding.json";
import * as Schema from "effect/Schema";
import { loadBootstrap, makeClient, type ConversationClient } from "../../src/runtime/bootstrap.ts";
import { fetchEventStreamTransport } from "../../src/runtime/rpc/sse.ts";

let hub: MockHub | undefined;
let client: ConversationClient | undefined;

it.each([false, true])("offers GitHub discovery only when available (%s), including from Not listed", async (available) => {
  const connect = vi.fn();
  const view = render(<RepositoryPicker runner={undefined} value="example-org/unlisted" onChange={vi.fn()} refresh={vi.fn()} refreshing={false}
    github={{ available, connected: false, reconnect: false, repositories: [] }} connectGitHub={connect} />);
  const user = userEvent.setup();
  const button = screen.queryByRole("button", { name: /Connect GitHub to list more repositories/ });
  expect(Boolean(button)).toBe(available);
  if (available) {
    await user.click(button!);
    expect(connect).toHaveBeenCalledOnce();
  }
  view.rerender(<RepositoryPicker runner={undefined} value="" onChange={vi.fn()} refresh={vi.fn()} refreshing={false}
    github={{ available, connected: true, reconnect: false, repositories: [{ owner: "example-org", name: "project", private: true, pushed_at: null }] }} connectGitHub={connect} />);
  expect(screen.queryByRole("button", { name: /Connect GitHub to list more repositories/ })).toBeNull();
  await user.click(screen.getByRole("button", { name: "Not listed?" }));
  expect(Boolean(screen.queryByRole("button", { name: /Connect GitHub to list more repositories/ }))).toBe(available);
});

it("merges GitHub and runner repositories without duplicates and retains the runner list on reconnect", async () => {
  const runner = { ...Schema.decodeUnknownSync(Onboarding)(onboardingFixture).runners![0]!, repository_list: {
    request_id: "listed", pending: false, repositories: [{ owner: "example-org", name: "project", private: true, pushed_at: null }],
  } };
  const change = vi.fn();
  const view = render(<RepositoryPicker runner={runner} value="" onChange={change} refresh={vi.fn()} refreshing={false}
    github={{ available: true, connected: true, reconnect: false, repositories: [{ owner: "EXAMPLE-ORG", name: "PROJECT", private: true, pushed_at: null }, { owner: "example-user", name: "another", private: false, pushed_at: null }] }} />);
  expect(screen.getAllByRole("option", { name: /example-org\/project/ })).toHaveLength(1);
  expect(screen.getByRole("option", { name: /example-org\/project/ }).textContent).toContain("Runner + GitHub App");
  const added = screen.getByRole("option", { name: /example-user\/another/ });
  expect(added.textContent).toContain("GitHub App");
  expect(screen.getByText(/Listed by .* and the Detent Cloud GitHub App/)).toBeTruthy();
  await userEvent.setup().click(added);
  expect(change).toHaveBeenCalledWith("example-user/another");
  view.rerender(<RepositoryPicker runner={runner} value="" onChange={change} refresh={vi.fn()} refreshing={false}
    github={{ available: true, connected: false, reconnect: true, repositories: [] }} />);
  await userEvent.setup().click(screen.getByRole("combobox"));
  expect(screen.getByRole("option", { name: /example-org\/project/ })).toBeTruthy();
  expect(screen.queryByRole("option", { name: /example-user\/another/ })).toBeNull();
  expect(screen.getByText(/Reconnect GitHub to update its repositories/)).toBeTruthy();
});

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
    if (String(input).endsWith("/onboarding") && (!init?.method || init.method === "GET") && response.ok) {
      if (deferRefresh) {
        deferRefresh = false;
        await new Promise<void>((resolve) => { finishRefresh = resolve; });
      }
      const data = await response.json();
      return Response.json({ ...data, runners: data.runners.map((entry: Record<string, unknown>) => ({ ...entry, local_checks: { checkout: "passed", configuration: "missing", doctor: "pending", provider: "pending", observed_at: new Date().toISOString() } })) });
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
  await user.click(screen.getByRole("radio", { name: /Create starter files on/ }));
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
  expect((screen.getByRole("radio", { name: /Create starter files on/ }) as HTMLInputElement).checked).toBe(true);
  await user.click(screen.getByRole("button", { name: "Create starter files" }));
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

it.each(["listed", "typed", "offline", "github"] as const)("binds a %s repository and rejects invalid input", async (state) => {
  const mount = await mountSetup();
  const fetch = globalThis.fetch;
  let bound: Record<string, unknown> | undefined;
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    if (String(input).endsWith("/onboarding/repository")) bound = JSON.parse(String(init?.body));
    if (state === "github" && String(input).endsWith("/api/cloud/account/github/repositories")) {
      return Response.json({ available: true, connected: true, reconnect: false, repositories: [{ owner: "example-user", name: "added-project", private: true, pushed_at: null }] });
    }
    const response = await fetch(input, init);
    if (state === "github" && String(input).endsWith("/integration") && response.ok) return Response.json({ ...await response.json(), github_transport_available: true });
    if (state === "offline" && String(input).endsWith("/onboarding") && response.ok) {
      const data = await response.json();
      return Response.json({ ...data, runners: data.runners.map((entry: { runner: Record<string, unknown> }) => ({ ...entry, repository_list: { request_id: "loading", repositories: [], pending: true }, runner: { ...entry.runner, connection_health: "offline" } })) });
    }
    return response;
  });
  mount();
  const user = userEvent.setup();
  await user.click(await screen.findByRole("button", { name: /Repository action required/ }));
  const field = await screen.findByRole("combobox", { name: "Search or type owner/name" });
  await user.click(field);
  fireEvent.change(field, { target: { value: "invalid value" } });
  expect(screen.getByRole("button", { name: "Use invalid value" }).hasAttribute("disabled")).toBe(true);
  expect(bound).toBeUndefined();
  let repository: string;
  if (state === "listed") {
    fireEvent.change(field, { target: { value: "example-" } });
    expect(screen.queryByRole("option", { name: /project-notes/ })).toBeTruthy();
    fireEvent.change(field, { target: { value: "example-project" } });
    expect(screen.queryByRole("option", { name: /project-notes/ })).toBeNull();
    await user.keyboard("{ArrowDown}{Enter}");
    repository = "example-org/example-project";
  } else if (state === "github") {
    repository = "example-user/added-project";
    fireEvent.change(field, { target: { value: "added-project" } });
    await user.click(await screen.findByRole("option", { name: /example-user\/added-project.*GitHub App/ }));
  } else {
    repository = "example-org/unlisted";
    fireEvent.change(field, { target: { value: repository } });
    await user.click(screen.getByRole("option", { name: `Use ${repository} as typed` }));
    if (state === "offline") expect(screen.getByText(/You can still type owner\/name/)).toBeTruthy();
  }
  await user.click(screen.getByRole("button", { name: `Use ${repository}` }));
  await waitFor(() => expect(bound?.repository).toBe(repository));
  expect(bound?.source).toBe("runner_checkout");
  expect(bound?.expected_revision).toBeDefined();
  expect(bound?.idempotency_key).toBeTruthy();
  const integration = await (await fetch(`${client!.http.origin}${client!.http.apiBase}/projects/proj_beta/integration`)).json();
  expect(integration.checkout_repository).toBe(repository);
});

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
        runners: onboarding.runners.map((entry: Record<string, unknown>) => ({ ...entry, local_checks: { checkout: "passed", configuration: state === "configuration needed" ? "missing" : "passed", doctor: "passed", provider: "passed", observed_at: new Date().toISOString() } })),
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
  const repositoryPicker = await screen.findByRole("combobox", { name: "Search or type owner/name" });
  expect(repositoryPicker.getAttribute("aria-expanded")).toBe(state === "no repository" ? "true" : "false");
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
    await user.click(screen.getByRole("radio", { name: /Create starter files on/ }));
    await user.click(screen.getByRole("button", { name: "Create starter files" }));
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
])("uses connection health %s with runner health %s", async (connection_health, health, _label) => {
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
  expect(await screen.findByRole("heading", { level: 1, name: /Example runner is preparing/ })).toBeTruthy();
  expect(screen.getByRole("list", { name: "Runner checks" }).textContent).toContain("Failed");
  if (connection_health === "offline") expect(screen.getByText(/Reconnect Example runner/)).toBeTruthy();
  else expect(screen.queryByText(/Reconnect Example runner/)).toBeNull();
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
  expect(await screen.findByRole("heading", { level: 1, name: "Which repository is beta?" })).toBeTruthy();
  expect(routed).toMatchObject({ expected_revision: 4, project_ids: scope === "organization" ? [] : ["proj_alpha", "proj_beta"], project_ranks: { proj_alpha: 7 }, scope });
});

it.each([false, true])("enrolls inline when there are no runners and advances when a connected runner appears (return to enrollment: %s)", async (returnToEnrollment) => {
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
  if (returnToEnrollment) {
    await userEvent.setup().click(screen.getByRole("button", { name: "Use an existing runner" }));
    await userEvent.setup().click(screen.getByRole("button", { name: "Enroll a runner" }));
  }
  connected = true;
  await act(async () => poll!());
  expect(await screen.findByRole("heading", { level: 1, name: "Which repository is beta?" })).toBeTruthy();
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
  for (const [link, heading] of [["Change Runner", "Which computer should work on beta?"], ["Change Repository", "Which repository is beta?"], ["View Checkout", "Build runner is preparing example-org/example-project"], ["Change Run files", "Where should run files go?"]]) {
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

it.each([
  { checkout: "running", configuration: "pending", doctor: "pending", provider: "pending", statuses: ["Running", "Waiting", "Waiting", "Waiting"] },
  { checkout: "passed", configuration: "passed", doctor: "running", provider: "pending", statuses: ["Passed", "Passed", "Running", "Waiting"] },
  { checkout: "passed", configuration: "passed", doctor: "warning", provider: "passed", statuses: ["Passed", "Passed", "Warnings", "Passed"] },
  { checkout: "passed", configuration: "passed", doctor: "failed", provider: "passed", statuses: ["Passed", "Passed", "Failed", "Passed"] },
  { checkout: "passed", configuration: "passed", doctor: "passed", provider: "passed", statuses: ["Passed", "Passed", "Passed", "Passed"] },
])("renders incremental runner checks: $doctor / $provider", async ({ statuses, ...checks }) => {
  const mount = await mountSetup();
  const fetch = globalThis.fetch;
  const ready = ["passed", "warning"].includes(checks.doctor) && checks.provider === "passed";
  const writes: unknown[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    if (String(input).endsWith("/onboarding") && init?.method === "PUT") writes.push(JSON.parse(String(init.body)));
    const response = await fetch(input, init);
    if (String(input).endsWith("/onboarding") && (!init?.method || init.method === "GET")) {
      const data = await response.json();
      return Response.json({ ...data, runners: data.runners.slice(0, 1).map((entry: { runner: object }) => ({ ...entry, runner: { ...entry.runner, display_name: "Example runner" }, local_checks: { ...checks, observed_at: new Date(Date.now() - 12_000).toISOString() } })), steps: data.steps.map((step: { name: string }) => step.name === "Checkout" ? { ...step, state: ready ? "ready" : "action_required" } : step) });
    }
    return response;
  });
  mount();
  const user = userEvent.setup();
  await user.click(await screen.findByRole("button", { name: /Checkout/ }));
  const rows = within(screen.getByRole("list", { name: "Runner checks" })).getAllByRole("listitem");
  statuses.forEach((status, index) => expect(within(rows[index]!).getByText(status, { exact: true })).toBeTruthy());
  expect((screen.getByRole("button", { name: /^Continue$/ }) as HTMLButtonElement).disabled).toBe(!ready);
  expect(screen.getByRole("button", { name: "Continue in background" })).toBeTruthy();
  expect(document.body.textContent).not.toMatch(/restart/i);
  if (checks.doctor === "failed") {
    expect(rows[2]!.textContent).toContain("Run detent doctor --config");
    await user.click(screen.getByRole("button", { name: "Retry detent doctor" }));
    await waitFor(() => expect(writes).toHaveLength(1));
  }
});

it("saves the choice to add missing configuration without creating files", async () => {
  const mount = await mountSetup();
  const fetch = globalThis.fetch;
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const response = await fetch(input, init);
    if (String(input).endsWith("/onboarding") && (!init?.method || init.method === "GET")) {
      const data = await response.json();
      return Response.json({ ...data, runners: data.runners.slice(0, 1).map((entry: object) => ({ ...entry, local_checks: { checkout: "passed", configuration: "missing", doctor: "pending", provider: "pending", observed_at: new Date().toISOString() } })) });
    }
    return response;
  });
  mount();
  const user = userEvent.setup();
  await user.click(await screen.findByRole("button", { name: /Checkout/ }));
  expect(screen.getByText("detent.yaml and WORKFLOW.md not found")).toBeTruthy();
  expect(within(screen.getByRole("list", { name: "Runner checks" })).getAllByText("Waiting")).toHaveLength(2);
  await user.click(screen.getByRole("radio", { name: "I’ll add them myself" }));
  await user.click(screen.getByRole("button", { name: "Wait for files" }));
  await waitFor(async () => {
    const data = await fetch(`${client!.http.origin}${client!.http.apiBase}/projects/proj_beta/onboarding`).then((response) => response.json());
    expect(data.progress.repository).toBe("existing");
  });
  expect(document.body.textContent).not.toMatch(/restart/i);
});
