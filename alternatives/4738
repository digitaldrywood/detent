// @vitest-environment jsdom
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { subscribeProjectEvents } from "../src/app/work/lib/projectEvents.ts";

class Source extends EventTarget {
  static instances: Source[] = [];
  readyState = 0;
  close = vi.fn(() => { this.readyState = 2; });
  constructor(readonly url: string) { super(); Source.instances.push(this); }
  emit(type: string, data = "{}", id = "") {
    this.dispatchEvent(new MessageEvent(type, { data, lastEventId: id }));
  }
}
const stops: (() => void)[] = [];
beforeEach(() => { vi.useFakeTimers(); Source.instances = []; vi.stubGlobal("EventSource", Source); });
afterEach(() => { stops.splice(0).forEach((stop) => stop()); vi.restoreAllMocks(); vi.unstubAllGlobals(); vi.useRealTimers(); });
const http = () => ({ eventsUrl: (id: string) => `/projects/${id}/events` });
it("closes native reconnect and resumes the shared cursor without refreshing subscribers", async () => {
  const client = http();
  const changed = vi.fn();
  const opened = vi.fn();
  stops.push(subscribeProjectEvents(client, "p", { work_item: changed, open: opened }));
  stops.push(subscribeProjectEvents(client, "p", { "health.findings": vi.fn() }));
  expect(Source.instances).toHaveLength(1);
  const first = Source.instances[0]!;
  first.emit("work_item", '{"entity_id":"i","entity_revision":2}', "19");
  first.emit("checkpoint", "{}", "42");
  first.emit("error");
  expect(first.close).toHaveBeenCalledOnce();
  await vi.advanceTimersByTimeAsync(30_000);
  expect(Source.instances).toHaveLength(2);
  expect(Source.instances[1]!.url).toContain("since=42");
  Source.instances[1]!.emit("work_item", '{"entity_id":"i","entity_revision":2}', "19");
  expect(changed).toHaveBeenCalledOnce();
  expect(opened).not.toHaveBeenCalled();
});

import { observeProjectQuery, subscribeProjectQuery, PROJECT_SNAPSHOT_BUFFER } from "../src/app/work/lib/projectEvents.ts";

it("buffers above the snapshot watermark once and accepts sequence skips and entity revisions", async () => {
  const changed = vi.fn();
  const refresh = vi.fn();
  const query = subscribeProjectQuery(http(), "p", { topics: ["work_item"], apply: changed, refresh });
  stops.push(query.close);
  const snapshot = query.beginSnapshot();
  const source = Source.instances[0]!;
  const emit = (id: string, revision: number) => source.emit("work_item", JSON.stringify({ entity_id: "i", entity_revision: revision }), id);
  emit("9", 1);
  emit("37", 2);
  emit("37", 2);
  expect(changed).not.toHaveBeenCalled();
  snapshot.complete(12);
  expect(changed).toHaveBeenCalledOnce();
  emit("81", 2);
  emit("150", 3);
  expect(changed).toHaveBeenCalledTimes(2);
  expect(refresh).not.toHaveBeenCalled();
});

it("restarts an overflowing snapshot and discards its late response", async () => {
  const releases: ((value: { value: string; watermark: number }) => void)[] = [];
  const applied = vi.fn();
  const event = vi.fn();
  const load = vi.fn(() => new Promise<{ value: string; watermark: number }>((resolve) => releases.push(resolve)));
  const query = observeProjectQuery(http(), "p", { topics: ["work_item"], load, apply: applied, event });
  stops.push(() => { query.close(); releases.forEach((release) => release({ value: "cleanup", watermark: 9999 })); });
  const source = Source.instances[0]!;
  for (let i = 1; i <= PROJECT_SNAPSHOT_BUFFER + 1; i++) source.emit("work_item", JSON.stringify({ entity_id: "i", entity_revision: i }), String(i));
  await Promise.resolve();
  expect(load).toHaveBeenCalledTimes(2);
  releases[0]!({ value: "stale", watermark: 0 });
  releases[1]!({ value: "fresh", watermark: PROJECT_SNAPSHOT_BUFFER + 1 });
  await Promise.resolve();
  expect(applied).toHaveBeenCalledExactlyOnceWith("fresh");
  expect(event).not.toHaveBeenCalled();
});

it("coalesces reset per query generation and resumes from the oldest replacement snapshot", async () => {
  const client = http();
  const changes = vi.fn();
  const snapshots: ReturnType<ReturnType<typeof subscribeProjectQuery>["beginSnapshot"]>[] = [];
  const queries = ["work_item", "health.findings"].map((topic) => {
    const query = subscribeProjectQuery(client, "p", { topics: [topic], apply: changes,
      refresh: () => snapshots.push(query.beginSnapshot()) });
    stops.push(query.close);
    return query;
  });
  const source = Source.instances[0]!;
  source.emit("checkpoint", "{}", "10");
  source.emit("reset", "{}", "100");
  source.emit("reset", "{}", "100");
  await Promise.resolve();
  expect(snapshots).toHaveLength(2);
  await vi.advanceTimersByTimeAsync(30_000);
  expect(Source.instances).toHaveLength(1);
  snapshots[0]!.complete(102);
  snapshots[1]!.complete(101);
  expect(Source.instances).toHaveLength(2);
  expect(Source.instances[1]!.url).toContain("since=101");
  expect(changes).not.toHaveBeenCalled();
  queries.forEach((query) => query.close());
});

it("never rewinds a shared cursor for late subscribers or overlapping snapshots", async () => {
  const client = http();
  const options = { topics: ["work_item"], apply: vi.fn(), refresh: vi.fn() };
  const first = subscribeProjectQuery(client, "p", options);
  stops.push(first.close);
  const old = first.beginSnapshot();
  const recent = first.beginSnapshot();
  Source.instances[0]!.emit("checkpoint", "{}", "88");
  recent.complete(70);
  old.complete(1);
  const late = subscribeProjectQuery(client, "p", options);
  stops.push(late.close);
  late.beginSnapshot().complete(2);
  Source.instances[0]!.emit("error");
  await vi.advanceTimersByTimeAsync(30_000);
  expect(Source.instances[1]!.url).toContain("since=88");
});

it("resumes visible tabs by checkpoint without blanket invalidation or stale generation delivery", () => {
  let visibility = "visible";
  vi.spyOn(document, "visibilityState", "get").mockImplementation(() => visibility as DocumentVisibilityState);
  const apply = vi.fn();
  const refresh = vi.fn();
  const query = subscribeProjectQuery(http(), "p", { topics: ["work_item"], apply, refresh });
  stops.push(query.close);
  const source = Source.instances[0]!;
  source.emit("checkpoint", "{}", "9007199254740993");
  visibility = "hidden";
  document.dispatchEvent(new Event("visibilitychange"));
  expect(source.close).toHaveBeenCalledOnce();
  visibility = "visible";
  document.dispatchEvent(new Event("visibilitychange"));
  expect(Source.instances[1]!.url).toContain("since=9007199254740993");
  source.emit("work_item", "{}", "9007199254740995");
  Source.instances[1]!.emit("work_item", "{}", "9007199254740994");
  expect(apply).toHaveBeenCalledOnce();
  expect(refresh).not.toHaveBeenCalled();
});

it.each([
  { random: 0, firstDelay: 500, cappedDelay: 15_000 },
  { random: 0.5, firstDelay: 750, cappedDelay: 22_500 },
  { random: 0.99, firstDelay: 995, cappedDelay: 29_850 },
])("grows retries, caps every wait at 30 seconds and varies jitter with random=$random", async ({ random, firstDelay, cappedDelay }) => {
  vi.spyOn(Math, "random").mockReturnValue(random);
  const stop = subscribeProjectEvents(http(), "p", {});
  stops.push(stop);
  const delays: number[] = [];
  for (let attempt = 0; attempt < 12; attempt++) {
    const count = Source.instances.length;
    const startedAt = Date.now();
    Source.instances.at(-1)!.emit("error");
    expect(Source.instances).toHaveLength(count);
    await vi.advanceTimersToNextTimerAsync();
    expect(Source.instances).toHaveLength(count + 1);
    const delay = Date.now() - startedAt;
    expect(delay).toBeGreaterThan(0);
    expect(delay).toBeLessThanOrEqual(30_000);
    if (attempt > 0 && attempt < 5) expect(delay).toBeGreaterThan(delays.at(-1)!);
    delays.push(delay);
  }
  expect(delays[0]).toBe(firstDelay);
  expect(delays.slice(-3)).toEqual([cappedDelay, cappedDelay, cappedDelay]);
  Source.instances.at(-1)!.emit("error");
  stop();
  expect(vi.getTimerCount()).toBe(0);
  const count = Source.instances.length;
  await vi.advanceTimersByTimeAsync(30_000);
  expect(Source.instances).toHaveLength(count);
});
