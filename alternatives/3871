// The settings and setup screens, loaded the first time their route matches.
import { useNavigate, useParams, useSearch } from "@tanstack/react-router";
import React from "react";

import { SetupRoute } from "./account/Setup.tsx";
import { usePageTitle } from "./pageTitle.ts";
import { navigationTarget } from "./routes.account.tsx";
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
  const { section } = useParams({ strict: false }) as { section?: string };
  const search = useSearch({ strict: false }) as { project?: string };
  const id = section !== undefined && isSettingsSectionId(section) ? section : DEFAULT_SECTION;
  return <SettingsRoute section={id} project={search.project ?? null} onNavigate={useGo()} />;
}

export function SetupScreen(): React.ReactElement {
  const { project } = useParams({ strict: false }) as { project?: string };
  usePageTitle("Project setup");
  return <SetupRoute projectId={project ?? ""} onNavigate={useGo()} />;
}
