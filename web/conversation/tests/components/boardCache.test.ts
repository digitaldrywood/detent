import { afterEach, describe, expect, it, vi } from "vitest";

import { makeAccountApi } from "../../src/app/account/api.ts";
import * as disk from "../../src/app/work/lib/boardDisk.ts";
import { boardAccountKey, clearBoardCache, confirmWorkItem, getBoardRead, rejectBoardCache } from "../../src/app/work/lib/boardStore.ts";
import { DEFAULT_VIEW_STATE } from "../../src/app/work/lib/viewState.ts";
import { makeWorkHttp, type FetchLike } from "../../src/app/work/lib/workHttp.ts";
import { getQueryData, setQueryData } from "../../src/app/lib/queryCache.ts";
import type { NativeIssue } from "../../src/contracts/work.ts";
import { isLive } from "../../src/app/work/lib/model.ts";
import { workPaginationFixture } from "../workPaginationFixture.ts";

afterEach(() => { clearBoardCache(); vi.restoreAllMocks(); });

function deferred<A>() {
  let resolve!: (value: A) => void;
  const promise = new Promise<A>((done) => { resolve = done; });
  return { promise, resolve };
}

async function fixture() {
  const source = workPaginationFixture();
  await source.control();
  const http = makeWorkHttp({ ...source.client.http, fetch: source.fetch });
  const read = getBoardRead(source.client, http, null, { ...DEFAULT_VIEW_STATE, view: "list", tab: "all" });
  return { ...source, http, read };
}

describe("the Work snapshot ordering", () => {
  it("keeps summary windows in the cache identity, page requests and stream", async () => {
    const source = workPaginationFixture();
    await source.control();
    const http = makeWorkHttp({ ...source.client.http, fetch: source.fetch });
    const window = { completedFrom: "2026-10-26T11:00:00Z", completedTo: "2026-10-27T11:00:00Z" };
    const read = getBoardRead(source.client, http, "proj_alpha", { ...DEFAULT_VIEW_STATE, ...window, view: "list", tab: "closed", state: ["Done"] });
    expect(read).not.toBe(getBoardRead(source.client, http, "proj_alpha", { ...DEFAULT_VIEW_STATE, view: "list", tab: "closed", state: ["Done"] }));
    read.start();
    await expect.poll(() => read.snapshot().error ?? read.snapshot().resolved).toBe(true);
    const pages = source.requests.filter(({ url }) => url.pathname.endsWith("/work-items"));
    expect(pages.length).toBeGreaterThan(0);
    for (const { url } of pages) {
      expect(url.searchParams.get("completed_from")).toBe(window.completedFrom);
      expect(url.searchParams.get("completed_to")).toBe(window.completedTo);
    }
    const events = new URL(http.eventsUrl("proj_alpha", undefined, undefined, { since: 0, filters: read.streamFilters("proj_alpha") }), "http://example.test");
    expect(events.searchParams.get("completed_from")).toBe(window.completedFrom);
    expect(events.searchParams.get("completed_to")).toBe(window.completedTo);
  });

  it("loads one page per lane and continues only Backlog", async () => {
    const source = workPaginationFixture();
    await source.control({ activeOverflow: true, backlogOverflow: true });
    const http = makeWorkHttp({ ...source.client.http, fetch: source.fetch });
    const read = getBoardRead(source.client, http, "proj_alpha", DEFAULT_VIEW_STATE);
    read.start();
    await expect.poll(() => read.snapshot().resolved).toBe(true);
    expect(read.snapshot().hasMore).toBe(false);
    expect(read.snapshot().items.filter((item) => item.state !== "Backlog")).toHaveLength(217);
    expect(read.snapshot().items.filter((item) => item.state === "Backlog")).toHaveLength(200);
    expect(read.snapshot().backlogHasMore).toBe(true);
    expect(read.snapshot().backlogTotal).toBe(405);
    expect(read.snapshot().totals?.lanes.Backlog).toBe(405);
    const initial = source.requests.filter(({ url }) => url.pathname.endsWith("/work-items"));
    expect(initial).toHaveLength(6);
    expect(initial.every(({ url }) => url.searchParams.get("include") === "board" && url.searchParams.getAll("state").length === 1)).toBe(true);
    expect(source.requests.filter(({ url }) => /\/(attempts|changes)(\?|$)/.test(url.pathname))).toHaveLength(0);
    const before = source.requests.length;
    read.loadBacklog();
    await expect.poll(() => read.snapshot().backlogLoading).toBe(false);
    expect(read.snapshot().items.filter((item) => item.state === "Backlog")).toHaveLength(400);
    expect(read.snapshot().hasMore).toBe(false);
    const pages = source.requests.slice(before).filter(({ url }) => url.pathname.endsWith("/work-items"));
    expect(pages).toHaveLength(1);
    expect(pages[0]!.url.searchParams.getAll("state")).toEqual(["Backlog"]);
    expect(pages[0]!.url.searchParams.get("include")).toBe("board");
    read.reload();
    await expect.poll(() => read.snapshot().refreshing).toBe(false);
    expect(read.snapshot().items.filter((item) => item.state === "Backlog")).toHaveLength(400);
    read.loadBacklog();
    await expect.poll(() => read.snapshot().backlogHasMore).toBe(false);
    expect(read.snapshot().items.filter((item) => item.state === "Backlog")).toHaveLength(405);
  });

  it.each([
    { name: "reads lanes beside the project when the bootstrap carries the workflow", known: true },
    { name: "reads lanes after the project when the workflow is unknown", known: false },
  ])("$name", async ({ known }) => {
    const source = workPaginationFixture();
    await source.control();
    const project = await makeWorkHttp({ ...source.client.http, fetch: source.fetch }).getProject("proj_alpha");
    const gate = deferred<void>();
    const fetch: FetchLike = async (input, init) => {
      if (new URL(input, "http://fixture.test").pathname.endsWith("/projects/proj_alpha")) await gate.promise;
      return source.fetch(input, init);
    };
    const bootstrap = source.client.bootstrap;
    const client = known
      ? { ...source.client, bootstrap: { ...bootstrap, projects: bootstrap.projects.map((entry) => ({ ...entry, states: project.states })) } }
      : source.client;
    const read = getBoardRead(client, makeWorkHttp({ ...source.client.http, fetch }), "proj_alpha", DEFAULT_VIEW_STATE);
    const lanes = () => source.requests.filter(({ url }) => url.pathname.endsWith("/work-items")).length;
    const before = lanes();
    read.start();
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(lanes() > before).toBe(known);
    gate.resolve();
    await expect.poll(() => read.snapshot().resolved).toBe(true);
    expect(read.snapshot().items.length).toBeGreaterThan(0);
    expect(read.snapshot().lanes.map((lane) => lane.name)).toEqual(project.states.map((state) => state.name));
  });

  it.each(["disk-first", "network-first"])("keeps fresh revisions with %s completion", async (order) => {
    const stored: disk.BoardDiskRecord[] = [];
    vi.spyOn(disk, "updateBoardDisk").mockImplementation(async (_account, record) => { if (record) stored.push(record); });
    vi.spyOn(disk, "readBoardDisk").mockResolvedValue(null);
    const source = await fixture();
    source.read.reload();
    await expect.poll(() => source.read.snapshot().resolved).toBe(true);
    const snapshot = stored.at(-1)!;
    expect(snapshot).toBeDefined();
    expect(snapshot).not.toHaveProperty("live");
    expect(snapshot).not.toHaveProperty("csrfToken");
    const stale = snapshot.entries as { issues: NativeIssue[] }[];
    const id = stale[0]!.issues[0]!.work_item_id;
    const previous = stale[0]!.issues[0]!;
    clearBoardCache();
    const diskRead = deferred<disk.BoardDiskRecord | null>();
    vi.mocked(disk.readBoardDisk).mockReturnValue(diskRead.promise);
    const network = deferred<void>();
    const http = makeWorkHttp({ ...source.client.http, fetch: async (...args) => {
      if (String(args[0]).includes("/work-items?")) await network.promise;
      const response = await source.fetch(...args);
      if (!String(args[0]).includes("/work-items?") || !response.ok) return response;
      const body = await response.json();
      body.items = body.items.map((issue: NativeIssue) => issue.work_item_id === id ? { ...issue, revision: "900", title: "Fresh network title" } : issue);
      if (body.work) body.work.items = body.work.items.map((issue: NativeIssue) => issue.work_item_id === id ? { ...issue, revision: "900", title: "Fresh network title" } : issue);
      return Response.json(body);
    } });
    const read = getBoardRead(source.client, http, null, { ...DEFAULT_VIEW_STATE, view: "list", tab: "all" });
    read.reload();
    if (order === "disk-first") {
      diskRead.resolve(snapshot);
      await expect.poll(() => read.snapshot().cached).toBe(true);
      expect(read.snapshot().items.find((item) => item.id === id)?.title).toBe(previous.title);
      expect(read.snapshot().loading).toBe(false);
      expect(read.snapshot().hasMore).toBe(true);
      expect(read.snapshot().asOf).toBe(snapshot.asOf);
    }
    network.resolve();
    await expect.poll(() => read.snapshot().items.find((item) => item.id === id)?.title).toBe("Fresh network title");
    if (order === "network-first") diskRead.resolve(snapshot);
    await Promise.resolve();
    expect(read.snapshot().items.find((item) => item.id === id)?.revision).toBe("900");
    expect(read.snapshot().cached).toBe(false);
  });

  it.each(["schema", "account", "storage-failure"])("ignores %s snapshots without delaying network reads", async (failure) => {
    const stored: disk.BoardDiskRecord[] = [];
    vi.spyOn(disk, "updateBoardDisk").mockImplementation(async (_account, record) => { if (record) stored.push(record); });
    vi.spyOn(disk, "readBoardDisk").mockResolvedValue(null);
    const source = await fixture();
    source.read.reload();
    await expect.poll(() => source.read.snapshot().resolved).toBe(true);
    const snapshot = stored.at(-1)!;
    clearBoardCache();
    const diskRead = deferred<disk.BoardDiskRecord | null>();
    vi.mocked(disk.readBoardDisk).mockReturnValue(diskRead.promise);
    const network = deferred<void>();
    let started = false;
    const http = makeWorkHttp({ ...source.client.http, fetch: async (...args) => {
      started = true;
      await network.promise;
      return source.fetch(...args);
    } });
    const read = getBoardRead(source.client, http, null, { ...DEFAULT_VIEW_STATE, view: "list", tab: "all" });
    read.reload();
    expect(started).toBe(true);
    diskRead.resolve(failure === "storage-failure" ? null : { ...snapshot,
      account: failure === "account" ? "other" : snapshot.account,
      version: failure === "schema" ? 900 : disk.BOARD_CACHE_VERSION,
    });
    await Promise.resolve();
    await Promise.resolve();
    expect(read.snapshot().resolved).toBe(false);
    expect(read.snapshot().items).toHaveLength(0);
    network.resolve();
    await expect.poll(() => read.snapshot().resolved).toBe(true);
    expect(read.snapshot().cached).toBe(false);
  });

  it("does not publish an older read or enrichment over a confirmed mutation", async () => {
    const stored: disk.BoardDiskRecord[] = [];
    vi.spyOn(disk, "updateBoardDisk").mockImplementation(async (_account, record) => { if (record) stored.push(record); });
    vi.spyOn(disk, "readBoardDisk").mockResolvedValue(null);
    const source = await fixture();
    const details = deferred<void>();
    const network = deferred<void>();
    let holding = false;
    const http = makeWorkHttp({ ...source.client.http, fetch: async (...args) => {
      const response = await source.fetch(...args);
      if (/\/(attempts|changes)(\?|$)/.test(String(args[0]))) await details.promise;
      if (holding && String(args[0]).includes("/work-items?")) await network.promise;
      return response;
    } });
    const read = getBoardRead(source.client, http, "proj_alpha", { ...DEFAULT_VIEW_STATE, view: "list", tab: "all" });
    read.reload();
    await expect.poll(() => read.snapshot().resolved).toBe(true);
    const native = (stored.at(-1)!.entries as { issues: NativeIssue[] }[])[0]!.issues.find((issue) => !issue.terminal)!;
    holding = true;
    read.reload();
    confirmWorkItem({ ...native, state: "Done", terminal: true, revision: "900" });
    expect(read.snapshot().items.find((item) => item.id === native.work_item_id)?.state).toBe("Done");
    details.resolve();
    network.resolve();
    await expect.poll(() => read.snapshot().refreshing).toBe(false);
    expect(read.snapshot().items.find((item) => item.id === native.work_item_id)).toMatchObject({ state: "Done", revision: "900", attempt: null });
    expect((stored.at(-1)!.entries as { issues: NativeIssue[] }[])[0]!.issues.find((issue) => issue.work_item_id === native.work_item_id)?.revision).toBe("900");
  });

  it.each([
    { view: "board", order: "confirmation-first" },
    { view: "board", order: "snapshot-first" },
    { view: "list", order: "confirmation-first" },
    { view: "list", order: "snapshot-first" },
  ] as const)("refreshes shipped totals after a terminal move with $view and $order", async ({ view, order }) => {
    vi.spyOn(disk, "updateBoardDisk").mockResolvedValue();
    vi.spyOn(disk, "readBoardDisk").mockResolvedValue(null);
    const source = await fixture();
    let moved = false;
    const http = makeWorkHttp({ ...source.client.http, fetch: async (...args) => {
      const response = await source.fetch(...args);
      if (!moved || !String(args[0]).includes("/work-items?")) return response;
      const body = await response.json();
      if (body.work) body.work.completed = 131;
      return Response.json(body);
    } });
    const read = getBoardRead(source.client, http, "proj_alpha", { ...DEFAULT_VIEW_STATE, view, tab: "active" });
    read.start();
    await expect.poll(() => read.snapshot().resolved).toBe(true);
    expect(read.snapshot().totals?.completed).toBe(130);
    const item = read.snapshot().items.find((item) => item.state === "Todo")!;
    let issue = await source.http.getWorkItem("proj_alpha", item.id);
    for (const state of ["In Progress", "In Review", "Merging", "Done"]) {
      issue = await source.http.transition({ projectId: "proj_alpha", itemId: item.id,
        key: `terminal-move-${state}`, expectedRevision: issue.revision!, state, reason: "user_requested" });
    }
    moved = true;
    if (order === "snapshot-first") {
      read.reload();
      await expect.poll(() => read.snapshot().refreshing).toBe(false);
      expect(read.snapshot().totals?.completed).toBe(131);
    }
    confirmWorkItem(issue);
    expect(read.snapshot().items.some((candidate) => candidate.id === item.id)).toBe(false);
    await expect.poll(() => read.snapshot().totals?.completed).toBe(131);
    await expect.poll(() => read.snapshot().refreshing).toBe(false);
    expect(read.snapshot().totals?.completed).toBe(131);
  });

  it("keeps live summaries consistent across unchanged and revised reloads without per-card reads", async () => {
    vi.spyOn(disk, "updateBoardDisk").mockResolvedValue();
    vi.spyOn(disk, "readBoardDisk").mockResolvedValue(null);
    const source = await fixture();
    let revision = "1";
    let perCardReads = 0;
    const http = makeWorkHttp({ ...source.client.http, fetch: async (...args) => {
      if (/\/(attempts|changes)(\?|$)/.test(String(args[0]))) {
        perCardReads++;
        throw new Error("Unexpected per-card read");
      }
      const response = await source.fetch(...args);
      if (!String(args[0]).includes("/work-items?")) return response;
      const body = await response.json();
      body.items = body.items.map((issue: NativeIssue) => ({ ...issue, revision }));
      if (body.work) body.work.items = body.work.items.map((issue: NativeIssue) => ({ ...issue, revision }));
      return Response.json(body);
    } });
    const read = getBoardRead(source.client, http, null, DEFAULT_VIEW_STATE);
    read.reload();
    await expect.poll(() => read.snapshot().resolved).toBe(true);
    expect(read.snapshot().refreshing).toBe(false);
    expect(read.snapshot().totals?.running).toBe(1);
    expect(read.snapshot().items.filter((item) => isLive(item))).toHaveLength(1);
    expect(perCardReads).toBe(0);
    read.reload();
    await expect.poll(() => read.snapshot().refreshing).toBe(false);
    expect(read.snapshot().items.filter((item) => isLive(item))).toHaveLength(1);
    expect(perCardReads).toBe(0);
    revision = "2";
    read.reload();
    await expect.poll(() => read.snapshot().refreshing).toBe(false);
    expect(read.snapshot().cached).toBe(false);
    expect(read.snapshot().totals?.running).toBe(1);
    expect(read.snapshot().items.filter((item) => isLive(item))).toHaveLength(1);
    expect(perCardReads).toBe(0);
  });

  it("normalizes identical queries and separates server filters, archive, account and permissions", async () => {
    const source = await fixture();
    const view = { ...DEFAULT_VIEW_STATE, label: ["b", "a", "a"] };
    const read = getBoardRead(source.client, source.http, null, view);
    expect(getBoardRead(source.client, source.http, null, { ...view, label: ["a", "b"] })).toBe(read);
    expect(getBoardRead(source.client, source.http, null, { ...view, archived: true })).not.toBe(read);
    expect(getBoardRead(source.client, source.http, "proj_alpha", view)).not.toBe(read);
    const other = { ...source.client, bootstrap: { ...source.client.bootstrap, actor: { ...source.client.bootstrap.actor, principal_id: "another" } } };
    expect(getBoardRead(other, source.http, null, view).snapshot().items).toHaveLength(0);
    expect(read.snapshot().items).toHaveLength(0);
    const revoked = { ...source.client, bootstrap: { ...source.client.bootstrap, projects: [] } };
    expect(boardAccountKey(revoked)).not.toBe(boardAccountKey(source.client));
    expect(getBoardRead(revoked, source.http, "proj_alpha", view).snapshot().items).toHaveLength(0);
  });

  it.each(["logout", "switch"])("clears cached reads after successful %s", async (action) => {
    const writes = vi.spyOn(disk, "updateBoardDisk").mockResolvedValue();
    const source = await fixture();
    source.read.reload();
    await expect.poll(() => source.read.snapshot().resolved).toBe(true);
    setQueryData(["reports", "cached"], "previous organization");
    const api = makeAccountApi({ ...source.client.http,
      fetch: async () => action === "logout" ? new Response(null, { status: 204 }) : Response.json({ next: "/work" }) });
    if (action === "logout") await api.logout();
    else await api.switchOrganization({ organization: "another", key: "switch" });
    expect(source.read.snapshot().items).toHaveLength(0);
    expect(getQueryData(["reports", "cached"])).toBeUndefined();
    expect(writes.mock.calls.at(-1)?.[0]).toBeNull();
  });

  it("clears authorized memory and disk on rejection and does not clear a newer account", async () => {
    const writes = vi.spyOn(disk, "updateBoardDisk").mockResolvedValue();
    const source = await fixture();
    source.read.reload();
    await expect.poll(() => source.read.snapshot().resolved).toBe(true);
    rejectBoardCache(source.read.owner);
    expect(source.read.snapshot().items).toHaveLength(0);
    expect(source.read.snapshot().totals).toBeNull();
    expect(writes.mock.calls.at(-1)?.[0]).toBeNull();
    const other = { ...source.client, bootstrap: { ...source.client.bootstrap, actor: { ...source.client.bootstrap.actor, principal_id: "another" } } };
    const read = getBoardRead(other, source.http, null, { ...DEFAULT_VIEW_STATE, view: "list", tab: "all" });
    read.reload();
    await expect.poll(() => read.snapshot().resolved).toBe(true);
    rejectBoardCache(source.read.owner);
    expect(read.snapshot().items.length).toBeGreaterThan(0);
  });
});
