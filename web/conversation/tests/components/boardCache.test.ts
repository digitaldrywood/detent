import { afterEach, describe, expect, it, vi } from "vitest";

import { makeAccountApi } from "../../src/app/account/api.ts";
import * as disk from "../../src/app/work/lib/boardDisk.ts";
import { boardAccountKey, clearBoardCache, confirmWorkItem, getBoardRead, rejectBoardCache } from "../../src/app/work/lib/boardStore.ts";
import { DEFAULT_VIEW_STATE } from "../../src/app/work/lib/viewState.ts";
import { makeWorkHttp } from "../../src/app/work/lib/workHttp.ts";
import type { NativeIssue } from "../../src/contracts/work.ts";
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
  it("drains active pages before settling and continues only Backlog", async () => {
    const source = workPaginationFixture();
    await source.control({ activeOverflow: true, backlogOverflow: true });
    const http = makeWorkHttp({ ...source.client.http, fetch: source.fetch });
    const read = getBoardRead(source.client, http, "proj_alpha", DEFAULT_VIEW_STATE);
    const held = source.deferPage();
    read.start();
    await held.waiting;
    expect(read.snapshot().resolved).toBe(false);
    expect(read.snapshot().loading).toBe(true);
    held.release();
    await expect.poll(() => read.snapshot().resolved).toBe(true);
    expect(read.snapshot().hasMore).toBe(false);
    expect(read.snapshot().items.filter((item) => item.state !== "Backlog")).toHaveLength(217);
    expect(read.snapshot().items.filter((item) => item.state === "Backlog")).toHaveLength(200);
    expect(read.snapshot().backlogHasMore).toBe(true);
    expect(read.snapshot().backlogTotal).toBe(405);
    expect(read.snapshot().totals?.lanes.Backlog).toBe(405);
    const initial = source.requests.filter(({ url }) => url.pathname.endsWith("/work-items"));
    expect(initial).toHaveLength(3);
    expect(initial.filter(({ url }) => url.searchParams.get("include") === "work")).toHaveLength(1);
    expect(initial[0]!.url.searchParams.getAll("state")).not.toContain("Backlog");
    expect(initial.every(({ url }) => url.searchParams.get("open") === "true" && url.searchParams.get("limit") === "200")).toBe(true);
    const before = source.requests.length;
    read.loadBacklog();
    await expect.poll(() => read.snapshot().backlogLoading).toBe(false);
    expect(read.snapshot().items.filter((item) => item.state === "Backlog")).toHaveLength(400);
    expect(read.snapshot().hasMore).toBe(false);
    const pages = source.requests.slice(before).filter(({ url }) => url.pathname.endsWith("/work-items"));
    expect(pages).toHaveLength(1);
    expect(pages[0]!.url.searchParams.getAll("state")).toEqual(["Backlog"]);
    expect(pages[0]!.url.searchParams.has("include")).toBe(false);
    read.reload();
    await expect.poll(() => read.snapshot().refreshing).toBe(false);
    expect(read.snapshot().items.filter((item) => item.state === "Backlog")).toHaveLength(400);
    read.loadBacklog();
    await expect.poll(() => read.snapshot().backlogHasMore).toBe(false);
    expect(read.snapshot().items.filter((item) => item.state === "Backlog")).toHaveLength(405);
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
    const api = makeAccountApi({ ...source.client.http,
      fetch: async () => action === "logout" ? new Response(null, { status: 204 }) : Response.json({ next: "/work" }) });
    if (action === "logout") await api.logout();
    else await api.switchOrganization({ organization: "another", key: "switch" });
    expect(source.read.snapshot().items).toHaveLength(0);
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
