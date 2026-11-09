// @vitest-environment jsdom
//
// `/support`: the one screen a
// staff account reaches before it has any access, so it renders without the
// bootstrap and has to find the organization somewhere else.
import {
  cleanup,
  render,
  screen,
} from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import {
  SupportCard,
  supportCSRF,
  supportOrganization,
} from "../../src/app/account/Support.tsx";

afterEach(() => { cleanup(); vi.restoreAllMocks(); });

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

  it.each(["org_7/part", "//other.example.test/path", "https://other.example.test/path?x=1#y"])("opens the existing entry support form safely for %s", (organization) => {
    const fetchImpl = vi.spyOn(globalThis, "fetch");
    render(<SupportCard organization={organization} />);
    const link = screen.getByRole("link", { name: "Start the support session" });
    expect(link.getAttribute("href")).toBe(`/support?organization=${encodeURIComponent(organization)}`);
    expect(new URL(link.getAttribute("href")!, "https://app.example.test").origin).toBe("https://app.example.test");
    expect(link.getAttribute("target")).toBeNull();
    expect(fetchImpl).not.toHaveBeenCalled();
  });
});
