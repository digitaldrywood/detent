// @vitest-environment jsdom
//
// `/support` (decisions.md §12, "Plan, billing, support"): the one screen a
// staff account reaches before it has any access, so it renders without the
// bootstrap and has to find the organization somewhere else.
import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import {
  SupportCard,
  supportCSRF,
  supportOrganization,
} from "../../src/app/account/Support.tsx";

afterEach(cleanup);

describe("supportOrganization", () => {
  it("reads the organization out of the path", () => {
    expect(supportOrganization({ pathname: "/support/org_7" })).toBe("org_7");
  });

  it("falls back to the query string", () => {
    expect(
      supportOrganization({
        pathname: "/support",
        search: "?organization=org_9",
      }),
    ).toBe("org_9");
  });

  it("falls back to what the refused bootstrap named", () => {
    expect(
      supportOrganization({
        pathname: "/support",
        body: { details: { organization: "org_3" } },
      }),
    ).toBe("org_3");
  });

  it("finds nothing when nothing names one", () => {
    expect(
      supportOrganization({
        pathname: "/support",
        body: { code: "forbidden" },
      }),
    ).toBeNull();
  });
});

describe("supportCSRF", () => {
  it.each([
    {
      name: "a refused bootstrap with a token",
      body: { details: { organization: "org_3", csrf_token: "csrf_3" } },
      want: "csrf_3",
    },
    {
      name: "a refusal without a token",
      body: { details: { organization: "org_3" } },
      want: null,
    },
    {
      name: "an empty token",
      body: { details: { csrf_token: "" } },
      want: null,
    },
    { name: "no body", body: undefined, want: null },
  ])("reads $name", ({ body, want }) => {
    expect(supportCSRF(body)).toBe(want);
  });
});

describe("SupportCard", () => {
  it("asks the reader to sign in as a support actor when no organization is named", () => {
    render(<SupportCard organization={null} />);
    expect(screen.getByTestId("support-no-organization").textContent).toContain(
      "Sign in as a support actor",
    );
    expect(screen.queryByTestId("support-start")).toBeNull();
  });

  it("starts the window against the named organization, then names the provider step", async () => {
    const onStarted = vi.fn();
    const fetchImpl = vi.fn(
      async () =>
        new Response(
          JSON.stringify({
            support: {
              actor: "support@example.test",
              reason: "",
              expires_at: "2026-09-10T22:30:00Z",
            },
          }),
          { status: 200 },
        ),
    );
    render(
      <SupportCard
        organization="org_7"
        csrfToken="csrf_1"
        fetchImpl={fetchImpl as unknown as typeof globalThis.fetch}
        onStarted={onStarted}
      />,
    );
    fireEvent.click(screen.getByTestId("support-start"));
    await waitFor(() => expect(onStarted).toHaveBeenCalled());
    expect(fetchImpl).toHaveBeenCalledWith(
      "/api/v2/organizations/org_7/support/start",
      expect.objectContaining({
        method: "POST",
        headers: expect.objectContaining({ "X-CSRF-Token": "csrf_1" }),
      }),
    );
    const started = await screen.findByTestId("support-started");
    expect(started.textContent).toContain("impersonate the customer");
    expect(started.querySelector("time")?.getAttribute("dateTime")).toBe(
      "2026-09-10T22:30:00Z",
    );
    expect(screen.queryByTestId("support-start")).toBeNull();
  });

  it("says what the hub said when it refuses", async () => {
    const fetchImpl = vi.fn(
      async () =>
        new Response(
          JSON.stringify({
            code: "forbidden",
            message: "Not a support actor.",
          }),
          {
            status: 403,
          },
        ),
    );
    render(
      <SupportCard
        organization="org_7"
        fetchImpl={fetchImpl as unknown as typeof globalThis.fetch}
      />,
    );
    fireEvent.click(screen.getByTestId("support-start"));
    expect((await screen.findByTestId("support-error")).textContent).toBe(
      "Not a support actor.",
    );
  });
});
