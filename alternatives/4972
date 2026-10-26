// @vitest-environment jsdom
import React from "react";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { createMemoryHistory } from "@tanstack/react-router";
import { afterEach, describe, expect, it, vi } from "vitest";

import itemFixture from "../src/contracts/fixtures/work-item.json";
import projectFixture from "../src/contracts/fixtures/work-project.json";
import attemptsFixture from "../src/contracts/fixtures/work-attempt-list.json";
import historyFixture from "../src/contracts/fixtures/work-history.json";
import changeFixture from "../src/contracts/fixtures/work-change-detail.json";
import type { ChangeDetail, CollaborationEvent, NativeAttempt, NativeIssue, NativeProject } from "../src/contracts/work.ts";
import { readLastProject } from "../src/app/client.ts";
import { makeRouter } from "../src/app/router.tsx";
import { changeResourceRows, selectChangeId, useIssue } from "../src/app/work/IssuePage.tsx";
import { WorkApiError, type WorkHttp } from "../src/app/work/lib/workHttp.ts";
import { routerBasePath } from "../src/runtime/basePath.ts";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
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


describe("issue live status", () => {
  async function fixture() {
    const issue = itemFixture as unknown as NativeIssue;
    const project = projectFixture as unknown as NativeProject;
    const initialAttempt = (attemptsFixture as { items: unknown[] }).items[0] as NativeAttempt;
    const event = (historyFixture as { items: unknown[] }).items[0] as CollaborationEvent;
    let sequence = "1";
    let status: NativeAttempt["status"] = "running";
    let hold: Promise<void> | null = null;
    let failure: Error | null = null;
    const sources: EventTarget[] = [];
    vi.stubGlobal("EventSource", class extends EventTarget {
      readyState = 1;
      constructor(readonly url: string) { super(); sources.push(this); }
      close() { this.readyState = 2; }
    });
    const getWorkItemById = vi.fn(async () => {
      if (hold !== null) await hold;
      if (failure !== null) throw failure;
      return { ...issue, state: status === "succeeded" ? "Done" : "In Progress", terminal: status === "succeeded" };
    });
    const eventsUrl = vi.fn((_project: string, _workspace?: string, item?: string) => "/events?work_item=" + item);
    const http = {
      getWorkItemById, eventsUrl,
      getProject: vi.fn(async () => project),
      listAttempts: vi.fn(async () => ({ items: [{ ...initialAttempt, status, updated_at: sequence }] })),
      listHistory: vi.fn(async () => ({ items: [{ ...event, aggregate_sequence: sequence, type: status === "running" ? "run.started" : "run.finished" }] })),
      listComments: vi.fn(async () => ({ items: [] })),
      listChanges: vi.fn(async () => []),
    } as unknown as WorkHttp;
    let renders = 0;
    function Probe() {
      const value = useIssue(http, issue.work_item_id);
      const [draft, setDraft] = React.useState("");
      renders++;
      return <><div data-testid="issue-live-state">{value.data?.attempts[0]?.status}:{value.data?.history.at(-1)?.aggregate_sequence}:{value.data?.issue.state}</div><div data-testid="issue-live-error">{value.error}</div><input aria-label="draft" value={draft} onChange={(event) => setDraft(event.target.value)} /></>;
    }
    render(<Probe />);
    await waitFor(() => expect(sources).toHaveLength(1));
    return {
      getWorkItemById, eventsUrl, issue, project,
      renders: () => renders,
      set: (next: string, nextStatus: NativeAttempt["status"] = "running") => { sequence = next; status = nextStatus; },
      hold: (value: Promise<void> | null) => { hold = value; },
      fail: (value: Error | null) => { failure = value; },
      activity: (value: string) => sources[0]!.dispatchEvent(new MessageEvent("activity", { data: value })),
    };
  }

  it("updates native completion without remounting drafts or reacting to unchanged issue activity", async () => {
    const f = await fixture();
    await waitFor(() => expect(screen.getByTestId("issue-live-state").textContent).toBe("running:1:In Progress"));
    expect(f.eventsUrl).toHaveBeenCalledWith(f.project.project_id, undefined, f.issue.work_item_id);
    const draft = screen.getByRole("textbox", { name: "draft" });
    fireEvent.change(draft, { target: { value: "keep this reply" } });
    const renders = f.renders();
    act(() => { f.activity("1"); f.activity("1"); f.activity("not a sequence"); });
    expect(f.getWorkItemById).toHaveBeenCalledTimes(1);
    expect(f.renders()).toBe(renders);
    f.set("2", "failed");
    act(() => f.activity("2"));
    await waitFor(() => expect(screen.getByTestId("issue-live-state").textContent).toBe("failed:2:In Progress"));
    expect(f.getWorkItemById).toHaveBeenCalledTimes(2);
    expect(screen.getByRole("textbox", { name: "draft" })).toBe(draft);
    expect((draft as HTMLInputElement).value).toBe("keep this reply");
    f.set("3", "succeeded");
    act(() => f.activity("3"));
    await waitFor(() => expect(screen.getByTestId("issue-live-state").textContent).toBe("succeeded:3:Done"));
    expect(screen.getByRole("textbox", { name: "draft" })).toBe(draft);
    expect((draft as HTMLInputElement).value).toBe("keep this reply");
  });

  it("refreshes when the first stream frame is newer than the initial detail read", async () => {
    const f = await fixture();
    await waitFor(() => expect(screen.getByTestId("issue-live-state").textContent).toBe("running:1:In Progress"));
    f.set("2", "succeeded");
    act(() => f.activity("2"));
    await waitFor(() => expect(screen.getByTestId("issue-live-state").textContent).toBe("succeeded:2:Done"));
    expect(f.getWorkItemById).toHaveBeenCalledTimes(2);
  });

  it.each([401, 403, 404])("retains data during a temporary read failure and clears it after access loss %s", async (status) => {
    const f = await fixture();
    await waitFor(() => expect(screen.getByTestId("issue-live-state").textContent).toBe("running:1:In Progress"));
    act(() => f.activity("1"));
    f.fail(new WorkApiError({ status: 503, code: "unavailable", message: "Temporarily unavailable" }));
    act(() => f.activity("2"));
    await waitFor(() => expect(screen.getByTestId("issue-live-error").textContent).toBe("Temporarily unavailable"));
    expect(screen.getByTestId("issue-live-state").textContent).toBe("running:1:In Progress");
    f.fail(new WorkApiError({ status, code: "access_lost", message: "Access lost" }));
    act(() => f.activity("3"));
    await waitFor(() => expect(screen.getByTestId("issue-live-error").textContent).toBe("Access lost"));
    expect(screen.getByTestId("issue-live-state").textContent).toBe("::");
    expect(f.getWorkItemById).toHaveBeenCalledTimes(3);
  });

  it("finishes an in-flight read before one coalesced refresh", async () => {
    const f = await fixture();
    await waitFor(() => expect(screen.getByTestId("issue-live-state").textContent).toBe("running:1:In Progress"));
    let release!: () => void;
    f.hold(new Promise<void>((resolve) => { release = resolve; }));
    f.set("2");
    act(() => f.activity("2"));
    await waitFor(() => expect(f.getWorkItemById).toHaveBeenCalledTimes(2));
    act(() => { f.activity("3"); f.activity("4"); f.activity("5"); });
    expect(f.getWorkItemById).toHaveBeenCalledTimes(2);
    expect(screen.getByTestId("issue-live-state").textContent).toBe("running:1:In Progress");
    f.set("5", "succeeded");
    f.hold(null);
    await act(async () => release());
    await waitFor(() => expect(screen.getByTestId("issue-live-state").textContent).toBe("succeeded:5:Done"));
    expect(f.getWorkItemById).toHaveBeenCalledTimes(3);
  });
});
