import React from "react";

import type { Onboarding, ProjectIntegration } from "../../contracts/account.ts";
import { useAccountApi } from "./context.ts";
import { useResource, type Resource } from "./useResource.ts";

interface ProjectSetup {
  readonly projectId: string;
  readonly onboarding: Resource<Onboarding>;
  readonly integration: Resource<ProjectIntegration>;
}

export const ProjectSetupContext = React.createContext<ProjectSetup | null>(null);

export function ProjectSetupProvider({ projectId, children }: {
  readonly projectId: string;
  readonly children: React.ReactNode;
}): React.ReactElement {
  return projectId ? <ProjectSetupResources key={projectId} projectId={projectId}>{children}</ProjectSetupResources> : <>{children}</>;
}

function ProjectSetupResources({ projectId, children }: { readonly projectId: string; readonly children: React.ReactNode }): React.ReactElement {
  const api = useAccountApi();
  const onboarding = useResource(() => api.onboarding(projectId), [api, projectId]);
  const integration = useResource(() => api.integration(projectId), [api, projectId]);
  const waiting = (onboarding.value !== undefined && !onboarding.value.ready)
    || (Boolean(integration.value?.checkout_repository) && (
      integration.value?.checkout_verified !== true
      || (!onboarding.value?.policy && (onboarding.value?.observed_policies?.length ?? 0) === 0)
    ));
  React.useEffect(() => {
    if (!waiting) return;
    const timer = window.setInterval(() => {
      void onboarding.refresh();
      void integration.refresh();
    }, 5_000);
    return () => window.clearInterval(timer);
  }, [waiting, onboarding.refresh, integration.refresh]);
  return <ProjectSetupContext.Provider value={{ projectId, onboarding, integration }}>{children}</ProjectSetupContext.Provider>;
}
