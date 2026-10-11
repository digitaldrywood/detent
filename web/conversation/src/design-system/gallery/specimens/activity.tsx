import { useState } from "react";
import { ActivityView } from "../../../app/activity/ActivityPage.tsx";
import type { ActivityAttempt } from "../../../app/activity/activityModel.ts";
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
  const projects: AccountProject[] = ["detent", "parable", "detent.build"].map(
    (name) => ({
      id: name,
      name,
      profile: "native",
      can_write: true,
      can_manage_runners: true,
      states: [],
    }),
  );
  const base = decodeFleet(fleet).runners[0]!;
  const names = [
    "mac-studio",
    "jarvis-1",
    "jarvis-2",
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
    leases: Array.from(
      { length: usage[index]! },
      (_, slot) => ({ ...base.leases[0]!, lease_id: `lease_${index}_${slot}` }),
    ),
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
  const stages = [
    "review",
    "code",
    "merge",
    "code",
    "code",
    "rework",
    "code",
    "plan",
    "review",
    "merge",
    "plan",
    "plan",
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
    stage: stages[index]!,
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
    window: { from: ago(1440), to: ago(0), bucket_ns: 0 },
    running,
    finished: running
      .slice(0, 6)
      .map((row, index) => ({
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
        outcome:
          index === 2 ? "failed" : index === 5 ? "cancelled" : "succeeded",
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
  return (
    <div className="flex h-full min-h-0 flex-col">
      <ActivityView
        report={report}
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
      title: "Twelve runners with running jobs and recent finishes",
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
