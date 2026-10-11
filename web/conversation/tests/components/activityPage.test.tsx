import React from "react";
import {
  act,
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
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
  timing,
  type ActivityAttempt,
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
  bootstrap: { projects: [] as AccountProject[] },
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
  fields: Partial<ActivityAttempt> = {},
): ActivityAttempt {
  return {
    work_item_id: `wi_${number}`,
    number,
    title: `Job ${number}`,
    project_id: "detent",
    project_name: "detent",
    runner_id: "runner_a",
    attempt_id: `attempt_${number}`,
    stage: "code",
    stage_started_at: at(10),
    stage_elapsed_seconds: 600,
    started_at: at(20),
    workspace_ids: [],
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
    running: [
      attempt(1, {
        stage: "review",
        stage_started_at: at(31),
        change_id: "change_1",
        pull_request_url: "https://example.test/pull/4611",
      }),
      attempt(2, {
        runner_id: "runner_b",
        stage: "plan",
        stage_started_at: at(3),
        started_at: at(5),
        project_id: "other",
        project_name: "other",
      }),
      attempt(3, {
        stage: "rework",
        stage_started_at: at(14),
        pull_request_url: "https://example.test/pull/4612",
      }),
    ],
    finished: [
      attempt(4, {
        outcome: "succeeded",
        finished_at: at(4),
        stage_duration_seconds: 180,
      }),
      attempt(5, {
        outcome: "failed",
        finished_at: at(12),
        stage_duration_seconds: 2100,
      }),
      attempt(6, {
        outcome: "cancelled",
        finished_at: at(18),
        stage_duration_seconds: 300,
      }),
    ],
    typical_durations: ["detent", "other"].flatMap((project_id) =>
      ["plan", "code", "review", "merge"].map((stage) => ({
        project_id,
        stage,
        count: 20,
        seconds: 600,
        p50_seconds: 600,
        p90_seconds: 1200,
        partial: false,
      })),
    ),
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
      project={project}
      onProjectChange={setProject}
      live
    />
  );
}
const jobTable = () => screen.getByRole("table", { name: "Running jobs" });
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
  vi.clearAllMocks();
});
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe("Activity stage timing", () => {
  it.each([
    ["at p90", "code", 1200, false, "succeeded", false, false, 1, "working"],
    ["above p90", "code", 1201, false, "succeeded", true, false, 1, "warning"],
    ["half median", "code", 300, true, "succeeded", false, true, 1, "success"],
    [
      "below half median",
      "code",
      299,
      true,
      "succeeded",
      false,
      true,
      1,
      "success",
    ],
    [
      "above half median",
      "code",
      301,
      true,
      "succeeded",
      false,
      false,
      1,
      "success",
    ],
    ["rework", "rework", 600, false, "succeeded", false, false, 1, "working"],
    [
      "failure wins over slow",
      "review",
      1201,
      true,
      "failed",
      true,
      false,
      2,
      "error",
    ],
    ["cancelled", "plan", 300, true, "cancelled", false, true, 0, "muted"],
    ["partial slow baseline", "code", 1201, false, "succeeded", false, false, 1, "working", true],
    ["partial fast baseline", "code", 300, true, "succeeded", false, false, 1, "success", true],
    ["partial cancelled baseline", "plan", 300, true, "cancelled", false, false, 0, "muted", true],
  ] as const)(
    "handles %s",
    (_name, stage, seconds, finished, outcome, slow, fast, index, tone, partial?: boolean) => {
      const row = attempt(1, {
        stage,
        stage_started_at: new Date(NOW - seconds * 1000).toISOString(),
        stage_duration_seconds: seconds,
        ...(finished ? { outcome } : {}),
      });
      const fixture = report();
      const result = timing(row, report({
        typical_durations: fixture.typical_durations.map((baseline) => ({ ...baseline, partial: partial === true })),
      }), NOW, finished);
      expect({
        slow: result.slow,
        fast: result.fast,
        index: result.index,
        tone: result.tone,
      }).toEqual({ slow, fast, index, tone });
      expect(result.label).toContain(`Stage ${index + 1} of 4`);
      expect(result.description).toContain(partial
        ? "Typical time in detent is unavailable because the baseline is incomplete."
        : "Typical in detent: 10m median, 20m for the slowest 10%.");
      if (partial) {
        expect(result.label).not.toMatch(/, (slow|fast)/);
        expect(result.progress).toBe(finished ? outcome === "cancelled" ? 0.5 : 1 : 0);
      } else if (outcome === "cancelled") expect(result.progress).toBe(0.25);
    },
  );
  it("uses elapsed snapshots when a stage start is missing and never fabricates missing timing", () => {
    const snapshot = attempt(1, {
      stage_started_at: undefined,
      stage_elapsed_seconds: 600,
    });
    expect(timing(snapshot, report(), NOW + 60_000, false).duration).toBe(
      "11m",
    );
    const missing = timing(
      attempt(1, {
        stage: undefined,
        stage_started_at: undefined,
        stage_elapsed_seconds: undefined,
      }),
      report({ typical_durations: [] }),
      NOW,
      false,
    );
    expect(missing.label).toContain("Stage unavailable, Time unavailable");
    expect(missing.slow).toBe(false);
    expect(missing.progress).toBe(0);
  });
});

it("discloses incomplete activity and suppresses speed claims from partial baselines", () => {
  const fixture = report();
  render(<View initial={report({
    partial: true,
    typical_durations: fixture.typical_durations.map((baseline) => ({ ...baseline, partial: true })),
  })} />);
  expect(screen.getByText("Activity is incomplete; some attempts or timing data may be missing.")).toBeTruthy();
  expect(screen.queryByText(/slower than usual/)).toBeNull();
  expect(screen.queryByText(/ · (slow|fast)$/)).toBeNull();
  const review = screen.getByRole("img", { name: /Stage 3 of 4, Review, 31m/ });
  expect(review.getAttribute("aria-label")).toContain("baseline is incomplete");
  expect(review.getAttribute("aria-label")).not.toContain(", slow");
  const chip = screen.getByRole("button", { name: "Filter by Alpha" });
  expect(chip.querySelector(".text-warning-foreground")).toBeNull();
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
    ["Longest in stage", ["#1", "#3", "#2"]],
    ["Most recently started", ["#2", "#1", "#3"]],
    ["Issue number", ["#1", "#2", "#3"]],
    ["Needs attention first", ["#1", "#3", "#2"]],
  ] as const) {
    await choose("Sort", option);
    expect(issueNumbers()).toEqual(expected);
  }
  for (const [option, labels] of [
    ["Runner", ["Alpha · 2 jobs", "Beta · 1 job"]],
    ["Project", ["detent · 2 jobs", "other · 1 job"]],
    ["Stage", ["Plan · 1 job", "Code · 1 job", "Review · 1 job"]],
  ] as const) {
    await choose("Group", option);
    const headers = within(jobTable())
      .getAllByRole("rowgroup")
      .map((row) => within(row).getAllByRole("row")[0]!.textContent);
    expect(headers).toEqual(labels);
  }
  expect(
    within(screen.getByRole("table", { name: "Finished attempts" }))
      .getAllByRole("link", { name: /^#\d/ })
      .map((link) => link.textContent),
  ).toEqual(["#4 Job 4", "#5 Job 5", "#6 Job 6"]);
  await choose("Group", "None");
  expect(within(jobTable()).queryByText("Alpha · 2 jobs")).toBeNull();
  const grouped = activityGroups(
    [
      attempt(7, { stage: "merge" }),
      attempt(8, { stage: "plan" }),
      attempt(9, { stage: undefined }),
    ],
    report(),
    runners,
    "",
    "",
    "stage",
    "number",
    NOW,
    false,
  );
  expect(grouped.map((entry) => entry.label)).toEqual([
    "Plan",
    "Merge",
    "Stage unavailable",
  ]);
});

it("renders outcomes, accessible stage timing, slow chips and full tooltip text", async () => {
  render(<View />);
  const slow = screen.getByRole("img", {
    name: /Stage 3 of 4, Review, 31m, slow/,
  });
  expect(
    screen.getByRole("button", { name: "Filter by Alpha" }).textContent,
  ).toContain("3");
  const finishes = screen.getByRole("table", { name: "Finished attempts" });
  expect(within(finishes).getByText("succeeded")).toBeTruthy();
  expect(
    within(finishes)
      .getByText("failed")
      .classList.contains("text-error-foreground"),
  ).toBe(true);
  expect(within(finishes).getByText("cancelled")).toBeTruthy();
  expect(within(finishes).getByText("3m · fast")).toBeTruthy();
  expect(within(finishes).getByText("35m · slow")).toBeTruthy();
  expect(
    screen.getByRole("img", { name: /Stage 2 of 4, Rework, 14m/ }),
  ).toBeTruthy();
  act(() => {
    document.dispatchEvent(new KeyboardEvent("keydown", { key: "Tab" }));
    slow.focus();
  });
  const popup = await screen.findByText(
    "Review for 31m. Typical in detent: 10m median, 20m for the slowest 10%.",
  );
  expect(popup.textContent).toBe(
    "Review for 31m. Typical in detent: 10m median, 20m for the slowest 10%.",
  );
  const relative = screen.getByRole("button", { name: /finished 4m ago/ });
  act(() => relative.focus());
  expect(await screen.findByText(/9th October 2026/)).toBeTruthy();
});

it("advances relative times and stage durations without a read, toggles every timestamp, and cleans up its clock", () => {
  vi.restoreAllMocks();
  vi.useFakeTimers();
  vi.setSystemTime(NOW);
  const view = render(<View />);
  expect(screen.getByRole("button", { name: /finished 4m ago/ })).toBeTruthy();
  act(() => vi.advanceTimersByTime(60_000));
  expect(screen.getByRole("button", { name: /finished 5m ago/ })).toBeTruthy();
  expect(
    screen.getByRole("img", { name: /Stage 3 of 4, Review, 32m/ }),
  ).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: /finished 5m ago/ }));
  expect(
    screen.queryByRole("button", { name: /ago; show all times/ }),
  ).toBeNull();
  expect(
    screen.getAllByRole("button", {
      name: /at .*; show all times as relative times/,
    }),
  ).toHaveLength(6);
  const time = screen.getAllByRole("button", {
    name: /finished at .*; show all times as relative times/,
  })[0]!;
  fireEvent.click(time);
  expect(screen.getByRole("button", { name: /finished 5m ago/ })).toBeTruthy();
  expect(mocks.api.activity).not.toHaveBeenCalled();
  view.unmount();
  expect(vi.getTimerCount()).toBe(0);
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
        initial={report({ running: [] })}
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
  vi.stubGlobal("EventSource", ProjectEvents);
  vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
  ProjectEvents.sources = [];
  const retained = subscribeProjectEvents(mocks.http, "detent", {
    activity: () => {},
  });
  let current = report({ running: [], finished: [] });
  mocks.api.activity.mockImplementation(async () => current);
  mocks.api.fleet.mockResolvedValue({ runners });
  const view = render(<ActivityRoute />);
  await screen.findByText("Nothing is running for this filter.");
  expect(ProjectEvents.sources).toHaveLength(2);
  act(() =>
    ProjectEvents.sources.forEach((source) =>
      source.dispatchEvent(new Event("open")),
    ),
  );
  expect(screen.getByText("Live")).toBeTruthy();
  current = report({ running: [attempt(10)], finished: [] });
  act(() =>
    ProjectEvents.sources[0]!.dispatchEvent(
      new MessageEvent("activity", { data: "1" }),
    ),
  );
  await screen.findByRole("link", { name: "#10 Job 10" });
  expect(screen.getByText("1 job")).toBeTruthy();
  current = report({
    running: [],
    finished: [
      attempt(10, {
        outcome: "succeeded",
        finished_at: at(0),
        stage_duration_seconds: 600,
      }),
    ],
  });
  act(() =>
    ProjectEvents.sources[0]!.dispatchEvent(
      new MessageEvent("activity", { data: "2" }),
    ),
  );
  await screen.findByText("Nothing is running for this filter.");
  expect(
    within(screen.getByRole("region", { name: "Recent finishes" })).getByRole(
      "link",
      { name: "#10 Job 10" },
    ),
  ).toBeTruthy();
  act(() => ProjectEvents.sources[1]!.dispatchEvent(new Event("error")));
  expect(screen.getByText("Reconnecting")).toBeTruthy();
  act(() => ProjectEvents.sources[1]!.dispatchEvent(new Event("open")));
  expect(screen.getByText("Live")).toBeTruthy();
  await choose("Project", "detent");
  await waitFor(() =>
    expect(mocks.api.activity).toHaveBeenLastCalledWith({
      project_id: "detent",
    }),
  );
  expect(ProjectEvents.sources).toHaveLength(2);
  expect(ProjectEvents.sources[1]!.close).toHaveBeenCalledOnce();
  view.unmount();
  expect(ProjectEvents.sources[0]!.close).not.toHaveBeenCalled();
  retained();
  expect(ProjectEvents.sources[0]!.close).toHaveBeenCalledOnce();
});
