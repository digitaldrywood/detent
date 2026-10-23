import type { ReactElement } from "react";

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
  return (
    <ProjectFavicon
      project={{
        environmentId: HUB_ENVIRONMENT_ID,
        workspaceRoot: projectId ?? "",
        title: projectName ?? "",
        faviconPath: null,
        projectIcon: null,
      }}
      className={className}
    />
  );
}
