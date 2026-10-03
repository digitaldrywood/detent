// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it } from "vitest";
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
});

it("shows the hosted checkout action and its verified private repository", async () => {
  hub = await startMockHub({ deltaDelayMs: 0, heartbeatMs: 5_000, coordinator: "hub", organization: "seeded", account: "write" });
  const bootstrap = await loadBootstrap(hub.url);
  client = makeClient({ origin: hub.url, bootstrap, transport: fetchEventStreamTransport(globalThis.fetch), heartbeatTimeoutMs: 20_000 });
  render(
    <ClientContext.Provider value={client}>
      <SetupRoute projectId="proj_beta" />
    </ClientContext.Provider>,
  );
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
