import type { WorkHttp } from "./workHttp.ts";

type EventHttp = Pick<WorkHttp, "eventsUrl">;
const topics = ["work_item", "board", "lane", "activity", "human_action", "health.findings", "checkpoint", "reset",
  ...["requested", "starting", "ready", "idle", "unreachable", "closing", "closed", "failed"].map((state) => `workspace.${state}`),
  ...["queued", "running", "succeeded", "failed"].map((state) => `action_run.${state}`)];

interface ProjectStream {
  source: EventSource | null;
  cursor: bigint | null;
  generation: number;
  attempts: number;
  resetCursor: bigint | null;
  resetting: boolean;
  queries: Map<symbol, { pending: boolean; watermark: bigint | null }>;
  resume: () => void;
  timer?: ReturnType<typeof setTimeout>;
  listeners: Map<string, Set<EventListener>>;
  subscribers: number;
  connect: () => void;
  visibility: () => void;
}

const streams = new WeakMap<object, Map<string, ProjectStream>>();

function sequence(value: unknown): bigint | null {
  if (typeof value !== "string" && typeof value !== "number" && typeof value !== "bigint") return null;
  try { return /^\d+$/.test(String(value)) ? BigInt(value) : null; } catch { return null; }
}

export function projectEventData(event: Event): Record<string, unknown> {
  try {
    const data: unknown = JSON.parse((event as MessageEvent<string>).data);
    return typeof data === "object" && data !== null ? data as Record<string, unknown> : {};
  } catch { return {}; }
}

export function eventNamesItem(event: Event, itemId: string): boolean {
  const data = projectEventData(event);
  return data.entity_id === itemId || data.related_id === itemId || data.subject_work_item_id === itemId;
}

function createStream(http: EventHttp, projectId: string): ProjectStream {
  const stream: ProjectStream = { source: null, cursor: null, generation: 0, attempts: 0, resetCursor: null, resetting: false, queries: new Map(), resume: () => {}, listeners: new Map(), subscribers: 0,
    connect: () => {}, visibility: () => {} };
  const publish = (type: string, event: Event) => {
    for (const listener of stream.listeners.get(type) ?? []) listener(event);
  };
  const disconnect = () => {
    stream.generation++;
    stream.source?.close();
    stream.source = null;
    clearTimeout(stream.timer);
    stream.timer = undefined;
  };
  const reconnect = () => {
    disconnect();
    if (stream.subscribers === 0 || document.visibilityState === "hidden") return;
    const ceiling = Math.min(30_000, 1000 * 2 ** Math.min(stream.attempts++, 5));
    stream.timer = setTimeout(stream.connect, ceiling * (0.5 + Math.random() * 0.5));
  };
  stream.connect = () => {
    disconnect();
    if (stream.subscribers === 0 || document.visibilityState === "hidden") return;
    if (stream.resetting && [...stream.queries.values()].some((query) => query.pending)) return;
    if (stream.resetting) {
      const watermarks = [...stream.queries.values()].map((query) => query.watermark ?? stream.resetCursor).filter((value): value is bigint => value !== null);
      stream.cursor = watermarks.length === 0 ? stream.resetCursor : watermarks.reduce((a, b) => a < b ? a : b);
      stream.resetting = false;
    }
    const generation = stream.generation;
    const base = http.eventsUrl(projectId);
    const target = stream.cursor === null ? base : `${base}${base.includes("?") ? "&" : "?"}since=${stream.cursor}`;
    const source = new globalThis.EventSource(target, { withCredentials: true });
    stream.source = source;
    const current = () => stream.generation === generation && stream.source === source;
    source.addEventListener("open", (event) => { if (current()) publish("open", event); });
    source.addEventListener("error", (event) => {
      if (!current()) return;
      publish("error", event);
      reconnect();
    });
    for (const type of topics) source.addEventListener(type, (event) => {
      if (!current()) return;
      const next = sequence((event as MessageEvent).lastEventId);
      if (type === "reset") {
        stream.cursor = null;
        stream.resetCursor = next;
        stream.resetting = true;
        for (const query of stream.queries.values()) { query.pending = true; query.watermark = null; }
        publish(type, event);
        reconnect();
        return;
      }
      if (next !== null) {
        if (stream.cursor !== null && next <= stream.cursor) return;
        stream.cursor = next;
        stream.attempts = 0;
      }
      publish(type, event);
    });
  };
  stream.resume = () => {
    if (stream.resetting && stream.timer === undefined && ![...stream.queries.values()].some((query) => query.pending)) stream.connect();
  };
  stream.visibility = () => {
    if (document.visibilityState === "hidden") disconnect();
    else stream.connect();
  };
  return stream;
}

export function subscribeProjectEvents(
  http: EventHttp,
  projectId: string,
  listeners: Readonly<Record<string, EventListener>>,
): () => void {
  if (typeof globalThis.EventSource !== "function") return () => {};
  let projects = streams.get(http);
  if (projects === undefined) { projects = new Map(); streams.set(http, projects); }
  let stream = projects.get(projectId);
  if (stream === undefined) { stream = createStream(http, projectId); projects.set(projectId, stream); }
  const current = stream;
  const first = current.subscribers++ === 0;
  const subscriptions = Object.entries(listeners).map(([type, listener]) => {
    let observers = current.listeners.get(type);
    if (observers === undefined) { observers = new Set(); current.listeners.set(type, observers); }
    const observer: EventListener = (event) => listener(event);
    observers.add(observer);
    return { type, observer };
  });
  if (first) {
    document.addEventListener("visibilitychange", current.visibility);
    current.connect();
  } else if (current.source?.readyState === 1) listeners.open?.(new Event("open"));
  let stopped = false;
  return () => {
    if (stopped) return;
    stopped = true;
    for (const { type, observer } of subscriptions) current.listeners.get(type)?.delete(observer);
    if (--current.subscribers !== 0) return;
    current.generation++;
    current.source?.close();
    clearTimeout(current.timer);
    document.removeEventListener("visibilitychange", current.visibility);
    projects.delete(projectId);
  };
}

export const PROJECT_SNAPSHOT_BUFFER = 256;

export function subscribeProjectQuery(http: EventHttp, projectId: string, options: {
  readonly topics: readonly string[];
  readonly apply: (event: Event) => void;
  readonly refresh: () => void;
  readonly restart?: () => void;
  readonly accepts?: (event: Event) => boolean;
  readonly connection?: Readonly<Record<string, EventListener>>;
}) {
  let active = true;
  let generation = 0;
  let loading = false;
  let watermark: bigint | null = null;
  let frames: Event[] = [];
  const revisions = new Map<string, bigint>();
  let scheduled = false;
  let restarting = false;
  const refresh = (restart = false) => {
    restarting ||= restart;
    if (scheduled || !active) return;
    scheduled = true;
    queueMicrotask(() => {
      scheduled = false;
      const run = restarting ? options.restart ?? options.refresh : options.refresh;
      restarting = false;
      if (active) run();
    });
  };
  const apply = (event: Event) => {
    const cursor = sequence((event as MessageEvent).lastEventId);
    if (cursor !== null && watermark !== null && cursor <= watermark) return;
    const data = projectEventData(event);
    const id = data.entity_id ?? data.id;
    const revision = sequence(data.entity_revision ?? data.revision);
    const key = `${event.type.split(".")[0]}:${String(id)}`;
    if (id !== undefined && revision !== null) {
      if (revision <= (revisions.get(key) ?? -1n)) return;
      revisions.delete(key);
      revisions.set(key, revision);
      if (revisions.size > 1024) revisions.delete(revisions.keys().next().value!);
    }
    options.apply(event);
  };
  const listener: EventListener = (event) => {
    if (options.accepts?.(event) === false) return;
    if (!loading) { apply(event); return; }
    if (frames.length >= PROJECT_SNAPSHOT_BUFFER) {
      generation++;
      frames = [];
      refresh(true);
    } else frames.push(event);
  };
  const closeStream = subscribeProjectEvents(http, projectId, {
    ...options.connection,
    ...Object.fromEntries(options.topics.map((topic) => [topic, listener])),
    reset: () => { generation++; loading = true; frames = []; revisions.clear(); watermark = null; refresh(true); },
  });
  const stream = streams.get(http)?.get(projectId);
  const token = Symbol();
  const queryState = { pending: stream?.resetting ?? false, watermark: null as bigint | null };
  stream?.queries.set(token, queryState);
  return {
    refresh: () => refresh(),
    beginSnapshot() {
      const version = ++generation;
      loading = true;
      frames = [];
      let completed = false;
      return {
        current: () => active && version === generation,
        complete(value?: number | string | bigint) {
          if (!active || version !== generation || completed) return;
          completed = true;
          watermark = sequence(value);
          queryState.watermark = watermark;
          queryState.pending = false;
          stream?.resume();
          loading = false;
          const buffered = frames;
          frames = [];
          for (const frame of buffered) apply(frame);
        },
      };
    },
    close() { active = false; generation++; frames = []; stream?.queries.delete(token); closeStream(); stream?.resume(); },
  };
}

export function observeProjectQuery<T>(http: EventHttp, projectId: string, options: {
  readonly topics: readonly string[];
  readonly load: (signal: AbortSignal) => Promise<{ readonly value: T; readonly watermark?: number | string }>;
  readonly apply: (value: T) => void;
  readonly event: (event: Event) => void;
  readonly beforeRestart?: () => void;
  readonly accepts?: (event: Event) => boolean;
  readonly error?: (cause: unknown) => void;
  readonly connection?: Readonly<Record<string, EventListener>>;
}) {
  let controller: AbortController | undefined;
  const query = subscribeProjectQuery(http, projectId, { topics: options.topics, apply: options.event,
    accepts: options.accepts, connection: options.connection, refresh: () => { void load(); },
    restart: () => { options.beforeRestart?.(); void load(); } });
  const load = async () => {
    controller?.abort();
    controller = new AbortController();
    const snapshot = query.beginSnapshot();
    try {
      const result = await options.load(controller.signal);
      if (!snapshot.current()) return;
      options.apply(result.value);
      snapshot.complete(result.watermark);
    } catch (cause) {
      if (snapshot.current()) { options.error?.(cause); snapshot.complete(); }
    }
  };
  void load();
  return { refresh: query.refresh, close: () => { controller?.abort(); query.close(); } };
}
