// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { NativeBoardCard } from "../src/contracts/work.ts";
import { statusPill } from "../src/app/work/components/IssueCard.tsx";
import * as disk from "../src/app/work/lib/boardDisk.ts";
import { boardAccountKey, clearBoardCache, confirmWorkItem, getBoardRead, rejectBoardCache } from "../src/app/work/lib/boardStore.ts";
import { LIVE_DELTA_WINDOW_MS, LIVE_FALLBACK_MS } from "../src/app/work/lib/liveBoard.ts";
import { DEFAULT_VIEW_STATE } from "../src/app/work/lib/viewState.ts";
import { makeWorkHttp } from "../src/app/work/lib/workHttp.ts";
import { workPaginationFixture } from "./workPaginationFixture.ts";

class Source extends EventTarget {
  static instances: Source[] = [];
  readyState = 0;
  close = vi.fn(() => { this.readyState = 2; });
  constructor(readonly url: string) { super(); Source.instances.push(this); }
  emit(type: string, data = "{}", id = "") {
    this.dispatchEvent(new MessageEvent(type, { data, lastEventId: id }));
  }
}

const PROJECTS = [{ id: "proj_alpha", name: "alpha" }, { id: "proj_beta", name: "beta" }, { id: "proj_gamma", name: "gamma" }];
type BoardBody = {
  projects: { project: { project_id: string }; sequence: number; projection_pending: boolean; work?: unknown }[];
  cards: {
    issue: { work_item_id: string; project_id: string; actor: { kind: string } };
    liveness_expires_at?: NativeBoardCard["liveness_expires_at"];
    execution?: NativeBoardCard["execution"];
    attempt?: NativeBoardCard["attempt"];
  }[];
};

let visibility: DocumentVisibilityState = "visible";
beforeEach(() => {
  vi.useFakeTimers();
  vi.spyOn(Math, "random").mockReturnValue(0);
  vi.spyOn(disk, "readBoardDisk").mockResolvedValue(null);
  vi.spyOn(disk, "updateBoardDisk").mockResolvedValue();
  visibility = "visible";
  vi.spyOn(document, "visibilityState", "get").mockImplementation(() => visibility);
  Source.instances = [];
  vi.stubGlobal("EventSource", Source);
});
afterEach(async () => {
  clearBoardCache();
  await vi.advanceTimersByTimeAsync(0);
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

function deferred() {
  let release!: () => void;
  const promise = new Promise<void>((resolve) => { release = resolve; });
  return { promise, release };
}

async function liveBoard(rewrite: (body: BoardBody, url: URL) => void = () => {}) {
  const fixture = workPaginationFixture(PROJECTS);
  await fixture.control();
  const boards: URL[] = [];
  const others: URL[] = [];
  const gate: { next?: Promise<void> } = {};
  const http = makeWorkHttp({ ...fixture.client.http, fetch: async (input, init) => {
    const url = new URL(input, "http://fixture.test");
    const response = await fixture.fetch(input, init);
    if (!url.pathname.endsWith("/work-board")) {
      others.push(url);
      return response;
    }
    boards.push(url);
    const held = gate.next;
    gate.next = undefined;
    const body = await response.json() as BoardBody;
    rewrite(body, url);
    await held;
    return Response.json(body);
  } });
  const direct = makeWorkHttp({ ...fixture.client.http, fetch: fixture.fetch });
  const read = getBoardRead(fixture.client, http, null, DEFAULT_VIEW_STATE);
  const stream = (id: string) => Source.instances.findLast((source) => source.url.includes(`/projects/${id}/`))!;
  const openAll = () => { for (const { id } of PROJECTS) stream(id).emit("open"); };
  return { fixture, http, direct, read, boards, others, stream, openAll, gate };
}

describe("the live board", () => {
  it("loads three projects with one board request and nothing else", async () => {
    const { read, boards, others } = await liveBoard();
    read.start();
    await vi.advanceTimersByTimeAsync(0);
    expect(read.snapshot().resolved).toBe(true);
    expect(new Set(read.snapshot().items.map((item) => item.projectId))).toEqual(new Set(PROJECTS.map(({ id }) => id)));
    expect(boards).toHaveLength(1);
    expect(boards[0]!.searchParams.getAll("project")).toEqual(PROJECTS.map(({ id }) => id));
    expect(boards[0]!.searchParams.has("item")).toBe(false);
    expect(others).toHaveLength(0);
  });

  it("keeps applying pushed changes while no board is mounted and renders them on return without a request", async () => {
    const { fixture, http, read, direct, boards, stream, openAll } = await liveBoard();
    const stop = read.subscribe(() => {});
    read.start();
    openAll();
    await vi.advanceTimersByTimeAsync(0);
    const todo = read.snapshot().items.find((item) => item.projectId === "proj_alpha" && item.state === "Todo")!;
    stop();
    const issue = await direct.getWorkItem("proj_alpha", todo.id);
    await direct.transition({ projectId: "proj_alpha", itemId: todo.id, key: "live-move", expectedRevision: issue.revision!, state: "In Progress", reason: "user_requested" });
    stream("proj_alpha").emit("work_item", JSON.stringify({ entity_id: todo.id, kind: "upsert" }), "1");
    await vi.advanceTimersByTimeAsync(LIVE_DELTA_WINDOW_MS);
    expect(boards).toHaveLength(2);
    expect(boards[1]!.searchParams.getAll("item")).toEqual([todo.id]);
    expect(boards[1]!.searchParams.getAll("project")).toEqual(["proj_alpha"]);
    expect(stream("proj_alpha").close).not.toHaveBeenCalled();

    const returned = getBoardRead(fixture.client, http, null, DEFAULT_VIEW_STATE);
    expect(returned).toBe(read);
    const notify = vi.fn();
    const again = returned.subscribe(notify);
    returned.start();
    expect(returned.snapshot().resolved).toBe(true);
    expect(returned.snapshot().loading).toBe(false);
    expect(returned.snapshot().items.find((item) => item.id === todo.id)?.state).toBe("In Progress");
    await vi.advanceTimersByTimeAsync(LIVE_FALLBACK_MS * 2);
    expect(boards).toHaveLength(2);
    again();
  });

  it("coalesces a burst of pushed events across projects into one item request", async () => {
    const { read, boards, stream, openAll } = await liveBoard();
    read.start();
    openAll();
    await vi.advanceTimersByTimeAsync(0);
    const ids = (project: string) => read.snapshot().items.filter((item) => item.projectId === project).slice(0, 3).map((item) => item.id);
    const burst: [string, string][] = [...ids("proj_alpha").map((id): [string, string] => ["proj_alpha", id]), ...ids("proj_beta").slice(0, 2).map((id): [string, string] => ["proj_beta", id])];
    expect(burst).toHaveLength(5);
    let sequence = 0;
    for (const [project, id] of burst) {
      stream(project).emit("work_item", JSON.stringify({ entity_id: id, kind: "upsert" }), String(++sequence));
      await vi.advanceTimersByTimeAsync(5);
    }
    stream("proj_alpha").emit("work_item", JSON.stringify({ entity_id: burst[0]![1], kind: "upsert" }), String(++sequence));
    await vi.advanceTimersByTimeAsync(LIVE_DELTA_WINDOW_MS - 5 * burst.length - 1);
    expect(boards).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(5 * burst.length + 1);
    expect(boards).toHaveLength(2);
    expect(boards[1]!.searchParams.getAll("item").toSorted()).toEqual(burst.map(([, id]) => id).toSorted());
    expect(boards[1]!.searchParams.getAll("project")).toEqual(["proj_alpha", "proj_beta"]);
    expect(read.snapshot().items.length).toBeGreaterThan(0);
  });

  it("expires and refreshes visible workers together on an unchanged connected board", async () => {
    const deadline = Date.now() + 2_000;
    const expires = new Date(deadline).toISOString();
    const ids = new Map<string, string>();
    const sequences: number[][] = [];
    const { read, boards, openAll, gate } = await liveBoard((body, url) => {
      sequences.push(body.projects.filter((entry) => entry.project.project_id !== "proj_gamma").map((entry) => entry.sequence));
      if (!url.searchParams.has("item")) {
        for (const card of body.cards) {
          const project = card.issue.project_id;
          if (project === "proj_gamma" || ids.has(project)) continue;
          ids.set(project, card.issue.work_item_id);
          card.liveness_expires_at = expires;
          card.execution = { id: card.issue.work_item_id, running: true, stage: "implementation", expires_at: new Date(deadline + 90_000).toISOString() };
        }
      } else {
        for (const card of body.cards) {
          const recovered = card.issue.project_id === "proj_alpha";
          card.attempt = { attempt_id: card.issue.work_item_id, status: recovered ? "running" : "interrupted", started_at: expires };
          if (recovered) card.liveness_expires_at = new Date(deadline + 60_000).toISOString();
        }
      }
    });
    const held = deferred();
    read.start();
    openAll();
    try {
      await vi.advanceTimersByTimeAsync(0);
      const worker = (project: string) => read.snapshot().items.find((item) => item.id === ids.get(project));
      expect(worker("proj_alpha")?.attempt?.running).toBe(true);
      expect(worker("proj_beta")?.attempt?.running).toBe(true);
      gate.next = held.promise;
      await vi.advanceTimersByTimeAsync(2_000);
      expect(statusPill(worker("proj_alpha")!)?.label).toBe("Runner status unknown");
      expect(worker("proj_beta")?.attempt).toBeNull();
      await vi.advanceTimersByTimeAsync(LIVE_DELTA_WINDOW_MS);
      expect(boards).toHaveLength(2);
      expect(boards[1]!.searchParams.getAll("item").toSorted()).toEqual([...ids.values()].toSorted());
      expect(sequences[1]).toEqual(sequences[0]);
      held.release();
      await vi.advanceTimersByTimeAsync(0);
      expect(worker("proj_alpha")?.attempt?.running).toBe(true);
      expect(worker("proj_beta")?.attempt?.status).toBe("interrupted");
    } finally {
      held.release();
      await vi.advanceTimersByTimeAsync(0);
    }
  });

  it("checks the board on a timer only while a stream is disconnected and the tab is visible", async () => {
    const { read, boards, stream, openAll } = await liveBoard();
    read.start();
    openAll();
    await vi.advanceTimersByTimeAsync(0);
    expect(boards).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(LIVE_FALLBACK_MS * 2);
    expect(boards).toHaveLength(1);

    stream("proj_beta").dispatchEvent(new Event("error"));
    await vi.advanceTimersByTimeAsync(LIVE_FALLBACK_MS - 1);
    expect(boards).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(boards).toHaveLength(2);
    expect(boards[1]!.searchParams.getAll("project")).toEqual(PROJECTS.map(({ id }) => id));
    await vi.advanceTimersByTimeAsync(LIVE_FALLBACK_MS);
    expect(boards).toHaveLength(3);

    stream("proj_beta").emit("open");
    await vi.advanceTimersByTimeAsync(LIVE_FALLBACK_MS * 2);
    expect(boards).toHaveLength(3);

    visibility = "hidden";
    document.dispatchEvent(new Event("visibilitychange"));
    await vi.advanceTimersByTimeAsync(LIVE_FALLBACK_MS * 3);
    expect(boards).toHaveLength(3);
  });

  it("keeps the other projects and the last cards of a pending or missing project", async () => {
    let broken = false;
    const { read, boards } = await liveBoard((body) => {
      if (!broken) return;
      body.projects = body.projects.filter((entry) => entry.project.project_id !== "proj_beta");
      body.cards = body.cards.filter((card) => card.issue.project_id !== "proj_beta");
      for (const entry of body.projects) {
        if (entry.project.project_id !== "proj_gamma") continue;
        entry.projection_pending = true;
        delete entry.work;
      }
    });
    read.start();
    await vi.advanceTimersByTimeAsync(0);
    const before = read.snapshot().items;
    const count = (project: string) => read.snapshot().items.filter((item) => item.projectId === project).length;
    const counts = PROJECTS.map(({ id }) => count(id));
    expect(counts.every((value) => value > 0)).toBe(true);
    broken = true;
    read.reload();
    expect(read.snapshot().items).toBe(before);
    await vi.advanceTimersByTimeAsync(0);
    expect(boards).toHaveLength(2);
    expect(read.snapshot().resolved).toBe(true);
    expect(PROJECTS.map(({ id }) => count(id))).toEqual(counts);
    expect(read.snapshot().error).toContain("beta: Project is unavailable");
    expect(read.snapshot().error).toContain("gamma: Board projection is pending");
    expect(read.snapshot().error).not.toContain("alpha");
    expect(read.snapshot().totals?.lanes).toBeDefined();
  });

  it("keeps a card closed by a confirmation over an older item response that was in flight", async () => {
    const { read, boards, stream, openAll, gate, direct } = await liveBoard();
    read.start();
    openAll();
    await vi.advanceTimersByTimeAsync(0);
    const target = read.snapshot().items.find((item) => item.projectId === "proj_alpha" && item.state === "Todo")!;
    const held = deferred();
    gate.next = held.promise;
    stream("proj_alpha").emit("work_item", JSON.stringify({ entity_id: target.id, kind: "upsert" }), "1");
    await vi.advanceTimersByTimeAsync(LIVE_DELTA_WINDOW_MS);
    expect(boards).toHaveLength(2);
    let issue = await direct.getWorkItem("proj_alpha", target.id);
    for (const state of ["In Progress", "In Review", "Merging", "Done"]) {
      issue = await direct.transition({ projectId: "proj_alpha", itemId: target.id, key: `close-${state}`, expectedRevision: issue.revision!, state, reason: "user_requested" });
    }
    confirmWorkItem(issue);
    expect(read.snapshot().items.some((item) => item.id === target.id)).toBe(false);
    held.release();
    await vi.advanceTimersByTimeAsync(0);
    expect(read.snapshot().items.some((item) => item.id === target.id)).toBe(false);
    await vi.advanceTimersByTimeAsync(LIVE_DELTA_WINDOW_MS);
    expect(boards).toHaveLength(3);
    expect(boards[2]!.searchParams.getAll("item")).toEqual([target.id]);
    expect(read.snapshot().items.some((item) => item.id === target.id)).toBe(false);
  });

  it("keeps counts from a newer item response over a later response with an older sequence", async () => {
    const sequences = [20, 15];
    const { read, stream, openAll } = await liveBoard((body, url) => {
      if (!url.searchParams.has("item")) return;
      const sequence = sequences.shift()!;
      for (const entry of body.projects) {
        entry.sequence = sequence;
        entry.work = { items: [], lanes: [{ state: "Todo", total: sequence, running: 0 }], truncated: false, as_of: "2026-11-06T12:00:00Z", completed: 0, cancelled: 0, shipped_windows: [] };
      }
    });
    read.start();
    openAll();
    await vi.advanceTimersByTimeAsync(0);
    const item = read.snapshot().items.find((candidate) => candidate.projectId === "proj_alpha")!;
    for (const id of ["1", "2"]) {
      stream("proj_alpha").emit("work_item", JSON.stringify({ entity_id: item.id, kind: "upsert" }), id);
      await vi.advanceTimersByTimeAsync(LIVE_DELTA_WINDOW_MS);
    }
    expect(sequences).toHaveLength(0);
    expect(read.projectSnapshot("proj_alpha").totals?.lanes.Todo).toBe(20);
  });

  it("reloads the remaining projects when one is revoked during a shared board read", async () => {
    const { fixture, read, boards, stream, openAll, gate } = await liveBoard();
    read.start();
    openAll();
    await vi.advanceTimersByTimeAsync(0);
    const beta = getBoardRead(fixture.client, read.http, "proj_beta", DEFAULT_VIEW_STATE);
    expect(beta.live).toBe(read.live);
    const held = deferred();
    gate.next = held.promise;
    read.reload();
    await vi.advanceTimersByTimeAsync(0);
    expect(boards).toHaveLength(2);
    rejectBoardCache(boardAccountKey(fixture.client), "proj_alpha");
    await vi.advanceTimersByTimeAsync(0);
    expect(boards).toHaveLength(3);
    expect(boards[2]!.searchParams.getAll("project")).toEqual(["proj_beta", "proj_gamma"]);
    expect(beta.snapshot().items.length).toBeGreaterThan(0);
    expect(read.snapshot().items).toHaveLength(0);
    held.release();
    await vi.advanceTimersByTimeAsync(0);
    const item = beta.snapshot().items[0]!;
    stream("proj_beta").emit("work_item", JSON.stringify({ entity_id: item.id, kind: "upsert" }), "1");
    await vi.advanceTimersByTimeAsync(LIVE_DELTA_WINDOW_MS);
    expect(boards).toHaveLength(4);
    expect(boards[3]!.searchParams.getAll("item")).toEqual([item.id]);
    expect(boards[3]!.searchParams.getAll("project")).toEqual(["proj_beta"]);
  });

  it("keeps a project's cards when an item response omits that project and reloads the board", async () => {
    let omit = false;
    const held = deferred();
    const { read, boards, stream, openAll, gate } = await liveBoard((body, url) => {
      if (!omit || !url.searchParams.has("item")) return;
      body.projects = body.projects.filter((entry) => entry.project.project_id !== "proj_beta");
      body.cards = body.cards.filter((card) => card.issue.project_id !== "proj_beta");
      gate.next = held.promise;
    });
    read.start();
    openAll();
    await vi.advanceTimersByTimeAsync(0);
    const item = read.snapshot().items.find((candidate) => candidate.projectId === "proj_beta")!;
    omit = true;
    stream("proj_beta").emit("work_item", JSON.stringify({ entity_id: item.id, kind: "upsert" }), "1");
    await vi.advanceTimersByTimeAsync(LIVE_DELTA_WINDOW_MS);
    expect(boards).toHaveLength(3);
    expect(boards[2]!.searchParams.has("item")).toBe(false);
    expect(read.snapshot().items.some((candidate) => candidate.id === item.id)).toBe(true);
    held.release();
    await vi.advanceTimersByTimeAsync(0);
    expect(read.snapshot().items.some((candidate) => candidate.id === item.id)).toBe(true);
  });

  it("loads a new scope while every other kept scope is mounted", async () => {
    const fixture = workPaginationFixture(PROJECTS);
    await fixture.control();
    const http = makeWorkHttp({ ...fixture.client.http, fetch: fixture.fetch });
    const scopes = [["proj_alpha"], ["proj_beta"], ["proj_gamma"], ["proj_alpha", "proj_beta"], ["proj_beta", "proj_gamma"]];
    const stops = scopes.map((ids) => {
      const read = getBoardRead(fixture.client, http, null, DEFAULT_VIEW_STATE, ids);
      const stop = read.subscribe(() => {});
      read.start();
      return { read, stop };
    });
    await vi.advanceTimersByTimeAsync(0);
    expect(stops.every(({ read }) => read.snapshot().resolved && read.snapshot().items.length > 0)).toBe(true);
    for (const { stop } of stops) stop();
  });

  it("drops one card that does not match the contract and renders the rest", async () => {
    let dropped = "";
    const { read } = await liveBoard((body) => {
      const card = body.cards.find((candidate) => candidate.issue.project_id === "proj_beta")!;
      dropped = card.issue.work_item_id;
      card.issue.actor.kind = "unknown_kind";
    });
    const errors = vi.spyOn(console, "error").mockImplementation(() => {});
    read.start();
    await vi.advanceTimersByTimeAsync(0);
    expect(read.snapshot().resolved).toBe(true);
    expect(read.snapshot().error).toBeNull();
    expect(read.snapshot().items.some((item) => item.id === dropped)).toBe(false);
    expect(new Set(read.snapshot().items.map((item) => item.projectId))).toEqual(new Set(PROJECTS.map(({ id }) => id)));
    expect(errors).toHaveBeenCalledOnce();
    expect(errors.mock.calls[0]![1]).toMatchObject({ work_item_id: dropped });
  });

  it("keeps a card whose pushed update does not match the contract and rereads the board", async () => {
    let broken = "";
    const { read, boards, stream, openAll } = await liveBoard((body, url) => {
      if (!url.searchParams.has("item")) return;
      for (const card of body.cards) if (card.issue.work_item_id === broken) card.issue.actor.kind = "";
    });
    vi.spyOn(console, "error").mockImplementation(() => {});
    read.start();
    openAll();
    await vi.advanceTimersByTimeAsync(0);
    broken = read.snapshot().items.find((item) => item.projectId === "proj_alpha")!.id;
    stream("proj_alpha").emit("work_item", JSON.stringify({ entity_id: broken, kind: "upsert" }), "1");
    await vi.advanceTimersByTimeAsync(LIVE_DELTA_WINDOW_MS);
    await vi.advanceTimersByTimeAsync(0);
    expect(boards.filter((url) => url.searchParams.has("item"))).toHaveLength(1);
    expect(boards.filter((url) => !url.searchParams.has("item")).length).toBeGreaterThan(1);
    expect(read.snapshot().items.some((item) => item.id === broken)).toBe(true);
    expect(read.snapshot().error).toBeNull();
  });

  it.each([["every card", 0], ["more than one card in twenty", 2]])("reports an error instead of a board when %s does not match the contract", async (_, kept) => {
    let total = 0;
    let bad = 0;
    const { read } = await liveBoard((body) => {
      total = body.cards.length;
      bad = total - kept;
      for (const card of body.cards.slice(kept)) card.issue.actor.kind = "";
    });
    vi.spyOn(console, "error").mockImplementation(() => {});
    read.start();
    await vi.advanceTimersByTimeAsync(0);
    expect(bad).toBeGreaterThan(1);
    expect(read.snapshot().error).toBe(`The board could not be shown: ${bad} of ${total} cards do not match this version of the app.`);
    expect(read.snapshot().items).toEqual([]);
  });
});
