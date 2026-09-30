// @vitest-environment jsdom
//
// Project settings after setup: a runner reports the policy it resolved when
// the repository's detent.yaml or WORKFLOW.md changed, and an owner approves
// exactly that descriptor with one click, through the route the hosted Hub
// serves.
import { RegistryProvider } from "@effect/atom-react";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { createMemoryHistory, RouterProvider } from "@tanstack/react-router";
import { afterEach, describe, expect, it, vi } from "vitest";

import { startMockHub, type MockHub } from "../../dev/mock-hub.ts";
import { ClientContext } from "../../src/app/client.ts";
import { makeRouter } from "../../src/app/router.tsx";
import { navigationTarget } from "../../src/app/routes.account.tsx";
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

const sameRealmFetch: typeof globalThis.fetch = (input, init) => {
  const { signal: _abort, ...rest } = (init ?? {}) as RequestInit;
  return globalThis.fetch(input as string, rest);
};

async function mount(): Promise<{ base: string; approvedId: string }> {
  hub = await startMockHub({ deltaDelayMs: 0, heartbeatMs: 5_000, coordinator: "hub", organization: "seeded", account: "write" });
  const bootstrap = await loadBootstrap(hub.url);
  client = makeClient({ origin: hub.url, bootstrap, transport: fetchEventStreamTransport(sameRealmFetch), heartbeatTimeoutMs: 20_000 });
  const base = `${hub.url}${client.http.apiBase}/projects/proj_alpha`;
  const approved = (await (await fetch(`${base}/policy`)).json()) as { policy: Record<string, unknown> };
  return { base, approvedId: String(approved.policy.policy_id) };
}

Object.defineProperty(globalThis, "scrollTo", { value: () => {}, writable: true });

function renderSettings(): void {
  const router = makeRouter(createMemoryHistory({ initialEntries: ["/settings/integrations?project=proj_alpha"] }));
  render(
    <RegistryProvider>
      <ClientContext.Provider value={client!}>
        <RouterProvider router={router} />
      </ClientContext.Provider>
    </RegistryProvider>,
  );
}

describe("repository policy after setup", () => {
  it("approves the policy a runner reported with one click", async () => {
    const { base, approvedId } = await mount();
    const current = (await (await fetch(`${base}/policy`)).json()) as { policy: Record<string, unknown> };
    const reported = { ...current.policy, policy_id: "pol_changed", source_revision: "b".repeat(40) };
    const posted = await fetch(`${base}/policy/observed`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(reported),
    });
    expect(posted.status).toBe(204);

    const requests = vi.spyOn(globalThis, "fetch");
    renderSettings();
    expect(await screen.findByText("A runner is waiting for a new policy.")).toBeTruthy();
    expect(screen.getByText(/runner upgrade changed the resolved policy/)).toBeTruthy();
    expect(screen.getByText("pol_changed")).toBeTruthy();
    expect(screen.getAllByText(approvedId).length).toBeGreaterThan(0);

    fireEvent.click(screen.getAllByRole("button", { name: "Approve updated policy" })[0]!);
    await waitFor(() => expect(screen.queryByText("A runner is waiting for a new policy.")).toBeNull());
    const after = (await (await fetch(`${base}/policy`)).json()) as { policy: Record<string, unknown> };
    expect(after.policy.policy_id).toBe("pol_changed");
    const puts = requests.mock.calls.filter(([, init]) => init?.method === "PUT").map(([url]) => String(url));
    expect(puts.some((url) => url.endsWith("/projects/proj_alpha/onboarding/policy"))).toBe(true);
    expect(puts.some((url) => url.endsWith("/projects/proj_alpha/policy"))).toBe(false);
    requests.mockRestore();
    expect(screen.queryByRole("button", { name: "Approve updated policy" })).toBeNull();
  });

  it("names the current approval even while the policy read is still loading", async () => {
    const { base } = await mount();
    const current = (await (await fetch(`${base}/policy`)).json()) as { policy: Record<string, unknown> };
    await fetch(`${base}/policy/observed`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ ...current.policy, policy_id: "pol_slow" }),
    });
    const realFetch = globalThis.fetch;
    let releasePolicy: () => void = () => undefined;
    const policyHeld = new Promise<void>((resolve) => {
      releasePolicy = resolve;
    });
    const requests = vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
      if ((init?.method ?? "GET") === "GET" && String(input).endsWith("/projects/proj_alpha/policy")) await policyHeld;
      return realFetch(input, init);
    });
    renderSettings();
    fireEvent.click(await screen.findByRole("button", { name: "Approve updated policy pol_slow" }));
    await waitFor(async () => {
      const after = (await (await realFetch(`${base}/policy`)).json()) as { policy: Record<string, unknown> };
      expect(after.policy.policy_id).toBe("pol_slow");
    });
    const put = requests.mock.calls.find(([, init]) => init?.method === "PUT");
    expect(JSON.parse(String(put?.[1]?.body)).expected_policy_id).toBe(current.policy.policy_id);
    releasePolicy();
    requests.mockRestore();
  });

  it("lists each runner's reported policy and approves the chosen one after a conflicting approval", async () => {
    const { base } = await mount();
    const current = (await (await fetch(`${base}/policy`)).json()) as { policy: Record<string, unknown> };
    for (const [runner, id] of [["runner_a", "pol_a"], ["runner_b", "pol_b"]] as const) {
      await fetch(`${base}/policy/observed?runner=${runner}`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ ...current.policy, policy_id: id }),
      });
    }
    renderSettings();
    expect(await screen.findByRole("button", { name: "Approve updated policy pol_a" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Approve updated policy pol_b" })).toBeTruthy();

    // Another owner approves pol_b while this page is open.
    await fetch(`${base}/onboarding/policy`, {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ expected_policy_id: current.policy.policy_id, policy: { ...current.policy, policy_id: "pol_b" } }),
    });
    fireEvent.click(screen.getByRole("button", { name: "Approve updated policy pol_a" }));
    await waitFor(() => expect(screen.queryByRole("button", { name: "Approve updated policy pol_b" })).toBeNull());
    fireEvent.click(screen.getByRole("button", { name: "Approve updated policy pol_a" }));
    await waitFor(async () => {
      const after = (await (await fetch(`${base}/policy`)).json()) as { policy: Record<string, unknown> };
      expect(after.policy.policy_id).toBe("pol_a");
    });
  });

  it("approves a pasted descriptor and refuses one that is not JSON", async () => {
    const { base } = await mount();
    const current = (await (await fetch(`${base}/policy`)).json()) as { policy: Record<string, unknown> };
    renderSettings();
    fireEvent.click(await screen.findByRole("button", { name: "Paste a descriptor" }));
    const box = screen.getByLabelText("Policy descriptor");
    fireEvent.change(box, { target: { value: "not json" } });
    fireEvent.click(screen.getByRole("button", { name: "Approve pasted descriptor" }));
    expect(await screen.findByText(/The pasted descriptor is not JSON/)).toBeTruthy();

    fireEvent.change(box, { target: { value: JSON.stringify({ ...current.policy, policy_id: "pol_pasted" }) } });
    fireEvent.click(screen.getByRole("button", { name: "Approve pasted descriptor" }));
    await waitFor(async () => {
      const after = (await (await fetch(`${base}/policy`)).json()) as { policy: Record<string, unknown> };
      expect(after.policy.policy_id).toBe("pol_pasted");
    });
  });
});

describe("navigationTarget", () => {
  it.each([
    { to: "/settings/runners", want: { to: "/settings/runners" } },
    { to: "/settings/integrations?project=proj_alpha", want: { to: "/settings/integrations", search: { project: "proj_alpha" } } },
    { to: "/work?a=1&b=two%20words", want: { to: "/work", search: { a: "1", b: "two words" } } },
  ])("splits $to", ({ to, want }) => {
    expect(navigationTarget(to)).toEqual(want);
  });
});
