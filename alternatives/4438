// @vitest-environment jsdom
import { act, cleanup, renderHook } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { useViewState } from "../../src/app/work/lib/useViewState.ts";
import { DEFAULT_VIEW_STATE, parseViewState, serializeViewState, readStoredViewState, writeStoredViewState } from "../../src/app/work/lib/viewState.ts";

const fixture = vi.hoisted(() => ({
  search: "",
  navigate: vi.fn(),
  http: { getViewPreference: vi.fn(), setViewPreference: vi.fn() },
  client: { http: { origin: "", apiBase: "/api/v2/organizations/org_test" },
    bootstrap: { organization: { id: "org_test" }, actor: { principal_id: "alice" } } },
}));

vi.mock("@tanstack/react-router", () => ({
  useNavigate: () => fixture.navigate,
  useRouterState: ({ select }: { select: (state: unknown) => unknown }) => select({ location: { searchStr: fixture.search } }),
}));
vi.mock("../../src/app/client.ts", () => ({ useClient: () => fixture.client }));
vi.mock("../../src/app/work/lib/useWork.ts", () => ({ useWorkHttp: () => fixture.http }));

const cacheKey = (project: string | null, user = "alice") => JSON.stringify(["", fixture.client.http.apiBase, "org_test", user, project ?? ""]);

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => { resolve = done; });
  return { promise, resolve };
}

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.resetAllMocks();
  localStorage.clear();
  fixture.search = "";
  fixture.client.bootstrap.actor.principal_id = "alice";
});

describe("Hub Work view preferences", () => {
  it.each([
    { name: "Hub before defaults", url: "", cache: null, hub: "view=list&tab=closed&collapsed=Todo&completed=7d", first: "", final: "view=list&tab=closed&collapsed=Todo&completed=7d" },
    { name: "cache for first paint, Hub thereafter", url: "", cache: "sort=title", hub: "view=list", first: "sort=title", final: "view=list" },
    { name: "missing Hub record clears stale cache", url: "", cache: "view=list", hub: null, first: "view=list", final: "" },
    { name: "explicit URL wins", url: "?view=list&tab=all", cache: "sort=title", hub: "sort=updated", first: "view=list&tab=all", final: "view=list&tab=all" },
    { name: "explicit default URL wins", url: "?view=board", cache: "view=list", hub: "view=list", first: "", final: "" },
  ])("$name", async ({ url, cache, hub, first, final }) => {
    fixture.search = url;
    if (cache !== null) writeStoredViewState(cacheKey("project"), parseViewState(cache));
    const read = deferred<{ query: string | null }>();
    fixture.http.getViewPreference.mockReturnValue(read.promise);
    const { result } = renderHook(() => useViewState("project"));
    expect(serializeViewState(result.current[0])).toBe(first);
    await act(async () => { read.resolve({ query: hub }); });
    expect(serializeViewState(result.current[0])).toBe(final);
    expect(readStoredViewState(cacheKey("project"))).toEqual(hub === null ? null : parseViewState(hub));
    if (url !== "") expect(fixture.navigate).not.toHaveBeenCalled();
    expect(fixture.http.setViewPreference).not.toHaveBeenCalled();
  });

  it("debounces user edits, ignores a delayed read, and caches only confirmed writes", async () => {
    vi.useFakeTimers();
    const read = deferred<{ query: string | null }>();
    const saved = deferred<{ query: string | null }>();
    fixture.http.getViewPreference.mockReturnValue(read.promise);
    fixture.http.setViewPreference.mockReturnValue(saved.promise);
    const { result } = renderHook(() => useViewState("project"));
    act(() => { result.current[1](parseViewState("view=list")); });
    act(() => { result.current[1](parseViewState("view=list&tab=closed&completed=7d")); });
    await act(async () => { read.resolve({ query: "sort=title" }); });
    expect(serializeViewState(result.current[0])).toBe("view=list&tab=closed&completed=7d");
    expect(readStoredViewState(cacheKey("project"))).toBeNull();
    await act(async () => { await vi.advanceTimersByTimeAsync(399); });
    expect(fixture.http.setViewPreference).not.toHaveBeenCalled();
    await act(async () => { await vi.advanceTimersByTimeAsync(1); });
    expect(fixture.http.setViewPreference).toHaveBeenCalledExactlyOnceWith("project", "view=list&tab=closed&completed=7d");
    expect(readStoredViewState(cacheKey("project"))).toBeNull();
    await act(async () => { saved.resolve({ query: "view=list&tab=closed&completed=7d" }); });
    expect(readStoredViewState(cacheKey("project"))?.tab).toBe("closed");
  });

  it("saves edits to an explicit URL and deletes the Hub record on reset", async () => {
    vi.useFakeTimers();
    fixture.search = "?view=list";
    fixture.http.getViewPreference.mockResolvedValue({ query: "sort=title" });
    fixture.http.setViewPreference.mockImplementation(async (_project, query) => ({ query }));
    const { result } = renderHook(() => useViewState(null));
    await act(async () => {});
    expect(fixture.http.setViewPreference).not.toHaveBeenCalled();
    act(() => { result.current[1](parseViewState("view=list&tab=closed")); });
    await act(async () => { await vi.advanceTimersByTimeAsync(400); });
    expect(fixture.http.setViewPreference).toHaveBeenLastCalledWith(null, "view=list&tab=closed");
    act(() => { result.current[1](DEFAULT_VIEW_STATE); });
    await act(async () => { await vi.advanceTimersByTimeAsync(400); });
    expect(fixture.http.setViewPreference).toHaveBeenLastCalledWith(null, null);
    expect(readStoredViewState(cacheKey(null))).toBeNull();
  });

  it("isolates cached first paint across users and scopes and ignores obsolete reads", async () => {
    writeStoredViewState(cacheKey("project"), parseViewState("sort=title"));
    const old = deferred<{ query: string | null }>();
    fixture.http.getViewPreference.mockReturnValueOnce(old.promise).mockResolvedValue({ query: null });
    const { result, rerender } = renderHook(({ project }) => useViewState(project), { initialProps: { project: "project" as string | null } });
    expect(result.current[0].sort).toBe("title");
    rerender({ project: null });
    expect(result.current[0]).toEqual(DEFAULT_VIEW_STATE);
    await act(async () => { old.resolve({ query: "view=list" }); });
    expect(result.current[0]).toEqual(DEFAULT_VIEW_STATE);
    fixture.client.bootstrap.actor.principal_id = "bob";
    rerender({ project: "project" });
    expect(result.current[0]).toEqual(DEFAULT_VIEW_STATE);
    await act(async () => {});
    expect(fixture.http.setViewPreference).not.toHaveBeenCalled();
  });

  it("flushes a pending edit when the page is hidden", async () => {
    vi.useFakeTimers();
    fixture.http.getViewPreference.mockResolvedValue({ query: null });
    fixture.http.setViewPreference.mockImplementation(async (_project, query) => ({ query }));
    const { result } = renderHook(() => useViewState("project"));
    await act(async () => {});
    act(() => { result.current[1](parseViewState("view=list&tab=closed")); });
    await act(async () => { window.dispatchEvent(new Event("pagehide")); });
    expect(fixture.http.setViewPreference).toHaveBeenCalledExactlyOnceWith("project", "view=list&tab=closed");
    await act(async () => { await vi.advanceTimersByTimeAsync(400); });
    expect(fixture.http.setViewPreference).toHaveBeenCalledTimes(1);
  });

  it("flushes a pending edit on scope exit and serializes saves", async () => {
    vi.useFakeTimers();
    fixture.http.getViewPreference.mockResolvedValue({ query: null });
    const first = deferred<{ query: string | null }>();
    fixture.http.setViewPreference.mockReturnValueOnce(first.promise).mockResolvedValue({ query: null });
    const { result, rerender } = renderHook(({ project }) => useViewState(project), { initialProps: { project: "project" as string | null } });
    await act(async () => {});
    act(() => { result.current[1](parseViewState("view=list")); });
    rerender({ project: null });
    await act(async () => {});
    expect(fixture.http.setViewPreference).toHaveBeenCalledExactlyOnceWith("project", "view=list");
    act(() => { result.current[1](DEFAULT_VIEW_STATE); });
    await act(async () => { await vi.advanceTimersByTimeAsync(400); });
    expect(fixture.http.setViewPreference).toHaveBeenCalledTimes(1);
    await act(async () => { first.resolve({ query: "view=list" }); });
    expect(fixture.http.setViewPreference).toHaveBeenLastCalledWith(null, null);
  });
});
