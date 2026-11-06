import { describe, expect, it } from "vitest";

import { formatPageTitle } from "../src/app/pageTitle.ts";

describe("browser page titles", () => {
  it.each([
    { page: "Work", context: "Acme", title: "Work · Acme · Detent" },
    { page: "Runners", context: "Acme", title: "Runners · Acme · Detent" },
    { page: "Issue #12: Fix access", context: "Project A", title: "Issue #12: Fix access · Project A · Detent" },
    { page: "Sign in", context: null, title: "Sign in · Detent" },
    { page: " Chat ", context: " Acme ", title: "Chat · Acme · Detent" },
  ])("formats $page with its context", ({ page, context, title }) => {
    expect(formatPageTitle(page, context)).toBe(title);
  });
});
