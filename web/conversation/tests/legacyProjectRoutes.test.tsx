// @vitest-environment jsdom
import { createMemoryHistory } from "@tanstack/react-router";
import { afterEach, describe, expect, it, vi } from "vitest";

import changeFixture from "../src/contracts/fixtures/work-change-detail.json";
import type { ChangeDetail } from "../src/contracts/work.ts";
import { readLastProject } from "../src/app/client.ts";
import { makeRouter } from "../src/app/router.tsx";
import { changeResourceRows, selectChangeId } from "../src/app/work/IssuePage.tsx";
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

  it.each(["", "/organizations/org_a"])("base %j keeps the Change a Change link named", async (base) => {
    const router = makeRouter(
      createMemoryHistory({ initialEntries: [`${base}/projects/prj_a/issues/wi_1/changes/chg_1`] }),
      routerBasePath(base),
    );
    await router.load();
    expect(router.state.location.pathname).toBe("/work/i/wi_1");
    expect(router.state.location.search).toEqual({ change: "chg_1" });
  });

  it("does not invent a Change for a plain issue link", async () => {
    const router = makeRouter(createMemoryHistory({ initialEntries: ["/projects/prj_a/issues/wi_1"] }));
    await router.load();
    expect(router.state.location.search).toEqual({});
  });

  it.each([
    ["the named change", [{ change_id: "c1" }, { change_id: "c2" }], "c1", "c1"],
    ["the latest without a name", [{ change_id: "c1" }, { change_id: "c2" }], null, "c2"],
    ["the latest for an unknown name", [{ change_id: "c1" }, { change_id: "c2" }], "gone", "c2"],
    ["nothing for an issue without changes", [], "c1", null],
  ])("selectChangeId opens %s", (_name, changes, requested, want) => {
    expect(selectChangeId(changes, requested)).toBe(want);
  });

  it("links every Change Request with its current state", () => {
    const open = vi.fn();
    const detail = changeFixture as unknown as ChangeDetail;
    const rows = changeResourceRows([
      { record: { change_id: "change_1", title: "First" }, detail: { ...detail, summary: { ...detail.summary, status: "landed" } } },
      { record: { change_id: "change_2", title: "Second" }, detail: { ...detail, summary: { ...detail.summary, status: "needs_evidence" } } },
      { record: { change_id: "change_3", title: "Third" }, detail: null },
    ], open);
    expect(rows.map(({ key, label, detail }) => [key, label, detail])).toEqual([
      ["change_1", "First", "landed"],
      ["change_2", "Second", "needs evidence"],
      ["change_3", "Third", "unavailable"],
    ]);
    rows[0]!.onOpen();
    rows[2]!.onOpen();
    expect(open.mock.calls).toEqual([["change_1"], ["change_3"]]);
  });

  it("keeps the project setup route", async () => {
    const router = makeRouter(createMemoryHistory({ initialEntries: ["/projects/prj_a/setup"] }));
    await router.load();
    expect(router.state.location.pathname).toBe("/projects/prj_a/setup");
    expect(readLastProject()).toBeNull();
  });
});

describe("the Change Request route", () => {
  it("resolves under the issue that owns the change", async () => {
    const router = makeRouter(
      createMemoryHistory({ initialEntries: ["/work/i/wi_1/changes/change_1"] }),
    );
    await router.load();
    expect(router.state.location.pathname).toBe("/work/i/wi_1/changes/change_1");
    expect(router.state.matches.at(-1)?.params).toEqual({ workItemId: "wi_1", changeId: "change_1" });
  });
});
