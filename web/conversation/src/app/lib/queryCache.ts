// The application's client data cache.
//
// Every screen reads through it, so a revisit paints what the last visit
// read and revalidates behind it. It follows the Work board store's rules
// (`work/lib/boardStore.ts`): entries belong to one account, an account change,
// sign-out or refused credential drops all of them, and a response that
// arrives for a cleared account is never published.
import React from "react";

export type QueryKey = readonly unknown[];

export interface QuerySnapshot<T> {
  readonly data: T | undefined;
  readonly error: unknown;
  readonly updatedAt: number;
  readonly fetching: boolean;
}

export interface FetchOptions {
  /** How long a successful read stays fresh, in milliseconds. */
  readonly staleTime?: number;
  /** Read again even when fresh; a read already in flight runs once more after it. */
  readonly force?: boolean;
  /** With `force`, share a read already in flight instead of queueing another. */
  readonly join?: boolean;
}

/**
 * One read. `partial` paints data the read already has while it finishes,
 * such as a page's first paint before its lazy details.
 */
export type Read<T> = (signal: AbortSignal, partial: (data: T) => void) => Promise<T>;

interface Entry {
  readonly key: QueryKey;
  snapshot: QuerySnapshot<unknown>;
  read: Read<unknown> | null;
  promise: Promise<unknown> | null;
  controller: AbortController | null;
  again: boolean;
  invalidated: boolean;
  observers: number;
}

const EMPTY: QuerySnapshot<never> = { data: undefined, error: null, updatedAt: 0, fetching: false };
/**
 * Entries kept per class. Issue reads (one per opened or hovered issue) have
 * their own small budget, so browsing issues never evicts page reports.
 */
const LIMITS = { issue: 24, page: 64, archive: 8 } as const;
type EntryClass = keyof typeof LIMITS;

function classOf(key: QueryKey): EntryClass {
  if (key[0] === "archive") return "archive";
  return key[0] === "issue" || key[0] === "issue-head" ? "issue" : "page";
}

function countOf(kind: EntryClass): number {
  let count = 0;
  for (const entry of entries.values()) if (classOf(entry.key) === kind) count++;
  return count;
}

let account: string | null = null;
let epoch = 0;
const entries = new Map<string, Entry>();
const listeners = new Set<() => void>();
const archiveSizes = new Map<string, { bytes: number; timer: ReturnType<typeof setTimeout> }>();

function removeArchive(id: string): void {
  const item = entries.get(id)?.key[4];
  const size = archiveSizes.get(id);
  if (size !== undefined) clearTimeout(size.timer);
  archiveSizes.delete(id);
  entries.get(id)?.controller?.abort();
  entries.delete(id);
  if (typeof item === "string") {
    for (const [relatedId, related] of entries) {
      if (classOf(related.key) === "issue" && related.key[1] === item && related.observers === 0) {
        related.controller?.abort();
        entries.delete(relatedId);
      }
    }
  }
}

export async function fetchArchiveQuery<T>(key: QueryKey, bytes: number, read: Read<T>): Promise<T> {
  const id = hash(key);
  const result = await fetchQuery(key, read, { staleTime: Infinity });
  const snapshot = getQuerySnapshot<T>(key);
  if (snapshot.error !== null) throw snapshot.error;
  if (result === undefined || snapshot.data === undefined) throw new Error("Archived detail is unavailable");
  const previous = archiveSizes.get(id);
  if (previous !== undefined) clearTimeout(previous.timer);
  archiveSizes.delete(id);
  archiveSizes.set(id, { bytes, timer: setTimeout(() => removeArchive(id), 10 * 60_000) });
  let total = [...archiveSizes.values()].reduce((sum, value) => sum + value.bytes, 0);
  for (const [old, size] of archiveSizes) {
    if (total <= 64 * 1024 * 1024 && archiveSizes.size <= 8) break;
    total -= size.bytes;
    removeArchive(old);
  }
  return result;
}

export function invalidateArchiveRevision(scope: QueryKey, revision: string): void {
  for (const [id, entry] of entries) {
    if (entry.key[0] === "archive" && matches(entry.key, scope) && entry.key[scope.length] !== revision) removeArchive(id);
  }
}

function hash(key: QueryKey): string {
  return JSON.stringify(key);
}

function emit(): void {
  for (const listener of listeners) listener();
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

function entryFor(key: QueryKey): Entry {
  const id = hash(key);
  let entry = entries.get(id);
  if (entry === undefined) {
    entry = { key, snapshot: EMPTY, read: null, promise: null, controller: null, again: false, invalidated: false, observers: 0 };
    entries.set(id, entry);
    prune(classOf(key));
  } else {
    entries.delete(id);
    entries.set(id, entry);
  }
  return entry;
}

function prune(kind: EntryClass): void {
  let count = countOf(kind);
  for (const [id, entry] of entries) {
    if (count <= LIMITS[kind]) return;
    if (classOf(entry.key) !== kind || entry.observers > 0 || entry.promise !== null) continue;
    if (kind === "archive") removeArchive(id);
    else entries.delete(id);
    count--;
  }
}

function publish(entry: Entry, snapshot: QuerySnapshot<unknown>): void {
  entry.snapshot = snapshot;
  emit();
}

function current(id: string, entry: Entry, owner: number): boolean {
  return owner === epoch && entries.get(id) === entry;
}

function run(id: string, entry: Entry): Promise<unknown> {
  const owner = epoch;
  const controller = new AbortController();
  entry.controller = controller;
  entry.again = false;
  entry.invalidated = false;
  publish(entry, { ...entry.snapshot, fetching: true });
  const read = entry.read!;
  const promise = (async () => {
    try {
      const data = await read(controller.signal, (data) => {
        if (current(id, entry, owner) && entry.controller === controller) publish(entry, { data, error: null, updatedAt: Date.now(), fetching: true });
      });
      if (!current(id, entry, owner)) return undefined;
      publish(entry, { data, error: null, updatedAt: Date.now(), fetching: false });
      return data;
    } catch (error) {
      if (!current(id, entry, owner)) return undefined;
      publish(entry, { ...entry.snapshot, error, fetching: false });
      return undefined;
    } finally {
      if (entry.controller === controller) {
        entry.promise = null;
        entry.controller = null;
        if (entry.again && current(id, entry, owner)) void run(id, entry);
      }
    }
  })();
  entry.promise = promise;
  return promise;
}

function fresh(entry: Entry, staleTime: number): boolean {
  return !entry.invalidated && entry.snapshot.updatedAt > 0 && entry.snapshot.error === null &&
    Date.now() - entry.snapshot.updatedAt < staleTime;
}

/**
 * Reads `key` through the cache. It never rejects: a failed read resolves to
 * `undefined` and leaves the error, and any earlier data, in the snapshot.
 */
export function fetchQuery<T>(key: QueryKey, read: Read<T>, options: FetchOptions = {}): Promise<T | undefined> {
  const id = hash(key);
  const entry = entryFor(key);
  entry.read = read as Read<unknown>;
  if (entry.promise !== null) {
    if (options.force === true && options.join !== true) entry.again = true;
    return entry.promise as Promise<T | undefined>;
  }
  if (options.force !== true && fresh(entry, options.staleTime ?? 0)) return Promise.resolve(entry.snapshot.data as T);
  return run(id, entry) as Promise<T | undefined>;
}

/** Starts a read for a destination the reader is about to open. */
/**
 * Starts a read for a destination the reader is about to open. It is skipped
 * when its class is already full of entries in use, and `signal` cancels it
 * while no screen has started showing it.
 */
export function prefetchQuery<T>(key: QueryKey, read: Read<T>, staleTime: number, signal?: AbortSignal): void {
  const id = hash(key);
  if (signal?.aborted === true) return;
  if (!entries.has(id)) {
    const kind = classOf(key);
    prune(kind);
    if (countOf(kind) >= LIMITS[kind]) return;
  }
  void fetchQuery(key, read, { staleTime });
  signal?.addEventListener("abort", () => {
    const entry = entries.get(id);
    if (entry !== undefined && entry.observers === 0 && entry.promise !== null) entry.controller?.abort();
  }, { once: true });
}

export function getQuerySnapshot<T>(key: QueryKey): QuerySnapshot<T> {
  return (entries.get(hash(key))?.snapshot ?? EMPTY) as QuerySnapshot<T>;
}

export function getQueryData<T>(key: QueryKey): T | undefined {
  return getQuerySnapshot<T>(key).data;
}

/** Replaces cached data without a round trip: a mutation's answer or a live event. */
export function setQueryData<T>(key: QueryKey, update: T | ((previous: T | undefined) => T)): void {
  const entry = entryFor(key);
  const data = typeof update === "function" ? (update as (previous: T | undefined) => T)(entry.snapshot.data as T | undefined) : update;
  publish(entry, { data, error: null, updatedAt: Date.now(), fetching: entry.snapshot.fetching });
}

function matches(key: QueryKey, prefix: QueryKey): boolean {
  return prefix.every((part, index) => hash([part]) === hash([key[index]]));
}

/**
 * Marks every entry under `prefix` stale. Entries a screen is showing read
 * again now; the rest read again on their next visit, so data a change event
 * outdated never paints as current.
 */
export function invalidateQueries(prefix: QueryKey): void {
  for (const [id, entry] of entries) {
    if (!matches(entry.key, prefix)) continue;
    entry.invalidated = true;
    if (entry.observers === 0 || entry.read === null) continue;
    if (entry.promise !== null) entry.again = true;
    else void run(id, entry);
  }
}

/** Drops every entry; responses still in flight are discarded. */
export function clearQueryCache(): void {
  epoch++;
  account = null;
  for (const entry of entries.values()) entry.controller?.abort();
  entries.clear();
  for (const value of archiveSizes.values()) clearTimeout(value.timer);
  archiveSizes.clear();
  emit();
}

/** Scopes the cache to one account; a different account starts empty. */
export function authorizeQueryCache(next: string): void {
  if (next === account) return;
  clearQueryCache();
  account = next;
}

function currentEpoch(): number {
  return epoch;
}

const identities = new WeakMap<object, number>();
let identity = 0;

/** A stable number for an API client, so two clients never share entries. */
export function scopeOf(client: object): number {
  let id = identities.get(client);
  if (id === undefined) {
    id = ++identity;
    identities.set(client, id);
  }
  return id;
}

export interface QueryOptions {
  readonly staleTime?: number;
  /**
   * Keeps the last data on screen while a new key loads, as long as both keys
   * share this group (a range or filter change within one scope).
   */
  readonly keepPreviousIn?: string;
}

export interface QueryResult<T> {
  readonly data: T | undefined;
  readonly error: unknown;
  /** True only on a true first load: nothing to show yet and no error. */
  readonly pending: boolean;
  /** A read is in flight behind data already on screen. */
  readonly refreshing: boolean;
  /** The data shown belongs to the previous key. */
  readonly placeholder: boolean;
  readonly refresh: () => Promise<void>;
}

export function useQuery<T>(key: QueryKey | null, read: Read<T>, options: QueryOptions = {}): QueryResult<T> {
  const id = key === null ? null : hash(key);
  const owner = React.useSyncExternalStore(subscribe, currentEpoch, currentEpoch);
  const snapshot = React.useSyncExternalStore(
    subscribe,
    () => (id === null ? EMPTY : (entries.get(id)?.snapshot ?? EMPTY)) as QuerySnapshot<T>,
    () => EMPTY as QuerySnapshot<T>,
  );
  const latest = React.useRef(read);
  latest.current = read;
  const staleTime = options.staleTime ?? 0;
  React.useEffect(() => {
    if (key === null) return;
    const entry = entryFor(key);
    entry.observers++;
    void fetchQuery(key, (signal, partial) => latest.current(signal, partial), { staleTime });
    return () => {
      entry.observers--;
      prune(classOf(entry.key));
    };
  }, [id, owner, staleTime]);
  const previous = React.useRef<{ owner: number; group: string; data: T } | null>(null);
  const group = options.keepPreviousIn;
  if (snapshot.data !== undefined && group !== undefined) previous.current = { owner, group, data: snapshot.data };
  const kept = snapshot.data === undefined && group !== undefined && previous.current !== null &&
    previous.current.owner === owner && previous.current.group === group ? previous.current.data : undefined;
  const data = snapshot.data ?? kept;
  const refresh = React.useCallback(async () => {
    if (key === null) return;
    await fetchQuery(key, (signal, partial) => latest.current(signal, partial), { force: true });
  }, [id]);
  return {
    data,
    error: snapshot.error,
    pending: key !== null && data === undefined && snapshot.error === null,
    refreshing: snapshot.fetching && data !== undefined,
    placeholder: kept !== undefined,
    refresh,
  };
}
