// @vitest-environment jsdom
import { RegistryProvider } from "@effect/atom-react";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
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
let releaseRead: (() => void) | undefined;

// These assertions span routing, real HTTP reads, and React updates. The
// default one-second DOM wait is not a latency contract for that sequence.
const httpWait = { timeout: 10_000 };

afterEach(async () => {
  cleanup();
  // A failed assertion must not leave a held request or a fetch spy for the
  // next test to capture as its "real" fetch.
  releaseRead?.();
  releaseRead = undefined;
  vi.restoreAllMocks();
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
if (typeof globalThis.PointerEvent === "undefined") {
  globalThis.PointerEvent = globalThis.MouseEvent as unknown as typeof PointerEvent;
}

async function renderSettings() {
  const router = makeRouter(createMemoryHistory({ initialEntries: ["/settings/integrations?project=proj_alpha"] }));
  await router.load();
  await act(async () => {
    render(
      <RegistryProvider>
        <ClientContext.Provider value={client!}>
          <RouterProvider router={router} />
        </ClientContext.Provider>
      </RegistryProvider>,
    );
  });
  return router;
}

describe("repository policy after setup", () => {
  it.each([false, true])("offers project setup only while unfinished (ready=%s)", async (ready) => {
    const { base } = await mount();
    const realFetch = globalThis.fetch;
    vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
      const response = await realFetch(input, init);
      if ((init?.method ?? "GET") === "GET" && String(input) === `${base}/onboarding`) {
        const onboarding = await response.json();
        return Response.json({
          ...onboarding,
          ready,
          steps: onboarding.steps.map((step: { state: string }, index: number) => ({ ...step, state: ready || index < 2 ? "ready" : "action_required" })),
          observed_policies: [{ policy: { ...onboarding.policy.policy, policy_id: "pol_reported" }, runner_id: "runner_example", observed_at: "2026-10-13T12:00:00Z" }],
        });
      }
      return response;
    });
    const router = await renderSettings();
    const importLink = await screen.findByRole("link", { name: "GitHub issue import in project setup" }, httpWait);
    expect(importLink.getAttribute("href")).toBe("/projects/proj_alpha/setup");
    await screen.findByText("Configuration reported", {}, httpWait);
    if (ready) {
      expect(screen.queryByText(/setup steps? left/)).toBeNull();
      expect(screen.queryByRole("button", { name: "Finish setup" })).toBeNull();
    } else {
      await screen.findByText("2 setup steps left", {}, httpWait);
      fireEvent.click(screen.getByRole("button", { name: "Finish setup" }));
      await waitFor(() => expect(router.state.location.pathname).toBe("/projects/proj_alpha/setup"), httpWait);
    }
  });

  it.each([
    { name: "native bound settings", profile: "native", repository: "acme/orders", checkout_repository: "", intake: "automatic", workflow: false, expectedIntake: "disabled" },
    { name: "native manual import allowed", profile: "native", repository: "acme/orders", checkout_repository: "", intake: "automatic", manual_import_enabled: true, workflow: false, expectedIntake: "manual" },
    { name: "native bound workflow", profile: "native", repository: "acme/orders", checkout_repository: "", intake: "automatic", workflow: true, expectedIntake: "disabled" },
    { name: "native checkout workflow", profile: "native", repository: "", checkout_repository: "acme/orders", intake: "automatic", workflow: true, expectedIntake: "disabled" },
    { name: "native unbound workflow", profile: "native", repository: "", checkout_repository: "", intake: "disabled", workflow: true, expectedIntake: "disabled" },
    { name: "compatibility settings", profile: "github_compatible", repository: "acme/orders", checkout_repository: "", intake: "manual", manual_import_enabled: true, workflow: false, expectedIntake: "disabled" },
  ])("saves $name with only editable intake", async ({ name: _name, workflow, expectedIntake, ...fields }) => {
    const { base } = await mount();
    const realFetch = globalThis.fetch;
    const requests = vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
      const response = await realFetch(input, init);
      if ((init?.method ?? "GET") === "GET" && String(input) === `${base}/integration`) {
        return Response.json({ ...await response.json(), ...fields });
      }
      return response;
    });
    await renderSettings();
    await screen.findByRole("heading", { name: "New issues" }, httpWait);
    expect(screen.queryByRole("combobox", { name: "Intake" })).toBeNull();
    if (workflow) {
      const definition = await screen.findByRole("textbox", { name: "Workflow definition" }, httpWait);
      fireEvent.change(definition, { target: { value: `${(definition as HTMLTextAreaElement).value}\nUpdated instructions.` } });
      fireEvent.click(screen.getByRole("button", { name: "Save workflow" }));
    } else {
      if (fields.profile === "github_compatible") {
        fireEvent.click(screen.getByRole("switch", { name: "Existing issues" }));
      } else {
        fireEvent.click(screen.getByRole("switch", { name: "Pull requests" }));
      }
      fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    }
    await waitFor(() => {
      const save = requests.mock.calls.find(([url, init]) => init?.method === "PUT" && String(url).endsWith("/onboarding/integration"));
      expect(save).toBeDefined();
      const body = JSON.parse(String(save![1]?.body));
      if (expectedIntake === undefined) expect(body).not.toHaveProperty("intake");
      else expect(body.intake).toBe(expectedIntake);
      expect(body).toHaveProperty("expected_revision");
      expect(body).toHaveProperty(workflow ? "workflow_markdown" : "repository_enabled");
    }, httpWait);
    await waitFor(async () => {
      const saved = await (await realFetch(`${base}/integration`)).json() as { revision: string };
      expect(saved.revision).toBe("8");
    }, httpWait);
  });

  it.each([1, 2])("does not require approval for %s reported configurations", async (count) => {
    const { base } = await mount();
    const current = (await (await fetch(`${base}/policy`)).json()) as { policy: Record<string, unknown> };
    for (let index = 0; index < count; index++) {
      const response = await fetch(`${base}/policy/observed?runner=runner_${index}`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ ...current.policy, policy_id: `configuration_${index}` }),
      });
      expect(response.status).toBe(204);
    }
    const requests = vi.spyOn(globalThis, "fetch");
    await renderSettings();
    expect(await screen.findByText("Configuration reported", {}, httpWait)).toBeTruthy();
    expect(screen.queryByRole("button", { name: /Approve|Paste policy/ })).toBeNull();
    expect(screen.queryByText(/Work is blocked until|waiting for a new policy/)).toBeNull();
    expect(requests.mock.calls.some(([url, init]) => init?.method === "PUT" && String(url).endsWith("/onboarding/policy"))).toBe(false);
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
