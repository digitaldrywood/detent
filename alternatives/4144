import { ChevronDownIcon, ServerIcon } from "lucide-react";
import React from "react";
import { useNavigate } from "@tanstack/react-router";
import { Menu, MenuTrigger, MenuPopup, MenuItem } from "../../components/ui/menu.tsx";
import { useOptionalShell } from "../App.tsx";
import { accountKey, readLastChatProject, useClient } from "../client.ts";
import { SpritePoolCard } from "../account/SpritePoolCard.tsx";

import { Button } from "../../components/ui/button.tsx";
import { Dialog, DialogDescription, DialogFooter, DialogHeader, DialogPanel, DialogPopup, DialogTitle } from "../../components/ui/dialog.tsx";
import type { FleetResponse, FleetRunner, RunnerRouting } from "../../contracts/account.ts";
import type { LoadHostHistory } from "./HostLoad.tsx";
import { useRunnerSuggestions } from "./runnerSuggestions.ts";
import { cn } from "../../lib/utils.ts";
import { useAccountApi, useAccountBootstrap } from "../account/context.ts";
import { useResource } from "../account/useResource.ts";
import { SettingsPageContainer, SettingsRow, SettingsSection } from "../settings/settingsLayout.tsx";
import {
  EnrollRunnerDialog,
  INSTALL_DETENT_COMMAND,
  BREW_INSTALL_DETENT_COMMAND,
  PendingEnrollments,
  type PendingEnrollment,
} from "./EnrollRunner.tsx";
import { HostCard } from "./HostCard.tsx";
import { RunnerDetailPage, type RunnerProject } from "./RunnerDetailPage.tsx";
import { NativeSelect } from "../account/controls.tsx";
import { RunnerStatusDot } from "../../components/RunnerStatusDot.tsx";
import { runnerNeedsAttention } from "./runnerPresentation.ts";
import { RunnerRemoval } from "./RunnerRemoval.tsx";

export function RunnersSectionView({
  fleet, now, organizationName = "this organization", onEnroll, enrollments = [], projects = [], onOpenRunner, onRemoveRunner, onAssist, spritePools, spritePoolAttentionCount = 0,
}: {
  readonly fleet: FleetResponse;
  readonly onAssist?: () => void;
  readonly spritePools?: (attentionOnly: boolean) => React.ReactNode;
  readonly spritePoolAttentionCount?: number;
  readonly projects?: readonly RunnerProject[];
  readonly now?: number;
  readonly organizationName?: string;
  readonly onEnroll?: (name?: string) => void;
  readonly enrollments?: readonly PendingEnrollment[];
  readonly onOpenRunner?: (runner: FleetRunner) => void;
  readonly onRemoveRunner?: (runner: FleetRunner) => Promise<void>;
}): React.ReactElement {
  const [attentionOnly, setAttentionOnly] = React.useState(() => new URLSearchParams(window.location.search).get("health") === "needs_attention");
  const attentionCount = fleet.runners.filter((runner) => runnerNeedsAttention(runner)).length + spritePoolAttentionCount;
  const runners = attentionOnly ? fleet.runners.filter((runner) => runnerNeedsAttention(runner)) : fleet.runners;
  function filterUrl(only: boolean): string {
    const url = new URL(window.location.href);
    if (only) url.searchParams.set("health", "needs_attention");
    else url.searchParams.delete("health");
    return url.pathname + url.search + url.hash;
  }
  function selectAttentionFilter(event: React.MouseEvent<HTMLAnchorElement>, only: boolean): void {
    if (event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
    event.preventDefault();
    window.history.replaceState(window.history.state, "", filterUrl(only));
    setAttentionOnly(only);
  }
  React.useEffect(() => {
    const update = () => setAttentionOnly(new URLSearchParams(window.location.search).get("health") === "needs_attention");
    window.addEventListener("popstate", update);
    return () => window.removeEventListener("popstate", update);
  }, []);
  const [runnerToRemove, setRunnerToRemove] = React.useState<FleetRunner | null>(null);
  return (
    <>
      {runnerToRemove && onRemoveRunner ? <RunnerRemoval runner={runnerToRemove} onRemove={onRemoveRunner} onClose={() => setRunnerToRemove(null)} /> : null}
      <header className="flex flex-col items-start justify-between gap-4 sm:flex-row">
        <div className="min-w-0 space-y-2">
          <h2 className="text-xl font-semibold tracking-tight">Runners</h2>
          <p className="text-[13px] text-muted-foreground">Runners take work for {organizationName}. Add capacity manually, or let Luna walk you through setup.</p>
        </div>
        {onEnroll === undefined ? null : <Menu>
          <MenuTrigger render={<Button size="sm">Add runner <ChevronDownIcon aria-hidden="true" /></Button>} />
          <MenuPopup align="end">
            <MenuItem onClick={() => onEnroll()}>Manual</MenuItem>
            <MenuItem disabled={onAssist === undefined} onClick={onAssist}>AI assisted</MenuItem>
          </MenuPopup>
        </Menu>}
      </header>
      {fleet.runners.length === 0 ? (
        <div className="flex flex-col items-start gap-4 rounded-xl border border-border/60 bg-card/40 p-6">
          <p className="text-sm">No runners yet. Add a runner manually or let Luna help you set one up.</p>
        </div>
      ) : (
        <>
          <SettingsSection id="settings-runners" title="Runner list" variant="plain" hideTitle>
            <nav aria-label="Filter runners" className="mb-3 flex w-fit max-w-full rounded-lg border border-border/60 bg-muted/40 p-1 text-xs">
              {([[false, "All " + fleet.runners.length], [true, "Needs attention " + attentionCount]] as const).map(([only, label]) => (
                <a key={label} href={filterUrl(only)} aria-current={attentionOnly === only ? "true" : undefined} onClick={(event) => selectAttentionFilter(event, only)} className={cn("rounded-md px-3 py-1.5 outline-none focus-visible:ring-2 focus-visible:ring-ring", attentionOnly === only ? "bg-background text-foreground shadow-xs" : "text-muted-foreground hover:text-foreground")}>{label}</a>
              ))}
            </nav>
            <div className="@container/runner-list overflow-hidden rounded-xl border border-border/60 bg-card/40">
              <div aria-hidden="true" className="hidden grid-cols-[minmax(0,1.5fr)_minmax(0,1fr)_5rem_minmax(0,1fr)_7rem] gap-4 border-b border-border/50 px-4 py-3 text-xs text-muted-foreground @[48rem]/runner-list:grid"><span>Runner</span><span>Version</span><span>Slots</span><span>Projects</span><span /></div>
              <div className="divide-y divide-border/50">
                {runners.map((runner) => <HostCard key={runner.id} runner={runner} now={now} projects={projects} onOpen={() => onOpenRunner?.(runner)} {...(fleet.editable !== false && onRemoveRunner ? { onRemove: () => { setRunnerToRemove(runner); } } : {})} />)}
                {runners.length === 0 ? <p className="px-4 py-6 text-[13px] text-muted-foreground">No runners need attention.</p> : null}
              </div>
              {spritePools?.(attentionOnly)}
            </div>
          </SettingsSection>
        </>
      )}
      {fleet.runners.length === 0 ? spritePools?.(false) : null}
      {enrollments.length === 0 ? null : (
        <SettingsSection id="settings-enrollments" title="Waiting to connect" icon={<ServerIcon className="size-3.5" />} headerAction={<span className="text-xs text-muted-foreground">Created in this session</span>}>
          <PendingEnrollments enrollments={enrollments} onRenew={onEnroll} />
        </SettingsSection>
      )}
      {fleet.runners.length > 0 ? <div aria-label="Runner status legend" className="flex flex-wrap gap-4 text-xs text-muted-foreground">
        {[["healthy", "active", "Healthy"], ["needs_attention", "active", "Needs attention"], ["healthy", "draining", "Draining"], ["offline", "active", "Offline"]].map(([health, state, label]) => <span key={label} className="inline-flex items-center gap-1.5"><RunnerStatusDot runner={{ health: health!, state: state! }} />{label}</span>)}
      </div> : null}
    </>
  );
}

/** The section as the settings page mounts it: it owns the one read. */
export function RunnersSettings({ runnerId, onNavigate, onRunnerName }: { readonly runnerId?: string; readonly onNavigate?: (to: string) => void; readonly onRunnerName?: (name: string) => void }): React.ReactElement {
  const shell = useOptionalShell();
  const api = useAccountApi();
  const bootstrap = useAccountBootstrap();
  const client = useClient();
  const navigate = useNavigate();
  const [assistOpen, setAssistOpen] = React.useState(false);
  const [poolAttention, setPoolAttention] = React.useState<Readonly<Record<string, boolean>>>({});
  const onPoolAttentionChange = React.useCallback((projectId: string, needsAttention: boolean) => {
    setPoolAttention((current) => current[projectId] === needsAttention ? current : { ...current, [projectId]: needsAttention });
  }, []);
  const writableProjects = bootstrap?.projects.filter((project) => project.can_write) ?? [];
  const [assistProject, setAssistProject] = React.useState("");
  function assist(projectId: string): void {
    const chatProjectId = projectId || writableProjects.find((project) => project.id === (shell?.projectId || readLastChatProject()))?.id || writableProjects[0]?.id || "";
    const scope = { accountKey: accountKey(client), projectId: chatProjectId, conversationId: "new" };
    shell?.setProjectId(chatProjectId);
    const request = `Help me add a runner for ${projectId ? "this project only" : "all current and future projects in this organization"}. Ask whether I want a Fly Sprite or a machine I run, then walk me through enrollment, the register command, provider login and the first connection.${projectId ? "" : " Configure one organization Sprite pool if I choose Sprites."} For a machine I run, help me install Detent before creating the enrollment token: ${INSTALL_DETENT_COMMAND}. On macOS also offer ${BREW_INSTALL_DETENT_COMMAND}. Check the Hub's minimum runner version and help me update an older installation before registering.`;
    const existing = client.drafts.readDraft(scope);
    client.drafts.writeDraft(scope, existing.trim() ? `${existing}\n\n${request}` : request);
    setAssistOpen(false);
    if (projectId) void navigate({ to: "/chat/p/$projectId", params: { projectId } });
    else void navigate({ to: "/chat" });
  }
  const fleet = useResource(() => api.fleet(), [api]);
  const loadHostHistory = React.useCallback<LoadHostHistory>((runner, from, to) => api.runnerHostHistory(runner, from, to), [api]);
  const [open, setOpen] = React.useState(false);
  const [enrollmentName, setEnrollmentName] = React.useState("");
  const [enrollments, setEnrollments] = React.useState<readonly PendingEnrollment[]>([]);
  const canEnroll = bootstrap?.actor.can_manage_runners ?? false;
  const runnerRevisions = React.useMemo(() => fleet.value?.runners.filter((runner) => runner.id === runnerId).map((runner) => ({ id: runner.id, revision: runner.revision ?? 0 })) ?? [], [fleet.value, runnerId]);
  const suggestions = useRunnerSuggestions(api.runnerCapacity, runnerRevisions, canEnroll);
  async function reloadRunner(): Promise<void> {
    fleet.set(await api.fleet());
  }

  const selected = fleet.value?.runners.find((runner) => runner.id === runnerId);
  React.useEffect(() => { onRunnerName?.(selected?.display_name ?? "Runner"); }, [selected?.display_name, onRunnerName]);
  async function saveRouting(runner: FleetRunner, routing: RunnerRouting): Promise<void> {
    await api.setRunnerRouting({ runner: runner.id, revision: runner.revision ?? 0, displayName: routing.display_name,
      tags: routing.tags, state: routing.state, capacityLimit: routing.capacity_limit, projectIds: routing.project_ids, scope: routing.scope,
      isolationTier: routing.isolation_tier, hostServices: routing.host_services, availability: routing.availability });
    await reloadRunner();
  }
  async function removeRunner(runner: FleetRunner): Promise<void> {
    await api.removeRunner(runner.id);
    if (fleet.value) fleet.set({ ...fleet.value, runners: fleet.value.runners.filter((entry) => entry.id !== runner.id) });
    if (runnerId) onNavigate?.("/settings/runners");
    void fleet.refresh();
  }

  return (
    <SettingsPageContainer width="wide">
      {fleet.value === undefined ? (
        fleet.error !== null ? (
          <SettingsSection
            title="Runners"
            icon={<ServerIcon className="size-3.5" />}
          >
            <SettingsRow
              title="The fleet is not available"
              description={
                fleet.error.isAccessError
                  ? "You do not have access to this organization's fleet."
                  : fleet.error.message
              }
              control={
                <Button size="sm" variant="outline" onClick={() => void fleet.refresh()}>
                  Try again
                </Button>
              }
            />
          </SettingsSection>
        ) : (
          <SettingsSection
            title="Runners"
            icon={<ServerIcon className="size-3.5" />}
          >
            <SettingsRow title="Loading the fleet" />
          </SettingsSection>
        )
      ) : runnerId ? (
        selected ? <RunnerDetailPage
          key={selected.id}
          runner={selected}
          projects={bootstrap?.projects ?? []}
          editable={(selected.editable ?? fleet.value.editable) !== false && (canEnroll || selected.editable === true)}
          currentVersion={fleet.value.current}
          onClose={() => onNavigate?.("/settings/runners")}
          onSave={saveRouting}
          onReload={reloadRunner}
          suggestion={suggestions[selected.id]}
          loadHostHistory={loadHostHistory}
          {...(canEnroll && fleet.value.editable ? { onRemove: removeRunner } : {})}
        /> : <SettingsSection title="Runner unavailable">
          <SettingsRow title="This runner was removed or is not accessible." control={<Button variant="outline" onClick={() => onNavigate?.("/settings/runners")}>Back to Runners</Button>} />
        </SettingsSection>
      ) : (
        <RunnersSectionView
          fleet={fleet.value}
          organizationName={bootstrap?.organization.name}
          projects={bootstrap?.projects ?? []}
          onOpenRunner={(runner) => onNavigate?.(`/settings/runners/${encodeURIComponent(runner.id)}`)}
          spritePoolAttentionCount={poolAttention[""] ? 1 : 0}
          spritePools={(attentionOnly) => bootstrap ? <SpritePoolCard compact projectId="" canManage={bootstrap.actor.can_manage} attentionOnly={attentionOnly} onAttentionChange={onPoolAttentionChange} /> : null}
          {...(canEnroll ? { onAssist: () => {
            setAssistProject("");
            setAssistOpen(true);
          } } : {})}
          {...(canEnroll && fleet.value.editable ? { onRemoveRunner: removeRunner } : {})}
          enrollments={enrollments}
          {...(canEnroll ? { onEnroll: (name = "") => { setEnrollmentName(name); setOpen(true); } } : {})}

        />
      )}
      <Dialog open={assistOpen} onOpenChange={setAssistOpen}>
        <DialogPopup>
          <DialogHeader>
            <DialogTitle>Add runner with Luna</DialogTitle>
            <DialogDescription>Luna will help you choose a Fly Sprite or your own machine and walk you through setup.</DialogDescription>
          </DialogHeader>
          <DialogPanel>
            <label className="grid gap-2 text-sm">Project
              <NativeSelect aria-label="Project" value={assistProject} onValueChange={setAssistProject} options={[{ value: "", label: "All projects" }, ...writableProjects.map((project) => ({ value: project.id, label: project.name }))]} />
            </label>
          </DialogPanel>
          <DialogFooter>
            <Button variant="outline" onClick={() => setAssistOpen(false)}>Cancel</Button>
            <Button disabled={assistProject !== "" && !writableProjects.some((project) => project.id === assistProject)} onClick={() => assist(assistProject)}>Open Luna</Button>
          </DialogFooter>
        </DialogPopup>
      </Dialog>
      {canEnroll ? (
        <EnrollRunnerDialog
          open={open}
          initialName={enrollmentName}
          onOpenChange={setOpen}
          fleet={fleet}
          onConnected={(entry) => setEnrollments((current) => current.filter((old) => old.id !== entry.id))}
          onEnrolled={(entry) => {
            setEnrollments((current) => [entry, ...current.filter((old) => old.id !== entry.id)]);
            // A redeemed enrollment shows up as a runner, not as an
            // enrollment, so the fleet is re-read rather than guessed at.
            void fleet.refresh();
          }}
        />
      ) : null}
    </SettingsPageContainer>
  );
}
