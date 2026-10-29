import { useState } from "react";
import { ActivityView } from "../../../app/activity/ActivityPage.tsx";
import type {
  ActivityAttempt,
  ActivityIssue,
} from "../../../app/activity/activityModel.ts";
import {
  decodeFleet,
  type AccountProject,
} from "../../../contracts/account.ts";
import type { ActivityReport } from "../../../contracts/activity.ts";
import fleet from "../../../contracts/fixtures/account-fleet.json";
import type { GalleryDoc } from "../specimen.tsx";

function ActivitySpecimen({
  state = "populated",
}: {
  state?: "populated" | "partial" | "empty" | "loading" | "unavailable";
}) {
  const [project, setProject] = useState("");
  const [now] = useState(Date.now);
  const ago = (minutes: number) =>
    new Date(now - minutes * 60_000).toISOString();
  const projects: AccountProject[] = [
    "example-project-a",
    "example-project-b",
    "example-project-c",
  ].map((name) => ({
    id: name,
    name,
    profile: "native",
    can_write: true,
    can_manage_runners: true,
    states: [],
  }));
  const base = decodeFleet(fleet).runners[0]!;
  const names = [
    "mac-studio",
    "example-runner-1",
    "example-runner-2",
    "sprite-a",
    "sprite-b",
    "sprite-c",
    "sprite-d",
    "ci-linux-1",
    "sprite-e",
    "sprite-f",
    "macbook-air",
    "ci-linux-2",
  ];
  const capacities = [3, 2, 2, 1, 1, 1, 1, 2, 1, 1, 2, 1];
  const usage = [3, 2, 1, 1, 1, 1, 1, 2, 0, 0, 0, 0];
  const runners = names.map((name, index) => ({
    ...base,
    id: `runner_${index}`,
    machine_id: `machine_${index}`,
    display_name: name,
    state: "active",
    health: index === 11 ? "offline" : index === 8 ? "asleep" : "healthy",
    claim_refusal_reason: "",
    leases: Array.from({ length: usage[index]! }, (_, slot) => ({
      ...base.leases[0]!,
      lease_id: `lease_${index}_${slot}`,
    })),
    capacity_limit: capacities[index]!,
    reported_capacity: capacities[index]!,
    host_capacity: capacities[index]!,
    host_used: usage[index]!,
  }));
  const titles = [
    "Prevent card overflow with the approved compact layout",
    "Add the concurrency chapter exercises",
    "Keep the selected project when switching pages",
    "Expose runner capacity history through the supported API",
    "Activity page under Browse showing running jobs, runners and recent finishes",
    "Create a missing pull request from existing work",
    "Cache lane counts between board refreshes",
    "Add a runner enrollment guide to the help center",
    "Correct the channel buffering example",
    "Broken anchor links in the pricing FAQ",
    "Keep runner heartbeats current when the transport reconnects",
    "Add the select statement walkthrough",
  ];
  const elapsed = [31, 38, 11, 19, 14, 22, 9, 3, 6, 2, 2, 1];
  const hosts = [0, 1, 7, 6, 0, 3, 2, 1, 5, 4, 0, 7];
  const running: ActivityAttempt[] = titles.map((title, index) => ({
    work_item_id: `wi_gallery_${index}`,
    attempt_id: `attempt_gallery_${index}`,
    number: 732 + index,
    title,
    project_id: projects[index % projects.length]!.id,
    project_name: projects[index % projects.length]!.name,
    runner_id: runners[hosts[index]!]!.id,
    started_at: ago(45 + index),
    stage_started_at: ago(elapsed[index]!),
    stage_elapsed_seconds: elapsed[index]! * 60,
    workspace_ids: [],
    session_id: `session_${index}`,
    ...(index % 3 === 0
      ? {}
      : {
          change_id: `change_${index}`,
          pull_request_url: `https://example.test/pull/${4611 + index}`,
        }),
    partial: false,
  }));
  const report: ActivityReport = {
    organization_id: "org_gallery",
    observed_at: ago(0),
    window: { from: ago(10080), to: ago(0), bucket_ns: 0 },
    running,
    finished: running.slice(0, 6).map((row, index) => ({
      ...row,
      attempt_id: `finished_${index}`,
      work_item_id: `wi_finished_${index}`,
      title: [
        "Let runners claim the landing barrier",
        "Fingerprint paused rework replay",
        "Refresh the pricing page copy",
        "Report runner update channel in Fleet",
        "Retain pending recovery source",
        "Tidy the goroutine leak chapter",
      ][index]!,
      stage: ["merge", "review", "code", "code", "code", "plan"][index]!,
      outcome: index === 2 ? "failed" : index === 5 ? "cancelled" : "succeeded",
      finished_at: ago(4 + index * 12),
      stage_duration_seconds: [4, 3, 9, 16, 35, 2][index]! * 60,
    })),
    typical_durations: projects.flatMap((row) =>
      ["plan", "code", "review", "merge"].map((stage) => ({
        project_id: row.id,
        stage,
        count: 40,
        seconds: 600,
        p50_seconds: stage === "code" ? 1200 : 480,
        p90_seconds: stage === "merge" ? 300 : stage === "code" ? 1800 : 1200,
        partial: state === "partial",
      })),
    ),
    population_limit: 1000,
    partial: state === "partial",
  };
  const issueRows = (finished: boolean) =>
    (finished ? report.finished : running).map((row, index) => {
      const totals = finished
        ? [14700, 6600, 180, 180, 2220, 2640]
        : [7800, 6360, 3120, 900];
      const workedTimes = finished
        ? [3000, 2640, 54, 48, 1020, 2460]
        : [300, 5880, 2760, 840];
      const durations = finished
        ? [
            [540, 2460, 0, 300],
            [60, 2520, 0, 120],
            [120, 60, 0, 0],
            [120, 60, 0, 0],
            [1080, 960, 0, 180],
            [120, 2160, 180, 180],
          ]
        : [
            [7500, 300, 0, 0],
            [360, 6000, 0, 0],
            [180, 1500, 1200, 240],
            [60, 840, 0, 0],
          ];
      const total = totals[index % totals.length]!;
      const worked = workedTimes[index % workedTimes.length]!;
      const activeLanes = ["Todo", "In Progress", "Rework", "Merging"];
      const terminal = index === 3 ? "Cancelled" : "Done";
      const values = durations[index % durations.length]!;
      const current = [1, 1, 3, 1][index % 4]!;
      const finishMinutes = 4 + index * 12;
      const lanes: ActivityIssue["lanes"][number][] = activeLanes.map(
        (name, lane) => ({
          name,
          role: "active",
          seconds: values[lane]!,
          entries: values[lane]! > 0 ? (lane === 2 && index === 5 ? 2 : 1) : 0,
          current: !finished && lane === current,
        }),
      );
      if (finished && index < 2)
        lanes.push({
          name: index === 0 ? "Blocked" : "Human Review",
          role: "holding",
          seconds: index === 0 ? 11400 : 3900,
          entries: 1,
          current: false,
        });
      return {
        ...row,
        clock_started_at: ago(total / 60 + (finished ? finishMinutes : 0)),
        clock_lane: "Todo",
        current_lane: finished ? terminal : activeLanes[current]!,
        ...(finished
          ? { terminal_lane: terminal, finished_at: ago(finishMinutes) }
          : {}),
        total_seconds: total,
        worked_seconds: worked,
        worked_running: !finished,
        runner_ids:
          finished && index === 0
            ? [row.runner_id, runners[1]!.id]
            : [row.runner_id],
        lanes,
        worked_by_role: [
          {
            role: "code",
            attempts: 2,
            seconds: Math.max(0, worked - 30),
            running: !finished,
          },
          {
            role: "security_audit",
            attempts: 1,
            seconds: Math.min(worked, 30),
            running: false,
          },
        ],
      };
    });
  const issueReport: ActivityReport = {
    ...report,
    running_issues: issueRows(false).slice(0, 4),
    finished_issues: issueRows(true),
    issue_typical_durations: projects.map((row) => ({
      project_id: row.id,
      total: { count: 40, seconds: 3000, p50_seconds: 3000, p90_seconds: 7000 },
      lanes: Object.fromEntries(
        ["Todo", "In Progress", "Rework", "Merging", "Blocked"].map((lane) => [
          lane,
          { count: 40, seconds: 1200, p50_seconds: 1200, p90_seconds: 3600 },
        ]),
      ),
      partial: state === "partial",
    })),
  };
  return (
    <div className="flex h-full min-h-0 flex-col">
      <ActivityView
        report={issueReport}
        runners={state === "empty" ? [] : runners}
        projects={projects}
        project={project}
        onProjectChange={setProject}
        pending={state === "loading"}
        error={
          state === "unavailable"
            ? "Activity is temporarily unavailable."
            : null
        }
        live
      />
    </div>
  );
}

export const activityPage: GalleryDoc = {
  meta: { name: "Activity page", kind: "surface", group: "workspace" },
  specimens: [
    {
      id: "fleet",
      title: "Runners with working and finished issues",
      height: 1100,
      render: () => <ActivitySpecimen />,
    },
    {
      id: "partial",
      title: "Incomplete activity and timing baselines",
      height: 1100,
      render: () => <ActivitySpecimen state="partial" />,
    },
    {
      id: "empty",
      title: "No runners enrolled",
      height: 360,
      render: () => <ActivitySpecimen state="empty" />,
    },
    {
      id: "loading",
      title: "Loading rows",
      height: 480,
      render: () => <ActivitySpecimen state="loading" />,
    },
    {
      id: "unavailable",
      title: "Unavailable read",
      height: 360,
      render: () => <ActivitySpecimen state="unavailable" />,
    },
  ],
};
