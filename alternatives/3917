import React from "react";
import type { DiagnosticFinding } from "../../contracts/diagnostics.ts";
import type { BootstrapProject } from "../../contracts/conversation.ts";
import { useIssueSurfaces } from "../adapters/surfaces.ts";
import { useRightPanelWorkspace } from "../components/RightPanel.tsx";
import { IssueAskPanel, useIssueAsk } from "../work/IssueAsk.tsx";

function findingContext(finding: DiagnosticFinding, project: BootstrapProject): string {
  return JSON.stringify({
    finding_id: finding.id,
    class: finding.class,
    severity: finding.severity,
    summary: finding.summary,
    next_action: finding.next_action,
    time: finding.when,
    resolved_at: finding.resolved_at ?? null,
    project: { id: project.id, name: project.name },
    subject: finding.subject,
  });
}

export function FindingAskWorkspace({
  finding,
  project,
  focusRequest,
  children,
}: {
  readonly finding: DiagnosticFinding;
  readonly project: BootstrapProject;
  readonly focusRequest: number;
  readonly children: (controls: React.ReactNode) => React.ReactNode;
}): React.ReactElement {
  const ask = useIssueAsk(project.id, null, findingContext(finding, project));
  const surfaces = useIssueSurfaces(null, null);
  const workspace = useRightPanelWorkspace({
    scopeKey: `finding:${project.id}:${finding.id}`,
    projectId: project.id,
    projectName: project.name,
    surfaces,
    attempts: [],
    history: [],
    now: Date.now(),
    title: "Ask about this finding",
    ask: <IssueAskPanel ask={ask} projectId={project.id} identifier={project.name} title={finding.summary} canWrite={project.can_write !== false} focusRequest={focusRequest} finding />,
  });
  const workspaceRef = React.useRef(workspace);
  workspaceRef.current = workspace;
  React.useEffect(() => workspaceRef.current.openAsk(), [focusRequest]);
  return <div className="flex min-h-0 min-w-0 flex-1">
    {children(workspace.layoutControls)}
    {workspace.panel}
  </div>;
}
