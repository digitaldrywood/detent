import type { BootstrapProject } from "../../contracts/conversation.ts";
import { HUB_ENVIRONMENT_ID } from "../../contracts/index.ts";
import type { ProjectId } from "../../contracts/ui.ts";
import type { EnvironmentProject, EnvironmentThread, EnvironmentThreadShell } from "./models.ts";

export type { EnvironmentProject, EnvironmentThread, EnvironmentThreadShell };

export function toEnvironmentProject(project: BootstrapProject): EnvironmentProject {
  return {
    environmentId: HUB_ENVIRONMENT_ID,
    id: project.id as ProjectId,
    title: project.name,
    workspaceRoot: project.id,
    faviconPath: null,
    projectIcon: null,
    // The project grouping orders by freshness. A hub project publishes no
    // timestamps to this client, so every project reads as equally fresh and
    // the order falls through to the title, which is upstream's tiebreak.
    createdAt: "",
    updatedAt: "",
  };
}
