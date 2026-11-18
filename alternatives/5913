import { useBlocker, useNavigate } from "@tanstack/react-router";
import React from "react";

import { WorkspaceBreadcrumb, WorkspaceBreadcrumbItem, WorkspaceBreadcrumbSeparator } from "../../components/WorkspaceBreadcrumb.tsx";
import { WorkspacePageHeader } from "../../components/WorkspacePageHeader.tsx";
import { AlertDialog, AlertDialogPopup, AlertDialogHeader, AlertDialogTitle, AlertDialogDescription, AlertDialogFooter } from "../../components/ui/alert-dialog.tsx";
import { Button } from "../../components/ui/button.tsx";
import { Select, SelectItem, SelectPopup, SelectTrigger, SelectValue } from "../../components/ui/select.tsx";
import { Tabs, TabsList, TabsTab, TabsPanel } from "../../components/ui/tabs.tsx";
import { useAccountApi, useAccountBootstrap } from "../account/context.ts";
import { ProjectSettingsRoute, OBSERVED_POLICY_REFRESH_MS } from "../account/ProjectSettings.tsx";
import { useResource } from "../account/useResource.ts";
import { usePageTitle } from "../pageTitle.ts";
import { setupStepsLeftLabel } from "../work/components/FirstRun.tsx";
import { PROJECT_SETTINGS_TABS, projectSettingsPath, projectSettingsTab } from "./projectSettings.ts";
import { ProjectSettingsDirtyContext } from "./projectSettingsDirty.ts";
import { SettingsPageContainer, SettingsRow, SettingsSection } from "./settingsLayout.tsx";

export function ProjectSettingsPage({ project: requested, tab: requestedTab }: { readonly project: string; readonly tab?: string }): React.ReactElement {
  const bootstrap = useAccountBootstrap();
  const projects = bootstrap?.projects ?? [];
  const project = projects.find((candidate) => candidate.slug === requested || candidate.id === requested);
  const tab = projectSettingsTab(requestedTab);
  const navigate = useNavigate();
  const api = useAccountApi();
  const setup = useResource(() => project === undefined ? Promise.resolve(undefined) : api.onboarding(project.id), [api, project?.id]);
  const [dirtySections, setDirtySections] = React.useState<ReadonlySet<string>>(() => new Set());
  const register = React.useCallback((id: string, dirty: boolean) => {
    setDirtySections((current) => {
      if (current.has(id) === dirty) return current;
      const next = new Set(current);
      if (dirty) next.add(id);
      else next.delete(id);
      return next;
    });
  }, []);
  const dirty = dirtySections.size > 0;
  const blocker = useBlocker({
    shouldBlockFn: ({ current, next }) => dirty && current.pathname !== next.pathname,
    withResolver: true,
    enableBeforeUnload: false,
  });
  const go = React.useCallback((to: string) => { void navigate({ to } as never); }, [navigate]);
  React.useEffect(() => {
    if ((project !== undefined && requested !== (project.slug || project.id)) || requestedTab !== tab) {
      void navigate({ to: projectSettingsPath(project ?? requested, tab), replace: true } as never);
    }
  }, [navigate, project, requested, requestedTab, tab]);
  React.useEffect(() => {
    const timer = globalThis.setInterval(() => void setup.refresh(), OBSERVED_POLICY_REFRESH_MS);
    return () => globalThis.clearInterval(timer);
  }, [setup.refresh]);
  const tabsRef = React.useRef<HTMLDivElement>(null);
  React.useEffect(() => {
    tabsRef.current?.querySelector<HTMLElement>('[role="tab"][aria-selected="true"]')?.scrollIntoView?.({ block: "nearest", inline: "nearest" });
  }, [tab]);
  const name = project?.name ?? requested;
  const label = PROJECT_SETTINGS_TABS.find((candidate) => candidate.id === tab)!.label;
  usePageTitle(`${name} settings`);

  return (
    <ProjectSettingsDirtyContext.Provider value={register}>
      <div className="flex min-h-0 min-w-0 flex-1 flex-col bg-background text-foreground">
        <WorkspacePageHeader className="h-auto">
          <WorkspaceBreadcrumb ariaLabel="Settings breadcrumb" className="min-w-0 py-2">
            <WorkspaceBreadcrumbItem><Button variant="link" size="sm" onClick={() => go("/settings")}>Settings</Button></WorkspaceBreadcrumbItem>
            <WorkspaceBreadcrumbSeparator />
            <WorkspaceBreadcrumbItem><Button variant="link" size="sm" onClick={() => go("/settings/projects")}>Projects</Button></WorkspaceBreadcrumbItem>
            <WorkspaceBreadcrumbSeparator />
            <WorkspaceBreadcrumbItem current><span className="min-w-0 truncate">{name}</span></WorkspaceBreadcrumbItem>
          </WorkspaceBreadcrumb>
        </WorkspacePageHeader>
        <SettingsPageContainer>
          <div className="flex flex-wrap items-center justify-between gap-3 px-3 sm:px-4">
            <h1 className="min-w-0 break-words text-xl font-semibold tracking-[-0.01em]">{name} settings</h1>
            <Select value={project?.id ?? null} onValueChange={(value) => {
              const selected = projects.find((candidate) => candidate.id === value);
              if (selected) go(projectSettingsPath(selected, tab));
            }}>
              <SelectTrigger aria-label="Project" size="sm" className="w-44 max-w-full"><SelectValue>{name}</SelectValue></SelectTrigger>
              <SelectPopup>{projects.map((candidate) => <SelectItem key={candidate.id} value={candidate.id}>{candidate.name}</SelectItem>)}</SelectPopup>
            </Select>
          </div>
          {setup.value && !setup.value.ready ? <SettingsSection title="Project setup">
            <SettingsRow title={setupStepsLeftLabel(setup.value.steps.filter((step) => step.state !== "ready").length)} control={<Button size="sm" onClick={() => go(`/projects/${project?.id}/setup`)}>Finish setup</Button>} />
          </SettingsSection> : null}
          <Tabs variant="line" value={tab} onValueChange={(value) => go(projectSettingsPath(project ?? requested, projectSettingsTab(String(value))))}>
            <TabsList ref={tabsRef} aria-label="Project settings">
              {PROJECT_SETTINGS_TABS.map((candidate) => <TabsTab key={candidate.id} value={candidate.id}>{candidate.label}</TabsTab>)}
            </TabsList>
            <TabsPanel value={tab} className="pt-6">
              {project === undefined ? <SettingsSection title="Not available"><SettingsRow title="You do not have access to this project" description="Ask an owner or admin for a grant on it, or pick a different project." /></SettingsSection> : (
                <ProjectSettingsRoute key={`${project.id}:${tab}`} projectId={project.id} tab={tab} onboarding={setup.value} onNavigate={go} />
              )}
            </TabsPanel>
          </Tabs>
        </SettingsPageContainer>
        <AlertDialog open={blocker.status === "blocked"} onOpenChange={(open) => { if (!open) blocker.reset?.(); }}>
          <AlertDialogPopup>
            <AlertDialogHeader><AlertDialogTitle>Discard unsaved changes?</AlertDialogTitle><AlertDialogDescription>Your edits on {label} are not saved.</AlertDialogDescription></AlertDialogHeader>
            <AlertDialogFooter>
              <Button variant="outline" onClick={() => blocker.reset?.()}>Keep editing</Button>
              <Button variant="destructive" onClick={() => blocker.proceed?.()}>Discard</Button>
            </AlertDialogFooter>
          </AlertDialogPopup>
        </AlertDialog>
      </div>
    </ProjectSettingsDirtyContext.Provider>
  );
}
