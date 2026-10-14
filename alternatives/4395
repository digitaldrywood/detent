// @vitest-environment jsdom
//
// The organization billing screen: the state sentences, the Checkout return
// notice, and the portal and checkout controls against a scripted API.
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { createMemoryHistory, createRootRoute, createRoute, createRouter, RouterProvider } from "@tanstack/react-router";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { ClientContext } from "../../src/app/client.ts";
import type { BillingReport } from "../../src/contracts/account.ts";
import { billingReturnNotice, billingStatusText, BillingSettings } from "../../src/app/settings/Settings.tsx";

if (typeof globalThis.PointerEvent === "undefined") {
  globalThis.PointerEvent = globalThis.MouseEvent as unknown as typeof PointerEvent;
}

const assign = vi.fn();

beforeEach(() => {
  assign.mockReset();
  vi.stubGlobal("location", { assign, search: "", pathname: "/settings/billing" });
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const zero = "0001-01-01T00:00:00Z";

function report(overrides: Partial<BillingReport["state"]> = {}, extra: Partial<BillingReport> = {}): BillingReport {
  return {
    organization_id: "org_a",
    state: {
      subscription: { subscription_id: "", price_id: "", status: "" },
      status: "free",
      plan: { id: "", version: 0 },
      paid_through: zero,
      access_until: zero,
      grace_until: zero,
      ...overrides,
    },
    entitlement: {
      organization_id: "org_a",
      base: { id: "free", version: 1 },
      effective_base: { id: "free", version: 1 },
      source: "base",
      revision: 1,
      allowances: {},
      usage: {},
      window_ends_at: zero,
    },
    reconciled_at: "",
    pending_events: 0,
    prices: [{ id: "price_team", label: "Team" }],
    can_checkout: true,
    can_manage: false,
    ...extra,
  } as BillingReport;
}

describe("billing state text", () => {
  it.each([
    [report(), "Free plan"],
    [report({}, { entitlement: { ...report().entitlement, effective_base: { id: "payment_pending", version: 1 } } }), "Payment pending"],
    [report({}, { entitlement: { ...report().entitlement, name: "Starter", monthly_usd_cents: 4900, effective_base: { id: "starter", version: 1 } } }), "Complimentary access"],
    [report({}, { entitlement: { ...report().entitlement, source: "grant" } }), "Complimentary access"],
    [report({ status: "active", plan: { id: "team", version: 1 }, paid_through: "2026-10-26T00:00:00Z" }), "Subscribed"],
    [report({ status: "grace", grace_until: "2026-10-01T00:00:00Z" }), "Payment needed"],
    [report({ status: "payment_failed" }), "Payment failed"],
    [report({ status: "canceling", access_until: "2026-10-26T00:00:00Z" }), "Canceling"],
    [report({ status: "unapproved_plan" }), "Subscription unapproved plan"],
    [report({ status: "multiple_subscriptions" }), "Billing needs attention"],
  ])("names %#", (value, title) => {
    expect(billingStatusText(value).title).toBe(title);
  });

  it("reads the access deadlines for an active subscription", () => {
    const now = new Date("2026-10-15T00:00:00Z");
    expect(billingStatusText(report({ status: "active", paid_through: "2026-10-01T00:00:00Z", access_until: "2026-10-01T00:00:00Z" }), now).title).toBe("Access ended");
    expect(billingStatusText(report({ status: "active", paid_through: "2026-10-01T00:00:00Z", access_until: "2026-10-20T00:00:00Z" }), now).title).toBe("Renewal pending");
    expect(billingStatusText(report({ status: "active", paid_through: "2026-11-01T00:00:00Z", access_until: "2026-11-02T00:00:00Z" }), now).title).toBe("Subscribed");
  });

  it("recognizes the Checkout return only", () => {
    expect(billingReturnNotice("?checkout=returned")).toContain("Back from checkout");
    expect(billingReturnNotice("")).toBeNull();
    expect(billingReturnNotice("?checkout=other")).toBeNull();
  });
});

function fakeApi(value: BillingReport) {
  const calls: { url: string; body: unknown }[] = [];
  const fetchImpl = vi.fn(async (input: string, init?: RequestInit) => {
    const url = String(input);
    calls.push({ url, body: init?.body === undefined ? undefined : JSON.parse(String(init.body)) });
    if (url.endsWith("/billing")) return new Response(JSON.stringify(value), { status: 200 });
    if (url.endsWith("/billing/checkout")) return new Response(JSON.stringify({ url: "https://checkout.stripe.com/c/pay/test" }), { status: 200 });
    return new Response(JSON.stringify({ url: "https://billing.stripe.com/p/session/test" }), { status: 200 });
  });
  vi.stubGlobal("fetch", fetchImpl);
  return { calls };
}

function renderBilling(_api: ReturnType<typeof fakeApi>) {
  const client = { http: { origin: "", apiBase: "/api/v2/organizations/org_a", csrfToken: "csrf" } };
  const root = createRootRoute();
  const section = createRoute({ getParentRoute: () => root, path: "/settings/$section", component: () => <BillingSettings /> });
  const router = createRouter({
    routeTree: root.addChildren([section]),
    history: createMemoryHistory({ initialEntries: ["/settings/billing"] }),
  });
  return render(
    <ClientContext.Provider value={client as never}>
      <RouterProvider router={router as never} />
    </ClientContext.Provider>,
  );
}

describe("billing screen", () => {
  it("shows metered tokens and dollars for the billing period", async () => {
    renderBilling(fakeApi(report({}, { chat_usage: {
      range: { from: "2026-09-01T00:00:00Z", to: "2026-10-01T00:00:00Z" },
      input: 1_000_000, cached_input: 400_000, output: 100_000, reasoning_output: 30_000,
      tokens: 1_100_000, turns: 2, unpriced_turns: 1, cost_usd: .114,
    } })));
    expect(await screen.findByText("AI usage this billing period")).toBeTruthy();
    expect(screen.getByText("1,100,000 tokens · $0.114000 USD")).toBeTruthy();
    expect(screen.getByText(/2026-09-01 – 2026-10-01 · 2 turns · 1 turn awaiting pricing/)).toBeTruthy();
  });

  it("shows capacity, remaining space and contextual downgrade limits", async () => {
    const base = report();
    renderBilling(fakeApi(report({}, { entitlement: { ...base.entitlement, name: "Free", monthly_usd_cents: 0, allowances: { projects: 1, unarchived_issues: 200 }, usage: { projects: 2, unarchived_issues: 190 } } })));
    expect(await screen.findByText("$0 per organization/month · no card required")).toBeTruthy();
    expect(screen.getByText("over limit")).toBeTruthy();
    expect(screen.getByText("0 remaining")).toBeTruthy();
    expect(screen.getByText("10 remaining")).toBeTruthy();
    expect(screen.getByText(/no AI-dollar allowance is included/)).toBeTruthy();
  });

  it.each([false, true])("opens checkout with a selected creation plan: %s", async (created) => {
    vi.stubGlobal("location", { assign, search: created ? "?checkout_price=price_team" : "", pathname: "/settings/billing" });
    const api = fakeApi(report());
    renderBilling(api);
    expect(await screen.findByText("Free plan")).toBeTruthy();
    expect((screen.getByRole("button", { name: "Billing portal" }) as HTMLButtonElement).disabled).toBe(true);
    if (!created) fireEvent.click(screen.getByRole("button", { name: "Upgrade" }));
    await waitFor(() => expect(assign).toHaveBeenCalledWith("https://checkout.stripe.com/c/pay/test"));
    const checkout = api.calls.find((call) => call.url.endsWith("/billing/checkout"));
    expect((checkout?.body as { price: string }).price).toBe("price_team");
  });

  it("offers the portal to a customer and shows a pending checkout", async () => {
    const api = fakeApi(report({ status: "active", plan: { id: "team", version: 1 } }, { can_manage: true, can_checkout: false, checkout_pending: true }));
    renderBilling(api);
    expect(await screen.findByText("Checkout in progress")).toBeTruthy();
    expect((screen.getByRole("button", { name: "Upgrade" }) as HTMLButtonElement).disabled).toBe(true);
    fireEvent.click(screen.getByRole("button", { name: "Billing portal" }));
    await waitFor(() => expect(assign).toHaveBeenCalledWith("https://billing.stripe.com/p/session/test"));
  });

  it("re-reads billing after a Checkout return until Stripe confirms it", async () => {
    vi.stubGlobal("location", { assign, search: "?checkout=returned", pathname: "/settings/billing" });
    const confirmed = report({ status: "active", plan: { id: "team", version: 1 }, paid_through: "2099-01-01T00:00:00Z", access_until: "2099-01-02T00:00:00Z" }, { can_manage: true, can_checkout: false });
    let reads = 0;
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => {
        reads++;
        return new Response(JSON.stringify(reads < 2 ? report() : confirmed), { status: 200 });
      }),
    );
    renderBilling({ calls: [] });
    expect(await screen.findByText("Free plan")).toBeTruthy();
    expect(await screen.findByText("Subscribed", {}, { timeout: 5_000 })).toBeTruthy();
  }, 10_000);

  it("shows the Checkout return notice", async () => {
    vi.stubGlobal("location", { assign, search: "?checkout=returned", pathname: "/settings/billing" });
    renderBilling(fakeApi(report()));
    expect((await screen.findByRole("status")).textContent).toContain("Back from checkout");
  });
});
