import React from "react";
import type { FleetRunner } from "../../contracts/account.ts";
import { PathValue } from "../account/controls.tsx";
import { formatLocalTime, formatRelativeTime } from "./format.ts";

export function RunnerProblems({ runner, now = Date.now(), alert = false, needsHuman = false }: {
  readonly runner: FleetRunner;
  readonly now?: number;
  readonly alert?: boolean;
  readonly needsHuman?: boolean;
}): React.ReactElement | null {
  if (!runner.problems?.length) return null;
  const offline = runner.connection_health === "offline" || runner.health === "offline";
  const heartbeat = new Date(runner.last_heartbeat_at).getTime();
  const offlineSince = Number.isFinite(heartbeat) ? new Date(heartbeat + 120_000).toISOString() : runner.last_heartbeat_at;
  return <div className="min-w-0 space-y-3 text-xs">
    {runner.problems.map((problem) => {
      const reportedAt = problem.reported_at && !problem.reported_at.startsWith("0001-") ? problem.reported_at : runner.last_heartbeat_at;
      return <div key={`${problem.code}-${problem.project_id}-${problem.subject}-${problem.check}`} role={alert ? "alert" : undefined} className="min-w-0 space-y-2 break-words">
        {needsHuman ? <p className="font-medium text-warning-foreground">Needs human: {problem.code.replaceAll("_", " ")}</p> : null}
        <p className="font-medium text-warning-foreground">{problem.subject ? `${problem.subject}: ` : ""}{problem.message}</p>
        {problem.check ? <p>Check: <code className="break-all">{problem.check}</code></p> : null}
        {problem.error_output ? <pre className="whitespace-pre-wrap break-all font-mono">{problem.error_output}</pre> : null}
        <p>{problem.fix_hint}</p>
        {problem.fix_command ? <PathValue value={problem.fix_command} wrap /> : null}
        <p className="text-muted-foreground">{offline ? <>Offline since <time dateTime={offlineSince}>{formatLocalTime(offlineSince)}</time> ({formatRelativeTime(offlineSince, now)}); last report </> : "Last report "}<time dateTime={reportedAt}>{formatLocalTime(reportedAt)}</time></p>
      </div>;
    })}
  </div>;
}
