// @vitest-environment jsdom
import { createMemoryHistory } from "@tanstack/react-router";
import { afterEach, describe, expect, it } from "vitest";

import { readLastProject } from "../src/app/client.ts";
import { makeRouter } from "../src/app/router.tsx";
import { routerBasePath } from "../src/runtime/basePath.ts";

afterEach(() => {
  globalThis.localStorage?.clear();
});

describe("legacy project pages", () => {
  it.each([
    ["/projects/prj_a", "/work/p/prj_a"],
    ["/projects/prj_a/changes", "/work/changes"],
    ["/projects/prj_a/issues/wi_1", "/work/i/wi_1"],
    ["/projects/prj_a/issues/wi_1/changes/chg_1", "/work/i/wi_1"],
  ])("%s redirects to %s and remembers the project", async (from, to) => {
    for (const base of ["", "/organizations/org_a"]) {
      globalThis.localStorage?.clear();
      const router = makeRouter(
        createMemoryHistory({ initialEntries: [`${base}${from}`] }),
        routerBasePath(base),
      );
      await router.load();
      expect(router.state.location.pathname).toBe(to);
      expect(readLastProject()).toBe("prj_a");
    }
  });

  it("keeps the project setup route", async () => {
    const router = makeRouter(createMemoryHistory({ initialEntries: ["/projects/prj_a/setup"] }));
    await router.load();
    expect(router.state.location.pathname).toBe("/projects/prj_a/setup");
    expect(readLastProject()).toBeNull();
  });
});
