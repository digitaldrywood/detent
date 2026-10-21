// @vitest-environment jsdom
import { act, cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import {
  authorizeQueryCache,
  clearQueryCache,
  fetchQuery,
  fetchArchiveQuery,
  getQueryData,
  invalidateQueries,
  prefetchQuery,
  useQuery,
} from "../src/app/lib/queryCache.ts";
import { useReports } from "../src/app/reports/reportsQuery.ts";
import { useIssue } from "../src/app/work/IssuePage.tsx";
import { authorizeBoardCache, clearBoardCache, rejectBoardCache } from "../src/app/work/lib/boardStore.ts";
import { makeWorkHttp } from "../src/app/work/lib/workHttp.ts";
import type { AccountApi } from "../src/app/account/api.ts";

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (cause: unknown) => void;
  const promise = new Promise<T>((done, fail) => { resolve = done; reject = fail; });
  return { promise, resolve, reject };
}

function Probe({ id, read, group, staleTime = 0 }: { id: string; read: (signal: AbortSignal) => Promise<string>; group?: string; staleTime?: number }) {
  const query = useQuery(["probe", id], read, { staleTime, ...(group === undefined ? {} : { keepPreviousIn: group }) });
  return (
    <div data-testid="probe" data-pending={String(query.pending)} data-placeholder={String(query.placeholder)}>
      {query.pending ? "skeleton" : query.data}
    </div>
  );
}

describe("client data cache", () => {
  it("paints a revisited key from the cache and revalidates in the background", async () => {
    const read = vi.fn(async () => `value ${read.mock.calls.length}`);
    const first = render(<Probe id="a" read={read} />);
    expect(screen.getByTestId("probe").textContent).toBe("skeleton");
    await act(async () => {});
    expect(screen.getByTestId("probe").textContent).toBe("value 1");
    first.unmount();
    render(<Probe id="a" read={read} />);
    expect(screen.getByTestId("probe").textContent).toBe("value 1");
    expect(screen.getByTestId("probe").dataset.pending).toBe("false");
    await act(async () => {});
    expect(read).toHaveBeenCalledTimes(2);
    expect(screen.getByTestId("probe").textContent).toBe("value 2");
  });

  it.each([
    { name: "within one group keeps the previous data", group: ["scope", "scope"], shown: "a data", placeholder: "true" },
    { name: "across groups starts from a skeleton", group: ["one", "two"], shown: "skeleton", placeholder: "false" },
  ])("a key change $name", async ({ group, shown, placeholder }) => {
    const next = deferred<string>();
    const reads: Record<string, () => Promise<string>> = { a: async () => "a data", b: () => next.promise };
    const view = render(<Probe id="a" read={() => reads.a!()} group={group[0]} />);
    await act(async () => {});
    view.rerender(<Probe id="b" read={() => reads.b!()} group={group[1]} />);
    expect(screen.getByTestId("probe").textContent).toBe(shown);
    expect(screen.getByTestId("probe").dataset.placeholder).toBe(placeholder);
    await act(async () => next.resolve("b data"));
    expect(screen.getByTestId("probe").textContent).toBe("b data");
  });

  it.each([
    { name: "switching organization", change: () => authorizeQueryCache("account-two") },
    { name: "signing out", change: () => clearQueryCache() },
  ])("drops cached data and late responses when $name", async ({ change }) => {
    authorizeQueryCache("account-one");
    await fetchQuery(["cached"], async () => "secret");
    const late = deferred<string>();
    void fetchQuery(["late"], () => late.promise);
    change();
    await act(async () => late.resolve("late secret"));
    expect(getQueryData(["cached"])).toBeUndefined();
    expect(getQueryData(["late"])).toBeUndefined();
  });

  it("re-reads shown entries on a change event and marks hidden ones stale", async () => {
    const shown = vi.fn(async () => "shown");
    const hidden = vi.fn(async () => "hidden");
    render(<Probe id="shown" read={shown} staleTime={60_000} />);
    await act(async () => {});
    await fetchQuery(["probe", "hidden"], hidden, { staleTime: 60_000 });
    await act(async () => invalidateQueries(["probe"]));
    expect(shown).toHaveBeenCalledTimes(2);
    expect(hidden).toHaveBeenCalledTimes(1);
    await fetchQuery(["probe", "hidden"], hidden, { staleTime: 60_000 });
    expect(hidden).toHaveBeenCalledTimes(2);
  });

  it("shares one prefetch with the page that opens and honours staleTime", async () => {
    const read = vi.fn(async () => "prefetched");
    prefetchQuery(["probe", "p"], read, 30_000);
    render(<Probe id="p" read={read} staleTime={30_000} />);
    await act(async () => {});
    expect(screen.getByTestId("probe").textContent).toBe("prefetched");
    expect(read).toHaveBeenCalledTimes(1);
    prefetchQuery(["probe", "p"], read, 30_000);
    expect(read).toHaveBeenCalledTimes(1);
  });
});

describe("a refused read", () => {
  it.each([401, 403, 404])("reads once on a repeated %s, removes archives and keeps other screens' entries", async (status) => {
    vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] });
    authorizeBoardCache("account-one");
    const archive = ["archive", "session", "org_example", "prj_example", "wi_refused", "1", "digest"];
    await fetchArchiveQuery(archive, 1, async () => "decoded archive");
    expect(getQueryData(archive)).toBe("decoded archive");
    const otherArchive = [...archive.slice(0, 4), "wi_other", ...archive.slice(5)];
    await fetchArchiveQuery(otherArchive, 1, async () => "other archive");
    const pending = deferred<string>();
    const lateArchive = [...archive, "pending"];
    const late = fetchArchiveQuery(lateArchive, 1, () => pending.promise).then(() => null, (error: unknown) => error);
    const requests: string[] = [];
    const http = makeWorkHttp({
      origin: "", apiBase: "/api", csrfToken: "token",
      fetch: async (input) => {
        requests.push(String(input));
        return Response.json({ code: "forbidden", message: "Forbidden" }, { status: String(input).endsWith("/projects/missing") ? 404 : status });
      },
      onAuthorizationRejected: () => rejectBoardCache("account-one"),
    });
    await expect(http.getProject("missing")).rejects.toMatchObject({ status: 404 });
    expect(getQueryData(archive)).toBe("decoded archive");
    const other = vi.fn(async () => "report");
    function IssueProbe() {
      const issue = useIssue(http, "wi_refused");
      return <div data-testid="issue">{issue.error}</div>;
    }
    render(<><IssueProbe /><Probe id="report" read={other} /></>);
    await act(async () => { await vi.advanceTimersByTimeAsync(5_000); });
    expect(requests).toHaveLength(2);
    expect(other).toHaveBeenCalledTimes(1);
    expect(screen.getByTestId("issue").textContent).toBe("Forbidden");
    expect(screen.getByTestId("probe").textContent).toBe("report");
    expect(getQueryData(archive)).toBeUndefined();
    expect(getQueryData(otherArchive)).toBe(status === 404 ? "other archive" : undefined);
    await act(async () => pending.resolve("late archive"));
    expect(await late).toMatchObject({ message: "Archived detail is unavailable" });
    expect(getQueryData(lateArchive)).toBeUndefined();
    clearBoardCache();
  });
});

describe("cache bounds", () => {
  it("skips an issue prefetch when the issue budget is full of reads in flight", async () => {
    const reads = Array.from({ length: 25 }, () => vi.fn(() => new Promise<string>(() => {})));
    reads.forEach((read, index) => prefetchQuery(["issue", `wi_${index}`], read, 10_000));
    expect(reads.slice(0, 24).every((read) => read.mock.calls.length === 1)).toBe(true);
    expect(reads[24]).not.toHaveBeenCalled();
    const report = vi.fn(async () => "report");
    prefetchQuery(["reports", "project"], report, 60_000);
    expect(report).toHaveBeenCalledTimes(1);
  });

  it("cancels a prefetch nobody has opened when its signal aborts", async () => {
    const controller = new AbortController();
    let aborted = false;
    prefetchQuery(["issue-head", "wi_hover"], (signal) => new Promise<string>((_resolve, reject) => {
      signal.addEventListener("abort", () => { aborted = true; reject(new Error("aborted")); });
    }), 10_000, controller.signal);
    controller.abort();
    await act(async () => {});
    expect(aborted).toBe(true);
    expect(getQueryData(["issue-head", "wi_hover"])).toBeUndefined();
  });
});

describe("Reports read", () => {
  function ReportsProbe({ api, range }: { api: AccountApi; range: "24h" | "7d" }) {
    const read = useReports(api, { projectId: "project", range });
    return <div data-testid="reports">{read.pending ? "skeleton" : String((read.report as unknown as { id: string } | null)?.id)}</div>;
  }

  it("paints a revisit without a skeleton and keeps the report on a range change", async () => {
    const pending = deferred<unknown>();
    const reports = vi.fn(async (_project: string, range: string) => (range === "7d" ? pending.promise : { id: range }));
    const api = { reports } as unknown as AccountApi;
    const first = render(<ReportsProbe api={api} range="24h" />);
    await act(async () => {});
    first.unmount();
    const revisit = render(<ReportsProbe api={api} range="24h" />);
    expect(screen.getByTestId("reports").textContent).toBe("24h");
    revisit.rerender(<ReportsProbe api={api} range="7d" />);
    expect(screen.getByTestId("reports").textContent).toBe("24h");
    await act(async () => pending.resolve({ id: "7d" }));
    expect(screen.getByTestId("reports").textContent).toBe("7d");
    expect(reports).toHaveBeenCalledTimes(2);
  });
});
