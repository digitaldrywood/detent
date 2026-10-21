// The settings and setup screens, loaded the first time their route matches.
import { Navigate, useNavigate, useParams, useSearch } from "@tanstack/react-router";
import React from "react";

import { SetupRoute } from "./account/Setup.tsx";
import { usePageTitle } from "./pageTitle.ts";
import { navigationTarget } from "./routes.account.tsx";
import { ProjectSettingsPage } from "./settings/ProjectSettingsPage.tsx";
import { SettingsRoute } from "./settings/Settings.tsx";
import { DEFAULT_SECTION, isSettingsSectionId } from "./settings/sections.tsx";

function useGo(): (to: string) => void {
  const navigate = useNavigate();
  return React.useCallback(
    (to: string) => {
      void navigate(navigationTarget(to) as never);
    },
    [navigate],
  );
}

export function SettingsScreen(): React.ReactElement {
  const { section, organizationPage } = useParams({ strict: false }) as { section?: string; organizationPage?: string };
  const search = useSearch({ strict: false }) as { project?: string };
  const requested = organizationPage === undefined ? section : `organization/${organizationPage}`;
  const id = requested !== undefined && isSettingsSectionId(requested) ? requested : organizationPage === undefined ? DEFAULT_SECTION : "organization";
  const go = useGo();
  if (id === "integrations" && search.project) return <Navigate to="/settings/projects/$project/$tab" params={{ project: search.project, tab: "general" }} replace />;
  return <SettingsRoute section={id} onNavigate={go} />;
}

export function SetupScreen(): React.ReactElement {
  const { project } = useParams({ strict: false }) as { project?: string };
  usePageTitle("Project setup");
  return <SetupRoute projectId={project ?? ""} onNavigate={useGo()} />;
}

export function ProjectSettingsScreen(): React.ReactElement {
  const { project, tab } = useParams({ strict: false }) as { project: string; tab?: string };
  return <ProjectSettingsPage key={project} project={project} {...(tab === undefined ? {} : { tab })} />;
}
