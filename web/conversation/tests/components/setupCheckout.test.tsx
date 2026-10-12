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
  vi.restoreAllMocks();
  client?.handles.clear();
  client = undefined;
  await hub?.close();
  hub = undefined;
});

it.each([
  ["reported checkout", "mockorg/private", "Verified runner checkout:"],
  ["new checkout", "example-org/example-repository", "Waiting for Example runner to clone or verify checkout:"],
])("shows association progress for a %s", async (_name, repository, status) => {
  hub = await startMockHub({ deltaDelayMs: 0, heartbeatMs: 5_000, coordinator: "hub", organization: "seeded", account: "write" });
  const bootstrap = await loadBootstrap(hub.url);
  client = makeClient({ origin: hub.url, bootstrap, transport: fetchEventStreamTransport(globalThis.fetch), heartbeatTimeoutMs: 20_000 });
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
  render(
    <ClientContext.Provider value={client}>
      <SetupRoute projectId="proj_beta" />
    </ClientContext.Provider>,
  );
  const user = userEvent.setup();
  await user.click(await screen.findByRole("button", { name: /Repository configuration/ }));
  const field = await screen.findByLabelText("Associate the runner checkout");
  expect(screen.getByText(/GitHub API integration is optional/)).toBeTruthy();
  fireEvent.change(field, { target: { value: "https://github.com/example-org/example-repository" } });
  await user.click(screen.getByRole("button", { name: "Associate" }));
  expect(await screen.findByText("Enter the repository as owner/name, then retry.")).toBeTruthy();
  fireEvent.change(field, { target: { value: repository } });
  await user.click(screen.getByRole("button", { name: "Associate" }));
  await waitFor(() => expect(screen.getByRole("status").textContent).toContain(`${status} ${repository}`));
  await waitFor(() => expect(screen.getByRole("button", { name: "Associate" }).hasAttribute("disabled")).toBe(true));
  if (repository === "example-org/example-repository") {
    checkoutReported = true;
    await waitFor(() => expect(screen.getByRole("status").textContent).toContain(`Verified runner checkout: ${repository}`), { timeout: 6_000 });
  }
}, 10_000);
