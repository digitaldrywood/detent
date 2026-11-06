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
  return <ProjectSetupResources projectId={projectId}>{children}</ProjectSetupResources>;
}

function useProjectResource<A>(projectId: string, read: () => Promise<A | undefined>, dependencies: readonly unknown[]): Resource<A> {
  const resource = useResource<{ projectId: string; value: A | undefined }>(async () => ({ projectId, value: await read() }), dependencies);
  return {
    ...resource,
    value: resource.value?.projectId === projectId ? resource.value.value : undefined,
    set: (value) => resource.set({ projectId, value }),
  };
}

function ProjectSetupResources({ projectId, children }: { readonly projectId: string; readonly children: React.ReactNode }): React.ReactElement {
  const api = useAccountApi();
  const onboarding = useProjectResource<Onboarding>(projectId, () => projectId ? api.onboarding(projectId) : Promise.resolve(undefined), [api, projectId]);
  const integration = useProjectResource<ProjectIntegration>(projectId, () => projectId ? api.integration(projectId) : Promise.resolve(undefined), [api, projectId]);
  const waiting = (onboarding.value !== undefined && !onboarding.value.ready)
    || (Boolean(integration.value?.checkout_repository) && (
      integration.value?.checkout_verified !== true
      || (!onboarding.value?.policy && (onboarding.value?.observed_policies?.length ?? 0) === 0)
    ));
  React.useEffect(() => {
    if (!waiting || onboarding.loading || integration.loading) return;
    let active = true;
    const refresh = async () => {
      await Promise.all([onboarding.refresh(), integration.refresh()]);
      if (active) timer = window.setTimeout(() => void refresh(), 5_000);
    };
    let timer = window.setTimeout(() => void refresh(), 5_000);
    return () => { active = false; window.clearTimeout(timer); };
  }, [waiting, onboarding.loading, integration.loading, onboarding.refresh, integration.refresh]);
  return <ProjectSetupContext.Provider value={{ projectId, onboarding, integration }}>{children}</ProjectSetupContext.Provider>;
}
