// Chat compositions: app sidebar, composer, timeline, markdown, command palette.
//
// Each renders the real component with synthetic data and the providers its
// own tests use (`tests/components/sidebarHarness.tsx`, `composer.test.tsx`,
// `timeline.test.tsx`). Nothing here is a copy of the component.
import {
  ArrowRightIcon,
  FolderIcon,
  MessageSquareIcon,
  PlusIcon,
  SettingsIcon,
  SquarePenIcon,
  TriangleAlertIcon,
} from "lucide-react";
import React from "react";

import { SidebarDataProvider, publishSidebarData, type SidebarShellData } from "../../../app/adapters/sidebarData.tsx";
import type { ComposerAttachments, StagedAttachment } from "../../../app/adapters/attachments.ts";
import { usePendingQuestion } from "../../../app/adapters/pendingQuestions.ts";
import { Composer, type ComposerProps } from "../../../app/components/Composer.tsx";
import { ComposerContextStrip } from "../../../app/components/ComposerContextStrip.tsx";
import { Markdown } from "../../../app/components/Markdown.tsx";
import { Timeline } from "../../../app/components/Timeline.tsx";
import { AppSidebarLayout } from "../../../components/AppSidebarLayout.tsx";
import {
  filterCommandPaletteGroups,
  type CommandPaletteActionItem,
  type CommandPaletteGroup,
} from "../../../components/CommandPalette.logic.ts";
import { CommandPaletteContent } from "../../../components/CommandPaletteContent.tsx";
import { CommandPaletteResults } from "../../../components/CommandPaletteResults.tsx";
import { Button } from "../../../components/ui/button.tsx";
import { CommandDialog, CommandDialogPopup, CommandDialogTrigger } from "../../../components/ui/command.tsx";
import { SidebarInset } from "../../../components/ui/sidebar.tsx";
import { DEFAULT_TURN_PREFERENCES, HUB_ENVIRONMENT_ID } from "../../../contracts/index.ts";
import { scopedThreadKey, scopeThreadRef } from "../../../environment/scoped.ts";
import { useUiStateStore } from "../../../uiStateStore.ts";

import {
  ASSISTANT_MARKDOWN,
  assistantMessage,
  conversation,
  detail,
  minutesAgo,
  question,
  statusStep,
  userMessage,
} from "../fixtures";
import { LONG_LABEL, type GalleryDoc } from "../specimen";

const noop = (): void => undefined;

// --- App sidebar ------------------------------------------------------------

const PROJECTS = [
  { id: "proj_detent", name: "detent", can_write: true },
  { id: "proj_cloud", name: "detent-cloud", can_write: true },
];

const RUNNING = conversation({
  id: "conv_ds_running",
  project_id: "proj_detent",
  title: "Lock renewal waits on a healthy handoff",
  execution: { ...conversation().execution, status: "running", updated_at: minutesAgo(4) },
  last_message_at: minutesAgo(2),
});

const SIDEBAR_CONVERSATIONS = [
  RUNNING,
  conversation({
    id: "conv_ds_needs_you",
    project_id: "proj_detent",
    title: "Which renewal window should the lock use?",
    work_item_id: "wi_ds_2",
    work_item: { id: "wi_ds_2", identifier: "detent#142", title: "Renewal window", lane: "In progress", runner_bound: true },
    execution: { ...RUNNING.execution, status: "waiting_input", updated_at: minutesAgo(6) },
    last_message_at: minutesAgo(6),
  }),
  conversation({
    id: "conv_ds_unread",
    project_id: "proj_detent",
    title: "Port the design-system gallery",
    work_item_id: null,
    work_item: null,
    execution: { ...RUNNING.execution, status: "idle", updated_at: minutesAgo(9) },
    last_message_at: minutesAgo(9),
  }),
  conversation({
    id: "conv_ds_active",
    project_id: "proj_detent",
    title: "Usage table rounding",
    work_item_id: null,
    work_item: null,
    execution: { ...RUNNING.execution, status: "idle", updated_at: minutesAgo(40) },
    last_message_at: minutesAgo(40),
  }),
  conversation({
    id: "conv_ds_long",
    project_id: "proj_cloud",
    title: LONG_LABEL,
    work_item_id: null,
    work_item: null,
    execution: { ...RUNNING.execution, status: "idle", updated_at: minutesAgo(300) },
    last_message_at: minutesAgo(300),
  }),
];

function sidebarData(): SidebarShellData {
  return {
    organizationName: "Threefold",
    projects: PROJECTS,
    activeProjectId: null,
    onProjectChange: noop,
    conversations: SIDEBAR_CONVERSATIONS,
    serverResults: [],
    activeConversationId: "conv_ds_active",
    onSelect: noop,
    onNewChat: noop,
    onRename: async () => undefined,
    attention: new Set(["conv_ds_needs_you"]),
    navigation: { activePath: "/chat", onNavigate: noop },
  };
}

function AppSidebarSpecimen() {
  const data = React.useMemo(sidebarData, []);
  // "Unread" is a completion newer than the reader's last visit; seed the
  // visit an hour back so the idle thread reads as unseen.
  React.useState(() => {
    const key = scopedThreadKey(scopeThreadRef(HUB_ENVIRONMENT_ID, "conv_ds_unread" as never));
    useUiStateStore.setState((state) => ({
      threadLastVisitedAtById: { ...state.threadLastVisitedAtById, [key]: minutesAgo(60) },
    }));
    publishSidebarData(data);
    return null;
  });
  return (
    <SidebarDataProvider value={data}>
      <AppSidebarLayout>
        <SidebarInset className="dc-main min-h-0 overflow-hidden">
          <div className="flex h-full items-center justify-center text-muted-foreground text-sm">
            Main content
          </div>
        </SidebarInset>
      </AppSidebarLayout>
    </SidebarDataProvider>
  );
}

export const appSidebar: GalleryDoc = {
  meta: {
    name: "App sidebar",
    kind: "composition",
    group: "navigation",
    source: "src/components/AppSidebarLayout.tsx",
  },
  specimens: [
    {
      id: "rows",
      title: "Projects and threads: running, needs you, unread, active, long title",
      note: "The real AppSidebarLayout and Sidebar, in a desktop-width frame (the sidebar is inline from md). At 390px it becomes the mobile sheet behind the toggle.",
      height: 640,
      minWidth: 1024,
      render: () => <AppSidebarSpecimen />,
    },
  ],
};

// --- Composer ---------------------------------------------------------------

function staged(overrides: Partial<StagedAttachment>): StagedAttachment {
  return {
    localId: "local_1",
    name: "trace.log",
    mime: "text/plain",
    size: 18_432,
    previewUrl: null,
    status: "ready",
    id: "att_1",
    error: null,
    ...overrides,
  };
}

// A synthetic screenshot. The attachments adapter creates an object URL for
// every image file, so a real image tile always has a preview.
const SCREENSHOT_PREVIEW = `data:image/svg+xml,${encodeURIComponent(
  "<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 64 64'>" +
    "<rect width='64' height='64' fill='#e4e4e7'/>" +
    "<rect x='6' y='8' width='52' height='8' rx='2' fill='#a1a1aa'/>" +
    "<rect x='6' y='22' width='34' height='5' rx='2' fill='#71717b'/>" +
    "<rect x='6' y='31' width='44' height='5' rx='2' fill='#71717b'/>" +
    "<rect x='6' y='40' width='26' height='5' rx='2' fill='#346bf1'/>" +
    "</svg>",
)}`;

const ATTACHMENTS: ComposerAttachments = {
  staged: [
    staged({ localId: "a", name: "lock-renewal.log", status: "ready" }),
    staged({ localId: "b", name: "screenshot.png", mime: "image/png", size: 248_120, previewUrl: SCREENSHOT_PREVIEW, status: "uploading", id: null }),
    staged({ localId: "c", name: "too-large.mov", mime: "video/quicktime", size: 98_000_000, status: "failed", id: null, error: "File is larger than 50 MB." }),
  ],
  addFiles: noop,
  remove: noop,
  retry: noop,
  clear: noop,
  ids: ["att_1"],
  uploadAll: async () => null,
  uploading: true,
  failed: true,
};

function ComposerSpecimen(props: Partial<ComposerProps> & { readonly initial?: string }) {
  const { initial, ...rest } = props;
  const [value, setValue] = React.useState(initial ?? "");
  const [preferences, setPreferences] = React.useState(DEFAULT_TURN_PREFERENCES);
  return (
    <div className="mx-auto w-full max-w-3xl">
      <Composer
        value={value}
        onChange={setValue}
        onSend={noop}
        onStop={noop}
        sending={false}
        streaming={false}
        preferences={preferences}
        onPreferencesChange={setPreferences}
        {...rest}
      />
    </div>
  );
}

function PendingQuestionComposer() {
  const pending = usePendingQuestion({
    question: question(),
    control: undefined,
    disabled: false,
    onAnswer: noop,
    onRetry: undefined,
    onDismiss: noop,
  });
  return <ComposerSpecimen pendingQuestion={pending} />;
}

export const composer: GalleryDoc = {
  meta: {
    name: "Composer",
    kind: "composition",
    group: "chat",
    source: "src/app/components/Composer.tsx",
  },
  specimens: [
    {
      id: "empty",
      title: "Empty",
      minHeight: 220,
      render: () => <ComposerSpecimen placeholder="Ask Detent to plan, build or explain…" />,
    },
    {
      id: "draft",
      title: "With a draft and the context strip",
      note: "Type `/` for the slash menu. The pickers in the footer are the real preference controls.",
      minHeight: 360,
      render: () => (
        <ComposerSpecimen
          initial="Make the lock renew at half the lease, and add a test for a two minute lease."
          contextStrip={<ComposerContextStrip projectName="detent" issueIdentifier="detent#142" issueLane="In progress" onOpenIssue={noop} />}
        />
      ),
    },
    {
      id: "attachments",
      title: "Attachments (ready, uploading, failed) and a banner",
      minHeight: 320,
      render: () => (
        <ComposerSpecimen
          initial="See the attached trace."
          attachments={ATTACHMENTS}
          attachmentErrors={["too-large.mov: File is larger than 50 MB."]}
          banners={[
            {
              id: "usage",
              variant: "warning",
              icon: <TriangleAlertIcon />,
              title: "Usage limit at 90%",
              description: "New runs pause at 100% until the window resets.",
            },
          ]}
        />
      ),
    },
    {
      id: "streaming",
      title: "Streaming: send becomes stop",
      minHeight: 200,
      render: () => <ComposerSpecimen streaming initial="Queued follow-up" />,
    },
    {
      id: "disabled",
      title: "Disabled with a blocked reason",
      minHeight: 200,
      render: () => (
        <ComposerSpecimen disabled blockedReason="You can read this conversation but not reply to it." />
      ),
    },
    {
      id: "pending-question",
      title: "Pending input: the agent asked a question",
      minHeight: 360,
      render: () => <PendingQuestionComposer />,
    },
  ],
};

// --- Timeline ---------------------------------------------------------------

const TIMELINE_DETAIL = detail({
  conversation: conversation({ id: "conv_ds_timeline" }),
  messages: [
    userMessage({ id: "msg_ds_u1", seq: 1, text: "Why does the checkout lock renewal flake under load?", created_at: minutesAgo(12) }),
    statusStep("msg_ds_s1", minutesAgo(11), "Read internal/lock/lease.go"),
    statusStep("msg_ds_s2", minutesAgo(10), "Ran go test ./internal/lock/..."),
    statusStep("msg_ds_s3", minutesAgo(9), "Edited internal/lock/lease.go"),
    assistantMessage({ id: "msg_ds_a1", seq: 5, text: ASSISTANT_MARKDOWN, created_at: minutesAgo(8) }),
    userMessage({ id: "msg_ds_u2", seq: 6, text: "Ship it, and open the pull request.", created_at: minutesAgo(1), delivery: "queued" }),
  ],
  pending: [
    {
      key: "pending_ds_1",
      text: "Also bump the changelog.",
      attachments: null,
      createdAt: new Date().toISOString(),
      messageId: null,
      expected: null,
      receiptStatus: null,
      status: "unknown",
      error: null,
      errorCode: null,
      retryable: false,
    },
  ],
});

export const timeline: GalleryDoc = {
  meta: {
    name: "Message timeline",
    kind: "composition",
    group: "chat",
    source: "src/app/components/Timeline.tsx",
  },
  specimens: [
    {
      id: "transcript",
      title: "User, work log, assistant markdown (code, table, alert), queued and pending sends",
      height: 760,
      render: () => (
        <div className="flex h-full min-h-0 flex-col">
          <Timeline detail={TIMELINE_DETAIL} onLoadOlder={noop} onRetry={noop} onDismiss={noop} />
        </div>
      ),
    },
    {
      id: "streaming",
      title: "Streaming reply",
      height: 320,
      render: () => (
        <div className="flex h-full min-h-0 flex-col">
          <Timeline
            detail={detail({
              conversation: conversation({ id: "conv_ds_stream" }),
              messages: [
                userMessage({ id: "msg_ds_su", text: "Summarise the failing test." }),
                assistantMessage({ id: "msg_ds_stream", text: "" }),
              ],
              deltas: { msg_ds_stream: { parts: { 1: "The test waits for a renewal ", 2: "that never comes because" } } },
            })}
            onLoadOlder={noop}
            onRetry={noop}
            onDismiss={noop}
          />
        </div>
      ),
    },
  ],
};

export const markdown: GalleryDoc = {
  meta: {
    name: "Chat markdown",
    kind: "composition",
    group: "chat",
    source: "src/app/components/Markdown.tsx",
  },
  specimens: [
    {
      id: "rich",
      title: "Lists, code, table, alert",
      note: "Code is highlighted with the frame's theme (`useTheme` reads the frame document).",
      render: () => (
        <div className="max-w-3xl">
          <Markdown source={ASSISTANT_MARKDOWN} />
        </div>
      ),
    },
  ],
};

// --- Command palette --------------------------------------------------------

function action(value: string, title: string, icon: React.ReactNode, description?: string): CommandPaletteActionItem {
  return {
    kind: "action",
    value,
    searchTerms: [title],
    title,
    icon,
    run: async () => undefined,
    ...(description === undefined ? {} : { description }),
  };
}

const PALETTE_GROUPS: CommandPaletteGroup[] = [
  {
    value: "actions",
    label: "Actions",
    items: [
      action("new-chat", "New chat", <SquarePenIcon />),
      action("new-issue", "New issue", <PlusIcon />),
      action("settings", "Open settings", <SettingsIcon />),
    ],
  },
  {
    value: "threads",
    label: "Recent threads",
    items: SIDEBAR_CONVERSATIONS.slice(0, 4).map((thread) =>
      action(`thread:${thread.id}`, thread.title, <MessageSquareIcon />, "detent"),
    ),
  },
  {
    value: "projects",
    label: "Projects",
    items: PROJECTS.map((project) => action(`project:${project.id}`, project.name, <FolderIcon />)),
  },
];

function PalettePanel() {
  const [query, setQuery] = React.useState("");
  const [highlighted, setHighlighted] = React.useState<string | null>(null);
  const groups = filterCommandPaletteGroups({
    activeGroups: PALETTE_GROUPS,
    query,
    isInSubmenu: false,
    projectSearchItems: [],
    threadSearchItems: [],
  });
  return (
    <CommandPaletteContent
      aria-label="Command palette"
      inputProps={{ placeholder: "Search commands, threads and projects" }}
      mode="none"
      value={query}
      onValueChange={(value) => setQuery(typeof value === "string" ? value : "")}
      onItemHighlighted={(value) => setHighlighted(typeof value === "string" ? value : null)}
      panelClassName="max-h-[min(28rem,70vh)]"
      footerActionLabel="Run"
    >
      <CommandPaletteResults
        groups={groups}
        highlightedItemValue={highlighted}
        isActionsOnly={query.startsWith(">")}
        keybindings={{ bindings: {} }}
        onExecuteItem={noop}
      />
    </CommandPaletteContent>
  );
}

export const commandPalette: GalleryDoc = {
  meta: {
    name: "Command palette",
    kind: "composition",
    group: "navigation",
    source: "src/components/CommandPaletteContent.tsx",
  },
  specimens: [
    {
      id: "open",
      title: "Open",
      note: "The palette as it opens: search, then actions, projects and threads in groups.",
      minHeight: 560,
      render: () => (
        <CommandDialog defaultOpen>
          <CommandDialogPopup
            aria-label="Command palette"
            className="overflow-hidden p-0"
            data-command-palette="true"
            initialFocus={false}
          >
            <PalettePanel />
          </CommandDialogPopup>
        </CommandDialog>
      ),
    },
    {
      id: "dialog",
      title: "In its dialog",
      note: "Type to filter (prefix `>` for actions only); arrows move, Escape closes.",
      minHeight: 560,
      render: () => (
        <CommandDialog>
          <CommandDialogTrigger render={<Button variant="outline" />}>
            Open command palette
            <ArrowRightIcon />
          </CommandDialogTrigger>
          <CommandDialogPopup aria-label="Command palette" className="overflow-hidden p-0" data-command-palette="true">
            <PalettePanel />
          </CommandDialogPopup>
        </CommandDialog>
      ),
    },
  ],
};
