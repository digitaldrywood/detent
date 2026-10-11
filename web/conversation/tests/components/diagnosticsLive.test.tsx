import React from "react";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { DiagnosticsRoute } from "../../src/app/diagnostics/DiagnosticsPage.tsx";
import { decodeDiagnostics, decodeHealthFindings } from "../../src/contracts/diagnostics.ts";
import report from "../../src/contracts/fixtures/diagnostics.json";
import health from "../../src/contracts/fixtures/health-findings.json";

const mocks = vi.hoisted(() => ({
  api: { diagnostics: vi.fn(), fleet: vi.fn(), healthFindings: vi.fn() },
  bootstrap: { organization: { name: "Test organization" }, projects: [{ id: "prj_fixture", profile: "native" }] },
  http: { eventsUrl: () => "/events" },
}));
vi.mock("../../src/app/account/context.ts", () => ({ useAccountApi: () => mocks.api, useAccountBootstrap: () => mocks.bootstrap }));
vi.mock("../../src/app/work/lib/useWork.ts", () => ({ useWorkHttp: () => mocks.http }));
vi.mock("../../src/app/pageTitle.ts", () => ({ usePageTitle: () => {} }));
vi.mock("@tanstack/react-router", () => ({ Link: ({ to, children, ...props }: { to: string; children: React.ReactNode }) => <a href={to} {...props}>{children}</a> }));

class FindingEvents extends EventTarget {
  static current: FindingEvents;
  close = vi.fn();
  constructor() { super(); FindingEvents.current = this; }
}
const original = decodeHealthFindings(health).items[0]!;
const next = { ...original, id: "new_finding", summary: "New finding" };
const tick = "2026-10-07T10:00:00Z";
let open = [original];
let resolved: typeof open = [];
function emit(at = tick) {
  FindingEvents.current.dispatchEvent(new MessageEvent("health.findings", { data: at }));
}
function openCount() {
  return screen.getByText("Open findings").parentElement!.firstElementChild!.textContent;
}

beforeEach(() => {
  vi.clearAllMocks();
  vi.stubGlobal("EventSource", FindingEvents);
  vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
  open = [original];
  resolved = [];
  mocks.api.diagnostics.mockResolvedValue(decodeDiagnostics(report));
  mocks.api.fleet.mockResolvedValue({ runners: [] });
  mocks.api.healthFindings.mockImplementation(async (_projects, options) => ({ items: options?.state === "resolved" ? resolved : open, last_tick_at: options ? tick : health.last_tick_at }));
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); });

it("updates findings in place, retains resolutions until reload, and restores reopened findings", async () => {
  render(<DiagnosticsRoute />);
  await screen.findByText(original.summary);
  expect(openCount()).toBe("1");
  const row = screen.getByText(original.summary).closest("article")!;
  const headline = screen.getByRole("region", { name: "Diagnostics headline" });
  const viewport = headline.closest("[data-slot='scroll-area-viewport']")!;
  viewport.scrollTop = 120;
  open = [next];
  resolved = [{ ...original, resolved_at: tick }, { ...next, id: "historical", summary: "Historical resolved", resolved_at: tick }];
  act(() => emit());
  await screen.findByText(next.summary);
  await within(row).findByText("Resolved");
  expect(row.querySelector("time")!.dateTime).toBe(tick);
  expect(screen.getByText(original.summary).closest("article")).toBe(row);
  expect(screen.getByRole("region", { name: "Diagnostics headline" })).toBe(headline);
  expect(viewport.scrollTop).toBe(120);
  expect(screen.queryByText("Historical resolved")).toBeNull();
  expect(openCount()).toBe("1");
  expect(mocks.api.diagnostics).toHaveBeenCalledTimes(1);
  expect(mocks.api.fleet).toHaveBeenCalledTimes(1);
  open = [];
  resolved = [{ ...original, resolved_at: tick }, { ...next, resolved_at: tick }];
  act(() => emit("2026-10-07T10:00:30Z"));
  await waitFor(() => expect(openCount()).toBe("0"));
  expect(screen.getAllByText("Resolved")).toHaveLength(2);
  open = [original, next];
  resolved = [];
  act(() => emit("2026-10-07T10:01:00Z"));
  await waitFor(() => expect(within(row).queryByText("Resolved")).toBeNull());
  expect(openCount()).toBe("2");
  open = [next];
  resolved = [{ ...original, resolved_at: tick }];
  act(() => emit("2026-10-07T10:02:00Z"));
  await within(row).findByText("Resolved");
  fireEvent.click(screen.getByRole("button", { name: "Refresh diagnostics" }));
  await waitFor(() => expect(screen.queryByText(original.summary)).toBeNull());
  expect(screen.getByText(next.summary)).toBeTruthy();
});

it("catches up on visibility and ignores failed or superseded live reads after unmount", async () => {
  const view = render(<DiagnosticsRoute />);
  await screen.findByText(original.summary);
  const visibility = vi.spyOn(document, "visibilityState", "get").mockReturnValue("hidden");
  mocks.api.healthFindings.mockClear();
  act(() => emit());
  expect(mocks.api.healthFindings).not.toHaveBeenCalled();
  visibility.mockReturnValue("visible");
  open = [original, next];
  act(() => document.dispatchEvent(new Event("visibilitychange")));
  await screen.findByText(next.summary);
  let release!: (value: unknown) => void;
  mocks.api.healthFindings.mockImplementationOnce(() => new Promise((resolve) => { release = resolve; }));
  act(() => emit("older"));
  open = [next];
  resolved = [{ ...original, resolved_at: tick }];
  act(() => emit("newer"));
  await screen.findByText("Resolved");
  await act(async () => release({ items: [original], last_tick_at: tick }));
  expect(openCount()).toBe("1");
  expect(screen.getByText("Resolved")).toBeTruthy();
  mocks.api.healthFindings.mockRejectedValueOnce(new Error("offline"));
  act(() => emit("failed"));
  await act(async () => {});
  expect(screen.getByText("Resolved")).toBeTruthy();
  const source = FindingEvents.current;
  view.unmount();
  expect(source.close).toHaveBeenCalledOnce();
  mocks.api.healthFindings.mockClear();
  act(() => { emit("unmounted"); document.dispatchEvent(new Event("visibilitychange")); });
  expect(mocks.api.healthFindings).not.toHaveBeenCalled();
});
