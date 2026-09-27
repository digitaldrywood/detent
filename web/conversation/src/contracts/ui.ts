// UI contract types used by the Detent conversation client.
// Hub payloads live in conversation.ts and work.ts. The app adapters map
// those payloads into these component-facing types.
import * as Effect from "effect/Effect";
import * as Schema from "effect/Schema";
import * as SchemaTransformation from "effect/SchemaTransformation";

/** The conversation identifier used by UI components. */
export type ThreadId = string & { readonly ThreadId?: unique symbol };
/** A hub project identifier. */
export type ProjectId = string & { readonly ProjectId?: unique symbol };
/** The configured provider instance that produced a turn. */
export type ProviderInstanceId = string & { readonly ProviderInstanceId?: unique symbol };

// Constructors retain the component API while identifiers remain plain strings.
export const ThreadId = { make: (value: string): ThreadId => value };
export const ProjectId = { make: (value: string): ProjectId => value };
export const ProviderInstanceId = { make: (value: string): ProviderInstanceId => value };
/** `editors.ts`: which local editor an Open-In entry launches. */
/**
 * `editor.ts`: which local editor an Open-In entry launches.
 *
 * The value half is `Schema.Literals(EDITORS.map(…))`, declared beside
 * `EDITORS` further down because it is derived from it. It is a schema rather
 * than a transcribed type because the copied `editorPreferences.ts` decodes a
 * stored editor id out of local storage with it, and a value written by an
 * older build — or by hand — has to be rejected rather than trusted.
 */
export type EditorId = (typeof EDITORS)[number]["id"];

/** `orchestration.ts`. */
export type ProjectScriptIcon = string;

/** `orchestration.ts`: one project script, as the header's action menu lists it. */
export interface ProjectScript {
  readonly id: string;
  readonly name: string;
  readonly command: string;
  readonly icon: ProjectScriptIcon;
  readonly runOnWorktreeCreate: boolean;
  readonly previewUrl?: string;
  readonly autoOpenPreview?: boolean;
}

export interface ProjectFileScript {
  readonly name: string;
  readonly command: string;
  readonly icon?: ProjectScriptIcon | undefined;
  readonly runOnWorktreeCreate?: boolean | undefined;
  readonly previewUrl?: string | undefined;
  readonly autoOpenPreview?: boolean | undefined;
}

/** `keybindings.ts`: the id length a `script.<id>.run` command may carry. */
export const MAX_SCRIPT_ID_LENGTH = 24;

/** `keybindings.ts`: the chord string length a keybinding rule may carry. */
export const MAX_KEYBINDING_VALUE_LENGTH = 64;

/**
 * `keybindings.ts`: the shape of a per-script run command, verbatim.
 *
 * This is one of the two schemas in this file with a value half (the other is
 * `AssistantCitation`), and for the same reason: the copied `projectScripts.ts`
 * asks it whether a script id can carry a shortcut at all, and reads its
 * `parts` back to recover the id from a command. A transcribed type could
 * answer neither question.
 */
export const SCRIPT_RUN_COMMAND_PATTERN = Schema.TemplateLiteral([
  Schema.Literal("script."),
  Schema.NonEmptyString.check(
    Schema.isMaxLength(MAX_SCRIPT_ID_LENGTH),
    Schema.isPattern(/^[a-z0-9][a-z0-9-]*$/),
  ),
  Schema.Literal(".run"),
]);

/**
 * `baseSchemas.ts`' `TrimmedString`, verbatim. Only `KeybindingRule` below
 * needs it, and it needs it exactly: upstream's own test for the project
 * script dialog decodes `"  mod+k  "` and expects `"mod+k"` back.
 */
const TrimmedString = Schema.String.pipe(
  Schema.decodeTo(
    Schema.String,
    SchemaTransformation.transformOrFail({
      decode: (value: string) => Effect.succeed(value.trim()),
      encode: (value: string) => Effect.succeed(value.trim()),
    }),
  ),
);

/**
 * `keybindings.ts`: one keybinding rule, as the project script dialog decodes
 * the chord a reader pressed.
 *
 * Upstream's `command`
 * is `Schema.Union([Schema.Literals(STATIC_KEYBINDING_COMMANDS),
 * SCRIPT_RUN_COMMAND_PATTERN])`; Detent's `KeybindingCommand` is an open
 * string (see its own note above), and the static list lives in
 * `app/adapters/keybindings.ts` rather than here. The only decoder of this
 * schema is `lib/projectScriptKeybindings.ts`, whose command is always a
 * `script.<id>.run`, so `command` is narrowed to that pattern — which keeps
 * the one behaviour upstream's own test asks of it, that `script.BAD.run` is
 * rejected rather than stored.
 */
export const KeybindingRule = Schema.Struct({
  key: TrimmedString.check(
    Schema.isMinLength(1),
    Schema.isMaxLength(MAX_KEYBINDING_VALUE_LENGTH),
  ),
  command: SCRIPT_RUN_COMMAND_PATTERN,
  when: Schema.optional(TrimmedString.check(Schema.isMinLength(1), Schema.isMaxLength(256))),
});
export type KeybindingRule = typeof KeybindingRule.Type;

/** `orchestration.ts`: the palette `ProjectFavicon` tints a lucide mark with. */
export type ProjectIconColor =
  | "gray"
  | "red"
  | "orange"
  | "amber"
  | "yellow"
  | "lime"
  | "green"
  | "emerald"
  | "teal"
  | "cyan"
  | "sky"
  | "blue"
  | "indigo"
  | "violet"
  | "purple"
  | "fuchsia"
  | "pink"
  | "rose";

/** `orchestration.ts`: a project's saved icon override. */
export type ProjectIconOverride =
  | { readonly kind: "emoji"; readonly emoji: string }
  | { readonly kind: "lucide"; readonly name: string; readonly color: ProjectIconColor };

/** `ipc.ts`: an entry in the desktop context menu. Never built in a browser. */
export interface ContextMenuItem<T extends string = string> {
  id: T;
  label: string;
  destructive?: boolean;
  disabled?: boolean;
  header?: boolean;
  icon?: string;
  separatorBefore?: boolean;
  children?: readonly ContextMenuItem<T>[];
}

/** `preview.ts`: where a browser surface has navigated to. */
export type PreviewNavStatus =
  | { readonly _tag: "Idle" }
  | { readonly _tag: "Navigated"; readonly url: string; readonly title: string };

/** `preview.ts`: one live browser surface. Detent opens none. */
export interface PreviewSessionSnapshot {
  readonly threadId: string;
  readonly tabId: string;
  readonly navStatus: PreviewNavStatus;
  readonly canGoBack: boolean;
  readonly canGoForward: boolean;
  readonly profileId?: string;
  readonly updatedAt: string;
}

/** `pullRequest.ts`. */
export type PullRequestState = "open" | "closed" | "merged";
/** `pullRequest.ts`: how a change request lands on its base branch. */
export type PullRequestMergeMethod = "merge" | "squash" | "rebase";
export type PullRequestMergeability = "mergeable" | "conflicting" | "unknown";
export type PullRequestChecksState = "passing" | "failing" | "pending";
export type PullRequestCheckStatus =
  | "pending"
  | "action-required"
  | "success"
  | "failure"
  | "skipped"
  | "neutral"
  | "cancelled";

export interface PullRequestActor {
  readonly login: string;
  readonly name: string | null;
  readonly avatarUrl: string | null;
}

export interface PullRequestCheck {
  readonly name: string;
  readonly status: PullRequestCheckStatus;
  readonly description: string | null;
  readonly url: string | null;
}

/** `keybindings.ts`: the resolved chord table a control labels itself from. */
export interface ResolvedKeybindingsConfig {
  readonly bindings: Readonly<Record<string, readonly string[]>>;
}

/**
 * `keybindings.ts`: the nine positional jump commands the command palette
 * hands to its first nine rows. Copied verbatim from
 * `packages/contracts/src/keybindings.ts`.
 */
export const THREAD_JUMP_KEYBINDING_COMMANDS = [
  "thread.jump.1",
  "thread.jump.2",
  "thread.jump.3",
  "thread.jump.4",
  "thread.jump.5",
  "thread.jump.6",
  "thread.jump.7",
  "thread.jump.8",
  "thread.jump.9",
] as const;
export type ThreadJumpKeybindingCommand = (typeof THREAD_JUMP_KEYBINDING_COMMANDS)[number];

export type KeybindingCommand = string;

/** `filesystem.ts`: one entry of a browsed directory. */
export interface FilesystemBrowseEntry {
  readonly name: string;
  readonly fullPath: string;
}

/**
 * `providerInstance.ts`: which harness produced a turn. Upstream this is an
 * Effect-branded slug, so a caller writes `ProviderDriverKind.make(value)`;
 * the schema is not vendored, so the name carries a value half with the same
 * `make` and nothing else — enough for the copied
 * `chat/providerIconUtils.ts` to key its icon table as it does upstream.
 */
export type ProviderDriverKind = string & { readonly ProviderDriverKind?: unique symbol };
export const ProviderDriverKind = { make: (value: string): ProviderDriverKind => value };

/** `providerInstance.ts`: the default instance id for a driver is its slug. */
export const defaultInstanceIdForDriver = (driver: ProviderDriverKind): ProviderInstanceId =>
  driver as string as ProviderInstanceId;

/** `model.ts`: the brand label each driver kind wears. */
export const PROVIDER_DISPLAY_NAMES: Partial<Record<ProviderDriverKind, string>> = {
  antigravity: "Antigravity",
  codex: "Codex",
  claudeAgent: "Claude",
  cursor: "Cursor",
  grok: "Grok",
  opencode: "OpenCode",
};

/** `server.ts`: one configured provider instance a server reports. */
export type ServerProviderState =
  | "ready"
  | "installing"
  | "error"
  | "unknown"
  | "warning"
  | "disabled";

/**
 * `baseSchemas.ts`: the identifier of one pending request a provider raised.
 * Detent's is the question id the conversation stream carries (§5).
 */
export type ApprovalRequestId = string & { readonly ApprovalRequestId?: unique symbol };
export const ApprovalRequestId = { make: (value: string): ApprovalRequestId => value };

/** `providerRuntime.ts`: one option of a question a turn asked. */
export interface UserInputQuestionOption {
  readonly label: string;
  readonly description: string;
  readonly value?: string;
}

/**
 * `providerRuntime.ts`: one question a turn asked, as
 * `ComposerPendingUserInputPanel` draws it. Detent's questions decode into
 * this shape in `src/app/adapters/pendingQuestions.ts`.
 */
export interface UserInputQuestion {
  readonly id: string;
  readonly header: string;
  readonly question: string;
  readonly options: ReadonlyArray<UserInputQuestionOption>;
  readonly allowCustomAnswer?: boolean;
  readonly multiSelect?: boolean;
}

export interface ServerProviderAuth {
  readonly status: "authenticated" | "unauthenticated" | "unknown";
  readonly email?: string;
  readonly label?: string;
}

export interface ServerProviderSetup {
  readonly canAuthenticate?: boolean;
  readonly canInstall?: boolean;
}

export interface ServerProvider {
  readonly instanceId: ProviderInstanceId;
  readonly driver: ProviderDriverKind;
  readonly status: ServerProviderState;
  readonly installed: boolean;

  readonly enabled?: boolean | undefined;
  readonly auth: ServerProviderAuth;
  readonly displayName?: string | undefined;
  readonly message?: string | undefined;
  readonly setup?: ServerProviderSetup | undefined;
  readonly accentColor?: string | undefined;
  /**
   * The skills and slash commands a provider advertises, and the per-workspace
   * snapshots of both. Upstream defaults the two arrays to empty on decode and
   * leaves `workspaceSnapshots` absent, which is what the copied
   * `runtime/providerSkills.ts` is written to tolerate — and what a Detent
   * provider always is, because the hub publishes no provider inventory to
   * this client (see `state/server.ts`).
   */
  readonly slashCommands?: ReadonlyArray<ServerProviderSlashCommand> | undefined;
  readonly skills?: ReadonlyArray<ServerProviderSkill> | undefined;
  readonly workspaceSnapshots?: ReadonlyArray<ServerProviderWorkspaceSnapshot> | undefined;
}

/** `server.ts`: the skills and commands one workspace reported. */
export interface ServerProviderWorkspaceSnapshot {
  readonly cwd: string;
  readonly checkedAt: string;
  readonly slashCommands: ReadonlyArray<ServerProviderSlashCommand>;
  readonly skills: ReadonlyArray<ServerProviderSkill>;
}

/** One rolling quota window. */
export interface ServerProviderUsageWindow {
  readonly id: string;
  readonly kind: "session" | "weekly" | "monthly" | "other";
  readonly label: string;
  readonly usedPercent: number;
  readonly resetsAt?: string;
  readonly windowDurationMins?: number;
}

/**
 * Reset credits a provider banks on an account. Detent's entitlement has no
 * such thing, so nothing ever sets this and the redeem control stays unbuilt.
 */
export interface ServerProviderResetCredits {
  readonly availableCount: number;
  readonly nextExpiresAt?: string;
  readonly nextCreditId?: string;
}

/** Subscription usage as a provider knows it for one account. */
export interface ServerProviderUsageLimits {
  readonly checkedAt: string;
  readonly windows: ReadonlyArray<ServerProviderUsageWindow>;
  readonly resetCredits?: ServerProviderResetCredits;
  readonly unavailable?: {
    readonly reason: "unsupported" | "probeFailed";
    readonly message?: string;
  };
}

/** Where a reset credit would be redeemed. Detent never builds one. */
export type ProviderConsumeResetCreditInput =
  | { readonly instanceId: ProviderInstanceId }
  | { readonly sourceId: string; readonly accountId: string; readonly creditId: string };

/** A point-in-time view of one account's limits, as the composer banner draws it. */
export interface UsageLimitsReport {
  readonly createdAt: string;
  readonly accounts: ReadonlyArray<{
    readonly id: string;
    readonly driver: ProviderDriverKind;
    readonly label: string;
    readonly plan?: string;
    readonly email?: string;
    readonly sourceLabel?: string;
    readonly instanceId?: ProviderInstanceId;
    readonly resetCreditInput?: ProviderConsumeResetCreditInput;
    readonly displayName?: string;
    readonly accentColor?: string;
    readonly limits: ServerProviderUsageLimits;
  }>;
  readonly notices: ReadonlyArray<string>;
}

// `ipc.ts`: the browser-preview picking and annotation payloads (§18.7).
// Transcribed field for field so `lib/elementContext.ts`,
// `lib/previewAnnotation.ts` and `lib/terminalContext.ts` — and through them
// the byte-identical `chat/composerPromptHistory.ts` — stay unedited. Detent's
// preview surface does not pick or annotate yet, so nothing builds one; the
// prompt-history helpers that strip these blocks off a recalled prompt are
// then no-ops rather than missing code.

/** One frame of the owner stack a picked element carries. */
export interface PickedElementStackFrame {
  functionName: string | null;
  fileName: string | null;
  lineNumber: number | null;
  columnNumber: number | null;
}

/** An element picked out of the preview webview. Every field is best-effort. */
export interface PickedElementPayload {
  pageUrl: string;
  pageTitle: string | null;
  tagName: string;
  selector: string | null;
  htmlPreview: string;
  componentName: string | null;
  source: PickedElementStackFrame | null;
  stack: ReadonlyArray<PickedElementStackFrame>;
  styles: string;
  pickedAt: string;
}

export interface PreviewAnnotationRect {
  x: number;
  y: number;
  width: number;
  height: number;
}

export interface PreviewAnnotationPoint {
  x: number;
  y: number;
}

export interface PreviewAnnotationElementTarget {
  id: string;
  element: PickedElementPayload;
  rect: PreviewAnnotationRect;
}

export interface PreviewAnnotationRegionTarget {
  id: string;
  rect: PreviewAnnotationRect;
}

export interface PreviewAnnotationStrokeTarget {
  id: string;
  color: string;
  width: number;
  points: ReadonlyArray<PreviewAnnotationPoint>;
  bounds: PreviewAnnotationRect;
}

export interface PreviewAnnotationStyleChange {
  targetId: string;
  selector: string | null;
  property: string;
  previousValue: string;
  value: string;
}

export interface PreviewAnnotationScreenshot {
  dataUrl: string;
  width: number;
  height: number;
  cropRect: PreviewAnnotationRect;
}

/** A submitted preview annotation: elements, regions, ink and a screenshot. */
export interface PreviewAnnotationPayload {
  id: string;
  pageUrl: string;
  pageTitle: string | null;
  comment: string;
  elements: ReadonlyArray<PreviewAnnotationElementTarget>;
  regions: ReadonlyArray<PreviewAnnotationRegionTarget>;
  strokes: ReadonlyArray<PreviewAnnotationStrokeTarget>;
  styleChanges: ReadonlyArray<PreviewAnnotationStyleChange>;
  screenshot: PreviewAnnotationScreenshot | null;
  createdAt: string;
}

// ---------------------------------------------------------------------------
// The thread shell the copied `components/Sidebar.tsx` and its modules read.
//
// Transcribed from `packages/contracts/src/{orchestration,environment,git,
// sourceControl}.ts` at the pinned commit, as plain TypeScript, in upstream
// field order. Nothing here decodes a wire body: `src/app/adapters/models.ts`
// is where a Detent `Conversation` becomes one of these.
// ---------------------------------------------------------------------------

/** `orchestration.ts`: the identifier of one model turn. */
export type TurnId = string & { readonly TurnId?: unique symbol };
/**
 * `orchestration.ts`: the identifier of one message. Named for its package
 * because the hub has a `MessageId` of its own (`conversation.ts`), and that
 * one is what every payload in this client carries.
 */
export type OrchestrationMessageId = string & { readonly OrchestrationMessageId?: unique symbol };
/** `orchestration.ts`: the identifier of one dispatched command. */
export type CommandId = string & { readonly CommandId?: unique symbol };

/** `orchestration.ts`: which model a turn runs under, and on which instance. */
export interface ModelSelection {
  readonly instanceId: ProviderInstanceId;
  readonly model: string;
  readonly options?: Readonly<Record<string, unknown>> | undefined;
}

/** `orchestration.ts`: how much a turn may do without asking. */
export type RuntimeMode = "approval-required" | "auto-accept-edits" | "auto" | "full-access";
export const DEFAULT_RUNTIME_MODE: RuntimeMode = "full-access";

/** `orchestration.ts`: which interaction mode a provider runs in. */
export type ProviderInteractionMode = string;
export const DEFAULT_PROVIDER_INTERACTION_MODE: ProviderInteractionMode = "default";

/** `orchestration.ts`. */
export type OrchestrationSessionStatus =
  | "idle"
  | "starting"
  | "running"
  | "ready"
  | "interrupted"
  | "stopped"
  | "error";

/** `orchestration.ts`: the live session a thread's turns run inside. */
export interface OrchestrationSession {
  readonly threadId: ThreadId;
  readonly status: OrchestrationSessionStatus;
  readonly providerName: string | null;
  readonly providerInstanceId?: ProviderInstanceId | undefined;
  readonly runtimeMode: RuntimeMode;
  readonly activeTurnId: TurnId | null;
  readonly lastError: string | null;
  readonly updatedAt: string;
}

/** `orchestration.ts`. */
export type OrchestrationLatestTurnState = "running" | "interrupted" | "completed" | "error";

/** `orchestration.ts`: the most recent turn, as the sidebar row reads it. */
export interface OrchestrationLatestTurn {
  readonly turnId: TurnId;
  readonly state: OrchestrationLatestTurnState;
  readonly requestedAt: string;
  readonly startedAt: string | null;
  readonly completedAt: string | null;
  readonly assistantMessageId: OrchestrationMessageId | null;
  readonly sourceProposedPlan?: unknown;
}

/** `orchestration.ts`: a title regeneration in flight. */
export interface ThreadTitleRegeneration {
  readonly requestId: CommandId;
  readonly startedAt: string;
}

/** `orchestration.ts`: the change request a thread's branch belongs to. */
export interface ThreadLinkedPullRequest {
  readonly projectId: ProjectId;
  readonly repository: string;
  readonly number: number;
  readonly url: string;
}

export interface OrchestrationThreadShell {
  readonly id: ThreadId;
  readonly projectId: ProjectId;
  readonly title: string;
  readonly modelSelection: ModelSelection;
  readonly runtimeMode: RuntimeMode;
  readonly interactionMode: ProviderInteractionMode;
  readonly branch: string | null;
  readonly worktreePath: string | null;
  readonly linkedPullRequest?: ThreadLinkedPullRequest | null | undefined;
  readonly branchPullRequest?: ThreadLinkedPullRequest | null | undefined;
  readonly latestTurn: OrchestrationLatestTurn | null;
  readonly createdAt: string;
  readonly updatedAt: string;
  readonly archivedAt: string | null;
  readonly settledOverride: "settled" | "active" | null;
  readonly settledAt: string | null;
  readonly unsettledAt?: string | null | undefined;
  readonly snoozedUntil?: string | null | undefined;
  readonly snoozedAt?: string | null | undefined;
  readonly pinnedAt?: string | null | undefined;
  readonly pinOrderKey?: string | null | undefined;
  readonly activeOrderKey?: string | null | undefined;
  readonly titleRegeneration?: ThreadTitleRegeneration | null | undefined;
  readonly session: OrchestrationSession | null;
  readonly latestUserMessageAt: string | null;
  readonly hasPendingApprovals: boolean;
  readonly hasPendingUserInput: boolean;
  readonly hasActionableProposedPlan: boolean;
  readonly backgroundLiveness?: "working" | "monitoring" | null | undefined;
  readonly planProgress?:
    | {
        readonly step: string;
        readonly completedSteps: number;
        readonly totalSteps: number;
      }
    | null
    | undefined;
}

/** `orchestration.ts`: the project shell `ProjectFavicon` and the rows read. */
export interface OrchestrationProjectShell {
  readonly id: ProjectId;
  readonly title: string;
  readonly workspaceRoot: string;

  readonly repositoryIdentity?: RepositoryIdentity | null | undefined;
  readonly faviconPath?: string | null | undefined;
  readonly projectIcon?: ProjectIconOverride | null | undefined;
  readonly defaultModelSelection?: ModelSelection | null | undefined;
  readonly scripts?: readonly ProjectScript[] | undefined;
  readonly createdAt: string;
  readonly updatedAt: string;
}

/** `environment.ts`: how a project's repository is named across checkouts. */
export interface RepositoryIdentityLocator {
  readonly source: "git-remote";
  readonly remoteName: string;
  readonly remoteUrl: string;
}

export interface RepositoryIdentity {
  readonly canonicalKey: string;
  readonly locator: RepositoryIdentityLocator;
  readonly rootPath?: string;
  readonly displayName?: string;
  readonly provider?: string;
  readonly owner?: string;
  readonly name?: string;
}

/** `environment.ts`: the hardware an environment runs on. */
export const ENVIRONMENT_MACHINE_KINDS = [
  "server",
  "cloud",
  "linux",
  "desktop",
  "laptop",
  "mac-mini",
  "mac-studio",
] as const;
export type EnvironmentMachineKind = (typeof ENVIRONMENT_MACHINE_KINDS)[number];

export function resolveEnvironmentMachineKind(
  serverConfig:
    | { readonly environment?: { readonly machine?: EnvironmentMachineKind } }
    | null
    | undefined,
): EnvironmentMachineKind {
  return serverConfig?.environment?.machine ?? "server";
}

/** `sourceControl.ts`. */
export type SourceControlProviderKind =
  | "github"
  | "gitlab"
  | "azure-devops"
  | "bitbucket"
  | "unknown";

/** `sourceControl.ts`. */
export interface SourceControlProviderInfo {
  readonly kind: SourceControlProviderKind;
  readonly name: string;
  readonly baseUrl: string;
}

export type GitStackedAction = "commit" | "push" | "create_pr" | "commit_push" | "commit_push_pr";

/** `git.ts`: one change request as a working-copy status reports it. */
export interface VcsStatusChangeRequest {
  readonly number: number;
  readonly title: string;
  readonly url: string;
  readonly baseRef: string;
  readonly headRef: string;
  readonly state: PullRequestState;
  readonly isDraft?: boolean | undefined;
  readonly updatedAt?: string | null | undefined;
}

export interface VcsStatusResult {
  readonly isRepo: boolean;
  readonly sourceControlProvider?: SourceControlProviderInfo | undefined;
  readonly hasPrimaryRemote: boolean;
  readonly isDefaultRef: boolean;
  readonly refName: string | null;
  readonly hasWorkingTreeChanges: boolean;
  readonly workingTree: {
    readonly files: readonly {
      readonly path: string;
      readonly insertions: number;
      readonly deletions: number;
    }[];
    readonly insertions: number;
    readonly deletions: number;
  };
  readonly hasUpstream: boolean;
  readonly aheadCount: number;
  readonly behindCount: number;
  readonly aheadOfDefaultCount?: number | undefined;
  readonly pr: VcsStatusChangeRequest | null;
}

// ---------------------------------------------------------------------------
// `assets.ts`: the resources a client can ask an environment to sign a URL for.
//
// Detent signs none of them — `state/assets.ts` says why — but the copied
// media pipeline (`chat/ExpandedImagePreview.tsx`, `runtime/mediaSource.ts`,
// `assets/assetUrls.ts`) is written against these shapes, so they are
// transcribed here as plain TypeScript from the same upstream file.
// ---------------------------------------------------------------------------

/** `assets.ts`: pixel size read from an image header. */
export interface AssetImageDimensions {
  readonly width: number;
  readonly height: number;
}

/** `assets.ts`: what a signed asset URL can point at. */
export type AssetResource =
  | { readonly _tag: "workspace-file"; readonly threadId: ThreadId; readonly path: string }
  | { readonly _tag: "media-file"; readonly threadId: ThreadId; readonly path: string }
  | {
      readonly _tag: "attachment";
      readonly attachmentId: string;
      readonly fileName?: string;
      readonly mimeType?: string;
      readonly disposition?: "inline" | "attachment";
    }
  | { readonly _tag: "project-favicon"; readonly cwd: string; readonly path?: string }
  | { readonly _tag: "native-app-icon"; readonly app: unknown };

/** `assets.ts`: what the environment answers a sign request with. */
export interface AssetCreateUrlResult {
  readonly relativeUrl: string;
  readonly expiresAt: number;
  readonly sourcePath?: string;
  readonly imageDimensions?: AssetImageDimensions;
}

/** `orchestration.ts`: the identifier of one attachment. */
export type ChatAttachmentId = string;

/** `orchestration.ts`: where one node of a captured window sits on screen. */
export interface SnapShotAccessibilityBounds {
  readonly x: number;
  readonly y: number;
  readonly width: number;
  readonly height: number;
}

/** `orchestration.ts`: what a captured node's controls were doing. */
export interface SnapShotAccessibilityState {
  readonly active?: boolean;
  readonly busy?: boolean;
  readonly checked?: "on" | "off" | "mixed";
  readonly editable?: boolean;
  readonly enabled?: boolean;
  readonly expanded?: boolean;
  readonly focused?: boolean;
  readonly selected?: boolean;
  readonly visible?: boolean;
}

/** `orchestration.ts`: one node of a captured window's accessibility tree. */
export interface SnapShotAccessibilityNode {
  readonly role: string;
  readonly name?: string;
  readonly value?: string;
  readonly description?: string;
  readonly bounds: SnapShotAccessibilityBounds | null;
  readonly state?: SnapShotAccessibilityState;
  readonly actions?: Array<string>;
  readonly children: Array<SnapShotAccessibilityNode>;
}

/** `orchestration.ts`: the readable content of a captured window. */
export type SnapShotAccessibility =
  | { readonly format: "flat-text"; readonly text: string; readonly truncated: boolean }
  | {
      readonly format: "element-tree";
      readonly coordinateSpace: "captured-image";
      readonly imageSize: { readonly width: number; readonly height: number };
      readonly truncated: boolean;
      readonly root: SnapShotAccessibilityNode;
    };

export interface SnapShotSource {
  readonly kind: "snap-shot";
  readonly capturedAt: string;
  readonly appName: string;
  readonly windowTitle: string;
  readonly accessibleText?: string;
  readonly accessibility?: SnapShotAccessibility;
  readonly appIdentifier?: string;
  readonly appIconDataUrl?: string;
}

/** `orchestration.ts`: an image sent with a message. */
export interface ChatImageAttachment {
  readonly type: "image";
  readonly id: ChatAttachmentId;
  readonly name: string;
  readonly mimeType: string;
  readonly sizeBytes: number;
  readonly source?: SnapShotSource;
}

/** `orchestration.ts`: a non-image file sent with a message. */
export interface ChatFileAttachment {
  readonly type: "file";
  readonly id: ChatAttachmentId;
  readonly name: string;
  readonly mimeType: string;
  readonly sizeBytes: number;
}

/**
 * `orchestration.ts`: the catch-all. Attachment types this build does not know
 * decode with the shared base fields rather than failing the whole message, so
 * a newer server cannot break an older reader.
 */
export interface ChatUnknownAttachment {
  readonly type: string;
  readonly id: ChatAttachmentId;
  readonly name: string;
  readonly mimeType: string;
  readonly sizeBytes: number;
}

/** `orchestration.ts`: who wrote a message. */
export type OrchestrationMessageRole = "user" | "assistant" | "system";

/** `orchestration.ts`: one message in a transcript. */
export interface OrchestrationMessage {
  readonly id: string;
  readonly role: OrchestrationMessageRole;
  readonly text: string;
  readonly attachments?: ReadonlyArray<
    ChatImageAttachment | ChatFileAttachment | ChatUnknownAttachment
  >;
  readonly turnId: TurnId | null;
  readonly streaming: boolean;
  readonly createdAt: string;
  readonly updatedAt: string;
}

/** `orchestration.ts`: the identifier of one proposed plan. */
export type OrchestrationProposedPlanId = string;

/** `orchestration.ts`: a plan a turn proposed and a reader can implement. */
export interface OrchestrationProposedPlan {
  readonly id: OrchestrationProposedPlanId;
  readonly turnId: TurnId | null;
  readonly planMarkdown: string;
  readonly implementedAt: string | null;
  readonly implementationThreadId: ThreadId | null;
  readonly createdAt: string;
  readonly updatedAt: string;
}

/** `orchestration.ts`: one file in a turn's checkpoint diff. */
export interface OrchestrationCheckpointFile {
  readonly path: string;
  readonly kind: string;
  readonly additions: number;
  readonly deletions: number;
}

/** `orchestration.ts`: whether a checkpoint can still be read. */
export type OrchestrationCheckpointStatus = "ready" | "missing" | "error";

/** `orchestration.ts`: what one turn changed. */
export interface OrchestrationCheckpointSummary {
  readonly turnId: TurnId;
  readonly checkpointTurnCount: number;
  readonly checkpointRef: string;
  readonly status: OrchestrationCheckpointStatus;
  readonly files: ReadonlyArray<OrchestrationCheckpointFile>;
  readonly assistantMessageId: string | null;
  readonly completedAt: string;
}

/** `orchestration.ts`: the colour a work-log row takes. */
export type OrchestrationThreadActivityTone = "info" | "tool" | "approval" | "error" | "thinking";

/**
 * `orchestration.ts`: one entry in a turn's work log. `kind` is deliberately
 * open upstream so a newer server can add an activity without breaking an
 * older reader, and the copied row components branch on it as a string.
 */
export interface OrchestrationThreadActivity {
  readonly id: string;
  readonly kind: string;
  readonly tone: OrchestrationThreadActivityTone;
  readonly turnId: TurnId | null;
  readonly label: string;
  readonly detail?: string;
  readonly payload?: unknown;
  readonly createdAt: string;
}

// --- The tool-activity vocabulary the work rows are drawn from. ---

/** `providerRuntime.ts`: which surface a tool acted on. */
export type ToolActivitySurface = "browser" | "computer";

/** `providerRuntime.ts`: a native app a tool referenced, for its icon. */
export type ToolActivityNativeAppReference =
  | { readonly _tag: "app-id"; readonly appId: string }
  | { readonly _tag: "display-name"; readonly displayName: string };

/** `providerRuntime.ts`: which glyph a tool row takes. */
export type ToolActivityIcon =
  | {
      readonly _tag: "website";
      readonly pageUrl: string;
      readonly faviconUrl?: string | undefined;
      readonly faviconUrlDark?: string | undefined;
    }
  | { readonly _tag: "native-app"; readonly app: ToolActivityNativeAppReference }
  | {
      readonly _tag: "themed-logo";
      readonly logoUrl: string;
      readonly logoUrlDark?: string | undefined;
    };

/** `providerRuntime.ts`: where a tool row's content came from. */
export interface ToolActivitySource {
  readonly key: string;
  readonly name: string;
  readonly kind: "browser" | "computer" | "integration";
  readonly icon?: ToolActivityIcon | undefined;
}

/** `providerRuntime.ts`: the runtime item kinds that carry a tool lifecycle. */
const TOOL_LIFECYCLE_ITEM_TYPES = [
  "command_execution",
  "file_change",
  "mcp_tool_call",
  "dynamic_tool_call",
  "collab_agent_tool_call",
  "web_search",
  "image_view",
] as const;

export type ToolLifecycleItemType = (typeof TOOL_LIFECYCLE_ITEM_TYPES)[number];

/** `providerRuntime.ts`, verbatim: the guard the work log narrows with. */
export function isToolLifecycleItemType(value: string): value is ToolLifecycleItemType {
  return TOOL_LIFECYCLE_ITEM_TYPES.includes(value as ToolLifecycleItemType);
}

/** `providerRuntime.ts`: how far a runtime item has got. */
export type RuntimeItemStatus =
  | "inProgress"
  | "completed"
  | "failed"
  | "declined"
  | "stopped";

/** `server.ts`: one skill a provider advertises. */
export interface ServerProviderSkill {
  readonly name: string;
  readonly description?: string;
  readonly path: string;
  readonly scope?: string;
  readonly enabled: boolean;
  readonly displayName?: string;
  readonly shortDescription?: string;
  /**
   * The skill is hidden from the agent's own skill tool, so only the user can
   * start it — Claude Code's `disable-model-invocation`. Composers must offer
   * it as a slash command; naming it in prose does nothing.
   */
  readonly userInvocationOnly?: boolean;
  /**
   * The mirror of `userInvocationOnly`: Claude Code's `user-invocable: false`
   * keeps the skill out of its own slash commands, so only the agent can start
   * it. Composers must not offer it under `/`.
   */
  readonly userInvocable?: boolean;
}

/** `server.ts`: one slash command a provider advertises. */
export interface ServerProviderSlashCommand {
  readonly name: string;
  readonly description?: string;
}

/** `orchestration.ts`: what a reader answered, keyed by question id. */
export type ProviderUserInputAnswers = Readonly<Record<string, unknown>>;

/** `orchestration.ts`: the files a reader attached, keyed by question id. */
export type UserInputAttachments = Readonly<
  Record<string, ReadonlyArray<ChatImageAttachment | ChatFileAttachment>>
>;

/** `orchestration.ts`: the answer payload a user-input activity carries. */
export interface UserInputAttachmentAnswerPayload {
  readonly requestId: string;
  readonly questionTextById?: Readonly<Record<string, string>>;
  readonly answers: ProviderUserInputAnswers;
  readonly attachmentsByQuestionId: UserInputAttachments;
}

// --- Assistant citations: a reader quoting a span of a turn's answer. ---

/** `assistantCitations.ts`: how much text one citation may carry. */
export const ASSISTANT_CITATION_MAX_TEXT_LENGTH = 8_000;
/** `assistantCitations.ts`: how long a comment on a citation may be. */
export const ASSISTANT_CITATION_MAX_COMMENT_LENGTH = 8_000;
/** `assistantCitations.ts`: how much surrounding text is kept for anchoring. */
export const ASSISTANT_CITATION_CONTEXT_LENGTH = 32;

/**
 * `assistantCitations.ts`: a quote of rendered assistant text with an optional
 * user comment. Positions are UTF-16 offsets, not Markdown offsets.
 */
export interface AssistantCitation {
  readonly version: 1;
  /** `EnvironmentId`, which `contracts/index.ts` declares as a plain string. */
  readonly environmentId: string;
  readonly threadId: ThreadId;
  readonly messageId: string;
  readonly text: string;
  readonly comment?: string;
  readonly start: number;
  readonly end: number;
  readonly prefix: string;
  readonly suffix: string;
}

/**
 * The decoder for the above — the one place in this slice that needs a real
 * schema rather than a transcribed type.
 *
 * A citation travels inside a markdown link in a message body, so it arrives
 * as URL-encoded text the client did not write. `runtime/support/
 * assistantCitations.ts` decodes it with `Schema.decodeUnknownOption`, and the
 * point of doing so is that a malformed or hostile one is rejected rather than
 * rendered. Declaring it as a type alone would drop that check, so the schema
 * is reproduced here with upstream's own bounds and its `end > start`
 * invariant.
 */
export const AssistantCitation = Schema.Struct({
  version: Schema.Literal(1),
  environmentId: Schema.String.check(Schema.isMaxLength(512)),
  threadId: Schema.String.check(Schema.isMaxLength(512)),
  messageId: Schema.String.check(Schema.isMaxLength(512)),
  text: Schema.String.check(
    Schema.isNonEmpty(),
    Schema.isMaxLength(ASSISTANT_CITATION_MAX_TEXT_LENGTH),
  ),
  comment: Schema.optional(
    Schema.String.check(Schema.isMaxLength(ASSISTANT_CITATION_MAX_COMMENT_LENGTH)),
  ),
  start: Schema.Number.check(Schema.isGreaterThanOrEqualTo(0)),
  end: Schema.Number.check(Schema.isGreaterThanOrEqualTo(0)),
  prefix: Schema.String.check(Schema.isMaxLength(ASSISTANT_CITATION_CONTEXT_LENGTH)),
  suffix: Schema.String.check(Schema.isMaxLength(ASSISTANT_CITATION_CONTEXT_LENGTH)),
}).check(
  Schema.makeFilter(
    (citation: {
      readonly end: number;
      readonly start: number;
      readonly text: string;
    }) => citation.end > citation.start && citation.text.trim().length > 0,
  ),
);

// --- Platform vocabulary the "open in …" menus read. ---

/** `environment.ts`: the operating system an environment runs. */
export type ExecutionEnvironmentPlatformOs = "darwin" | "linux" | "windows" | "unknown";

/** `environment.ts`: the processor an environment runs on. */
export type ExecutionEnvironmentPlatformArch = "arm64" | "x64" | "other";

/** `editor.ts`: which file manager a "Reveal in …" item names. */
export type FileManagerRevealKind = "finder" | "file-explorer" | "files";

export const EDITORS = [
  {
    id: "cursor",
    label: "Cursor",
    commands: ["cursor"],
    launchStyle: "goto",
    remoteScheme: "cursor",
  },
  { id: "trae", label: "Trae", commands: ["trae"], launchStyle: "goto" },
  { id: "kiro", label: "Kiro", commands: ["kiro"], baseArgs: ["ide"], launchStyle: "goto" },
  {
    id: "vscode",
    label: "VS Code",
    commands: ["code"],
    launchStyle: "goto",
    remoteScheme: "vscode",
  },
  {
    id: "vscode-insiders",
    label: "VS Code Insiders",
    commands: ["code-insiders"],
    launchStyle: "goto",
    remoteScheme: "vscode-insiders",
  },
  {
    id: "vscodium",
    label: "VSCodium",
    commands: ["codium"],
    launchStyle: "goto",
    remoteScheme: "vscodium",
  },
  { id: "zed", label: "Zed", commands: ["zed", "zeditor"], launchStyle: "direct-path" },
  { id: "antigravity", label: "Antigravity", commands: ["agy"], launchStyle: "goto" },
  { id: "idea", label: "IntelliJ IDEA", commands: ["idea"], launchStyle: "line-column" },
  { id: "aqua", label: "Aqua", commands: ["aqua"], launchStyle: "line-column" },
  { id: "clion", label: "CLion", commands: ["clion"], launchStyle: "line-column" },
  { id: "datagrip", label: "DataGrip", commands: ["datagrip"], launchStyle: "line-column" },
  { id: "dataspell", label: "DataSpell", commands: ["dataspell"], launchStyle: "line-column" },
  { id: "goland", label: "GoLand", commands: ["goland"], launchStyle: "line-column" },
  { id: "phpstorm", label: "PhpStorm", commands: ["phpstorm"], launchStyle: "line-column" },
  { id: "pycharm", label: "PyCharm", commands: ["pycharm"], launchStyle: "line-column" },
  { id: "rider", label: "Rider", commands: ["rider"], launchStyle: "line-column" },
  { id: "rubymine", label: "RubyMine", commands: ["rubymine"], launchStyle: "line-column" },
  { id: "rustrover", label: "RustRover", commands: ["rustrover"], launchStyle: "line-column" },
  { id: "webstorm", label: "WebStorm", commands: ["webstorm"], launchStyle: "line-column" },
  { id: "file-manager", label: "File Manager", commands: null, launchStyle: "direct-path" },
] as const;

/** `editor.ts`: upstream's `Schema.Literals(EDITORS.map((e) => e.id))`. */
export const EditorId = Schema.Literals(EDITORS.map((editor) => editor.id));

// --- Where a link opens, and what a browser tab would be opened with. ---

/**
 * `settings.ts`: where a link clicked inside a thread opens. Upstream `app`
 * means a tab in their embedded Chromium guest; this client is itself a
 * browser tab and has no guest to hand a URL to, so `browser/
 * browserLinkTarget.ts` only ever resolves `system` — the reader's own
 * browser, which is where they already are.
 */
export type BrowserLinkTarget = "system" | "app";
/** `settings.ts`, verbatim. */
export const DEFAULT_BROWSER_LINK_TARGET: BrowserLinkTarget = "system";

/** `browserProfile.ts`: which Chromium partition a preview tab opens under. */
export type BrowserProfileId = string;
/** `browserProfile.ts`, verbatim: the built-in partition tabs default to. */
export const DEFAULT_BROWSER_PROFILE_ID: BrowserProfileId = "default";

/** `preview.ts`: the pixel size of a browser tab's viewport. */
export interface PreviewViewportSize {
  readonly width: number;
  readonly height: number;
}

/**
 * `preview.ts`: the viewport a browser tab is displayed at — filling its panel,
 * a size the reader dragged to, or one of Chrome's device presets. Upstream the
 * preset id is a closed list of device names; here it stays a string, because
 * nothing in this client renders the device menu that mints one.
 */
export type PreviewViewportSetting =
  | { readonly _tag: "fill" }
  | ({ readonly _tag: "freeform" } & PreviewViewportSize)
  | ({ readonly _tag: "preset"; readonly presetId: string } & PreviewViewportSize);

/** `preview.ts`, verbatim: the viewport a tab is born at when nobody says. */
export const FILL_PREVIEW_VIEWPORT = { _tag: "fill" } as const satisfies PreviewViewportSetting;

/**
 * `preview.ts`: what opening a browser tab asks for.
 *
 * Upstream this crosses the wire to an Electron main process that owns a live
 * Chromium guest. Detent's Browser surface is snapshot-first — the runner
 * renders captures of the app under review and the hub serves them, because
 * nothing connects inbound to a customer machine (decisions.md §1, §18.7) —
 * so no path in this client ever sends one. The shape is transcribed so the
 * copied `browser/openFileInPreview.ts` stays byte-identical (§16).
 */
export interface PreviewOpenInput {
  readonly threadId: ThreadId;
  /** Omit to create an empty (Idle) tab the user can type into. */
  readonly url?: string;
  /**
   * Initial viewport for the new tab. Omitting it keeps the historical
   * fill-panel behaviour; clients that have a configured default send it here
   * so the session is born at the right size instead of being resized a frame
   * later (which the user would see as a visible reflow).
   */
  readonly viewport?: PreviewViewportSetting;
  /** Omit to open under the client's configured default profile. */
  readonly profileId?: BrowserProfileId;
}

// --- Naming one change request, and one line of its diff. ---

/** `pullRequest.ts`: the three things that name a change request. */
export interface PullRequestRef {
  readonly projectId: ProjectId;
  readonly repository: string;
  readonly number: number;
}

/** `pullRequest.ts`: which column of a split diff a line was read from. */
export type PullRequestDiffSide = "left" | "right";

/** `pullRequest.ts`: the coordinates of one line in a pull request diff. */
export type PullRequestReviewPosition =
  | { readonly kind: "added"; readonly newLine: number }
  | { readonly kind: "deleted"; readonly oldLine: number }
  | {
      readonly kind: "context";
      readonly oldLine: number;
      readonly newLine: number;
      /** Which copy of an unchanged line the reviewer selected in a split diff. */
      readonly side: PullRequestDiffSide;
    };

/**
 * `pullRequest.ts`, verbatim: the host a project's repository is addressed
 * below. `canonicalKey` is the normalized remote, `host/owner/repo`, so its
 * first segment is the host; the provider kind stands in when there is no key
 * to read, which keeps one bucket per kind for identities recorded before it
 * existed.
 */
export function pullRequestHostOf(
  identity: { readonly canonicalKey?: string | undefined } | null | undefined,
  kind: SourceControlProviderKind,
): string {
  const host = identity?.canonicalKey?.split("/")[0]?.trim();
  return host === undefined || host.length === 0 ? kind : host.toLowerCase();
}
