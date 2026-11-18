import React from "react";
import type { FleetRunner } from "../../contracts/account.ts";
import { PathValue } from "../account/controls.tsx";
import { runnerProblemLabel } from "./runnerPresentation.ts";

export function RunnerProblems({ runner, projects = [], alert = false }: {
  readonly runner: FleetRunner;
  readonly projects?: readonly { readonly id: string; readonly name: string }[];
  readonly alert?: boolean;
}): React.ReactElement | null {
  if (!runner.problems?.length) return null;
  return <ul className="divide-y divide-warning/20 rounded-lg border border-warning/30 bg-warning/8">
      {runner.problems.map((problem) => <li key={`${problem.code}-${problem.project_id}-${problem.subject}-${problem.check}`} role={alert ? "alert" : undefined} className="flex min-w-0 flex-wrap items-center justify-between gap-3 px-3 py-2.5 text-xs">
        <div className="min-w-0 space-y-1">
          <p className="font-medium">{runnerProblemLabel(problem)}</p>
          {problem.subject || problem.project_id ? <p className="break-words text-muted-foreground">{[problem.subject, problem.project_id ? projects.find((project) => project.id === problem.project_id)?.name ?? "Unavailable project" : undefined].filter(Boolean).join(" · ")}</p> : null}
        </div>
        {problem.fix_command ? <PathValue value={problem.fix_command} wrap /> : <span className="text-muted-foreground">{problem.fix_hint}</span>}
      </li>)}
    </ul>;
}
