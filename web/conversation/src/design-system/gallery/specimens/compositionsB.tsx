// Panels, settings, usage and work compositions.
//
// Each renders the real component with synthetic data, wrapped the way its own
// tests wrap it (`filesSurface.test.tsx`, `work.test.tsx`, `activity.test.tsx`,
// `settings.test.tsx`, `usage.test.tsx`). Clients are in-memory stand-ins that
// answer from fixtures; nothing here reaches the network. A component that
// can only exist against the live hub is listed with the reason instead.
import * as Schema from "effect/Schema";
import { FileTextIcon, PanelRightOpenIcon } from "lucide-react";
import React from "react";

import accountFixture from "../../../contracts/fixtures/account-bootstrap.json";
import changeFixture from "../../../contracts/fixtures/work-change-detail.json";
import historyFixture from "../../../contracts/fixtures/work-history.json";
import attemptsFixture from "../../../contracts/fixtures/work-attempt-list.json";
import attemptDiffFixture from "../../../contracts/fixtures/work-attempt-diff.json";
import commentsFixture from "../../../contracts/fixtures/work-comment-list.json";
import { AccountBootstrap } from "../../../contracts/account.ts";
import type {
  AttemptDiff,
  ChangeDetail,
  CollaborationEvent,
  NativeAttempt,
  NativeComment,
} from "../../../contracts/work.ts";
import type { ServerProviderUsageWindow } from "../../../contracts/ui.ts";
import { useTheme } from "../../../app/adapters/theme.ts";
import { ClientContext } from "../../../app/client.ts";
import type { ConversationClient } from "../../../runtime/bootstrap.ts";
import {
  RelayError,
  type ContentPayload,
  type FileEntry,
  type ListedPayload,
} from "../../../app/adapters/workspaceRelay.ts";
import type { FilesClient } from "../../../app/components/surfaces/FilesSurface.tsx";
import { FileSurface, FilesSurface } from "../../../app/components/surfaces/FilesSurface.tsx";
import { FileBreadcrumbs } from "../../../app/components/surfaces/FileBreadcrumbs.tsx";
import { FileBrowserPanel } from "../../../app/components/surfaces/FileBrowserPanel.tsx";
import { DiffSurface } from "../../../app/components/surfaces/DiffSurface.tsx";
import { TerminalSurface } from "../../../app/components/surfaces/TerminalSurface.tsx";
import { PullRequestSurface } from "../../../app/components/surfaces/PullRequestSurface.tsx";
import type { TerminalHandle } from "../../../app/adapters/terminalStream.ts";
import { RightPanelSheet } from "../../../components/RightPanelSheet.tsx";
import { RightPanelResizeHandle } from "../../../components/preview/RightPanelResizeHandle.tsx";
import { DiffPanelLoadingState, DiffPanelShell } from "../../../components/DiffPanelShell.tsx";
import { PreviewPanelShell } from "../../../components/preview/PreviewPanelShell.tsx";
import { useResizableWidth } from "../../../hooks/useResizableWidth.ts";
import { SettingsSidebarNav } from "../../../components/settings/SettingsSidebarNav.tsx";
import { SettingsHelp } from "../../../app/settings/SettingsHelp.tsx";
import { ContextHelp } from "../../../app/components/ContextHelp.tsx";
import { ExpandableText } from "../../../app/settings/ExpandableText.tsx";
import { RedactedSensitiveText } from "../../../components/settings/RedactedSensitiveText.tsx";
import { SettingsRow, SettingsSection } from "../../../app/settings/settingsLayout.tsx";
import { SidebarProvider } from "../../../components/ui/sidebar.tsx";
import { Button } from "../../../components/ui/button.tsx";
import { UsageProviderChart } from "../../../app/usage/UsageProviderChart.tsx";
import { providersWithUsage } from "../../../app/usage/usageProviders.ts";
import type { DailyTotals, MergedUsage } from "../../../app/usage/adapter.ts";
import { UsageLimitsSection } from "../../../app/usage/UsageLimits.tsx";
import { LimitWindows } from "../../../components/usage/UsageLimits.tsx";
import { WorkTopBar } from "../../../app/work/components/WorkTopBar.tsx";
import { WorkToolbar } from "../../../app/work/components/WorkToolbar.tsx";
import { WorkList } from "../../../app/work/components/WorkList.tsx";
import { IssueProperties } from "../../../app/work/components/IssueProperties.tsx";
import { PropertyPicker } from "../../../app/work/components/IssuePickers.tsx";
import { ActivityFeed, LiveRow } from "../../../app/work/components/ActivityFeed.tsx";
import { IssueComposer } from "../../../app/work/components/IssueComposer.tsx";
import { mergeActivity } from "../../../app/work/lib/activity.ts";
import { DEFAULT_VIEW_STATE, type WorkViewState } from "../../../app/work/lib/viewState.ts";
import type { ConnectionChip } from "../../../app/App.tsx";

import { LANES, NOW, runningAttempt, usageDaily, usageEmpty, workItem } from "../fixtures";
import { LONG_LABEL, type GalleryDoc } from "../specimen";

const noop = (): void => undefined;

const CHANGE = changeFixture as unknown as ChangeDetail;
const HISTORY = (historyFixture as { items: unknown[] }).items as unknown as CollaborationEvent[];
const ATTEMPTS = (attemptsFixture as { items: unknown[] }).items as unknown as NativeAttempt[];
const ATTEMPT_DIFF = attemptDiffFixture as unknown as AttemptDiff;
const COMMENTS = (commentsFixture as { items: unknown[] }).items as unknown as NativeComment[];

/** The frame's theme, as the surfaces that take it as a prop want it. */
function useFrameTheme(): "light" | "dark" {
  return useTheme().resolvedTheme;
}

// --- In-memory workspace files ---------------------------------------------

function entry(name: string, kind: FileEntry["kind"] = "file", size = 1_024): FileEntry {
  return { name, kind, size, modified_at: "2026-09-09T11:00:00Z", ignored: false, denied: false };
}

const LISTINGS: Record<string, readonly FileEntry[]> = {
  "": [entry("internal", "dir"), entry("web", "dir"), entry("README.md"), entry("go.mod")],
  internal: [entry("lock", "dir"), entry("orchestrator", "dir")],
  "internal/lock": [entry("lease.ts"), entry("lease.test.ts")],
  "internal/orchestrator": [entry("ranking.ts")],
  web: [entry("conversation", "dir")],
  "web/conversation": [entry("package.json")],
};

const READS: Record<string, string> = {
  "README.md": "# Detent\n\nOrchestrates coding agents across runners.\n",
  "internal/lock/lease.ts": [
    "// renewLease renews at half the lease duration.",
    "export async function renewLease(lock: Lock): Promise<void> {",
    "  const interval = lock.leaseMs / 2;",
    "  await lock.store.renew(lock.key, interval);",
    "}",
  ].join("\n"),
};

/** A stand-in for the workspace relay that answers from the tables above. */
function memoryFiles(): FilesClient {
  return {
    list: (path) => {
      const entries = LISTINGS[path];
      if (entries === undefined) return Promise.reject(new RelayError("not_found"));
      return Promise.resolve({ path, entries } satisfies ListedPayload);
    },
    read: (path) => {
      const data = READS[path];
      if (data === undefined) return Promise.reject(new RelayError("not_found"));
      return Promise.resolve({
        path,
        mime: path.endsWith(".md") ? "text/markdown" : "text/x-go",
        size: data.length,
        offset: 0,
        data,
        truncated: false,
      } satisfies ContentPayload);
    },
  };
}

/**
 * Opens a help control at mount by clicking its trigger, for components that
 * own their open state and take no `defaultOpen`. `.click()` opens without
 * moving focus to the trigger.
 */
function OpenOnMount({ trigger, children }: { readonly trigger: string; readonly children: React.ReactNode }) {
  const host = React.useRef<HTMLDivElement>(null);
  React.useEffect(() => {
    host.current?.querySelector<HTMLElement>(trigger)?.click();
  }, [trigger]);
  return (
    <div ref={host} className="contents">
      {children}
    </div>
  );
}

// --- Panels -----------------------------------------------------------------

function SheetSpecimen({ initialOpen = false }: { readonly initialOpen?: boolean }) {
  const [open, setOpen] = React.useState(initialOpen);
  return (
    <>
      <Button variant="outline" onClick={() => setOpen(true)}>
        <PanelRightOpenIcon />
        Open right panel
      </Button>
      <RightPanelSheet open={open} animationDurationMs={200} label="Right panel: Diff" onClose={() => setOpen(false)}>
        <PullRequestSurface
          pullRequest={{
            number: 3363,
            repository: "digitaldrywood/detent",
            url: "#specimen",
            state: "open",
            isDraft: false,
            title: "Lock renewal waits on a healthy handoff",
          }}
        />
      </RightPanelSheet>
    </>
  );
}

function ResizeHandleSpecimen() {
  const { width, handlers } = useResizableWidth({
    storageKey: "detent:design-system:resize-handle-specimen",
    defaultWidth: 280,
    minWidth: 180,
    maxWidth: 520,
    edge: "left",
  });
  return (
    <div className="flex h-full justify-end">
      <div className="relative h-full border-l border-border bg-card" style={{ width }}>
        <RightPanelResizeHandle handlers={handlers} />
        <p className="p-4 text-muted-foreground text-sm">Drag the left edge. Width: {Math.round(width)}px</p>
      </div>
    </div>
  );
}

function DiffSurfaceSpecimen({ kind }: { readonly kind: "version" | "attempt" | "unavailable" }) {
  const theme = useFrameTheme();
  return (
    <div className="flex h-full min-h-0 flex-col">
      <DiffSurface
        change={kind === "unavailable" ? null : CHANGE}
        source={
          kind === "attempt"
            ? { kind: "attempt", diff: ATTEMPT_DIFF }
            : kind === "version"
              ? { kind: "version" }
              : { kind: "unavailable", reason: "No attempt has posted a diff for this issue yet." }
        }
        attempts={ATTEMPTS}
        history={HISTORY}
        now={NOW}
        theme={theme}
        onReload={noop}
      />
    </div>
  );
}

function FilesSurfaceSpecimen({ unavailable }: { readonly unavailable: boolean }) {
  const theme = useFrameTheme();
  const files = React.useMemo(memoryFiles, []);
  return (
    <div className="flex h-full min-h-0 flex-col">
      <FilesSurface
        state={unavailable ? "failed" : "ready"}
        reason={unavailable ? "checkout_failed" : null}
        error={null}
        loading={false}
        files={unavailable ? null : files}
        onRetry={noop}
        projectName="detent"
        theme={theme}
      />
    </div>
  );
}

function FileSurfaceSpecimen({ path }: { readonly path: string }) {
  const theme = useFrameTheme();
  const files = React.useMemo(memoryFiles, []);
  return (
    <div className="flex h-full min-h-0 flex-col">
      <FileSurface path={path} files={files} projectName="detent" theme={theme} />
    </div>
  );
}

function BreadcrumbsSpecimen() {
  const theme = useFrameTheme();
  const files = React.useMemo(memoryFiles, []);
  return (
    <FileBreadcrumbs
      files={files}
      projectName="detent"
      relativePath="internal/lock/lease.ts"
      theme={theme}
      onOpenFile={noop}
    />
  );
}

function BrowserPanelSpecimen() {
  const theme = useFrameTheme();
  const files = React.useMemo(memoryFiles, []);
  // The file opens after the tree's first listing, the way a breadcrumb or a
  // chat chip opens one. Opening it at mount overlaps the reveal with the root
  // listing, and under StrictMode's double effect both list `internal` and add
  // its rows twice ("Path already exists").
  const [selected, setSelected] = React.useState<string | null>(null);
  React.useEffect(() => {
    const timer = window.setTimeout(() => setSelected("internal/lock/lease.ts"), 0);
    return () => window.clearTimeout(timer);
  }, []);
  return (
    <div className="flex h-full w-72 max-w-full min-h-0 flex-col border-r border-border">
      <FileBrowserPanel
        files={files}
        projectName="detent"
        selectedPath={selected}
        onOpenFile={(_entry, path) => setSelected(path)}
        theme={theme}
      />
    </div>
  );
}

const TERMINAL_OUTPUT = [
  "\x1b[32m~/detent\x1b[0m $ go test ./internal/lock/...\r\n",
  "ok  \tgithub.com/digitaldrywood/detent/internal/lock\t0.412s\r\n",
  "\x1b[32m~/detent\x1b[0m $ ",
].join("");

function TerminalSpecimen({ state }: { readonly state: "live" | "empty" | "unavailable" }) {
  const terminals: readonly TerminalHandle[] =
    state === "live"
      ? [
          {
            id: "term_1",
            snapshot: { status: "open", pid: 4242, isolation: "worktree", exitCode: null, signal: null, error: null },
            stream: null,
          },
          {
            id: "term_2",
            snapshot: { status: "exited", pid: 4243, isolation: "worktree", exitCode: 1, signal: null, error: null },
            stream: null,
          },
        ]
      : [];
  const registerOutput = React.useCallback((_id: string, sink: (data: string) => void) => {
    sink(TERMINAL_OUTPUT);
    return noop;
  }, []);
  return (
    <div className="flex h-full min-h-0 flex-col">
      <TerminalSurface
        terminals={terminals}
        activeId={terminals[0]?.id ?? null}
        onSelect={noop}
        onNewTerminal={noop}
        onCloseTerminal={noop}
        registerOutput={registerOutput}
        state={state === "unavailable" ? "failed" : "ready"}
        reason={state === "unavailable" ? "no_runner" : null}
        error={null}
        loading={false}
        onRetry={noop}
      />
    </div>
  );
}

const panels: Record<string, GalleryDoc> = {
  "right-panel-sheet": {
    meta: { name: "Right panel sheet", kind: "composition", group: "Panels", source: "src/components/RightPanelSheet.tsx" },
    specimens: [
      {
        id: "open-default",
        title: "Open: sheet holding a surface",
        note: "Rendered open. Escape or the backdrop closes it; the button reopens it.",
        minHeight: 480,
        render: () => <SheetSpecimen initialOpen />,
      },
      {
        id: "open",
        title: "Sheet holding a surface",
        note: "The narrow-viewport home of the right panel. Escape or the backdrop closes it.",
        minHeight: 160,
        render: () => <SheetSpecimen />,
      },
    ],
  },
  "right-panel-resize-handle": {
    meta: {
      name: "Right panel resize handle",
      kind: "composition",
      group: "Panels",
      source: "src/components/preview/RightPanelResizeHandle.tsx",
    },
    specimens: [
      {
        id: "drag",
        title: "Left-edge handle on a panel",
        note: "The line lights on hover and while dragging; width is clamped and remembered.",
        height: 220,
        render: () => <ResizeHandleSpecimen />,
      },
    ],
  },
  "diff-panel-shell": {
    meta: { name: "Diff panel shell", kind: "composition", group: "Panels", source: "src/components/DiffPanelShell.tsx" },
    specimens: [
      {
        id: "loading",
        title: "Embedded shell with header and the loading skeleton",
        height: 320,
        render: () => (
          <DiffPanelShell
            mode="embedded"
            header={
              <span className="flex items-center gap-2 text-sm">
                <FileTextIcon className="size-4 text-muted-foreground" />
                internal/lock/lease.ts
              </span>
            }
          >
            <DiffPanelLoadingState label="Loading diff" />
          </DiffPanelShell>
        ),
      },
    ],
  },
  "preview-panel-shell": {
    meta: {
      name: "Preview panel shell",
      kind: "composition",
      group: "Panels",
      source: "src/components/preview/PreviewPanelShell.tsx",
    },
    specimens: [
      {
        id: "embedded",
        title: "Embedded panel",
        height: 260,
        render: () => (
          <PreviewPanelShell mode="embedded">
            <div className="flex h-full items-center justify-center text-muted-foreground text-sm">Panel content</div>
          </PreviewPanelShell>
        ),
      },
    ],
  },
  "diff-surface": {
    meta: { name: "Diff surface", kind: "surface", group: "Panels", source: "src/app/components/surfaces/DiffSurface.tsx" },
    specimens: [
      { id: "version", title: "Change version card with activity", height: 520, render: () => <DiffSurfaceSpecimen kind="version" /> },
      { id: "attempt", title: "Attempt diff (files and hunks)", height: 560, render: () => <DiffSurfaceSpecimen kind="attempt" /> },
      { id: "unavailable", title: "Unavailable: no diff yet", height: 200, render: () => <DiffSurfaceSpecimen kind="unavailable" /> },
    ],
  },
  "files-surface": {
    meta: { name: "Files surface", kind: "surface", group: "Panels", source: "src/app/components/surfaces/FilesSurface.tsx" },
    specimens: [
      { id: "tree", title: "Workspace tree", height: 420, render: () => <FilesSurfaceSpecimen unavailable={false} /> },
      { id: "file", title: "One open file with breadcrumbs", height: 420, render: () => <FileSurfaceSpecimen path="internal/lock/lease.ts" /> },
      { id: "missing", title: "File the runner cannot find", height: 240, render: () => <FileSurfaceSpecimen path="internal/gone.ts" /> },
      { id: "unavailable", title: "Workspace failed: checkout failed", height: 240, render: () => <FilesSurfaceSpecimen unavailable /> },
    ],
  },
  "file-breadcrumbs": {
    meta: {
      name: "File breadcrumbs",
      kind: "composition",
      group: "Panels",
      source: "src/app/components/surfaces/FileBreadcrumbs.tsx",
    },
    specimens: [
      {
        id: "path",
        title: "Project, directories, file",
        note: "Each directory crumb opens a menu of its siblings.",
        minHeight: 260,
        render: () => <BreadcrumbsSpecimen />,
      },
    ],
  },
  "file-browser-panel": {
    meta: {
      name: "File browser panel",
      kind: "composition",
      group: "Panels",
      source: "src/app/components/surfaces/FileBrowserPanel.tsx",
    },
    specimens: [
      { id: "tree", title: "Tree revealed on the open file", height: 420, render: () => <BrowserPanelSpecimen /> },
    ],
  },
  "terminal-surface": {
    meta: {
      name: "Terminal surface",
      kind: "surface",
      group: "Panels",
      source: "src/app/components/surfaces/TerminalSurface.tsx",
    },
    specimens: [
      {
        id: "live",
        title: "Two terminals, synthetic output",
        note: "No runner is attached: the output is written straight into the terminal view.",
        height: 360,
        render: () => <TerminalSpecimen state="live" />,
      },
      { id: "empty", title: "No terminal open", height: 200, render: () => <TerminalSpecimen state="empty" /> },
      { id: "unavailable", title: "Unavailable: no runner", height: 200, render: () => <TerminalSpecimen state="unavailable" /> },
    ],
  },
  "pull-request-surface": {
    meta: {
      name: "Pull request surface",
      kind: "surface",
      group: "Panels",
      source: "src/app/components/surfaces/PullRequestSurface.tsx",
    },
    specimens: [
      {
        id: "states",
        title: "Open, draft, merged, closed, none",
        render: () => (
          <div className="grid gap-3 sm:grid-cols-2">
            {(
              [
                ["open", false],
                ["open", true],
                ["merged", false],
                ["closed", false],
              ] as const
            ).map(([state, isDraft]) => (
              <div key={`${state}-${isDraft}`} className="flex rounded-lg border border-border">
                <PullRequestSurface
                  pullRequest={{
                    number: 3363,
                    repository: "digitaldrywood/detent",
                    url: "#specimen",
                    state,
                    isDraft,
                    title: state === "closed" ? LONG_LABEL : "Lock renewal waits on a healthy handoff",
                  }}
                />
              </div>
            ))}
            <div className="flex h-32 rounded-lg border border-border">
              <PullRequestSurface pullRequest={null} />
            </div>
          </div>
        ),
      },
    ],
  },
};

// --- Settings ---------------------------------------------------------------

/** What `settings.test.tsx` mounts: the owner account fixture, no requests at render. */
const OWNER_CLIENT = {
  http: { origin: "", apiBase: accountFixture.api_base, csrfToken: accountFixture.csrf_token },
  account: Schema.decodeUnknownSync(AccountBootstrap)(accountFixture),
} as unknown as ConversationClient;

const settings: Record<string, GalleryDoc> = {
  "settings-sidebar-nav": {
    meta: {
      name: "Settings sidebar nav",
      kind: "composition",
      group: "Settings",
      source: "src/components/settings/SettingsSidebarNav.tsx",
    },
    specimens: [
      {
        id: "owner",
        title: "Owner's sections with search",
        note: "Type in the search box to filter settings across sections.",
        height: 520,
        render: () => (
          <ClientContext.Provider value={OWNER_CLIENT}>
            <SidebarProvider className="h-full! min-h-0!">
              <div className="flex h-full w-64 flex-col border-r border-border bg-sidebar" data-app-sidebar="">
                <SettingsSidebarNav pathname="/settings/general" />
              </div>
            </SidebarProvider>
          </ClientContext.Provider>
        ),
      },
    ],
  },
  "settings-help": {
    meta: { name: "Settings help", kind: "composition", group: "Settings", source: "src/app/settings/SettingsHelp.tsx" },
    specimens: [
      {
        id: "open",
        title: "Open: help popover beside a setting",
        note: "Opened at mount by clicking the trigger (the component takes no `defaultOpen`). A click outside closes it.",
        minHeight: 240,
        render: () => (
          <OpenOnMount trigger='[aria-label="About Spend limit"]'>
            <div className="flex items-center gap-1 pt-24 text-sm">
              Spend limit
              <SettingsHelp label="Spend limit">
                New runs pause once the organization reaches this amount in the billing window. Running attempts finish.
              </SettingsHelp>
            </div>
          </OpenOnMount>
        ),
      },
      {
        id: "hover",
        title: "Help popover beside a setting",
        note: "Opens on hover after a short delay, or on click.",
        minHeight: 240,
        render: () => (
          <div className="flex items-center gap-1 pt-24 text-sm">
            Spend limit
            <SettingsHelp label="Spend limit">
              New runs pause once the organization reaches this amount in the billing window. Running attempts finish.
            </SettingsHelp>
          </div>
        ),
      },
    ],
  },
  "context-help": {
    meta: { name: "Context help", kind: "composition", group: "Settings", source: "src/app/components/ContextHelp.tsx" },
    specimens: [
      {
        id: "open",
        title: "Open: pinned help",
        note: "Pinned at mount by clicking the trigger (the component takes no `defaultOpen`). A click outside or Escape unpins it.",
        minHeight: 240,
        render: () => (
          <OpenOnMount trigger='[aria-label="Help for Runner token"]'>
            <div className="flex items-center gap-1 pt-24 text-sm">
              Runner token
              <ContextHelp label="Runner token">
                The token a runner uses to enroll. It is shown once; enroll a new runner to get another.
              </ContextHelp>
            </div>
          </OpenOnMount>
        ),
      },
      {
        id: "inline",
        title: "Hover preview, click to pin",
        minHeight: 240,
        render: () => (
          <div className="flex items-center gap-1 pt-24 text-sm">
            Runner token
            <ContextHelp label="Runner token">
              The token a runner uses to enroll. It is shown once; enroll a new runner to get another.
            </ContextHelp>
          </div>
        ),
      },
    ],
  },
  "expandable-text": {
    meta: { name: "Expandable text", kind: "composition", group: "Settings", source: "src/app/settings/ExpandableText.tsx" },
    specimens: [
      {
        id: "states",
        title: "Short (no control) and long (clamped, expandable)",
        render: () => (
          <div className="flex max-w-xl flex-col gap-4 text-sm">
            <ExpandableText text="Checkout failed: branch not found." />
            <ExpandableText
              text={`git fetch origin feat/design-system failed with exit status 128.\n${LONG_LABEL}. ${LONG_LABEL}. fatal: could not read Username for 'https://github.com': terminal prompts disabled.`}
            />
          </div>
        ),
      },
    ],
  },
  "redacted-sensitive-text": {
    meta: {
      name: "Redacted sensitive text",
      kind: "composition",
      group: "Settings",
      source: "src/components/settings/RedactedSensitiveText.tsx",
    },
    specimens: [
      {
        id: "secret",
        title: "Hidden until revealed",
        note: "Click to reveal; click again to hide.",
        minHeight: 140,
        render: () => (
          <div className="pt-10">
            <SettingsSection title="Runner">
              <SettingsRow
                title="Enrollment token"
                control={
                  <RedactedSensitiveText
                    value="dtr_live_8f2c41a0b7d84e6fa1c35d92"
                    ariaLabel="Enrollment token"
                    revealTooltip="Reveal token"
                    hideTooltip="Hide token"
                  />
                }
              />
            </SettingsSection>
          </div>
        ),
      },
    ],
  },
};

// --- Usage ------------------------------------------------------------------

/**
 * An empty report with one zero row per day of its window. Neither the chart
 * nor the usage page has a no-data message of its own: with no rows the plot
 * is bare, so the specimen shows the quiet window the way a report with
 * zero-usage days draws it — dated axis, flat baseline, `0` scale.
 */
function quietWindow(): MergedUsage {
  const empty = usageEmpty();
  const start = Date.parse(empty.from);
  const end = Date.parse(empty.to);
  const daily: DailyTotals[] = [];
  for (let time = start; time < end; time += 86_400_000) {
    daily.push({ day: new Date(time).toISOString().slice(0, 10), costUsd: 0, totalTokens: 0, byProvider: new Map() });
  }
  return { ...empty, daily };
}

function ChartSpecimen({ metric, empty }: { readonly metric: "cost" | "tokens"; readonly empty?: boolean }) {
  const merged = React.useMemo(() => (empty === true ? quietWindow() : usageDaily()), [empty]);
  return (
    <UsageProviderChart
      providers={providersWithUsage(merged.providers)}
      days={merged.daily.map((day) => day.day)}
      daily={merged.daily}
      hours={merged.hourly.map((hour) => hour.hourStart)}
      hourly={merged.hourly}
      metric={metric}
      referenceTime={merged.to === "" ? undefined : merged.to}
      resolution="day"
      timeZone="UTC"
    />
  );
}

const WINDOWS: ServerProviderUsageWindow[] = [
  { id: "session", kind: "session", label: "5-hour session", usedPercent: 38, resetsAt: new Date(NOW + 2 * 3_600_000).toISOString(), windowDurationMins: 300 },
  { id: "weekly", kind: "weekly", label: "Weekly", usedPercent: 91, resetsAt: new Date(NOW + 2 * 86_400_000).toISOString(), windowDurationMins: 10_080 },
  { id: "monthly", kind: "monthly", label: LONG_LABEL, usedPercent: 4 },
];

const usage: Record<string, GalleryDoc> = {
  "usage-provider-chart": {
    meta: { name: "Usage provider chart", kind: "composition", group: "Usage", source: "src/app/usage/UsageProviderChart.tsx" },
    specimens: [
      { id: "cost", title: "Daily cost by provider", note: "Hover the plot for the day's readout.", render: () => <ChartSpecimen metric="cost" /> },
      { id: "tokens", title: "Daily tokens", render: () => <ChartSpecimen metric="tokens" /> },
      {
        id: "empty",
        title: "Quiet window: every day at zero",
        note: "The chart has no no-data message; a window with no usage draws a flat baseline under its dates.",
        render: () => <ChartSpecimen metric="cost" empty />,
      },
    ],
  },
  "usage-limits": {
    meta: { name: "Usage limit windows", kind: "composition", group: "Usage", source: "src/components/usage/UsageLimits.tsx" },
    specimens: [
      {
        id: "windows",
        title: "Session, weekly (near the limit) and monthly windows",
        render: () => (
          <div className="flex max-w-2xl flex-col gap-6">
            <LimitWindows driver={"codex" as never} windows={WINDOWS} now={NOW} />
            <div className="w-80 max-w-full">
              <LimitWindows driver={"claudeAgent" as never} windows={WINDOWS} now={NOW} compact />
            </div>
          </div>
        ),
      },
    ],
  },
  "usage-limits-section": {
    meta: { name: "Usage limits section", kind: "composition", group: "Usage", source: "src/app/usage/UsageLimits.tsx" },
    specimens: [
      {
        id: "limits",
        title: "Allowances, one over its limit",
        render: () => (
          <UsageLimitsSection
            limits={[
              { name: "runs", used: 412, limit: 1_000, overLimit: false },
              { name: "api_mutations", used: 9_870, limit: 10_000, overLimit: false },
              { name: "runner_minutes", used: 6_400, limit: 6_000, overLimit: true },
            ]}
          />
        ),
      },
      { id: "empty", title: "No allowances reported", render: () => <UsageLimitsSection limits={[]} /> },
    ],
  },
};

// --- Work -------------------------------------------------------------------

const CONNECTED: ConnectionChip = { tone: "dc-ok", label: "Live", detail: null, tooltip: "Receiving updates" };
const STALE: ConnectionChip = {
  tone: "dc-warn",
  label: "Reconnecting",
  detail: "as of 2m ago",
  action: { label: "Retry", onClick: noop },
  tooltip: "The board may be out of date",
};

function ToolbarSpecimen() {
  const [view, setView] = React.useState<WorkViewState>(DEFAULT_VIEW_STATE);
  return (
    <WorkToolbar
      view={view}
      onChange={setView}
      lanes={LANES}
      facets={{
        state: LANES.map((lane) => lane.name),
        label: ["ui", "design-system", "bug"],
        assignee: ["michael@example.com", "agent"],
        priority: ["Urgent", "High", "Medium", "Low"],
      }}
    />
  );
}

const LIST_ITEMS = [
  workItem({ id: "wi_ds_l1", identifier: "detent#142", title: "Port the design-system gallery", attempt: runningAttempt() }),
  workItem({ id: "wi_ds_l2", identifier: "detent#139", title: LONG_LABEL, labels: ["ui", "design-system"] }),
  workItem({ id: "wi_ds_l3", identifier: "detent#131", title: "Usage table rounding", blockedBy: ["detent#128"] }),
  workItem({ id: "wi_ds_l4", identifier: "detent#120", title: "Retire the old dashboard", state: "Done", terminal: true }),
];

function PropertiesSpecimen({ canWrite }: { readonly canWrite: boolean }) {
  const [item, setItem] = React.useState(() =>
    workItem({ labels: ["ui", "design-system"], priority: "High", attempt: runningAttempt() }),
  );
  return (
    <div className="w-72 max-w-full">
      <IssueProperties
        item={item}
        states={LANES.map((lane) => ({ name: lane.name, category: lane.category }))}
        moves={LANES.map((lane) => lane.name)}
        laneCategory="started"
        onMove={(state) => setItem((current) => ({ ...current, state }))}
        onPriority={(priority) => setItem((current) => ({ ...current, priority }))}
        onLabels={(labels) => setItem((current) => ({ ...current, labels }))}
        labelCatalogue={[
          { name: "ui", color: "#6366f1", count: 12 },
          { name: "design-system", color: "#10b981", count: 4 },
          { name: "bug", color: "#ef4444", count: 31 },
        ]}
        onAssignees={noop}
        members={[
          { id: "michael@example.com", label: "Michael" },
          { id: "dana@example.com", label: "Dana" },
        ]}
        viewer={{ id: "michael@example.com", label: "Michael" }}
        onInvite={canWrite ? noop : null}
        inviteReason="Only owners can invite."
        canWrite={canWrite}
        saving={false}
        runner="Mac Studio"
        effort="high · gpt-6-sol"
        attempts="2 attempts"
        related={[
          { key: "r1", kind: "issue", label: "detent#128 Usage report", category: "completed", onOpen: noop, onRemove: canWrite ? noop : null },
          { key: "r2", kind: "chat", label: "Flaky checkout lock renewal", detail: "chat", onOpen: noop },
        ]}
        relatedCandidates={[{ id: "wi_ds_c1", label: "detent#131 Usage table rounding", detail: "Todo", category: "unstarted" }]}
        onAddRelated={noop}
        onOpenPullRequest={noop}
        onOpenChangeRequest={noop}
      />
    </div>
  );
}

function PickerSpecimen({ initialOpen = false }: { readonly initialOpen?: boolean }) {
  const [open, setOpen] = React.useState(initialOpen);
  const [query, setQuery] = React.useState("");
  const [picked, setPicked] = React.useState("High");
  const rows = ["Urgent", "High", "Medium", "Low"]
    .filter((name) => name.toLowerCase().includes(query.toLowerCase()))
    .map((name, index) => ({
      key: name,
      label: name,
      digit: String(index + 1),
      selected: name === picked,
      onSelect: () => {
        setPicked(name);
        setOpen(false);
      },
    }));
  return (
    <PropertyPicker
      open={open}
      onOpenChange={setOpen}
      trigger={<span>Priority: {picked}</span>}
      placeholder="Set priority…"
      hint="Type a number to pick"
      query={query}
      onQuery={setQuery}
      groups={[{ label: "Priority", rows }]}
      empty="No priority matches."
      label="Priority"
      testId="gallery-priority-picker"
    />
  );
}

const ACTIVITY_ROWS = mergeActivity({
  history: HISTORY,
  attempts: ATTEMPTS,
  comments: COMMENTS,
  conversation: null,
  viewerPrincipalId: "tok_7b21",
});

const work: Record<string, GalleryDoc> = {
  "work-top-bar": {
    meta: { name: "Work top bar", kind: "composition", group: "Work", source: "src/app/work/components/WorkTopBar.tsx" },
    specimens: [
      {
        id: "states",
        title: "Live, and reconnecting with an action",
        render: () => (
          <div className="flex flex-col gap-4">
            <WorkTopBar context="Work" title="detent" meta="24 issues" connection={CONNECTED} />
            <WorkTopBar
              context="detent"
              title={LONG_LABEL}
              identifier="#3363"
              connection={STALE}
              showConnectionDetailOnMobile
              actions={<Button size="xs">New issue</Button>}
            />
          </div>
        ),
      },
    ],
  },
  "work-toolbar": {
    meta: { name: "Work toolbar", kind: "composition", group: "Work", source: "src/app/work/components/WorkToolbar.tsx" },
    specimens: [
      {
        id: "filters",
        title: "Archived toggle, filters, lanes, sort, search",
        note: "Each filter opens a picker; the state lives in the query string in the app.",
        minHeight: 360,
        render: () => <ToolbarSpecimen />,
      },
    ],
  },
  "work-list": {
    meta: { name: "Work list", kind: "composition", group: "Work", source: "src/app/work/components/WorkList.tsx" },
    specimens: [
      {
        id: "rows",
        title: "Running, labelled, blocked and done rows",
        note: "Arrow keys move between rows.",
        render: () => (
          <WorkList items={LIST_ITEMS} showProject now={NOW} onOpen={noop} movesFor={() => LANES.map((lane) => lane.name)} onMove={noop} />
        ),
      },
      {
        id: "empty",
        title: "Empty",
        render: () => <WorkList items={[]} showProject={false} now={NOW} onOpen={noop} movesFor={() => []} onMove={noop} />,
      },
    ],
  },
  "issue-properties": {
    meta: { name: "Issue properties", kind: "composition", group: "Work", source: "src/app/work/components/IssueProperties.tsx" },
    specimens: [
      {
        id: "writer",
        title: "Editable (status, priority, assignee, labels, related)",
        note: "Each property opens its picker; edits apply to the specimen's own state.",
        minHeight: 560,
        render: () => <PropertiesSpecimen canWrite />,
      },
      { id: "reader", title: "Read-only reader", minHeight: 480, render: () => <PropertiesSpecimen canWrite={false} /> },
    ],
  },
  "property-picker": {
    meta: { name: "Property picker", kind: "composition", group: "Work", source: "src/app/work/components/IssuePickers.tsx" },
    specimens: [
      {
        id: "open",
        title: "Open: searchable picker",
        note: "Rendered open. The picker focuses its search field when it opens, so this frame takes focus on load. Type to filter or press a digit.",
        minHeight: 320,
        render: () => <PickerSpecimen initialOpen />,
      },
      {
        id: "priority",
        title: "Searchable picker with digit shortcuts",
        note: "Click the trigger, type to filter, or press a digit.",
        minHeight: 120,
        render: () => <PickerSpecimen />,
      },
    ],
  },
  "activity-feed": {
    meta: { name: "Activity feed", kind: "composition", group: "Work", source: "src/app/work/components/ActivityFeed.tsx" },
    specimens: [
      {
        id: "feed",
        title: "History, attempts and comments with the live row",
        render: () => (
          <div className="max-w-2xl">
            <ActivityFeed
              rows={ACTIVITY_ROWS}
              live={
                <LiveRow
                  running
                  sentence="Changed only main.go."
                  detail="Mac Studio · gpt-6-sol · attempt 2"
                  elapsed="1m 08s"
                  onInterrupt={noop}
                  onOpenConversation={noop}
                />
              }
              liveAt={Number.MAX_SAFE_INTEGER}
              onReply={async () => undefined}
              posting={false}
            />
          </div>
        ),
      },
      {
        id: "reader",
        title: "Read-only, nothing running",
        render: () => (
          <div className="max-w-2xl">
            <ActivityFeed
              rows={ACTIVITY_ROWS.slice(0, 3)}
              live={
                <LiveRow
                  running={false}
                  sentence="The issue is queued. Nothing is running yet."
                  detail=""
                  elapsed=""
                  onInterrupt={null}
                  onOpenConversation={noop}
                />
              }
              liveAt={Number.MAX_SAFE_INTEGER}
              onReply={null}
              posting={false}
            />
          </div>
        ),
      },
    ],
  },
  "issue-composer": {
    meta: { name: "Issue composer", kind: "composition", group: "Work", source: "src/app/work/components/IssueComposer.tsx" },
    specimens: [
      {
        id: "writer",
        title: "Comment composer",
        note: "Type `/` for its slash commands.",
        minHeight: 260,
        render: () => <IssueComposer canWrite onComment={async () => undefined} />,
      },
      { id: "reader", title: "Unavailable to a reader", render: () => <IssueComposer canWrite={false} onComment={async () => undefined} /> },
    ],
  },
};

export const COMPOSITIONS_B: Readonly<Record<string, GalleryDoc>> = {
  ...panels,
  ...settings,
  ...usage,
  ...work,
};
