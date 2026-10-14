// @vitest-environment jsdom
//
// Project settings after setup: a runner reports the policy it resolved when
// the repository's detent.yaml or WORKFLOW.md changed, and an owner approves
// exactly that descriptor with one click, through the route the hosted Hub
// serves.
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

async function renderSettings(): Promise<void> {
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
}

describe("repository policy after setup", () => {
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

  it.each(["immediate", "deferred"] as const)("approves the policy a runner reported with one click (%s onboarding)", async (delivery) => {
    const { base, approvedId } = await mount();
    const current = (await (await fetch(`${base}/policy`)).json()) as { policy: Record<string, unknown> };
    const reported = { ...current.policy, policy_id: "pol_changed", source_revision: "b".repeat(40), gates: { ...(current.policy.gates as Record<string, unknown>), human_review: true } };
    const posted = await fetch(`${base}/policy/observed`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(reported),
    });
    expect(posted.status).toBe(204);

    const realFetch = globalThis.fetch;
    let receivedOnboarding: () => void = () => undefined;
    const onboardingRequested = new Promise<void>((resolve) => {
      receivedOnboarding = resolve;
    });
    const onboardingHeld = new Promise<void>((resolve) => {
      releaseRead = resolve;
    });
    const requests = vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
      if (delivery === "deferred" && (init?.method ?? "GET") === "GET" && String(input).endsWith("/projects/proj_alpha/onboarding")) {
        receivedOnboarding();
        await onboardingHeld;
      }
      return realFetch(input, init);
    });
    await renderSettings();
    if (delivery === "deferred") {
      await onboardingRequested;
      expect(screen.queryByText("A runner is waiting for a new policy.")).toBeNull();
      releaseRead!();
    }
    expect(await screen.findByText("A runner is waiting for a new policy.", {}, httpWait)).toBeTruthy();
    expect(screen.getByText("Needs approval")).toBeTruthy();
    const review = screen.getByText(/Review policy from/).closest("details")!;
    expect(review.open).toBe(false);
    review.open = true;
    expect(review.textContent).toContain('"human_review": true');
    const approval = (await screen.findByText("Approval details", {}, httpWait)).closest("details")!;
    expect(approval.open).toBe(false);
    approval.open = true;
    expect(review.textContent).toContain("pol_changed");
    expect(approval.textContent).toContain(approvedId);

    fireEvent.click(screen.getAllByRole("button", { name: "Approve updated policy" })[0]!);
    await waitFor(() => expect(screen.queryByText("A runner is waiting for a new policy.")).toBeNull(), httpWait);
    const after = (await (await fetch(`${base}/policy`)).json()) as { policy: Record<string, unknown> };
    expect(after.policy.policy_id).toBe("pol_changed");
    const approvalCall = requests.mock.calls.find(([url, init]) => init?.method === "PUT" && String(url).endsWith("/onboarding/policy"));
    expect(JSON.parse(String(approvalCall?.[1]?.body)).policy.gates.human_review).toBe(true);
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
    const policyHeld = new Promise<void>((resolve) => {
      releaseRead = resolve;
    });
    const requests = vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
      if ((init?.method ?? "GET") === "GET" && String(input).endsWith("/projects/proj_alpha/policy")) await policyHeld;
      return realFetch(input, init);
    });
    await renderSettings();
    fireEvent.click(await screen.findByRole("button", { name: "Approve updated policy pol_slow" }, httpWait));
    await waitFor(async () => {
      const after = (await (await realFetch(`${base}/policy`)).json()) as { policy: Record<string, unknown> };
      expect(after.policy.policy_id).toBe("pol_slow");
    }, httpWait);
    const put = requests.mock.calls.find(([, init]) => init?.method === "PUT");
    expect(JSON.parse(String(put?.[1]?.body)).expected_policy_id).toBe(current.policy.policy_id);
    releaseRead!();
    requests.mockRestore();
  });

  it("explains conflicting runner configurations and does not alternate approvals", async () => {
    const { base } = await mount();
    const current = (await (await fetch(`${base}/policy`)).json()) as { policy: Record<string, unknown> };
    for (const [runner, id] of [["runner_a", "pol_a"], ["runner_b", "pol_b"]] as const) {
      await fetch(`${base}/policy/observed?runner=${runner}`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ ...current.policy, policy_id: id }),
      });
    }
    await renderSettings();
    expect(await screen.findByText("Runners have conflicting project configurations.", {}, httpWait)).toBeTruthy();
    expect(screen.getByText(/Inspect and approve one shared configuration/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: /Approve updated policy/ })).toBeNull();
    expect(screen.getByText("Review policy from runner_a")).toBeTruthy();
    expect(screen.getByText("Review policy from runner_b")).toBeTruthy();
    const after = (await (await fetch(`${base}/policy`)).json()) as { policy: Record<string, unknown> };
    expect(after.policy.policy_id).toBe(current.policy.policy_id);
  });

  it("approves a pasted descriptor and refuses one that is not JSON", async () => {
    const { base } = await mount();
    const current = (await (await fetch(`${base}/policy`)).json()) as { policy: Record<string, unknown> };
    await renderSettings();
    fireEvent.click(await screen.findByRole("button", { name: "Paste policy" }, httpWait));
    const box = screen.getByLabelText("Policy descriptor");
    fireEvent.change(box, { target: { value: "not json" } });
    fireEvent.click(screen.getByRole("button", { name: "Approve policy" }));
    expect(await screen.findByText(/The pasted descriptor is not JSON/, {}, httpWait)).toBeTruthy();

    fireEvent.change(box, { target: { value: JSON.stringify({ ...current.policy, policy_id: "pol_pasted" }) } });
    fireEvent.click(screen.getByRole("button", { name: "Approve policy" }));
    await waitFor(async () => {
      const after = (await (await fetch(`${base}/policy`)).json()) as { policy: Record<string, unknown> };
      expect(after.policy.policy_id).toBe("pol_pasted");
    }, httpWait);
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
