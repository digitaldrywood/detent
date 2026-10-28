// @vitest-environment jsdom
//
// The organization billing screen: the state sentences, the Checkout return
// notice, and the portal and checkout controls against a scripted API.
import { Profiler } from "react";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { createMemoryHistory, createRootRoute, createRoute, createRouter, RouterProvider } from "@tanstack/react-router";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { ClientContext } from "../../src/app/client.ts";
import type { AICredits, BillingReport } from "../../src/contracts/account.ts";
import { billingReturnNotice, billingStatusText, BillingSettings } from "../../src/app/settings/Settings.tsx";

if (typeof globalThis.PointerEvent === "undefined") {
  globalThis.PointerEvent = globalThis.MouseEvent as unknown as typeof PointerEvent;
}

const assign = vi.fn();

function credits(overrides: Partial<AICredits> = {}): AICredits {
  return {
    balance_micros: 99_820_000, auto_enabled: false, threshold_cents: 1000,
    price_id: "price_25", failure: "", in_flight: false, can_auto_fund: false,
    packs: [{ price_id: "price_25", label: "$25 credits", usd_cents: 2500 }, { price_id: "price_50", label: "$50 credits", usd_cents: 5000 }],
    history: [], next_cursor: "", total_rows: 0, ...overrides,
  };
}
const chatUsage = {
  range: { from: "2026-10-01T00:00:00Z", to: "2026-11-01T00:00:00Z" },
  input: 1_000_000, cached_input: 400_000, output: 100_000, reasoning_output: 30_000,
  tokens: 1_100_000, turns: 2, unpriced_turns: 0, cost_usd: 1234.567,
};

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
      name: "Free plan", monthly_usd_cents: 0,
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
  beforeEach(() => {
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date("2026-10-15T00:00:00Z"));
  });
  afterEach(() => {
    vi.useRealTimers();
  });

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
    expect(billingStatusText(value, new Date("2026-10-15T00:00:00Z")).title).toBe(title);
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

function fakeApi(value: BillingReport, nextPage?: AICredits) {
  const calls: { url: string; body: unknown }[] = [];
  const fetchImpl = vi.fn(async (input: string, init?: RequestInit) => {
    const url = String(input);
    calls.push({ url, body: init?.body === undefined ? undefined : JSON.parse(String(init.body)) });
    if (url.includes("/billing/credits/transactions?")) return new Response(JSON.stringify(nextPage ?? credits()), { status: 200 });
    if (url.endsWith("/billing/credits/auto-fund")) return new Response(null, { status: 204 });
    if (url.endsWith("/billing")) return new Response(JSON.stringify(value), { status: 200 });
    if (url.endsWith("/billing/checkout") || url.endsWith("/billing/credits/checkout")) return new Response(JSON.stringify({ url: "https://checkout.stripe.com/c/pay/test" }), { status: 200 });
    return new Response(JSON.stringify({ url: "https://billing.stripe.com/p/session/test" }), { status: 200 });
  });
  vi.stubGlobal("fetch", fetchImpl);
  return { calls };
}

function renderBilling(_api: ReturnType<typeof fakeApi>, onCommit?: () => void) {
  const client = { http: { origin: "", apiBase: "/api/v2/organizations/org_a", csrfToken: "csrf" } };
  const root = createRootRoute();
  const section = createRoute({ getParentRoute: () => root, path: "/settings/$section", component: () => onCommit ? <Profiler id="billing" onRender={onCommit}><BillingSettings /></Profiler> : <BillingSettings /> });
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
  it.each([
    ["Team", 9_900_000, 2_400_000],
    ["Team exhausted", 9_900_000, 9_900_000],
    ["Free", 0, 0],
    ["Legacy pilot", 0, 0],
  ])("shows the included allowance for %s", async (_name, allowance, used) => {
    const included = allowance > 0 ? { plan_name: "Team", allowance_micros: allowance, used_micros: used, period_start: "2026-10-08T00:00:00Z", period_end: "2026-11-08T00:00:00Z" } : undefined;
    renderBilling(fakeApi(report({}, { ai_credits: credits({ included }), chat_usage: chatUsage })));
    await screen.findByText("Charged this period");
    if (allowance === 0) {
      expect(screen.queryByRole("meter")).toBeNull();
    } else {
      const meter = screen.getByRole("meter", { name: "Included with Team" });
      expect(meter.getAttribute("aria-valuetext")).toBe(`$${(used / 1_000_000).toFixed(2)} / $9.90 used`);
      expect(screen.getByText("Resets Nov 8 · then purchased credits are used")).toBeTruthy();
    }
  });

  it("shows charged spend rather than model cost in the credit tiles", async () => {
    renderBilling(fakeApi(report({}, { ai_credits: credits(), charged_ai_micros: 180_000, chat_usage: chatUsage })));
    expect(await screen.findByText("Charged this period")).toBeTruthy();
    expect(screen.getByText("$0.18")).toBeTruthy();
    expect(screen.getByText("$99.82")).toBeTruthy();
    expect(screen.getByText("1.1M")).toBeTruthy();
    expect(screen.getByText("Tokens · 2 turns")).toBeTruthy();
    expect(screen.getByTitle(/Input: 1,000,000 · Cached input: 400,000/)).toBeTruthy();
    expect(screen.queryByText(/1234/)).toBeNull();
    expect(screen.getByRole("button", { name: "Refresh balance" })).toBeTruthy();
  });

  it("shows a complimentary plan, its expiry and price without raw ids or capacity diagnostics", async () => {
    const value = report();
    renderBilling(fakeApi(report({}, { entitlement: { ...value.entitlement, name: "Legacy complimentary pilot", source: "grant", effective_base: { id: "pilot_free", version: 1 }, monthly_usd_cents: 9900, grants: [{ id: "grant_a", plan: { id: "pilot_free", version: 1 }, starts_at: "2026-01-01T00:00:00Z", expires_at: "2099-12-31T00:00:00Z" }] } })));
    expect(await screen.findByText("Legacy complimentary pilot")).toBeTruthy();
    expect(screen.getByText("$0/mo")).toBeTruthy();
    expect(screen.getByText("Complimentary")).toBeTruthy();
    expect(screen.getByText(/Expires .*2099 · \d+ days left/)).toBeTruthy();
    expect(screen.queryByText(/pilot_free|Reconciled|Capacity/)).toBeNull();
    expect(screen.getByRole("link", { name: "Change plan →" }).getAttribute("href")).toBe("/settings/plan");
  });

  it.each([
    { status: "active", label: "Renews", date: "Nov 1, 2099" },
    { status: "canceling", label: "Cancels", date: "Nov 1, 2099" },
    { status: "trialing", label: "Trial ends", date: "Oct 20, 2099" },
    { status: "grace", label: "Access until", date: "Oct 22, 2099" },
    { status: "payment_failed", label: "Access until", date: "Oct 22, 2099" },
  ])("shows paid plan price and the $status deadline", async ({ status, label, date }) => {
    renderBilling(fakeApi(report({ status, access_until: "2099-10-20T00:00:00Z", grace_until: "2099-10-22T00:00:00Z", subscription: { subscription_id: "sub_example", price_id: "price_team", status: status === "trialing" ? "trialing" : "active", period_end: "2099-11-01T00:00:00Z", cancel_at_period_end: status === "canceling" } }, { entitlement: { ...report().entitlement, name: "Team", monthly_usd_cents: 9900 } })));
    expect(await screen.findByText("Team")).toBeTruthy();
    expect(screen.getByText("$99/mo")).toBeTruthy();
    expect(screen.getByText(`${label} ${date}`)).toBeTruthy();
    expect(screen.queryByText("Complimentary")).toBeNull();
  });

  it("keeps billing unavailable for a refused support or member session", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify({ code: "forbidden", message: "Forbidden" }), { status: 403 })));
    renderBilling({ calls: [] });
    expect(await screen.findByText("Billing is not available")).toBeTruthy();
    expect(screen.getByText("Billing needs owner access, and is closed during a support session.")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Buy" })).toBeNull();
  });

  it("locks auto top-up without a payment method and offers one purchase picker", async () => {
    const api = fakeApi(report({}, { ai_credits: credits(), chat_usage: chatUsage }));
    renderBilling(api);
    expect(await screen.findByRole("button", { name: "Buy" })).toBeTruthy();
    expect(screen.getAllByRole("combobox")).toHaveLength(2);
    expect(screen.getByRole("combobox", { name: "Buy credit pack" }).textContent).toContain("$25 credits · $25.00");
    expect((screen.getByRole("combobox", { name: "Auto top-up credit pack" }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole("spinbutton", { name: "Top up below (USD)" }) as HTMLInputElement).disabled).toBe(true);
    expect(screen.getByRole("switch", { name: "Auto top-up" }).getAttribute("aria-disabled")).toBe("true");
    act(() => {
      document.dispatchEvent(new KeyboardEvent("keydown", { key: "Tab" }));
      screen.getByRole("button", { name: "Auto top-up locked" }).focus();
    });
    expect(await screen.findByText("Buy credits or save a payment method in the billing portal first.")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Buy" }));
    await waitFor(() => expect(assign).toHaveBeenCalledWith("https://checkout.stripe.com/c/pay/test"));
    expect(api.calls.find((call) => call.url.endsWith("/credits/checkout"))?.body).toMatchObject({ price: "price_25" });
  });

  it.each([false, true])("formats grouped transactions and fetches the next cursor page on demand (click at commit: %s)", async (clickAtCommit) => {
    const history = Array.from({ length: 10 }, (_, index) => ({ at: `2026-10-${String(25 - index).padStart(2, "0")}T12:00:00Z`, kind: "usage", amount_micros: index === 0 ? -123 : -12345, turns: index + 1 }));
    const api = fakeApi(report({}, { ai_credits: credits({ history, total_rows: 12, next_cursor: "cursor-next" }), chat_usage: chatUsage }), credits({ history: [{ at: "2026-10-01T12:00:00Z", kind: "complimentary", amount_micros: 100_000_000 }, { at: "2026-10-01T00:00:00Z", kind: "purchase", amount_micros: 25_000_000 }], total_rows: 12 }));
    let clickedAtCommit = false;
    renderBilling(api, clickAtCommit ? () => {
      if (clickedAtCommit) return;
      const button = screen.queryByRole("button", { name: "Next transactions" }) as HTMLButtonElement | null;
      if (!button || button.disabled) return;
      clickedAtCommit = true;
      button.click();
    } : undefined);
    if (!clickAtCommit) {
      expect(await screen.findByText("1–10 of 12 transactions")).toBeTruthy();
      const table = screen.getByRole("table", { name: "Credit transactions" });
      expect(within(table).getAllByRole("row")).toHaveLength(11);
      expect(screen.getByText("Chat usage · 1 turn")).toBeTruthy();
      expect(screen.getByText("Chat usage · 10 turns")).toBeTruthy();
      expect(screen.getByTitle("$-0.000123").textContent).toBe("−<$0.01");
      expect(screen.getAllByText("−$0.01")).toHaveLength(9);
      const csv = screen.getByRole("link", { name: "Download CSV" }).getAttribute("href")!;
      expect(csv).toContain("format=csv");
      expect(new URL(csv, "https://example.test").searchParams.get("from")).toBe(chatUsage.range.from);
      fireEvent.click(screen.getByRole("button", { name: "Next transactions" }));
    }
    expect(await screen.findByText("11–12 of 12 transactions")).toBeTruthy();
    expect(clickedAtCommit).toBe(clickAtCommit);
    expect(screen.getByText("+$100.00")).toBeTruthy();
    expect(screen.getByText("+$25.00")).toBeTruthy();
    expect(screen.getByText("Credit purchase")).toBeTruthy();
    expect(api.calls.filter((call) => call.url.includes("cursor=cursor-next"))).toHaveLength(1);
    expect((screen.getByRole("button", { name: "Next transactions" }) as HTMLButtonElement).disabled).toBe(true);
    fireEvent.click(screen.getByRole("button", { name: "Previous transactions" }));
    expect(await screen.findByText("1–10 of 12 transactions")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Next transactions" }));
    expect(await screen.findByText("11–12 of 12 transactions")).toBeTruthy();
    expect(api.calls.filter((call) => call.url.includes("cursor=cursor-next"))).toHaveLength(1);
  });

  it("saves thresholds and switches auto top-up using the existing mutation", async () => {
    const api = fakeApi(report({}, { ai_credits: credits({ can_auto_fund: true }), chat_usage: chatUsage }));
    renderBilling(api);
    const input = await screen.findByRole("spinbutton", { name: "Top up below (USD)" });
    fireEvent.change(input, { target: { value: "12.34" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(api.calls.find((call) => call.url.endsWith("/auto-fund"))?.body).toEqual({ enabled: false, price: "price_25", threshold_cents: 1234 }));
    await waitFor(() => expect(screen.getByRole("switch", { name: "Auto top-up" }).getAttribute("aria-disabled")).not.toBe("true"));
    fireEvent.click(screen.getByRole("switch", { name: "Auto top-up" }));
    await waitFor(() => expect(api.calls.filter((call) => call.url.endsWith("/auto-fund")).at(-1)?.body).toMatchObject({ enabled: true }));
  });

  it("renders pending top-up, pricing and failed-payment states", async () => {
    renderBilling(fakeApi(report({}, { ai_credits: credits({ failure: "declined", in_flight: true }), chat_usage: { ...chatUsage, unpriced_turns: 3 } })));
    expect(await screen.findByText("Top-up pending")).toBeTruthy();
    expect(screen.getByText("3 turns unpriced")).toBeTruthy();
    expect(screen.getByText("Needs attention")).toBeTruthy();
    expect(screen.getByText("No credit transactions yet.")).toBeTruthy();
  });

  it.each([false, true])("opens checkout with a selected creation plan: %s", async (created) => {
    vi.stubGlobal("location", { assign, search: created ? "?checkout_price=price_team" : "", pathname: "/settings/billing" });
    const api = fakeApi(report());
    renderBilling(api);
    expect(await screen.findByText("Free plan")).toBeTruthy();
    expect((screen.getByRole("button", { name: "Billing portal" }) as HTMLButtonElement).disabled).toBe(true);
    if (!created) { expect(screen.queryByRole("button", { name: "Upgrade" })).toBeNull(); expect(assign).not.toHaveBeenCalled(); return; }
    await waitFor(() => expect(assign).toHaveBeenCalledWith("https://checkout.stripe.com/c/pay/test"));
    const checkout = api.calls.find((call) => call.url.endsWith("/billing/checkout"));
    expect((checkout?.body as { price: string }).price).toBe("price_team");
  });

  it("offers the portal to a customer and shows a pending checkout", async () => {
    const api = fakeApi(report({ status: "active", plan: { id: "team", version: 1 } }, { can_manage: true, can_checkout: false, checkout_pending: true }));
    renderBilling(api);
    expect(await screen.findByRole("button", { name: "Billing portal" })).toBeTruthy();
    expect(screen.queryByText("Checkout in progress")).toBeNull();
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
    expect(await screen.findByText(/Renews .*2099/, {}, { timeout: 5_000 })).toBeTruthy();
  }, 10_000);

  it("shows the Checkout return notice", async () => {
    vi.stubGlobal("location", { assign, search: "?checkout=returned", pathname: "/settings/billing" });
    renderBilling(fakeApi(report()));
    expect((await screen.findByRole("status")).textContent).toContain("Back from checkout");
  });
});
