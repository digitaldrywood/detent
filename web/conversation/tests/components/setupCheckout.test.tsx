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

it("shows the hosted checkout action and its verified private repository", async () => {
  const mount = await mountSetup();
  mount();
  const user = userEvent.setup();
  await user.click(await screen.findByRole("button", { name: /Repository configuration/ }));
  const field = await screen.findByLabelText("Associate the runner checkout");
  expect(screen.getByText(/GitHub API integration is optional/)).toBeTruthy();
  fireEvent.change(field, { target: { value: "https://github.com/mockorg/private" } });
  await user.click(screen.getByRole("button", { name: "Associate" }));
  expect(await screen.findByText("Enter the repository as owner/name, then retry.")).toBeTruthy();
  fireEvent.change(field, { target: { value: "mockorg/private" } });
  await user.click(screen.getByRole("button", { name: "Associate" }));
  await waitFor(() => expect(screen.getByText(/Verified runner checkout:/).textContent).toContain("mockorg/private"));
  await waitFor(() => expect(screen.getByRole("button", { name: "Associate" }).hasAttribute("disabled")).toBe(true));
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
  await user.click(await screen.findByRole("button", { name: /Execution runner/ }));
  const enroll = screen.getByRole("button", { name: "Enroll a runner" });
  const forward = screen.getByRole("button", { name: ready ? "Continue" : "Skip for now" });
  expect(enroll.classList.contains("bg-primary")).toBe(!ready);
  expect(forward.classList.contains("bg-primary")).toBe(ready);
  expect(screen.queryByRole("button", { name: ready ? "Skip for now" : "Continue" })).toBeNull();
  if (unrouted) {
    expect(screen.getByText("Shared build runner")).toBeTruthy();
    expect(screen.getByText("Runner is missing required tag gpu")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Route here" }).hasAttribute("disabled")).toBe(false);
  }

  await user.click(enroll);
  expect(await screen.findByRole("dialog")).toBeTruthy();
  await user.click(screen.getByRole("button", { name: "Cancel" }));
  await user.click(forward);
  await waitFor(() => expect(screen.getByRole("heading", { level: 1 }).textContent).toBe("Local validation"));
  expect(screen.getByRole("button", { name: ready ? /Execution runner ready/ : /Execution runner action required/ }).textContent).toContain(ready ? "✓" : "1");
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
