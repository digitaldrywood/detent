import type { WorkHttp } from "./workHttp.ts";

interface ProjectStream {
  readonly source: EventSource;
  readonly listeners: Map<string, Set<EventListener>>;
  activity?: Event;
  subscribers: number;
}

const streams = new WeakMap<object, Map<string, ProjectStream>>();

export function subscribeProjectEvents(
  http: Pick<WorkHttp, "eventsUrl">,
  projectId: string,
  listeners: Readonly<Record<string, EventListener>>,
): () => void {
  if (typeof globalThis.EventSource !== "function") return () => {};
  let projects = streams.get(http);
  if (projects === undefined) {
    projects = new Map();
    streams.set(http, projects);
  }
  let stream = projects.get(projectId);
  if (stream === undefined) {
    stream = { source: new globalThis.EventSource(http.eventsUrl(projectId), { withCredentials: true }), listeners: new Map(), subscribers: 0 };
    projects.set(projectId, stream);
  }
  const current = stream;
  current.subscribers++;
  const subscriptions = Object.entries(listeners).map(([type, listener]) => {
    let observers = current.listeners.get(type);
    if (observers === undefined) {
      observers = new Set();
      current.listeners.set(type, observers);
      current.source.addEventListener(type, (event) => {
        if (type === "activity") current.activity = event;
        for (const observer of current.listeners.get(type) ?? []) observer(event);
      });
    }
    const observer: EventListener = (event) => listener(event);
    observers.add(observer);
    if (type === "activity" && current.activity !== undefined) observer(current.activity);
    return { type, observer };
  });
  return () => {
    for (const { type, observer } of subscriptions) current.listeners.get(type)?.delete(observer);
    if (--current.subscribers !== 0) return;
    current.source.close();
    projects.delete(projectId);
  };
}
