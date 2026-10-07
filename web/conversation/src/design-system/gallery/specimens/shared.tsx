// Shared components from `src/components`: banners, chips, media, icons,
// header controls and the small structural helpers features compose.
//
// Each renders the real component with synthetic data. Images are inline SVG
// data URLs, so nothing here reaches the network; hub-backed lookups (pull
// request details, media URLs) answer from the client's seeded state.
import { Check, Copy, PanelLeft, PanelLeftClose, Pause, Play } from "lucide";
import { CircleAlertIcon, ExternalLinkIcon, TriangleAlertIcon } from "lucide-react";
import React from "react";

import { ConversationActionsProvider, conversationActions } from "../../../app/adapters/conversationActions.tsx";
import { DEFAULT_HEADER_ACTIONS, HeaderActionsProvider, type HeaderActions } from "../../../app/adapters/headerActions.tsx";
import { detentKeybindings } from "../../../app/adapters/keybindings.ts";
import { toEnvironmentProject } from "../../../app/adapters/shell.ts";
import { useTheme } from "../../../app/adapters/theme.ts";
import { AnimatedHeight } from "../../../components/AnimatedHeight.tsx";
import { AssistantCitationChip } from "../../../components/chat/AssistantCitationChip.tsx";
import { ChangedFilesCard } from "../../../components/chat/ChangedFilesTree.tsx";
import { ComposerBannerStack, type ComposerBannerStackItem } from "../../../components/chat/ComposerBannerStack.tsx";
import { feedbackBannerItem } from "../../../components/chat/ComposerFeedback.tsx";
import { usageLimitsBannerItem } from "../../../components/chat/ComposerUsageLimits.tsx";
import { DiffStatLabel } from "../../../components/chat/DiffStatLabel.tsx";
import { ExpandedImageDialog } from "../../../components/chat/ExpandedImageDialog.tsx";
import type { ExpandedImagePreview } from "../../../components/chat/ExpandedImagePreview.tsx";
import { MessageCopyButton } from "../../../components/chat/MessageCopyButton.tsx";
import { OpenInPicker } from "../../../components/chat/OpenInPicker.tsx";
import { PanelLayoutControls, RightPanelMaximizeControl } from "../../../components/chat/PanelLayoutControls.tsx";
import { PierreEntryIcon } from "../../../components/chat/PierreEntryIcon.tsx";
import { ProviderInstanceIcon } from "../../../components/chat/ProviderInstanceIcon.tsx";
import { ProviderStatusBanner } from "../../../components/chat/ProviderStatusBanner.tsx";
import { SkillInlineText } from "../../../components/chat/SkillInlineText.tsx";
import {
  SNAP_SHOT_ATTACHMENT_FRAME_CLASS,
  SnapShotAttachmentDetails,
} from "../../../components/chat/SnapShotAttachmentDetails.tsx";
import { TerminalContextInlineChip } from "../../../components/chat/TerminalContextInlineChip.tsx";
import { ThreadErrorBanner } from "../../../components/chat/ThreadErrorBanner.tsx";
import {
  ENVIRONMENT_MACHINE_KIND_LABELS,
  EnvironmentMachineIcon,
} from "../../../components/EnvironmentMachineIcon.tsx";
import * as BrandIcons from "../../../components/Icons.tsx";
import { MediaActions } from "../../../components/media/MediaActions.tsx";
import { MediaVideoPlayer } from "../../../components/media/MediaVideoPlayer.tsx";
import { OpenMediaLink } from "../../../components/media/OpenMediaLink.tsx";
import { MorphIcon } from "../../../components/MorphIcon.tsx";
import { FaviconImage } from "../../../components/preview/PreviewFaviconIcon.tsx";
import ProjectScriptsControl from "../../../components/ProjectScriptsControl.tsx";
import { ProjectScriptEditorDialog, ScriptIcon } from "../../../components/projectScriptEditor.tsx";
import { PullRequestLinkPreview } from "../../../components/pullRequest/PullRequestLinkPreview.tsx";
import {
  PullRequestActorAvatar,
  resolvePullRequestState,
} from "../../../components/pullRequest/pullRequestPresentation.tsx";
import { RenderErrorBoundary } from "../../../components/RenderErrorBoundary.tsx";
import { DesktopUpdateStatusIcon } from "../../../components/sidebar/DesktopUpdateStatusIcon.tsx";
import { ThreadCommandSubtitle, type ThreadCommandSubtitleVariant } from "../../../components/ThreadCommandSubtitle.tsx";
import { Button } from "../../../components/ui/button.tsx";
import type {
  AssistantCitation,
  EnvironmentMachineKind,
  ProjectScript,
  ProviderDriverKind,
  PullRequestActor,
  ServerProvider,
  SnapShotSource,
  UsageLimitsReport,
} from "../../../contracts/ui.ts";
import { settlePromise } from "../../../runtime/state/runtime.ts";
import type { TurnDiffFileChange } from "../../../types.ts";

import { NOW } from "../fixtures";
import { Cell, LONG_LABEL, Matrix, Row, type GalleryDoc } from "../specimen";

const noop = (): void => undefined;

/** A small inline image, so specimens never fetch one. */
function svgImage(label: string, from: string, to: string, width = 640, height = 400): string {
  const svg = [
    `<svg xmlns="http://www.w3.org/2000/svg" width="${width}" height="${height}" viewBox="0 0 ${width} ${height}">`,
    `<defs><linearGradient id="g" x1="0" y1="0" x2="1" y2="1"><stop offset="0" stop-color="${from}"/><stop offset="1" stop-color="${to}"/></linearGradient></defs>`,
    `<rect width="${width}" height="${height}" fill="url(#g)"/>`,
    `<text x="50%" y="50%" fill="white" font-family="system-ui, sans-serif" font-size="${Math.round(height / 9)}" text-anchor="middle" dominant-baseline="middle">${label}</text>`,
    "</svg>",
  ].join("");
  return `data:image/svg+xml,${encodeURIComponent(svg)}`;
}

const SCREENSHOT = svgImage("Settings window", "#1f2937", "#4b5563");
const DIAGRAM = svgImage("Lock renewal", "#1d4ed8", "#7c3aed");
const CHART = svgImage("Usage, 30 days", "#047857", "#0ea5e9");
const APP_ICON = svgImage("A", "#f97316", "#db2777", 64, 64);

function useResolvedTheme(): "light" | "dark" {
  return useTheme().resolvedTheme;
}

// --- Conversation: banners ----------------------------------------------------

const SHORT_ERROR = "The runner refused the attempt: the worktree has uncommitted changes.";
const LONG_ERROR = [
  "Provider request failed after 3 retries: the provider returned 529 (overloaded).",
  "The turn stopped before any tool call ran, so the worktree is unchanged.",
  "Retry the turn, or switch the model in the composer if the provider stays overloaded.",
  "Request id req_01J9ZK4W7C6XQ1V5N2M8 for the support thread.",
].join(" ");

const threadErrorBanner: GalleryDoc = {
  meta: {
    name: "Thread error banner",
    kind: "composition",
    group: "Conversation",
    source: "src/components/chat/ThreadErrorBanner.tsx",
  },
  specimens: [
    {
      id: "errors",
      title: "Short and long error, with and without dismiss",
      note: "Long errors clamp to three lines; the full text is in the tooltip.",
      render: () => (
        <div className="flex flex-col gap-2">
          <ThreadErrorBanner error={SHORT_ERROR} onDismiss={noop} />
          <ThreadErrorBanner error={LONG_ERROR} onDismiss={noop} />
          <ThreadErrorBanner error="Conversation not found." />
        </div>
      ),
    },
  ],
};

function provider(overrides: Partial<ServerProvider>): ServerProvider {
  return {
    instanceId: "codex",
    driver: "codex",
    status: "warning",
    installed: true,
    auth: { status: "authenticated" },
    ...overrides,
  };
}

const PROVIDER_STATES: Record<string, ServerProvider> = {
  warning: provider({ message: "Rate limited until 14:00. Requests may queue." }),
  "error, unauthenticated": provider({
    instanceId: "claudeAgent",
    driver: "claudeAgent",
    displayName: "Claude",
    status: "error",
    auth: { status: "unauthenticated" },
    setup: { canAuthenticate: true },
  }),
  "error, not installed": provider({
    instanceId: "opencode",
    driver: "opencode",
    status: "error",
    installed: false,
    auth: { status: "unknown" },
    setup: { canInstall: true },
  }),
  "error, long message": provider({ status: "error", message: LONG_ERROR }),
};

const providerStatusBanner: GalleryDoc = {
  meta: {
    name: "Provider status banner",
    kind: "composition",
    group: "Conversation",
    source: "src/components/chat/ProviderStatusBanner.tsx",
  },
  specimens: [
    {
      id: "states",
      title: "Warning, unauthenticated, not installed, long message",
      note: "Setup-capable providers offer Open provider setup; every banner dismisses.",
      render: () => (
        <div className="flex flex-col gap-3">
          {Object.entries(PROVIDER_STATES).map(([label, status]) => (
            <Cell key={label} label={label} className="w-full">
              <ProviderStatusBanner status={status} onDismiss={noop} onOpenProviderSetup={noop} />
            </Cell>
          ))}
        </div>
      ),
    },
  ],
};

const USAGE_REPORT: UsageLimitsReport = {
  createdAt: new Date(NOW).toISOString(),
  accounts: [
    {
      id: "acct_codex",
      driver: "codex",
      label: "Codex",
      plan: "Pro",
      limits: {
        checkedAt: new Date(NOW).toISOString(),
        windows: [
          { id: "session", kind: "session", label: "5h", usedPercent: 62, resetsAt: new Date(NOW + 2 * 3_600_000).toISOString(), windowDurationMins: 300 },
          { id: "weekly", kind: "weekly", label: "Weekly", usedPercent: 91, resetsAt: new Date(NOW + 3 * 86_400_000).toISOString(), windowDurationMins: 10_080 },
        ],
      },
    },
  ],
  notices: [],
};

function ComposerNoticesSpecimen({ kind }: { readonly kind: "usage" | "feedback" }) {
  const [dismissed, setDismissed] = React.useState<ReadonlySet<string>>(() => new Set());
  const dismiss = (id: string) => () => setDismissed((all) => new Set([...all, id]));
  const items: ComposerBannerStackItem[] =
    kind === "usage"
      ? [usageLimitsBannerItem("usage-limits", USAGE_REPORT, "hub", dismiss("usage-limits"))]
      : [
          feedbackBannerItem(
            { id: "fb_sending", command: "/feedback", createdAt: new Date(NOW).toISOString(), status: "uploading" },
            dismiss("feedback:fb_sending"),
          ),
          feedbackBannerItem(
            {
              id: "fb_sent",
              command: "/feedback",
              createdAt: new Date(NOW).toISOString(),
              status: "sent",
              feedbackId: "019274ad-4f1c-7e2b-9d3a-6c58e1f0b2aa",
            },
            dismiss("feedback:fb_sent"),
          ),
          feedbackBannerItem(
            {
              id: "fb_failed",
              command: "/feedback",
              createdAt: new Date(NOW).toISOString(),
              status: "failed",
              errorMessage: "The upload timed out. Try again from the composer.",
            },
            dismiss("feedback:fb_failed"),
          ),
        ].filter((item): item is ComposerBannerStackItem => item !== null);
  return (
    <div className="mx-auto w-full max-w-3xl">
      <ComposerBannerStack items={items.filter((item) => !dismissed.has(item.id))} />
    </div>
  );
}

const composerNotices: GalleryDoc = {
  meta: {
    name: "Composer notices",
    kind: "composition",
    group: "Conversation",
    source: "src/components/chat/ComposerUsageLimits.tsx",
  },
  specimens: [
    {
      id: "usage-limits",
      title: "Usage limits notice",
      note: "The /usage-limits result as a stack item: limit windows for one account.",
      minHeight: 220,
      render: () => <ComposerNoticesSpecimen kind="usage" />,
    },
    {
      id: "feedback",
      title: "Feedback notices: sending, sent, failed",
      note: "Sending leads as activity; sent offers Copy ID; click the peek to expand the stack.",
      minHeight: 260,
      render: () => <ComposerNoticesSpecimen kind="feedback" />,
    },
  ],
};

// --- Conversation: diff stats and changed files --------------------------------

const DIFF_STATS: Record<string, readonly [number, number]> = {
  small: [12, 3],
  "additions only": [48, 0],
  zero: [0, 0],
  compact: [12_345, 6_789_000],
};

const diffStatLabel: GalleryDoc = {
  meta: {
    name: "Diff stat label",
    kind: "composition",
    group: "Conversation",
    source: "src/components/chat/DiffStatLabel.tsx",
  },
  specimens: [
    {
      id: "layouts",
      title: "Counts × layouts",
      note: "Aligned keeps a fixed 4ch column per count for lists; inline flows with text.",
      render: () => (
        <Matrix
          rows={Object.keys(DIFF_STATS)}
          columns={["aligned", "inline", "parentheses"]}
          render={(row, column) => {
            const [additions, deletions] = DIFF_STATS[row] ?? [0, 0];
            return (
              <span className="text-xs">
                <DiffStatLabel
                  additions={additions}
                  deletions={deletions}
                  layout={column === "inline" ? "inline" : "aligned"}
                  showParentheses={column === "parentheses"}
                />
              </span>
            );
          }}
        />
      ),
    },
  ],
};

const CHANGED_FILES: TurnDiffFileChange[] = [
  { path: "internal/lock/renew.go", kind: "modified", additions: 24, deletions: 9 },
  { path: "internal/lock/renew_test.go", kind: "added", additions: 88, deletions: 0 },
  { path: "internal/lock/handoff.go", kind: "modified", additions: 6, deletions: 14 },
  { path: "web/conversation/src/app/components/Composer.tsx", kind: "modified", additions: 3, deletions: 1 },
  { path: "CHANGELOG.md", kind: "modified", additions: 4, deletions: 0 },
];

function ChangedFilesSpecimen({ files }: { readonly files: TurnDiffFileChange[] }) {
  const [expanded, setExpanded] = React.useState(true);
  return (
    <div className="w-full max-w-xl">
      <ChangedFilesCard
        turnId="turn_142"
        files={files}
        allDirectoriesExpanded={expanded}
        resolvedTheme={useResolvedTheme()}
        onToggleAllDirectories={() => setExpanded((value) => !value)}
        onOpenTurnDiff={noop}
      />
    </div>
  );
}

const changedFilesCard: GalleryDoc = {
  meta: {
    name: "Changed files card",
    kind: "composition",
    group: "Conversation",
    source: "src/components/chat/ChangedFilesTree.tsx",
  },
  specimens: [
    {
      id: "tree",
      title: "A turn's changed files as a tree",
      note: "Folders toggle one by one or all at once; a row opens that file in the diff.",
      render: () => <ChangedFilesSpecimen files={CHANGED_FILES} />,
    },
    {
      id: "flat",
      title: "Top-level files only",
      note: "Without folders the expand-all control is hidden.",
      render: () => <ChangedFilesSpecimen files={CHANGED_FILES.filter((file) => !file.path.includes("/"))} />,
    },
  ],
};

const messageCopyButton: GalleryDoc = {
  meta: {
    name: "Message copy button",
    kind: "composition",
    group: "Conversation",
    source: "src/components/chat/MessageCopyButton.tsx",
  },
  specimens: [
    {
      id: "matrix",
      title: "Sizes × variants",
      note: "Press one to see the copied state: a check, disabled, and an anchored toast.",
      render: () => (
        <Matrix
          rows={["xs", "icon-xs"] as const}
          columns={["outline", "ghost"] as const}
          render={(size, variant) => (
            <MessageCopyButton text="The lock renewal now waits on a healthy handoff." size={size} variant={variant} />
          )}
        />
      ),
    },
  ],
};

// --- Conversation: inline chips ------------------------------------------------

const SKILLS = [
  { name: "review-pr", displayName: "Review PR" },
  { name: "changelog" },
  { name: "release-notes:draft" },
];

const skillInlineText: GalleryDoc = {
  meta: {
    name: "Skill inline text",
    kind: "composition",
    group: "Conversation",
    source: "src/components/chat/SkillInlineText.tsx",
  },
  specimens: [
    {
      id: "text",
      title: "Known skills become chips; everything else stays text",
      render: () => (
        <div className="flex max-w-xl flex-col gap-3 text-sm">
          <p>
            <SkillInlineText text="Run $review-pr on the branch, then $changelog for the release." skills={SKILLS} />
          </p>
          <p>
            <SkillInlineText text="Use $release-notes:draft once the PR merges." skills={SKILLS} />
          </p>
          <p>
            <SkillInlineText text="Unknown $not-a-skill and amounts like $20 stay plain text." skills={SKILLS} />
          </p>
        </div>
      ),
    },
  ],
};

const terminalContextChip: GalleryDoc = {
  meta: {
    name: "Terminal context chip",
    kind: "composition",
    group: "Conversation",
    source: "src/components/chat/TerminalContextInlineChip.tsx",
  },
  specimens: [
    {
      id: "states",
      title: "Live and expired",
      note: "Hover for the captured lines.",
      render: () => (
        <Row>
          <Cell label="live">
            <span className="text-sm">
              <TerminalContextInlineChip
                label="Terminal 1 lines 12-40"
                tooltipText={"$ go test ./internal/lock\n--- FAIL: TestRenewHalfLease (0.02s)\nFAIL"}
              />
            </span>
          </Cell>
          <Cell label="expired">
            <span className="text-sm">
              <TerminalContextInlineChip
                label="Terminal 2 lines 1-8"
                tooltipText="This terminal output is no longer available."
                expired
              />
            </span>
          </Cell>
          <Cell label="long label">
            <span className="max-w-64 text-sm">
              <TerminalContextInlineChip label={LONG_LABEL} tooltipText={LONG_LABEL} />
            </span>
          </Cell>
        </Row>
      ),
    },
  ],
};

const CITATION: AssistantCitation = {
  version: 1,
  environmentId: "hub",
  threadId: "conv_dsa_running",
  messageId: "msg_assistant_1",
  text: "renewLease reads the lease duration instead of a constant",
  start: 4,
  end: 61,
  prefix: "1. `",
  suffix: "`.",
};

function CitationWithEditor() {
  const [open, setOpen] = React.useState(false);
  const [comment, setComment] = React.useState<string | undefined>("Should the minimum be 5s?");
  return (
    <AssistantCitationChip
      citation={{ ...CITATION, ...(comment ? { comment } : {}) }}
      onRemove={noop}
      commentEditor={{
        open,
        onOpenChange: setOpen,
        onSave: (next) => {
          setComment(next.trim() || undefined);
          return true;
        },
      }}
    />
  );
}

const assistantCitationChip: GalleryDoc = {
  meta: {
    name: "Assistant citation chip",
    kind: "composition",
    group: "Conversation",
    source: "src/components/chat/AssistantCitationChip.tsx",
  },
  specimens: [
    {
      id: "chips",
      title: "In a message, in the composer, with a comment",
      note: "The pencil opens the comment editor; Enter saves, Escape cancels.",
      minHeight: 260,
      render: () => (
        <Row>
          <Cell label="chat">
            <span className="text-sm">
              <AssistantCitationChip citation={CITATION} />
            </span>
          </Cell>
          <Cell label="composer (removable)">
            <span className="text-sm">
              <AssistantCitationChip citation={CITATION} onRemove={noop} />
            </span>
          </Cell>
          <Cell label="with comment editor">
            <span className="text-sm">
              <CitationWithEditor />
            </span>
          </Cell>
        </Row>
      ),
    },
  ],
};

// --- Conversation: pull request link preview ----------------------------------

const ACTORS: Record<string, PullRequestActor | null> = {
  "avatar": { login: "mvisser", name: "Michael Visser", avatarUrl: svgImage("M", "#0f766e", "#22c55e", 64, 64) },
  "no avatar": { login: "runner-bot", name: null, avatarUrl: null },
  "deleted user": null,
};

const PULL_REQUEST_STATES = [
  { label: "open", input: { state: "open", isDraft: false } },
  { label: "draft", input: { state: "open", isDraft: true } },
  { label: "conflicting", input: { state: "open", isDraft: false, mergeability: "conflicting", baseBranch: "develop" } },
  { label: "merged", input: { state: "merged", isDraft: false } },
  { label: "closed", input: { state: "closed", isDraft: false } },
] as const;

const pullRequestLinkPreview: GalleryDoc = {
  meta: {
    name: "Pull request link preview",
    kind: "composition",
    group: "Conversation",
    source: "src/components/pullRequest/PullRequestLinkPreview.tsx",
  },
  specimens: [
    {
      id: "link",
      title: "A pull request link in a message",
      note: "Hover the link for the preview card. This client has no pull request detail source yet, so the card shows the URL.",
      minHeight: 160,
      render: () => (
        <p className="max-w-xl text-sm">
          Opened{" "}
          <PullRequestLinkPreview
            link={
              <a className="text-primary underline underline-offset-2" href="#specimen">
                acme/detent#142
              </a>
            }
            originalUrl="https://github.com/acme/detent/pull/142"
            target={{ environmentId: "hub", input: { projectId: "proj_detent", repository: "acme/detent", number: 142 } }}
            confirmBeforeOpen={false}
            onOpenPullRequest={() => true}
            onOpenFallback={async () => undefined}
          />{" "}
          for review.
        </p>
      ),
    },
    {
      id: "presentation",
      title: "State and author presentation",
      note: "The same state ink the thread badge, the pull request surface and issue properties use.",
      render: () => (
        <div className="flex flex-col gap-4">
          <Row>
            {PULL_REQUEST_STATES.map(({ label, input }) => {
              const state = resolvePullRequestState(input);
              return (
                <Cell key={label} label={label}>
                  <span className="inline-flex items-center gap-1 text-xs text-muted-foreground">
                    <state.Icon aria-hidden className={`size-3 ${state.toneClassName}`} />
                    {state.label}
                  </span>
                </Cell>
              );
            })}
          </Row>
          <Row>
            {Object.entries(ACTORS).map(([label, actor]) => (
              <Cell key={label} label={label}>
                <span className="inline-flex items-center gap-1.5 text-xs text-muted-foreground">
                  <PullRequestActorAvatar actor={actor} className="size-5" />
                  {actor?.login ?? "ghost"}
                </span>
              </Cell>
            ))}
          </Row>
        </div>
      ),
    },
  ],
};

// --- Media ---------------------------------------------------------------------

const SNAP_SHOT: SnapShotSource = {
  kind: "snap-shot",
  capturedAt: new Date(NOW).toISOString(),
  appName: "System Settings",
  windowTitle: "Privacy & Security",
  appIconDataUrl: APP_ICON,
  accessibility: {
    format: "flat-text",
    text: "Privacy & Security\nLocation Services: On\nFull Disk Access: 3 apps",
    truncated: false,
  },
};

const PREVIEW: ExpandedImagePreview = {
  images: [
    { src: DIAGRAM, name: "lock-renewal.svg" },
    { src: SCREENSHOT, name: "Privacy & Security", source: SNAP_SHOT },
    { src: CHART, name: "usage-30d.svg" },
  ],
  index: 0,
};

const UNAVAILABLE: ExpandedImagePreview = {
  images: [{ src: null, name: "deleted-screenshot.png" }],
  index: 0,
};

function ExpandedImageSpecimen({ preview }: { readonly preview: ExpandedImagePreview }) {
  const [open, setOpen] = React.useState(true);
  return (
    <div>
      <Button variant="outline" size="sm" onClick={() => setOpen(true)}>
        Open preview
      </Button>
      {open ? <ExpandedImageDialog preview={preview} onClose={() => setOpen(false)} /> : null}
    </div>
  );
}

const expandedImageDialog: GalleryDoc = {
  meta: {
    name: "Expanded image dialog",
    kind: "composition",
    group: "Media",
    source: "src/components/chat/ExpandedImageDialog.tsx",
  },
  specimens: [
    {
      id: "gallery",
      title: "Three images, one a window capture",
      note: "Arrow keys or the side buttons page; Escape or a backdrop click closes; the text button on the capture shows its accessibility data.",
      minHeight: 560,
      render: () => <ExpandedImageSpecimen preview={PREVIEW} />,
    },
    {
      id: "unavailable",
      title: "Image unavailable",
      minHeight: 360,
      render: () => <ExpandedImageSpecimen preview={UNAVAILABLE} />,
    },
  ],
};

const snapShotAttachmentDetails: GalleryDoc = {
  meta: {
    name: "Snapshot attachment details",
    kind: "composition",
    group: "Media",
    source: "src/components/chat/SnapShotAttachmentDetails.tsx",
  },
  specimens: [
    {
      id: "frames",
      title: "With app icon, without, and without accessibility data",
      note: "The text button opens the captured accessibility data.",
      minHeight: 320,
      render: () => (
        <Row>
          {(
            [
              ["app icon", SNAP_SHOT],
              ["initial", { ...SNAP_SHOT, appIconDataUrl: undefined, appName: "Terminal", windowTitle: "" }],
              ["no accessibility data", { ...SNAP_SHOT, accessibility: undefined, appName: "Preview" }],
            ] as const
          ).map(([label, source]) => (
            <Cell key={label} label={label}>
              <div className={SNAP_SHOT_ATTACHMENT_FRAME_CLASS}>
                <img src={SCREENSHOT} alt="" className="size-full object-cover" />
                <SnapShotAttachmentDetails source={source as SnapShotSource} />
              </div>
            </Cell>
          ))}
        </Row>
      ),
    },
  ],
};

const mediaVideoPlayer: GalleryDoc = {
  meta: {
    name: "Media video player",
    kind: "composition",
    group: "Media",
    source: "src/components/media/MediaVideoPlayer.tsx",
  },
  specimens: [
    {
      id: "states",
      title: "Loading and failure states",
      note: "Playback needs a real video file, which the gallery does not ship; every state keeps the same 16:9 slot.",
      render: () => (
        <Row>
          <Cell label="loading" className="w-72">
            <MediaVideoPlayer src={null} label="demo.mp4" className="w-72" />
          </Cell>
          <Cell label="failed, original link" className="w-72">
            <MediaVideoPlayer
              src={null}
              label="demo.mp4"
              sourceFailed
              originalUrl="https://example.com/media/demo.mp4"
              className="w-72"
            />
          </Cell>
          <Cell label="failed, retry" className="w-72">
            <MediaVideoPlayer
              src={null}
              label="recording.mov"
              sourceFailed
              onRetry={async () => undefined}
              className="w-72"
            />
          </Cell>
        </Row>
      ),
    },
    {
      id: "open-link",
      title: "Open media link",
      note: "An authored web URL, a remote file and a local blob.",
      render: () => (
        <Row>
          <Cell label="originalUrl">
            <OpenMediaLink originalUrl="https://example.com/media/demo.mp4" />
          </Cell>
          <Cell label="src">
            <OpenMediaLink src="https://example.com/media/demo.mp4" />
          </Cell>
          <Cell label="blob src">
            <OpenMediaLink src="blob:https://example.com/7f0c2a" fileName="recording.mov" />
          </Cell>
        </Row>
      ),
    },
  ],
};

const mediaActions: GalleryDoc = {
  meta: {
    name: "Media actions",
    kind: "composition",
    group: "Media",
    source: "src/components/media/MediaActions.tsx",
  },
  specimens: [
    {
      id: "image",
      title: "An image with its source actions",
      note: "Hover or focus for the source path; right-click or Shift+F10 opens the copy, save and open menu.",
      minHeight: 220,
      render: () => (
        <MediaActions
          source={{
            kind: "image",
            name: "lock-renewal.svg",
            src: DIAGRAM,
            reference: {
              kind: "file",
              path: "/Users/runner/code/detent/docs/lock-renewal.svg",
              relativePath: "docs/lock-renewal.svg",
            },
            onOpenFile: noop,
          }}
        >
          <img src={DIAGRAM} alt="Lock renewal diagram" className="h-36 w-56 rounded-lg border border-border/70 object-cover" />
        </MediaActions>
      ),
    },
  ],
};

// --- Panels ----------------------------------------------------------------------

function PanelControlsSpecimen({
  available = true,
  agents = 0,
}: {
  readonly available?: boolean;
  readonly agents?: number;
}) {
  const [terminalOpen, setTerminalOpen] = React.useState(false);
  const [panelOpen, setPanelOpen] = React.useState(agents > 0);
  return (
    <PanelLayoutControls
      terminalAvailable={available}
      terminalOpen={terminalOpen}
      terminalShortcutLabel="⌃`"
      rightPanelAvailable={available}
      rightPanelOpen={panelOpen}
      rightPanelShortcutLabel="⌘⌥B"
      liveAgentCount={agents}
      onToggleTerminal={() => setTerminalOpen((value) => !value)}
      onToggleRightPanel={() => setPanelOpen((value) => !value)}
    />
  );
}

function MaximizeSpecimen() {
  const [maximized, setMaximized] = React.useState(false);
  return <RightPanelMaximizeControl maximized={maximized} onToggle={() => setMaximized((value) => !value)} />;
}

const panelLayoutControls: GalleryDoc = {
  meta: {
    name: "Panel layout controls",
    kind: "composition",
    group: "Panels",
    source: "src/components/chat/PanelLayoutControls.tsx",
  },
  specimens: [
    {
      id: "states",
      title: "Terminal and right panel toggles",
      note: "Each toggle shows its pressed state; the badge counts agents working in the panel.",
      render: () => (
        <Row>
          <Cell label="closed">
            <PanelControlsSpecimen />
          </Cell>
          <Cell label="2 agents working">
            <PanelControlsSpecimen agents={2} />
          </Cell>
          <Cell label="unavailable">
            <PanelControlsSpecimen available={false} />
          </Cell>
          <Cell label="maximize">
            <MaximizeSpecimen />
          </Cell>
        </Row>
      ),
    },
  ],
};

// --- Workspace ------------------------------------------------------------------

const SUBTITLE_PROJECT = toEnvironmentProject({ id: "proj_detent", name: "detent", can_write: true } as never);
const SUBTITLE_VARIANTS: readonly ThreadCommandSubtitleVariant[] = [
  "favicon-workspace-harness",
  "favicon-workspace",
  "favicon-branch-harness",
];

const threadCommandSubtitle: GalleryDoc = {
  meta: {
    name: "Thread command subtitle",
    kind: "composition",
    group: "Workspace",
    source: "src/components/ThreadCommandSubtitle.tsx",
  },
  specimens: [
    {
      id: "variants",
      title: "Variants × checkout kind",
      note: "A worktree always shows the worktree folder; a local checkout shows a folder or a branch by variant.",
      render: () => (
        <Matrix
          rows={SUBTITLE_VARIANTS}
          columns={["local", "worktree", "current"]}
          render={(variant, column) => (
            <ThreadCommandSubtitle
              variant={variant}
              project={SUBTITLE_PROJECT}
              projectTitle="detent"
              branch={column === "local" ? "develop" : "detent/detent_142"}
              worktreePath={column === "local" ? null : "/Users/runner/.detent/worktrees/detent_142"}
              isCurrent={column === "current"}
            />
          )}
        />
      ),
    },
    {
      id: "partial",
      title: "Partial and long data",
      render: () => (
        <div className="flex max-w-xs flex-col gap-3">
          <Cell label="branch only">
            <ThreadCommandSubtitle project={null} projectTitle={null} branch="main" worktreePath={null} isCurrent={false} />
          </Cell>
          <Cell label="current only">
            <ThreadCommandSubtitle project={null} projectTitle={null} branch={null} worktreePath={null} isCurrent />
          </Cell>
          <Cell label="long" className="w-full">
            <ThreadCommandSubtitle
              project={SUBTITLE_PROJECT}
              projectTitle={LONG_LABEL}
              branch={`detent/${LONG_LABEL.toLowerCase().replaceAll(" ", "-")}`}
              worktreePath="/tmp/wt"
              isCurrent
            />
          </Cell>
        </div>
      ),
    },
  ],
};

function headerActions(overrides: Partial<HeaderActions>): HeaderActions {
  return { ...DEFAULT_HEADER_ACTIONS, ...overrides };
}

const ISSUE_TARGET = { id: "issue", label: "Open issue detent#142", run: noop, external: true };

const OPEN_IN_CASES: Record<string, HeaderActions> = {
  "runner on this machine": headerActions({
    openLocation: { worktreePath: "/Users/runner/code/detent", hostname: "localhost", local: true, linked: true },
    openTargets: [ISSUE_TARGET],
  }),
  "remote runner": headerActions({
    openLocation: { worktreePath: "/home/runner/code/detent", hostname: "mac-studio", local: false, linked: true },
    openTargets: [ISSUE_TARGET],
  }),
  "no linked issue": headerActions({}),
};

const openInPicker: GalleryDoc = {
  meta: {
    name: "Open in picker",
    kind: "composition",
    group: "Workspace",
    source: "src/components/chat/OpenInPicker.tsx",
  },
  specimens: [
    {
      id: "states",
      title: "Reachable editor, remote runner, no issue",
      note: "When no editor is reachable the primary opens the first destination; the menu says why each editor cannot run.",
      minHeight: 260,
      render: () => (
        <Row>
          {Object.entries(OPEN_IN_CASES).map(([label, actions]) => (
            <Cell key={label} label={label}>
              <div className="@container/header-actions">
                <HeaderActionsProvider value={actions}>
                  <OpenInPicker enableShortcut={false} />
                </HeaderActionsProvider>
              </div>
            </Cell>
          ))}
        </Row>
      ),
    },
  ],
};

const SCRIPTS: ProjectScript[] = [
  { id: "test", name: "Test", command: "go test ./...", icon: "test", runOnWorktreeCreate: false },
  { id: "lint", name: "Lint", command: "golangci-lint run", icon: "lint", runOnWorktreeCreate: false },
  { id: "setup", name: "Install", command: "npm ci", icon: "configure", runOnWorktreeCreate: true },
];

const scriptResult = () => settlePromise(async () => undefined);

function ScriptsControl({ scripts }: { readonly scripts: ProjectScript[] }) {
  return (
    <ProjectScriptsControl
      scripts={scripts}
      keybindings={detentKeybindings}
      onRunScript={noop}
      onAddScript={scriptResult}
      onUpdateScript={scriptResult}
      onDeleteScript={scriptResult}
    />
  );
}

function ScriptEditorSpecimen() {
  const [open, setOpen] = React.useState(true);
  return (
    <div>
      <Button variant="outline" size="sm" onClick={() => setOpen(true)}>
        Edit Test
      </Button>
      <ProjectScriptEditorDialog
        request={
          open
            ? {
                scriptId: "test",
                initial: {
                  name: "Test",
                  command: "go test ./...",
                  icon: "test",
                  runOnWorktreeCreate: false,
                  keybinding: null,
                  previewUrl: null,
                  autoOpenPreview: false,
                },
              }
            : null
        }
        scripts={SCRIPTS}
        onSubmit={scriptResult}
        onDelete={noop}
        onClose={() => setOpen(false)}
      />
    </div>
  );
}

const projectScriptsControl: GalleryDoc = {
  meta: {
    name: "Project scripts control",
    kind: "composition",
    group: "Workspace",
    source: "src/components/ProjectScriptsControl.tsx",
  },
  specimens: [
    {
      id: "control",
      title: "With scripts, with only conversation actions, empty",
      note: "The split button runs the primary script; the menu lists every script with an edit button.",
      minHeight: 300,
      render: () => (
        <div className="@container/header-actions">
          <Row>
            <Cell label="scripts">
              <ScriptsControl scripts={SCRIPTS} />
            </Cell>
            <Cell label="conversation actions only">
              <ConversationActionsProvider
                value={{ actions: conversationActions({ canCreateIssue: true, issueIdentifier: null }), run: noop }}
              >
                <ScriptsControl scripts={[]} />
              </ConversationActionsProvider>
            </Cell>
            <Cell label="empty">
              <ScriptsControl scripts={[]} />
            </Cell>
          </Row>
        </div>
      ),
    },
    {
      id: "icons",
      title: "Script icons",
      render: () => (
        <Row>
          {["play", "test", "lint", "configure", "build", "debug"].map((icon) => (
            <Cell key={icon} label={icon}>
              <ScriptIcon icon={icon} className="size-4" />
            </Cell>
          ))}
        </Row>
      ),
    },
    {
      id: "editor",
      title: "Edit action dialog",
      note: "The preview switch is disabled and says why.",
      minHeight: 680,
      render: () => <ScriptEditorSpecimen />,
    },
  ],
};

function UpdateIconFrame({ children }: { readonly children: React.ReactNode }) {
  return (
    <span className="inline-flex size-8 items-center justify-center rounded-full bg-sidebar-control-surface text-sidebar-foreground">
      {children}
    </span>
  );
}

const sidebarUpdatePill: GalleryDoc = {
  meta: {
    name: "Sidebar update pill",
    kind: "composition",
    group: "Workspace",
    source: "src/components/sidebar/SidebarUpdatePill.tsx",
  },
  specimens: [
    {
      id: "status-icon",
      title: "Status icon states",
      note: "The pill itself reads the runner update watch; these are the icons it switches between.",
      render: () => (
        <Row>
          <Cell label="idle">
            <UpdateIconFrame>
              <DesktopUpdateStatusIcon status="idle" />
            </UpdateIconFrame>
          </Cell>
          <Cell label="checking">
            <UpdateIconFrame>
              <DesktopUpdateStatusIcon status="checking" isCheckAnimating />
            </UpdateIconFrame>
          </Cell>
          <Cell label="available">
            <UpdateIconFrame>
              <DesktopUpdateStatusIcon status="available" />
            </UpdateIconFrame>
          </Cell>
          <Cell label="downloading 40%">
            <UpdateIconFrame>
              <DesktopUpdateStatusIcon status="downloading" downloadPercent={40} />
            </UpdateIconFrame>
          </Cell>
          <Cell label="downloaded">
            <UpdateIconFrame>
              <DesktopUpdateStatusIcon status="downloaded" />
            </UpdateIconFrame>
          </Cell>
        </Row>
      ),
    },
  ],
};

// --- Structure -------------------------------------------------------------------

function AnimatedHeightSpecimen() {
  const [expanded, setExpanded] = React.useState(false);
  return (
    <div className="flex w-full max-w-md flex-col items-start gap-3">
      <Button variant="outline" size="sm" onClick={() => setExpanded((value) => !value)}>
        {expanded ? "Show less" : "Show more"}
      </Button>
      <div className="w-full rounded-lg border border-border/70 p-3 text-sm">
        <AnimatedHeight>
          <p>The lock renewal now waits on a healthy handoff.</p>
          {expanded ? (
            <>
              <p className="mt-2 text-muted-foreground">
                renewLease reads the lease duration instead of a constant, and the handoff check runs before the
                renewal instead of after it.
              </p>
              <p className="mt-2 text-muted-foreground">A short lease renews often; the minimum is capped at 5s.</p>
            </>
          ) : null}
        </AnimatedHeight>
      </div>
    </div>
  );
}

const animatedHeight: GalleryDoc = {
  meta: {
    name: "Animated height",
    kind: "primitive",
    group: "Structure",
    source: "src/components/AnimatedHeight.tsx",
  },
  specimens: [
    {
      id: "toggle",
      title: "Content that grows and shrinks",
      note: "The container eases to the new height; reduced motion snaps.",
      render: () => <AnimatedHeightSpecimen />,
    },
  ],
};

function Throws(): React.ReactElement {
  throw new Error("Malformed diff payload");
}

const renderErrorBoundary: GalleryDoc = {
  meta: {
    name: "Render error boundary",
    kind: "primitive",
    group: "Structure",
    source: "src/components/RenderErrorBoundary.tsx",
  },
  specimens: [
    {
      id: "fallback",
      title: "Healthy child and failed child",
      note: "A failed child is replaced by its fallback; the rest of the page keeps rendering.",
      render: () => (
        <Row>
          <Cell label="healthy">
            <RenderErrorBoundary fallback={<span>Fallback</span>}>
              <span className="text-sm">Rendered child</span>
            </RenderErrorBoundary>
          </Cell>
          <Cell label="child threw">
            <RenderErrorBoundary
              fallback={
                <span className="inline-flex items-center gap-1.5 text-sm text-muted-foreground">
                  <CircleAlertIcon className="size-3.5" />
                  This diff could not be rendered.
                </span>
              }
            >
              <Throws />
            </RenderErrorBoundary>
          </Cell>
        </Row>
      ),
    },
  ],
};

// --- Icons -----------------------------------------------------------------------

const BRAND_ICONS = Object.entries(BrandIcons).filter(
  (entry): entry is [string, React.FC<React.SVGProps<SVGSVGElement>>] => typeof entry[1] === "function",
);

const brandIcons: GalleryDoc = {
  meta: { name: "Brand icons", kind: "primitive", group: "Icons", source: "src/components/Icons.tsx" },
  specimens: [
    {
      id: "all",
      title: "Every exported icon at the sizes components use",
      note: "14px in menus, 16px in controls, 20px for provider marks.",
      render: () => (
        <div className="grid grid-cols-[repeat(auto-fill,minmax(10rem,1fr))] gap-x-4 gap-y-3">
          {BRAND_ICONS.map(([name, Icon]) => (
            <figure key={name} className="m-0 flex min-w-0 flex-col gap-1.5">
              <div className="flex items-center gap-3 text-foreground">
                <Icon aria-hidden className="size-3.5" />
                <Icon aria-hidden className="size-4" />
                <Icon aria-hidden className="size-5" />
              </div>
              <figcaption className="truncate font-mono text-2xs text-muted-foreground">{name}</figcaption>
            </figure>
          ))}
        </div>
      ),
    },
  ],
};

const MORPH_PAIRS = [
  ["copy → check", Copy, Check],
  ["panel → close", PanelLeft, PanelLeftClose],
  ["play → pause", Play, Pause],
] as const;

function MorphIconSpecimen() {
  const [flipped, setFlipped] = React.useState(false);
  return (
    <div className="flex flex-col items-start gap-3">
      <Button variant="outline" size="sm" onClick={() => setFlipped((value) => !value)}>
        Morph
      </Button>
      <Row>
        {MORPH_PAIRS.map(([label, from, to]) => (
          <Cell key={label} label={label}>
            <MorphIcon className="size-5" icon={flipped ? to : from} />
          </Cell>
        ))}
      </Row>
    </div>
  );
}

const morphIcon: GalleryDoc = {
  meta: { name: "Morph icon", kind: "primitive", group: "Icons", source: "src/components/MorphIcon.tsx" },
  specimens: [
    {
      id: "pairs",
      title: "Icons that morph when their state flips",
      note: "Press Morph; reduced motion swaps without the transition.",
      render: () => <MorphIconSpecimen />,
    },
  ],
};

const environmentMachineIcon: GalleryDoc = {
  meta: {
    name: "Environment machine icon",
    kind: "composition",
    group: "Icons",
    source: "src/components/EnvironmentMachineIcon.tsx",
  },
  specimens: [
    {
      id: "kinds",
      title: "Every machine kind with its label",
      render: () => (
        <Row>
          {(Object.keys(ENVIRONMENT_MACHINE_KIND_LABELS) as EnvironmentMachineKind[]).map((kind) => (
            <Cell key={kind} label={`${kind} · ${ENVIRONMENT_MACHINE_KIND_LABELS[kind]}`}>
              <EnvironmentMachineIcon kind={kind} className="size-4" />
            </Cell>
          ))}
        </Row>
      ),
    },
  ],
};

const ENTRY_PATHS = [
  "main.go",
  "Composer.tsx",
  "index.ts",
  "global.css",
  "README.md",
  "package.json",
  "Dockerfile",
  "schema.graphql",
  "build.zig",
  "logo.svg",
  "notes.unknownext",
];

function FileEntryIcons() {
  const theme = useResolvedTheme();
  return (
    <Row>
      {ENTRY_PATHS.map((path) => (
        <Cell key={path} label={path}>
          <PierreEntryIcon pathValue={path} kind="file" theme={theme} />
        </Cell>
      ))}
      <Cell label="src/ (directory)">
        <PierreEntryIcon pathValue="src" kind="directory" theme={theme} />
      </Cell>
      <Cell label=".github/ (directory)">
        <PierreEntryIcon pathValue=".github" kind="directory" theme={theme} />
      </Cell>
    </Row>
  );
}

const fileEntryIcon: GalleryDoc = {
  meta: {
    name: "File entry icon",
    kind: "composition",
    group: "Icons",
    source: "src/components/chat/PierreEntryIcon.tsx",
  },
  specimens: [
    {
      id: "types",
      title: "File types and directories",
      note: "Colour follows the file type and the frame's theme; unknown types fall back to a plain file.",
      render: () => <FileEntryIcons />,
    },
  ],
};

const PROVIDERS: Array<[string, ProviderDriverKind, string]> = [
  ["codex", "codex", "Codex"],
  ["claudeAgent", "claudeAgent", "Claude"],
  ["opencode", "opencode", "OpenCode"],
  ["cursor", "cursor", "Cursor"],
  ["grok", "grok", "Grok"],
  ["antigravity", "antigravity", "Antigravity"],
  ["unknown driver", "acme-agent", "Acme Agent"],
];

const providerInstanceIcon: GalleryDoc = {
  meta: {
    name: "Provider instance icon",
    kind: "composition",
    group: "Icons",
    source: "src/components/chat/ProviderInstanceIcon.tsx",
  },
  specimens: [
    {
      id: "drivers",
      title: "Drivers, with and without the instance badge",
      note: "An unknown driver shows its initials; an accent colour fills the badge.",
      render: () => (
        <Matrix
          rows={PROVIDERS.map(([label]) => label)}
          columns={["plain", "badge", "accent badge", "status dot"]}
          render={(row, column) => {
            const [, driver, name] = PROVIDERS.find(([label]) => label === row) ?? PROVIDERS[0]!;
            return (
              <ProviderInstanceIcon
                driverKind={driver}
                displayName={`${name} Work`}
                showBadge={column === "badge" || column === "accent badge"}
                {...(column === "accent badge" ? { accentColor: "#7c3aed" } : {})}
                {...(column === "status dot" ? { statusDotClassName: "bg-success" } : {})}
              />
            );
          }}
        />
      ),
    },
  ],
};

const faviconImage: GalleryDoc = {
  meta: {
    name: "Favicon image",
    kind: "primitive",
    group: "Icons",
    source: "src/components/preview/PreviewFaviconIcon.tsx",
  },
  specimens: [
    {
      id: "sources",
      title: "First loadable source, else the fallback",
      render: () => (
        <Row>
          <Cell label="loads">
            <FaviconImage sources={[APP_ICON]} fallback={<ExternalLinkIcon className="size-3" />} className="size-4" />
          </Cell>
          <Cell label="no source">
            <FaviconImage
              sources={[null]}
              fallback={<ExternalLinkIcon className="size-4 text-muted-foreground" />}
              className="size-4"
            />
          </Cell>
          <Cell label="first fails">
            <FaviconImage
              sources={["data:image/png;base64,broken", CHART]}
              fallback={<TriangleAlertIcon className="size-4" />}
              className="size-4"
            />
          </Cell>
        </Row>
      ),
    },
  ],
};

export const SHARED: Readonly<Record<string, GalleryDoc>> = {
  "thread-error-banner": threadErrorBanner,
  "provider-status-banner": providerStatusBanner,
  "composer-notices": composerNotices,
  "diff-stat-label": diffStatLabel,
  "changed-files-card": changedFilesCard,
  "message-copy-button": messageCopyButton,
  "skill-inline-text": skillInlineText,
  "terminal-context-chip": terminalContextChip,
  "assistant-citation-chip": assistantCitationChip,
  "pull-request-link-preview": pullRequestLinkPreview,
  "expanded-image-dialog": expandedImageDialog,
  "snapshot-attachment-details": snapShotAttachmentDetails,
  "media-video-player": mediaVideoPlayer,
  "media-actions": mediaActions,
  "panel-layout-controls": panelLayoutControls,
  "thread-command-subtitle": threadCommandSubtitle,
  "open-in-picker": openInPicker,
  "project-scripts-control": projectScriptsControl,
  "sidebar-update-pill": sidebarUpdatePill,
  "animated-height": animatedHeight,
  "render-error-boundary": renderErrorBoundary,
  "brand-icons": brandIcons,
  "morph-icon": morphIcon,
  "environment-machine-icon": environmentMachineIcon,
  "file-entry-icon": fileEntryIcon,
  "provider-instance-icon": providerInstanceIcon,
  "favicon-image": faviconImage,
};
