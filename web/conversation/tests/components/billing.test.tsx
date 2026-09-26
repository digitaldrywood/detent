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
    [report({}, { entitlement: { ...report().entitlement, source: "grant" } }), "Complimentary access"],
    [report({ status: "active", plan: { id: "team", version: 1 }, paid_through: "2026-10-26T00:00:00Z" }), "Subscribed"],
    [report({ status: "grace", grace_until: "2026-10-01T00:00:00Z" }), "Payment needed"],
    [report({ status: "payment_failed" }), "Payment failed"],
    [report({ status: "canceling", access_until: "2026-10-26T00:00:00Z" }), "Canceling"],
    [report({ status: "unapproved_plan" }), "Subscription unapproved plan"],
  ])("names %#", (value, title) => {
    expect(billingStatusText(value).title).toBe(title);
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
  it("opens Checkout for a price and keeps the portal closed without a customer", async () => {
    const api = fakeApi(report());
    renderBilling(api);
    expect(await screen.findByText("Free plan")).toBeTruthy();
    expect((screen.getByRole("button", { name: "Billing portal" }) as HTMLButtonElement).disabled).toBe(true);
    fireEvent.click(screen.getByRole("button", { name: "Subscribe" }));
    await waitFor(() => expect(assign).toHaveBeenCalledWith("https://checkout.stripe.com/c/pay/test"));
    const checkout = api.calls.find((call) => call.url.endsWith("/billing/checkout"));
    expect((checkout?.body as { price: string }).price).toBe("price_team");
  });

  it("offers the portal to a customer and shows a pending checkout", async () => {
    const api = fakeApi(report({ status: "active", plan: { id: "team", version: 1 } }, { can_manage: true, can_checkout: false, checkout_pending: true }));
    renderBilling(api);
    expect(await screen.findByText("Checkout in progress")).toBeTruthy();
    expect((screen.getByRole("button", { name: "Subscribe" }) as HTMLButtonElement).disabled).toBe(true);
    fireEvent.click(screen.getByRole("button", { name: "Billing portal" }));
    await waitFor(() => expect(assign).toHaveBeenCalledWith("https://billing.stripe.com/p/session/test"));
  });

  it("shows the Checkout return notice", async () => {
    vi.stubGlobal("location", { assign, search: "?checkout=returned", pathname: "/settings/billing" });
    renderBilling(fakeApi(report()));
    expect((await screen.findByRole("status")).textContent).toContain("Back from checkout");
  });
});
