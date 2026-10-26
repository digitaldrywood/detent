import React from "react";
import {
  act,
  cleanup,
  fireEvent,
  render,
  screen,
  within,
} from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  ActivityRoute,
  ActivityView,
} from "../../src/app/activity/ActivityPage.tsx";
import {
  activityGroups,
  issueTiming,
  laneTiming,
  issueMeta,
  type ActivityIssue,
} from "../../src/app/activity/activityModel.ts";
import { subscribeProjectEvents } from "../../src/app/work/lib/projectEvents.ts";
import {
  decodeFleet,
  type AccountProject,
} from "../../src/contracts/account.ts";
import type { ActivityReport } from "../../src/contracts/activity.ts";
import fleetFixture from "../../src/contracts/fixtures/account-fleet.json";

const mocks = vi.hoisted(() => ({
  api: { activity: vi.fn(), fleet: vi.fn() },
  bootstrap: { projects: [] as AccountProject[], organization: { name: "" } },
  http: { eventsUrl: (project: string) => `/projects/${project}/events` },
}));
vi.mock("../../src/app/account/context.ts", () => ({
  useAccountApi: () => mocks.api,
  useAccountBootstrap: () => mocks.bootstrap,
}));
vi.mock("../../src/app/work/lib/useWork.ts", () => ({
  useWorkHttp: () => mocks.http,
}));
vi.mock("../../src/app/pageTitle.ts", () => ({ usePageTitle: () => {} }));
vi.mock("@tanstack/react-router", () => ({
  Link: ({
    to,
    params = {},
    search,
    children,
    ...props
  }: {
    to: string;
    params?: Record<string, string>;
    search?: Record<string, string>;
    children: React.ReactNode;
  }) => {
    const path = to.replace(/\$(\w+)/g, (_match, key: string) =>
      encodeURIComponent(params[key] ?? ""),
    );
    return (
      <a
        href={`${path}${search ? `?${new URLSearchParams(search)}` : ""}`}
        {...props}
      >
        {children}
      </a>
    );
  },
}));

const NOW = Date.parse("2026-10-09T14:00:00Z");
const at = (minutes: number) => new Date(NOW - minutes * 60_000).toISOString();
const projects: AccountProject[] = ["detent", "other"].map((name) => ({
  id: name,
  name,
  profile: "native",
  can_manage_runners: true,
  can_write: true,
  states: [],
}));
const runners = [
  {
    ...decodeFleet(fleetFixture).runners[0]!,
    id: "runner_a",
    display_name: "Alpha",
    health: "healthy",
    state: "active",
    capacity_limit: 3,
    reported_capacity: 3,
  },
  {
    ...decodeFleet(fleetFixture).runners[0]!,
    id: "runner_b",
    display_name: "Beta",
    health: "healthy",
    state: "active",
    leases: [],
  },
  {
    ...decodeFleet(fleetFixture).runners[0]!,
    id: "runner_c",
    display_name: "Gamma",
    health: "offline",
    state: "active",
    leases: [],
  },
];
function attempt(
  number: number,
  fields: Partial<ActivityIssue> = {},
): ActivityIssue {
  return {
    work_item_id: `wi_${number}`,
    number,
    title: `Job ${number}`,
    project_id: "detent",
    project_name: "detent",
    runner_id: "runner_a",
    clock_started_at: at(20), clock_lane: "Todo", current_lane: "In Progress",
    total_seconds: 1200, worked_seconds: 600,
    runner_ids: [fields.runner_id ?? "runner_a"],
    lanes: [
      { name: "Todo", role: "active", seconds: 600, entries: 1, current: false },
      { name: "In Progress", role: "active", seconds: 600, entries: 1, current: true },
      { name: "Merging", role: "active", seconds: 0, entries: 0, current: false },
    ],
    worked_by_role: [{ role: "security_audit", attempts: 2, seconds: 600, running: true }],
    session_id: `session_${number}`,
    partial: false,
    ...fields,
  };
}
function report(fields: Partial<ActivityReport> = {}): ActivityReport {
  return {
    organization_id: "org_fixture",
    observed_at: new Date(NOW).toISOString(),
    window: { from: at(1440), to: at(0), bucket_ns: 0 },
    population_limit: 1000,
    partial: false,
    running: [],
    finished: [],
    running_issues: [
      attempt(1, {
        total_seconds: 1860,
        current_lane: "Reviewing",
        change_id: "change_1",
        pull_request_url: "https://example.test/pull/4611",
      }),
      attempt(2, {
        runner_id: "runner_b",
        total_seconds: 180, clock_started_at: at(5),
        project_id: "other",
        project_name: "other",
      }),
      attempt(3, {
        total_seconds: 840, clock_started_at: at(25), current_lane: "Repair",
        pull_request_url: "https://example.test/pull/4612",
      }),
    ],
    finished_issues: [
      attempt(4, {
        terminal_lane: "Done",
        finished_at: at(4),
        total_seconds: 180,
      }),
      attempt(5, {
        terminal_lane: "Done",
        finished_at: at(12),
        total_seconds: 2100,
      }),
      attempt(6, {
        terminal_lane: "Cancelled",
        finished_at: at(18),
        total_seconds: 300,
      }),
    ],
    typical_durations: [],
    issue_typical_durations: ["detent", "other"].map((project_id) => ({
      project_id, partial: false, total: { count: 20, seconds: 600, p50_seconds: 600, p90_seconds: 1200 },
      lanes: { "In Progress": { count: 20, seconds: 600, p50_seconds: 600, p90_seconds: 1200 } },
    })),
    ...fields,
  };
}
function View({
  initial = report(),
  pending = false,
  error = null,
  hosts = runners,
}: {
  initial?: ActivityReport | null;
  pending?: boolean;
  error?: string | null;
  hosts?: typeof runners;
}) {
  const [project, setProject] = React.useState("");
  return (
    <ActivityView
      report={initial}
      pending={pending}
      error={error}
      runners={hosts}
      projects={projects}
      organizationName={mocks.bootstrap.organization.name}
      project={project}
      onProjectChange={setProject}
      live
    />
  );
}
const jobTable = () => screen.getByRole("table", { name: "Working issues" });
const issueNumbers = () =>
  within(jobTable())
    .getAllByRole("link", { name: /^#\d/ })
    .map((link) => link.textContent?.match(/^#\d+/)?.[0]);
async function choose(label: string, option: string) {
  const user = userEvent.setup();
  await user.click(screen.getByRole("combobox", { name: label }));
  await user.click(await screen.findByRole("option", { name: option }));
}
beforeEach(() => {
  vi.spyOn(Date, "now").mockReturnValue(NOW);
  mocks.bootstrap.projects = projects;
  mocks.bootstrap.organization.name = "";
  mocks.api = { activity: vi.fn(), fleet: vi.fn() };
  vi.clearAllMocks();
});
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

it("keeps the Activity scope and project picker consistent across organization changes", async () => {
  mocks.bootstrap.organization.name = "Secondary Example Organization";
  const mounted = render(<View />);
  const scope = () => screen.getByRole("navigation", { name: "Activity breadcrumb" });
  expect(scope().textContent).toContain("All Secondary Example Organization projects");
  mocks.bootstrap.organization.name = "Tertiary Example Organization";
  mounted.rerender(<View />);
  expect(scope().textContent).toContain("All Tertiary Example Organization projects");
  await choose("Project", "detent");
  expect(scope().textContent).toContain("detent");
  expect(scope().textContent).not.toContain("All Tertiary Example Organization projects");
  await choose("Project", "All Tertiary Example Organization projects");
  expect(scope().textContent).toContain("All Tertiary Example Organization projects");
  mocks.bootstrap.organization.name = "";
  mounted.rerender(<View />);
  expect(scope().textContent).toContain("All projects");
});

describe("Activity issue timing", () => {
  it.each([
    ["at p90", 1200, false, false],
    ["above p90", 1201, false, true],
    ["partial baseline", 1201, true, false],
  ])("handles %s for total and lane time", (_name, seconds, partial, slow) => {
    const row = attempt(1, { total_seconds: seconds as number });
    const fixture = report();
    const data = report({ issue_typical_durations: fixture.issue_typical_durations!.map((value) => ({ ...value, partial: partial as boolean })) });
    expect(issueTiming(row, data, NOW, true).slow).toBe(slow);
    expect(laneTiming({ ...row.lanes[1]!, seconds: seconds as number }, row, data, NOW, true).slow).toBe(slow);
  });
  it.each([[0, 0, 0, "warning"], [100, 49, 0.49, "warning"], [100, 50, 0.5, "working"], [100, 120, 1, "working"]] as const)("bounds worked share for %s total and %s worked", (total, worked, share, tone) => {
    const metrics = issueTiming(attempt(1, { total_seconds: total, worked_seconds: worked }), report(), NOW, true);
    expect(metrics.share).toBe(share);
    expect(metrics.workedTone).toBe(tone);
  });
  it("advances total and the union of live work once between observations", () => {
    const row=attempt(1,{total_seconds:1200,worked_seconds:600,worked_running:true});
    const live=issueTiming(row,report(),NOW+60_000,false);
    expect(live.total).toBe(1260); expect(live.worked).toBe(660);
    expect(issueTiming(row,report(),NOW+60_000,true).worked).toBe(600);
  });
  it("never marks issues or lanes slow without a baseline and names configured lanes in meta text", () => {
    const row = attempt(1, { total_seconds: 9000, clock_lane: "Ready" });
    const data = report({ issue_typical_durations: [] });
    expect(issueTiming(row, data, NOW, false).slow).toBe(false);
    expect(laneTiming(row.lanes[0]!, row, data, NOW, false).slow).toBe(false);
    expect(issueMeta(row, false)).toEqual({ prefix: "in Ready since", at: row.clock_started_at });
    expect(issueMeta({ ...row, terminal_lane: "Cancelled", finished_at: at(4) }, true)).toEqual({ prefix: "Cancelled", at: at(4) });
  });
});

it("discloses partial activity and suppresses slow claims from an incomplete baseline", () => {
  const fixture = report();
  render(<View initial={report({ partial: true, issue_typical_durations: fixture.issue_typical_durations!.map((row) => ({ ...row, partial: true })) })} />);
  expect(screen.getByText("Activity is incomplete; some attempts or timing data may be missing.")).toBeTruthy();
  expect(screen.queryByText(/slower than usual/)).toBeNull();
  expect(screen.queryByText(/Stage unavailable/)).toBeNull();
});

it.each([
  ["deduplicated leases", 0, 3, "healthy", 2, 1],
  ["host usage beyond listed leases", 4, 4, "healthy", 2, 1],
  ["live leases during an offline transition", 0, 3, "offline", 0, 3],
])("counts shared host capacity once with %s and keeps sleeping runners idle", (_name, hostUsed, used, health, working, offline) => {
  const lease = runners[0]!.leases[0]!;
  const shared = { ...runners[0]!, machine_id: "shared", health, host_capacity: 8, host_used: hostUsed, capacity_limit: 8, reported_capacity: 8 };
  render(<View hosts={[
    { ...shared, leases: [lease, { ...lease, lease_id: "second" }] },
    { ...shared, id: "sibling", display_name: "Sibling", leases: [lease, { ...lease, lease_id: "third" }] },
    { ...runners[1]!, machine_id: "sleeping", health: "asleep", claim_refusal_reason: "", host_capacity: 2, host_used: 0, capacity_limit: 2, reported_capacity: 2 },
    { ...runners[2]!, machine_id: "offline", host_capacity: 2, host_used: 0 },
  ]} />);
  expect(screen.getByRole("heading", { name: `Runners · ${working} working · 1 idle · ${offline} offline · ${used} of 12 slots in use` })).toBeTruthy();
  const sleeping = within(screen.getByRole("button", { name: "Filter by Beta" }));
  expect(sleeping.getByRole("img", { name: "Asleep" })).toBeTruthy();
  expect(sleeping.queryByText("offline")).toBeNull();
  expect(sleeping.getByText("0/2")).toBeTruthy();
});

it("filters projects and runners, toggles runner chips, and retains stable issue, Change, PR and Session destinations", async () => {
  render(<View />);
  expect(issueNumbers()).toEqual(["#1", "#3", "#2"]);
  const first = within(jobTable())
    .getByRole("link", { name: "#1 Job 1" })
    .closest('[role="row"]')! as HTMLElement;
  expect(
    within(first).getByRole("link", { name: "PR #4611" }).getAttribute("href"),
  ).toBe("/work/i/wi_1/changes/change_1");
  expect(
    within(first).getByRole("link", { name: "Session" }).getAttribute("href"),
  ).toBe("/work/i/wi_1?panel=conversation");
  expect(
    within(jobTable())
      .getByRole("link", { name: "PR #4612" })
      .getAttribute("href"),
  ).toBe("https://example.test/pull/4612");
  await choose("Project", "other");
  expect(issueNumbers()).toEqual(["#2"]);
  await choose("Runner", "Alpha");
  expect(screen.getByText("Nothing is running for this filter.")).toBeTruthy();
  const chip = screen.getByRole("button", { name: "Filter by Alpha" });
  expect(chip.getAttribute("aria-pressed")).toBe("true");
  fireEvent.click(chip);
  expect(chip.getAttribute("aria-pressed")).toBe("false");
  expect(issueNumbers()).toEqual(["#2"]);
  fireEvent.click(screen.getByRole("button", { name: "Filter by Beta" }));
  expect(
    screen.getByRole("navigation", { name: "Activity breadcrumb" }).textContent,
  ).toContain("Beta");
  await choose("Project", "All projects");
  expect(issueNumbers()).toEqual(["#2"]);
});

it("applies every grouping and sorting option", async () => {
  render(<View />);
  for (const [option, expected] of [
    ["Longest total", ["#1", "#3", "#2"]],
    ["Most recently started", ["#2", "#1", "#3"]],
    ["Issue number", ["#1", "#2", "#3"]],
    ["Needs attention first", ["#1", "#3", "#2"]],
  ] as const) {
    await choose("Sort", option);
    expect(issueNumbers()).toEqual(expected);
  }
  for (const [option, labels] of [
    ["Runner", ["Alpha · 2 issues", "Beta · 1 issue"]],
    ["Project", ["detent · 2 issues", "other · 1 issue"]],
    ["Lane", ["Reviewing · 1 issue", "Repair · 1 issue", "In Progress · 1 issue"]],
  ] as const) {
    await choose("Group", option);
    const headers = within(jobTable())
      .getAllByRole("rowgroup")
      .map((row) => within(row).getAllByRole("row")[0]!.textContent);
    expect(headers).toEqual(labels);
  }
  expect(
    within(screen.getByRole("table", { name: "Finished issues" }))
      .getAllByRole("link", { name: /^#\d/ })
      .map((link) => link.textContent),
  ).toEqual(["#4 Job 4", "#5 Job 5", "#6 Job 6"]);
  await choose("Group", "None");
  expect(within(jobTable()).queryByText("Alpha · 2 issues")).toBeNull();
  const grouped = activityGroups([attempt(7, { current_lane: "Build" }), attempt(8, { current_lane: "Verify" })], report(), runners, "", "", "lane", "number", NOW, false);
  expect(grouped.map((entry) => entry.label)).toEqual(["Build", "Verify"]);
});

it("renders terminal outcomes, configured lane timing and role details", async () => {
  render(<View />);
  const finishes = screen.getByRole("table", { name: "Finished issues" });
  expect(within(finishes).getAllByRole("button", { name: /Done/ }).length).toBe(2);
  expect(within(finishes).getByRole("button", { name: /Cancelled/ })).toBeTruthy();
  const lane = screen.getAllByLabelText(/In Progress, 10m/)[0]!;
  act(() => { document.dispatchEvent(new KeyboardEvent("keydown", { key: "Tab" })); lane.focus(); });
  expect(await screen.findByText(/Typical in detent: 10m median, 20m for the slowest 10%/)).toBeTruthy();
  const worked = screen.getByLabelText("Worked 10m, 32%");
  act(() => worked.focus());
  expect(await screen.findByText("Worked 10m · waiting 21m")).toBeTruthy();
  expect(screen.getByText("security_audit · running")).toBeTruthy();

});

it("advances lane durations without a read, toggles timestamps and cleans up its clock", () => {
  vi.restoreAllMocks(); vi.useFakeTimers(); vi.setSystemTime(NOW);
  const view = render(<View />);
  expect(screen.getByRole("button", { name: /Done 4m ago/ })).toBeTruthy();
  act(() => vi.advanceTimersByTime(60_000));
  expect(screen.getByRole("button", { name: /Done 5m ago/ })).toBeTruthy();
  expect(screen.getAllByLabelText(/In Progress, 11m/).length).toBeGreaterThan(0);
  fireEvent.click(screen.getByRole("button", { name: /Done 5m ago/ }));
  expect(screen.queryByRole("button", { name: /ago; show all times/ })).toBeNull();
  expect(screen.getAllByRole("button", { name: /at .*; show all times as relative times/ })).toHaveLength(6);
  expect(mocks.api.activity).not.toHaveBeenCalled(); view.unmount(); expect(vi.getTimerCount()).toBe(0);
});

it.each(["loading", "error", "empty", "no-match"] as const)(
  "renders the %s state",
  (state) => {
    render(
      <View
        pending={state === "loading"}
        error={
          state === "error" ? "Activity is temporarily unavailable." : null
        }
        hosts={state === "empty" ? [] : runners}
        initial={report({ running_issues: [] })}
      />,
    );
    if (state === "loading")
      expect(screen.getByLabelText("Loading activity")).toBeTruthy();
    if (state === "error")
      expect(
        screen.getByText("Activity is temporarily unavailable."),
      ).toBeTruthy();
    if (state === "empty") {
      expect(
        screen.getByText(
          "No runners enrolled yet. Runners pick up work and show here while they run.",
        ),
      ).toBeTruthy();
      expect(
        screen
          .getByRole("link", { name: "Enroll a runner" })
          .getAttribute("href"),
      ).toBe("/settings/runners");
    }
    if (state === "no-match")
      expect(
        screen.getByText("Nothing is running for this filter."),
      ).toBeTruthy();
  },
);

class ProjectEvents extends EventTarget {
  static sources: ProjectEvents[] = [];
  close = vi.fn();
  constructor(readonly url: string) {
    super();
    ProjectEvents.sources.push(this);
  }
}
it("refreshes pickup and finish rows through reused project streams, tracks all connections and releases subscriptions", async () => {
  vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout", "performance"] });
  vi.stubGlobal("EventSource", ProjectEvents);
  vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
  ProjectEvents.sources = [];
  const retained = subscribeProjectEvents(mocks.http, "detent", {
    activity: () => {},
  });
  let current = report({ running_issues: [], finished: [] });
  mocks.api.activity.mockImplementation(async () => current);
  mocks.api.fleet.mockResolvedValue({ runners });
  const view = render(<ActivityRoute />);
  await act(async () => {});
  expect(screen.getByText("Nothing is running for this filter.")).toBeTruthy();
  expect(ProjectEvents.sources).toHaveLength(2);
  act(() =>
    ProjectEvents.sources.forEach((source) =>
      source.dispatchEvent(new Event("open")),
    ),
  );
  expect(screen.getByText("Live")).toBeTruthy();
  current = report({ running_issues: [attempt(10)], finished: [] });
  act(() => {
    for (let i = 0; i < 100; i++) {
      ProjectEvents.sources.forEach((source) => source.dispatchEvent(new MessageEvent("activity", { data: String(i) })));
    }
  });
  expect(mocks.api.activity).toHaveBeenCalledOnce();
  await act(async () => { await vi.advanceTimersByTimeAsync(2000); });
  expect(screen.getByRole("link", { name: "#10 Job 10" })).toBeTruthy();
  expect(screen.getByText("1 issue")).toBeTruthy();
  let finishRequest: ((value: ActivityReport) => void) | undefined;
  mocks.api.activity.mockImplementationOnce(() => new Promise<ActivityReport>((resolve) => { finishRequest = resolve; }));
  current = report({
    running_issues: [],
    finished_issues: [
      attempt(10, {
        terminal_lane: "Done",
        finished_at: at(0),
        total_seconds: 600,
      }),
    ],
  });
  act(() =>
    ProjectEvents.sources[0]!.dispatchEvent(
      new MessageEvent("activity", { data: "2" }),
    ),
  );
  await act(async () => { await vi.advanceTimersByTimeAsync(2000); });
  expect(mocks.api.activity).toHaveBeenCalledTimes(3);
  act(() => ProjectEvents.sources[0]!.dispatchEvent(new MessageEvent("activity", { data: "3" })));
  await act(async () => { await vi.advanceTimersByTimeAsync(6000); });
  expect(mocks.api.activity).toHaveBeenCalledTimes(3);
  await act(async () => { finishRequest!(current); });
  expect(screen.getByText("Nothing is running for this filter.")).toBeTruthy();
  expect(mocks.api.activity).toHaveBeenCalledTimes(4);
  expect(
    within(screen.getByRole("region", { name: "Finished" })).getByRole(
      "link",
      { name: "#10 Job 10" },
    ),
  ).toBeTruthy();
  act(() => ProjectEvents.sources[1]!.dispatchEvent(new Event("error")));
  expect(screen.getByText("Reconnecting")).toBeTruthy();
  act(() => ProjectEvents.sources[1]!.dispatchEvent(new Event("open")));
  expect(screen.getByText("Live")).toBeTruthy();
  fireEvent.click(screen.getByRole("combobox", { name: "Project" }));
  const option = screen.getByRole("option", { name: "detent" });
  fireEvent.pointerDown(option, { pointerType: "mouse" });
  fireEvent.click(option);
  await act(async () => { await vi.advanceTimersByTimeAsync(2000); });
  expect(mocks.api.activity).toHaveBeenLastCalledWith({ project_id: "detent" }, expect.any(AbortSignal));
  expect(ProjectEvents.sources).toHaveLength(2);
  expect(ProjectEvents.sources[1]!.close).toHaveBeenCalledOnce();
  view.unmount();
  expect(ProjectEvents.sources[0]!.close).not.toHaveBeenCalled();
  retained();
  expect(ProjectEvents.sources[0]!.close).toHaveBeenCalledOnce();
  const calls = mocks.api.activity.mock.calls.length;
  const revisit = render(<ActivityRoute />);
  expect(screen.queryByLabelText("Loading activity")).toBeNull();
  expect(screen.getByText("Nothing is running for this filter.")).toBeTruthy();
  expect(mocks.api.activity).toHaveBeenCalledTimes(calls);
  await act(async () => { await vi.advanceTimersByTimeAsync(2000); });
  expect(mocks.api.activity).toHaveBeenCalledTimes(calls + 1);
  revisit.unmount();
  expect(vi.getTimerCount()).toBe(0);
});


it("waits for a slow Activity read across remount and a failed fleet read, then revalidates when visible", async () => {
  vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout", "performance"] });
  vi.stubGlobal("EventSource", ProjectEvents);
  let visibility: DocumentVisibilityState = "visible";
  vi.spyOn(document, "visibilityState", "get").mockImplementation(() => visibility);
  ProjectEvents.sources = [];
  let resolveActivity: ((value: ActivityReport) => void) | undefined;
  mocks.api.activity.mockImplementationOnce(() => new Promise<ActivityReport>((resolve) => { resolveActivity = resolve; }));
  mocks.api.activity.mockResolvedValue(report());
  mocks.api.fleet.mockRejectedValueOnce(new Error("Fleet unavailable"));
  mocks.api.fleet.mockResolvedValue({ runners });
  const first = render(<ActivityRoute />);
  await act(async () => {});
  first.unmount();
  const next = render(<ActivityRoute />);
  act(() => ProjectEvents.sources.forEach((source) => source.dispatchEvent(new Event("open"))));
  await act(async () => { await vi.advanceTimersByTimeAsync(6000); });
  expect(mocks.api.activity).toHaveBeenCalledOnce();
  await act(async () => { resolveActivity!(report()); });
  expect(mocks.api.activity).toHaveBeenCalledTimes(2);
  expect(screen.getByRole("link", { name: "#1 Job 1" })).toBeTruthy();
  visibility = "hidden";
  act(() => ProjectEvents.sources.forEach((source) => source.dispatchEvent(new MessageEvent("activity", { data: "1" }))));
  await act(async () => { await vi.advanceTimersByTimeAsync(6000); });
  expect(mocks.api.activity).toHaveBeenCalledTimes(2);
  visibility = "visible";
  await act(async () => { document.dispatchEvent(new Event("visibilitychange")); });
  expect(mocks.api.activity).toHaveBeenCalledTimes(3);
  next.unmount();
  expect(vi.getTimerCount()).toBe(0);
});

it.each(["activity", "fleet"] as const)("recovers a stalled %s read across remount without publishing its late response", async (stalled) => {
  vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout", "performance"] });
  vi.stubGlobal("EventSource", ProjectEvents);
  vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
  ProjectEvents.sources = [];
  mocks.api.activity.mockResolvedValue(report({ running_issues: [attempt(20)] }));
  mocks.api.fleet.mockResolvedValue({ runners });
  let finishStalled: (() => void) | undefined;
  mocks.api[stalled].mockImplementationOnce(() => new Promise((resolve) => {
    finishStalled = () => resolve(stalled === "activity" ? report({ running_issues: [attempt(10)] }) : { runners });
  }));
  const first = render(<ActivityRoute />);
  await act(async () => {});
  first.unmount();
  const next = render(<ActivityRoute />);
  await act(async () => { await vi.advanceTimersByTimeAsync(29_999); });
  expect(mocks.api.activity).toHaveBeenCalledOnce();
  await act(async () => { await vi.advanceTimersByTimeAsync(1); });
  expect(mocks.api.activity).toHaveBeenCalledTimes(2);
  expect(mocks.api.activity.mock.calls[0]![1].aborted).toBe(true);
  expect(mocks.api.fleet.mock.calls[0]![0].aborted).toBe(true);
  expect(screen.getByRole("link", { name: "#20 Job 20" })).toBeTruthy();
  await act(async () => { finishStalled!(); });
  expect(screen.queryByRole("link", { name: "#10 Job 10" })).toBeNull();
  expect(screen.getByRole("link", { name: "#20 Job 20" })).toBeTruthy();
  expect(mocks.api.activity).toHaveBeenCalledTimes(2);
  next.unmount();
  expect(vi.getTimerCount()).toBe(0);
});
