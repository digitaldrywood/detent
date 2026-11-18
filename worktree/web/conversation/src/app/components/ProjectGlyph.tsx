import { useContext, type ReactElement } from "react";

import { ClientContext } from "../client.ts";
import { ProjectFavicon } from "../../components/ProjectFavicon.tsx";
import { HUB_ENVIRONMENT_ID } from "../../contracts/index.ts";

export function ProjectGlyph({
  className,
  projectId,
  projectName,
}: {
  className?: string;

  projectId?: string | null;

  projectName?: string | null;
}): ReactElement {
  const bootstrap = useContext(ClientContext)?.account;
  const saved = bootstrap?.projects.find((project) => project.id === projectId)?.project_icon;
  return (
    <ProjectFavicon
      project={{
        environmentId: HUB_ENVIRONMENT_ID,
        workspaceRoot: projectId ?? "",
        title: projectName ?? "",
        faviconPath: null,
        projectIcon: saved ?? null,
      }}
      className={className}
    />
  );
}
