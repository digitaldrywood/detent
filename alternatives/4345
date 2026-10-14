import { Atom } from "effect/unstable/reactivity";

import { detentKeybindings } from "../app/adapters/keybindings.ts";
import type { EnvironmentId, ResolvedKeybindingsConfig } from "../contracts/index.ts";
import { HUB_ENVIRONMENT_ID } from "../contracts/index.ts";
import type {
  EditorId,
  EnvironmentMachineKind,
  ExecutionEnvironmentPlatformOs,
  FileManagerRevealKind,
  ServerProvider,
} from "../contracts/ui.ts";

export const primaryServerKeybindingsAtom = Atom.make<ResolvedKeybindingsConfig>(
  detentKeybindings,
).pipe(Atom.withLabel("detent-keybindings"));

/** `serverConfig.environment.capabilities`, as the copied sidebar reads it. */
export interface EnvironmentCapabilities {
  /**
   * True. Detent settles conversations and the sidebar's Settled shelf is
   * that group (decisions.md §13.9, §14).
   */
  readonly threadSettlement: boolean;
  /** False: no snooze endpoint, so the clock button and shelf never appear. */
  readonly threadSnooze: boolean;
  /** False: no pinned state on the hub, so the pin marker never appears. */
  readonly threadPinning: boolean;
  /** False: nothing to reorder while there is nothing pinned. */
  readonly threadPinReorder: boolean;
  /** False: the active list is activity-ordered and the hub stores no key. */
  readonly threadActiveReorder: boolean;
  /** False: the hub does not regenerate a conversation title on request. */
  readonly threadTitleRegeneration: boolean;
  /** True: `/work/changes` is always served, so the footer's link leads somewhere. */
  readonly pullRequests: boolean;
}

export interface ServerConfig {
  readonly environment: {
    readonly capabilities: EnvironmentCapabilities;
    readonly machine?: EnvironmentMachineKind;
  };
  readonly providers: readonly ServerProvider[];
}

export const EMPTY_SERVER_PROVIDERS: readonly ServerProvider[] = [];

export const DETENT_SERVER_CONFIG: ServerConfig = {
  environment: {
    capabilities: {
      threadSettlement: true,
      threadSnooze: false,
      threadPinning: false,
      threadPinReorder: false,
      threadActiveReorder: false,
      threadTitleRegeneration: false,
      pullRequests: true,
    },
    machine: "cloud",
  },

  providers: EMPTY_SERVER_PROVIDERS,
};

export const environmentServerConfigsAtom = Atom.make<ReadonlyMap<EnvironmentId, ServerConfig>>(
  new Map([[HUB_ENVIRONMENT_ID, DETENT_SERVER_CONFIG]]),
).pipe(Atom.withLabel("detent-server-configs"));

// ---------------------------------------------------------------------------
// `serverEnvironment`, as the copied `components/ChatMarkdown.tsx` reads it.
//
// Upstream this is an atom family over the desktop connection: ask an
// environment for its config and you learn which editors are installed on that
// machine, whether its file manager can reveal a path, and which OS it runs —
// everything the markdown's file-link context menu needs to offer "Open in
// Cursor" or "Reveal in Finder".
//
// None of it exists here. A Detent reader is in a browser tab and the checkout
// is on the customer's runner, reachable only over the hub's relay
// (decisions.md §1, §18); there is no machine whose editors this client could
// enumerate, and nothing it could launch if there were.
//
// So the config is null. That is not a stub: null is the value upstream's own
// components take before a connection is prepared, and every reader of it
// here is written as `serverConfig?.availableEditors ?? []` or
// `serverConfig?.shellRevealInFileManager === true`. The menu items resolve to
// their disabled branch with the reason, which is what section 16 asks for.
// ---------------------------------------------------------------------------

export interface MarkdownServerConfig {
  readonly availableEditors: ReadonlyArray<EditorId>;
  readonly shellRevealInFileManager?: boolean;
  readonly shellRevealInFileManagerKind?: FileManagerRevealKind;
  readonly environment: {
    readonly platform: { readonly os: ExecutionEnvironmentPlatformOs };
    /**
     * The markdown reads two of these: whether a thread can be linked to a
     * pull request, and whether pull requests exist at all. Detent's are on
     * `EnvironmentCapabilities` above; this is the same set under the name
     * upstream's markdown reaches it by.
     */
    readonly capabilities: {
      readonly pullRequests: boolean;
      readonly threadPullRequestLinking: boolean;
    };
  };
}

const NO_SERVER_CONFIG = Atom.make<MarkdownServerConfig | null>(null).pipe(
  Atom.withLabel("detent-server-config:none"),
);

export const serverEnvironment = {
  configValueAtom: (_environmentId: EnvironmentId | null): Atom.Atom<MarkdownServerConfig | null> =>
    NO_SERVER_CONFIG,
};
