import { clearQueryCache, fetchArchiveQuery, invalidateArchiveRevision, scopeOf } from "../app/lib/queryCache.ts";
import { ArchiveUnavailable, validateArchive, type ArchiveDescriptor } from "./archiveDecode.ts";

type ArchiveFetch = (input: string, init?: RequestInit) => Promise<Response>;

function loadWorker(url: string, descriptor: ArchiveDescriptor, signal: AbortSignal): Promise<{ data?: unknown; expired?: boolean }> {
  return new Promise((resolve, reject) => {
    if (signal.aborted) { reject(new ArchiveUnavailable("cancelled")); return; }
    const worker = new Worker(new URL("./archive.worker.ts", import.meta.url), { type: "module" });
    const close = () => { signal.removeEventListener("abort", abort); worker.terminate(); };
    const abort = () => { close(); reject(new ArchiveUnavailable("cancelled")); };
    signal.addEventListener("abort", abort, { once: true });
    worker.onerror = () => { close(); reject(new ArchiveUnavailable("decoder failed")); };
    worker.onmessage = (event: MessageEvent<{ data?: unknown; expired?: boolean; error?: string }>) => {
      close();
      if (event.data.error !== undefined) reject(new ArchiveUnavailable(event.data.error));
      else resolve(event.data);
    };
    worker.postMessage({ url, descriptor });
  });
}

export async function resolveArchivedResponse(value: unknown, target: string, fetch: ArchiveFetch, owner: object, signal?: AbortSignal): Promise<unknown> {
  if (typeof value !== "object" || value === null || !("archive" in value)) return value;
  const descriptor = value.archive as ArchiveDescriptor;
  validateArchive(descriptor);
  const origin = new URL(target, globalThis.location?.origin).origin;
  const access = new URL(descriptor.access_path, origin);
  const expected = `/api/v2/organizations/${encodeURIComponent(descriptor.organization_id)}/projects/${encodeURIComponent(descriptor.project_id)}/work-items/${encodeURIComponent(descriptor.work_item_id)}/archives/${descriptor.sha256}/access`;
  if (access.origin !== origin || access.pathname !== expected || access.searchParams.get("section") !== descriptor.section_id || access.searchParams.get("revision") !== descriptor.revision) throw new ArchiveUnavailable("invalid access path");
  const scope = ["archive", scopeOf(owner), descriptor.organization_id, descriptor.project_id, descriptor.work_item_id];
  invalidateArchiveRevision(scope, descriptor.revision);
  const data = await fetchArchiveQuery([...scope, descriptor.revision, descriptor.sha256, descriptor.section_id], descriptor.uncompressed_size, async (readSignal) => {
    for (let attempt = 0; attempt < 2; attempt++) {
      if (signal?.aborted) throw new ArchiveUnavailable("cancelled");
      const response = await fetch(access.href, { credentials: "same-origin", cache: "no-store", signal: readSignal });
      if (!response.ok) {
        if ([401, 403].includes(response.status)) clearQueryCache();
        throw new ArchiveUnavailable(`authorization HTTP ${response.status}`);
      }
      const grant = await response.json() as { url: string; sha256: string; expires_at: string };
      if (grant.sha256 !== descriptor.sha256 || !Number.isFinite(Date.parse(grant.expires_at))) throw new ArchiveUnavailable("invalid grant");
      const url = new URL(grant.url);
      if (url.protocol !== "https:" && !(url.protocol === "http:" && ["localhost", "127.0.0.1"].includes(url.hostname))) throw new ArchiveUnavailable("invalid storage URL");
      if (Date.parse(grant.expires_at) <= Date.now()) {
        if (attempt === 0) continue;
        throw new ArchiveUnavailable("expired URL");
      }
      const result = await loadWorker(url.href, descriptor, readSignal);
      if (!result.expired) return result.data;
    }
    throw new ArchiveUnavailable("expired URL");
  }).catch((error: unknown) => {
    if (error instanceof ArchiveUnavailable) throw error;
    throw new ArchiveUnavailable("access revoked");
  });
  const current = "current" in value ? value.current : null;
  if ("projection" in value && value.projection === "issue") {
    if (typeof data !== "object" || data === null || !("body" in data) || typeof data.body !== "string" || typeof current !== "object" || current === null) throw new ArchiveUnavailable("issue schema");
    return { ...current, body: data.body, archive_summary: { revision: descriptor.revision, attempts: "attempts" in data ? data.attempts : [] } };
  }
  if ("projection" in value && value.projection === "revision") {
    if (typeof data !== "object" || data === null || typeof current !== "object" || current === null) throw new ArchiveUnavailable("revision schema");
    const historical = data as Record<string, unknown>;
    const live = current as Record<string, unknown>;
    const result = { ...historical };
    for (const key of ["dependencies", "blockers", "dependents", "related", "blocked_reason", "unblocks_reason"]) result[key] = live[key];
    return result;
  }
  if (typeof data === "object" && data !== null && typeof current === "object" && current !== null) return { ...data, ...current };
  return data;
}
