import { archiveFixture as fixture } from "./archiveFixture.ts";
import { afterEach, describe, expect, it, vi } from "vitest";
import { decodeArchive, type ArchiveDescriptor } from "../src/runtime/archiveDecode.ts";
import { resolveArchivedResponse } from "../src/runtime/archive.ts";
import { authorizeQueryCache, clearQueryCache, fetchArchiveQuery, getQueryData, invalidateArchiveRevision } from "../src/app/lib/queryCache.ts";

afterEach(() => { clearQueryCache(); vi.unstubAllGlobals(); vi.useRealTimers(); });


describe("archive bytes", () => {
  it("decodes gzip without HTTP Content-Encoding exactly once", async () => {
    const f = await fixture();
    expect(await decodeArchive(f.response(), f.descriptor)).toEqual({ body: "Archived history" });
  });

  it("refuses viewer-filtered relationships in shared issue objects", async () => {
    const f = await fixture({ body: "History", dependencies: ["hidden_issue"] });
    await expect(decodeArchive(f.response(), f.descriptor)).rejects.toThrow("shared relationships");
  });

  it.each(["checksum", "compressed limit", "decoded limit", "schema", "section", "403", "404", "content encoding"])("refuses %s", async (failure) => {
    const f = await fixture();
    const descriptor = { ...f.descriptor };
    let response = f.response();
    if (failure === "checksum") descriptor.sha256 = "0".repeat(64);
    if (failure === "compressed limit") descriptor.compressed_size--;
    if (failure === "decoded limit") descriptor.uncompressed_size--;
    if (failure === "schema") descriptor.schema_version = 2;
    if (failure === "section") descriptor.section_id = "attempts";
    if (failure === "403" || failure === "404") response = new Response(null, { status: Number(failure) });
    if (failure === "content encoding") response = new Response(f.bytes, { headers: { "Content-Encoding": "gzip" } });
    await expect(decodeArchive(response, descriptor)).rejects.toThrow("unavailable");
  });
});

function installWorker(read: (url: string, init: RequestInit) => Promise<Response>) {
  const terminated = vi.fn();
  class FixtureWorker {
    onmessage: ((event: { data: unknown }) => void) | null = null;
    onerror: (() => void) | null = null;
    terminate = terminated;
    postMessage(input: { url: string; descriptor: ArchiveDescriptor }) {
      void read(input.url, { credentials: "omit", cache: "no-store", redirect: "error", referrerPolicy: "no-referrer" }).then(async (response) => {
        if (response.status === 403) this.onmessage?.({ data: { expired: true } });
        else {
          try { this.onmessage?.({ data: { data: await decodeArchive(response, input.descriptor) } }); }
          catch { this.onmessage?.({ data: { error: "invalid archive" } }); }
        }
      });
    }
  }
  vi.stubGlobal("Worker", FixtureWorker);
  return terminated;
}

describe("authorized archive reads", () => {
  it.each([false, true])("renews an expired object URL once, repeated expiration=%s", async (alwaysExpired) => {
    const f = await fixture();
    let calls = 0;
    const objects = vi.fn(async (_url: string, _init: RequestInit) => ++calls === 1 || alwaysExpired ? new Response(null, { status: 403 }) : f.response());
    const terminated = installWorker(objects);
    const grants = vi.fn(async () => Response.json({ sha256: f.descriptor.sha256, url: "https://objects.example.test/archive", expires_at: new Date(Date.now() + 300_000).toISOString() }));
    const read = resolveArchivedResponse({ archive: f.descriptor }, "https://cloud.example.test/issue", grants, {});
    if (alwaysExpired) await expect(read).rejects.toThrow("expired URL");
    else expect(await read).toEqual({ body: "Archived history" });
    expect(grants).toHaveBeenCalledTimes(2);
    expect(objects).toHaveBeenCalledTimes(2);
    expect(objects.mock.calls[0]?.[1]).toMatchObject({ credentials: "omit" });
    expect(terminated).toHaveBeenCalledTimes(2);
  });

  it.each([401, 403, 404])("overlays live metadata, excludes archived relationships, and clears cached sections on a refused grant (%s)", async (status) => {
    const f = await fixture({ body: "Archived history" });
    const objects = vi.fn(async (_url: string, _init: RequestInit) => f.response());
    installWorker(objects);
    const grants = vi.fn(async () => Response.json({ sha256: f.descriptor.sha256, url: "https://objects.example.test/archive", expires_at: new Date(Date.now() + 300_000).toISOString() }));
    const owner = {};
    vi.stubGlobal("location", { origin: "https://cloud.example.test" });
    const envelope = { archive: f.descriptor, current: { title: "Current title", dependencies: [] }, projection: "issue" };
    const result = await resolveArchivedResponse(envelope, "/issue", grants, owner);
    expect(result).toMatchObject({ body: "Archived history", title: "Current title", dependencies: [] });
    await resolveArchivedResponse(envelope, "https://cloud.example.test/issue", grants, owner);
    expect(objects).toHaveBeenCalledTimes(1);
    const other = await fixture({}, "attempts");
    await expect(resolveArchivedResponse({ archive: other.descriptor }, "https://cloud.example.test/issue", async () => new Response(null, { status }), owner)).rejects.toThrow("unavailable");
    await resolveArchivedResponse(envelope, "https://cloud.example.test/issue", grants, owner);
    expect(objects).toHaveBeenCalledTimes(2);
  });
});

describe("archive cache bounds", () => {
  it.each(["sections", "bytes", "idle", "sign-out", "revision", "org switch", "grant change"])("evicts decoded data for %s", async (reason) => {
    vi.useFakeTimers();
    authorizeQueryCache("principal-session-org-grants-v1");
    const key = ["archive", "principal-session", "org_example", "prj_example", "wi_example"];
    const first = [...key, "1", "digest1"];
    await fetchArchiveQuery(first, 16 * 1024 * 1024, async () => ({ value: "first" }));
    expect(getQueryData(first)).toEqual({ value: "first" });
    if (reason === "sign-out") clearQueryCache();
    if (reason === "org switch" || reason === "grant change") authorizeQueryCache(`principal-session-${reason}`);
    if (reason === "revision") invalidateArchiveRevision(key, "2");
    if (reason === "idle") vi.advanceTimersByTime(10 * 60_000);
    if (reason === "sections" || reason === "bytes") {
      const count = reason === "sections" ? 8 : 4;
      for (let index = 0; index < count; index++) await fetchArchiveQuery([...key, "1", `digest${index + 2}`], reason === "bytes" ? 16 * 1024 * 1024 : 1, async () => index);
    }
    expect(getQueryData(first)).toBeUndefined();
  });
});
