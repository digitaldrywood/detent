// Workspace compositions: right panel tabs and surfaces, settings rows, work
// board lane and card, usage.
import React from "react";

import type { RightPanelSurface } from "../../../app/adapters/rightPanel.ts";
import { OutputSurface } from "../../../app/components/surfaces/OutputSurface.tsx";
import { WorkspaceStatusView } from "../../../app/components/surfaces/WorkspaceStatusView.tsx";
import { describeWorkspaceStatus } from "../../../app/lib/workspaceStatus.ts";
import type { WorkspaceReason, WorkspaceState } from "../../../contracts/work.ts";
import {
  SettingsRow,
  SettingsSection,
  SettingsUnavailableGroup,
} from "../../../app/settings/settingsLayout.tsx";
import { UsageView } from "../../../app/usage/UsagePage.tsx";
import type { UsagePagePreferences } from "../../../app/usage/usagePagePreferences.ts";
import { BoardLane } from "../../../app/work/components/BoardLane.tsx";
import { IssueCard } from "../../../app/work/components/IssueCard.tsx";
import type { Lane, WorkItemView } from "../../../app/work/lib/model.ts";
import { RightPanelTabs } from "../../../components/RightPanelTabs.tsx";
import { Button } from "../../../components/ui/button.tsx";
import { Select, SelectItem, SelectPopup, SelectTrigger, SelectValue } from "../../../components/ui/select.tsx";
import { Switch } from "../../../components/ui/switch.tsx";
import { HUB_ENVIRONMENT_ID } from "../../../contracts/index.ts";

import { LANES, NOW, runningAttempt, usageDaily, usageEmpty, workItem } from "../fixtures";
import { LONG_LABEL, type GalleryDoc } from "../specimen";

const noop = (): void => undefined;

// --- Right panel ------------------------------------------------------------

const SURFACES: RightPanelSurface[] = [
  { id: "output", kind: "output" },
  { id: "diff", kind: "diff" },
  { id: "files", kind: "files" },
];

function RightPanelSpecimen({ unavailable }: { readonly unavailable: boolean }) {
  const [active, setActive] = React.useState<string | null>("output");
  const [surfaces, setSurfaces] = React.useState<readonly RightPanelSurface[]>(SURFACES);
  return (
    <div className="flex h-full min-h-0">
      <RightPanelTabs
        mode="inline"
        open
        surfaces={surfaces}
        environmentId={HUB_ENVIRONMENT_ID as never}
        activeSurfaceId={active}
        pendingSurfaceIds={new Set()}
        previewSessions={{}}
        desktopByTabId={{}}
        terminalLabelsById={new Map()}
        onActivate={(surface) => setActive(surface.id)}
        onCloseSurface={(surface) => setSurfaces((all) => all.filter((s) => s.id !== surface.id))}
        onCloseOtherSurfaces={(surface) => setSurfaces([surface])}
        onCloseSurfacesToRight={noop}
        onCloseAllSurfaces={() => setSurfaces([])}
        onCopyFilePath={noop}
        onAddBrowser={noop}
        onAddBrowserInProfile={noop}
        onAddTerminal={noop}
        onAddDiff={noop}
        onAddFiles={noop}
        onAddPullRequest={noop}
        onAddOutput={noop}
        browserAvailable={false}
        terminalAvailable={false}
        terminalDisabledReason="Open an issue to start a terminal on its runner."
        diffAvailable
        filesAvailable={!unavailable}
        filesDisabledReason={unavailable ? "No runner has claimed this workspace." : null}
        pullRequestAvailable={false}
        outputAvailable
        liveAgentCount={0}
      >
        <OutputSurface
          runs={[]}
          activeRunId={null}
          onSelectRun={noop}
          state={unavailable ? "failed" : "ready"}
          reason={unavailable ? "no_runner" : null}
          error={null}
          loading={false}
          onRetry={noop}
        />
      </RightPanelTabs>
    </div>
  );
}

export const rightPanel: GalleryDoc = {
  meta: {
    name: "Right panel tabs",
    kind: "composition",
    group: "workspace",
    source: "src/components/RightPanelTabs.tsx",
  },
  specimens: [
    {
      id: "empty",
      title: "Tabs with the Output surface empty",
      note: "Hover a tab to reveal its close button; the + menu lists the surfaces and why some are unavailable.",
      height: 420,
      render: () => <RightPanelSpecimen unavailable={false} />,
    },
    {
      id: "unavailable",
      title: "Surface unavailable: no runner",
      height: 420,
      render: () => <RightPanelSpecimen unavailable />,
    },
  ],
};

const OUTPUT_STATES: ReadonlyArray<{ label: string; state: WorkspaceState | null; reason: WorkspaceReason | null }> = [
  { label: "No runs yet (ready)", state: "ready", reason: null },
  { label: "Opening (no answer yet)", state: null, reason: null },
  { label: "Starting", state: "starting", reason: null },
  { label: "Unavailable: no runner", state: "failed", reason: "no_runner" },
  { label: "Closed", state: "closed", reason: null },
];

export const outputSurface: GalleryDoc = {
  meta: {
    name: "Output surface",
    kind: "surface",
    group: "panels",
    source: "src/app/components/surfaces/OutputSurface.tsx",
  },
  specimens: OUTPUT_STATES.map(({ label, state, reason }, index) => ({
    id: `state-${index}`,
    title: label,
    render: () => (
      <div className="flex h-48 flex-col rounded-lg border">
        <OutputSurface
          runs={[]}
          activeRunId={null}
          onSelectRun={noop}
          state={state}
          reason={reason}
          error={null}
          loading={state === null}
          onRetry={noop}
        />
      </div>
    ),
  })),
};

export const workspaceStatusView: GalleryDoc = {
  meta: {
    name: "Workspace status view",
    kind: "surface",
    group: "panels",
    source: "src/app/components/surfaces/WorkspaceStatusView.tsx",
  },
  specimens: [
    {
      id: "states",
      title: "Opening, starting (with skeleton), unreachable, failed",
      render: () => (
        <div className="grid gap-3 sm:grid-cols-2">
          {(
            [
              ["requested", null],
              ["starting", null],
              ["unreachable", "runner_restarted"],
              ["failed", "checkout_failed"],
            ] as const
          ).map(([state, reason]) => {
            const status = describeWorkspaceStatus({ state, reason, capability: "files" });
            return (
              <div key={state} className="flex h-56 flex-col rounded-lg border">
                {status === null ? null : <WorkspaceStatusView status={status} testIdPrefix="files" onRetry={noop} />}
              </div>
            );
          })}
        </div>
      ),
    },
  ],
};

// --- Settings ---------------------------------------------------------------

function SettingsSpecimen() {
  const [notify, setNotify] = React.useState(true);
  return (
    <div className="mx-auto flex max-w-2xl flex-col gap-6">
      <SettingsSection title="Notifications" headerAction={<Button size="xs" variant="outline">Reset</Button>}>
        <SettingsRow
          title="Notify on review"
          description="Send a notification when a pull request is ready for you."
          control={<Switch checked={notify} onCheckedChange={setNotify} aria-label="Notify on review" />}
        />
        <SettingsRow
          title="Default lane"
          description="Where new issues land."
          help={{ label: "About lanes", text: "Lanes are the project's workflow states." }}
          control={
            <Select<string> defaultValue="Backlog">
              <SelectTrigger size="sm" className="w-36" aria-label="Default lane">
                <SelectValue />
              </SelectTrigger>
              <SelectPopup>
                {LANES.map((lane) => (
                  <SelectItem key={lane.id} value={lane.name}>
                    {lane.name}
                  </SelectItem>
                ))}
              </SelectPopup>
            </Select>
          }
        />
        <SettingsRow title="Runner" description={LONG_LABEL} status="Connected" serverScoped />
      </SettingsSection>
      <SettingsSection title="Billing">
        <SettingsUnavailableGroup message="Only organization owners can change billing.">
          <SettingsRow title="Plan" status="Pilot" />
          <SettingsRow title="Spend limit" control={<Switch disabled aria-label="Spend limit" />} />
        </SettingsUnavailableGroup>
      </SettingsSection>
    </div>
  );
}

export const settingsRows: GalleryDoc = {
  meta: {
    name: "Settings rows",
    kind: "composition",
    group: "settings",
    source: "src/app/settings/settingsLayout.tsx",
  },
  specimens: [
    {
      id: "groups",
      title: "Grouped section with controls, help, status; unavailable group",
      minHeight: 420,
      render: () => <SettingsSpecimen />,
    },
  ],
};

// --- Work board -------------------------------------------------------------

function laneNamed(name: string): Lane {
  return LANES.find((lane) => lane.name.toLowerCase() === name.toLowerCase()) ?? LANES[0]!;
}

function BoardSpecimen() {
  const progress = laneNamed("In Progress");
  const items: WorkItemView[] = [
    workItem({ id: "wi_ds_1", identifier: "detent#142", title: "Port the design-system gallery", state: progress.name, attempt: runningAttempt() }),
    workItem({ id: "wi_ds_2", identifier: "detent#139", title: LONG_LABEL, state: progress.name, labels: ["ui", "design-system", "needs-review"] }),
    workItem({ id: "wi_ds_3", identifier: "detent#131", title: "Usage table rounding", state: progress.name, blockedBy: ["detent#128"] }),
  ];
  const laneProps = {
    showProject: true,
    now: NOW,
    onOpen: noop,
    movesFor: () => LANES.map((lane) => lane.name),
    onMove: noop,
    movingIds: new Set<string>(),
    draggingId: null,
    onDragStart: noop,
    onDragEnd: noop,
    onDrop: null,
  };
  return (
    <div className="flex items-start gap-3 overflow-x-auto">
      <BoardLane
        {...laneProps}
        lane={progress}
        items={items}
        total={7}
      />
      <BoardLane
        {...laneProps}
        lane={laneNamed("Todo")}
        items={[]}
        emptyLabel="Nothing waiting"
        onCreate={noop}
      />
    </div>
  );
}

export const boardLane: GalleryDoc = {
  meta: {
    name: "Board lane",
    kind: "composition",
    group: "work",
    source: "src/app/work/components/BoardLane.tsx",
  },
  specimens: [
    {
      id: "lanes",
      title: "Lane with running, labelled and blocked cards; empty lane",
      note: "The lane count shows the cards loaded against the project total; each card's menu lists the moves.",
      minHeight: 520,
      render: () => <BoardSpecimen />,
    },
  ],
};

export const issueCard: GalleryDoc = {
  meta: {
    name: "Issue card",
    kind: "composition",
    group: "work",
    source: "src/app/work/components/IssueCard.tsx",
  },
  specimens: [
    {
      id: "states",
      title: "Idle, running, blocked, moving",
      render: () => (
        <div className="grid max-w-3xl gap-3 sm:grid-cols-2">
          <IssueCard item={workItem()} showProject={false} now={NOW} onOpen={noop} moves={[]} onMove={noop} />
          <IssueCard
            item={workItem({ id: "wi_ds_run", attempt: runningAttempt() })}
            showProject
            now={NOW}
            onOpen={noop}
            moves={["Done"]}
            onMove={noop}
          />
          <IssueCard
            item={workItem({ id: "wi_ds_blocked", title: LONG_LABEL, blockedBy: ["detent#128"] })}
            showProject={false}
            now={NOW}
            onOpen={noop}
            moves={[]}
            onMove={noop}
          />
          <IssueCard item={workItem({ id: "wi_ds_moving" })} showProject={false} now={NOW} onOpen={noop} moves={[]} onMove={noop} moving />
        </div>
      ),
    },
  ],
};

// --- Usage ------------------------------------------------------------------

function UsageSpecimen({ empty, error }: { readonly empty?: boolean; readonly error?: string }) {
  const [preferences, setPreferences] = React.useState<UsagePagePreferences>({ metric: "cost", windowDays: 30 });
  const merged = React.useMemo(() => (empty === true ? usageEmpty() : usageDaily()), [empty]);
  return (
    <UsageView
      merged={merged}
      isPending={false}
      errorMessage={error ?? null}
      scope="Threefold"
      preferences={preferences}
      onPreferencesChange={setPreferences}
      onRefresh={async () => undefined}
    />
  );
}

export const usage: GalleryDoc = {
  meta: {
    name: "Usage",
    kind: "composition",
    group: "usage",
    source: "src/app/usage/UsagePage.tsx",
  },
  specimens: [
    { id: "daily", title: "30 days of usage", minHeight: 480, render: () => <UsageSpecimen /> },
    { id: "empty", title: "Empty window", render: () => <UsageSpecimen empty /> },
    {
      id: "error",
      title: "Refresh failed",
      render: () => <UsageSpecimen empty error="The usage report could not be loaded." />,
    },
  ],
};
