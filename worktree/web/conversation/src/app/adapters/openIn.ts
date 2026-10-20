import React from "react";

import type { EditorId } from "../../contracts/ui.ts";

export const OPEN_IN_EDITORS: readonly EditorId[] = ["cursor", "vscode", "file-manager"];

export interface OpenInLocation {
  readonly worktreePath: string | null;
  readonly hostname: string | null;
  readonly local: boolean;
  /**
   * Whether the conversation has a linked issue at all.
   *
   * One field more than the path, the host and the verdict, and it is needed:
   * a conversation with no issue and a workspace that has been asked for but
   * not yet claimed by a runner both report a null path and a null hostname —
   * §18.1 fills `machine_hostname` only once a runner takes the workspace — and
   * they are owed different sentences. Without this the picker would tell a
   * reader who has a linked issue to link one.
   */
  readonly linked: boolean;
}

/** Loopback literals a browser can be served from. */
const LOOPBACK_HOSTS: ReadonlySet<string> = new Set(["localhost", "127.0.0.1", "::1", "[::1]"]);

/** `mac-studio.local.` and `MAC-STUDIO` are the same machine. */
function firstLabel(host: string): string {
  const trimmed = host.trim().toLowerCase().replace(/\.$/, "");
  const bare = trimmed.startsWith("[") ? trimmed : (trimmed.split(":")[0] ?? trimmed);
  return bare.split(".")[0] ?? bare;
}

/**
 * Whether the page and the runner are on the same machine.
 *
 * Compared on the first DNS label, case-insensitively and with a trailing dot
 * stripped, because the two sides learn the name from different places: a
 * runner reports `os.Hostname()` ("mac-studio") and mDNS serves the page as
 * "mac-studio.local".
 *
 * The deliberate trade-off is the loopback refusal. A page served from
 * `localhost` is very likely on the runner's machine — but "very likely" is
 * not proof, and it is precisely the case where it can be wrong: a tunnel, a
 * port forward, a container, or the hub itself proxied to a local port all
 * present as `localhost` while the worktree is somewhere else entirely.
 * Handing the operating system an absolute path that is not there is the one
 * failure worth being conservative about — it does not fail visibly, it opens
 * an editor on a *different* file tree, or silently creates one — so a
 * loopback page is treated as remote even when the runner also reported a
 * loopback name. The cost is a Detent developer on their own machine seeing
 * "This worktree is on localhost." and using the path by hand, which is a
 * nuisance rather than a wrong file.
 */
export function isLocalRunner(pageHostname: string, runnerHostname: string | null): boolean {
  if (runnerHostname === null) return false;
  const page = firstLabel(pageHostname);
  const runner = firstLabel(runnerHostname);
  if (page === "" || runner === "") return false;
  if (LOOPBACK_HOSTS.has(pageHostname.trim().toLowerCase()) || LOOPBACK_HOSTS.has(page)) {
    return false;
  }
  return page === runner;
}

export function editorOpenUrl(editor: EditorId, worktreePath: string): string | null {
  if (worktreePath.trim().length === 0) return null;
  const scheme = editor === "cursor" ? "cursor" : editor === "vscode" ? "vscode" : null;
  if (scheme === null) return null;
  const encoded = worktreePath
    .split("/")
    .map((segment) => encodeURIComponent(segment))
    .join("/");
  return `${scheme}://file/${encoded.startsWith("/") ? encoded.slice(1) : encoded}`;
}

/**
 * The fixed sentences this picker can refuse with. The fourth is
 * `remoteSentence`, which is a function because it names a host.
 */
export const OPEN_IN_SENTENCES = {
  noIssue: "Link an issue to get a worktree.",
  fileManager: "Available on the runner's machine only.",
  noWorktreePath: "This worktree has not reported a path yet.",
} as const;

/**
 * Names the machine the worktree is on.
 *
 * The fallback matters: a runner that reports no hostname still has a machine,
 * and "This worktree is on the runner's machine." says the true thing — the
 * worktree is not here — without pretending to a name nobody sent. Saying
 * nothing, or naming `null`, would leave the reader thinking the row is broken
 * rather than remote.
 */
export function remoteSentence(hostname: string | null): string {
  const where = hostname === null || hostname.trim().length === 0 ? "the runner's machine" : hostname;
  return `This worktree is on ${where}.`;
}

/**
 * Why one row cannot run, or null.
 *
 * Ordered structurally, like `gitStatus.ts`'s policy layer and for the same
 * reason: no issue means no worktree, no worktree path means nothing to hand
 * over, a remote runner means nothing a browser can reach, and only then is
 * the file manager's own missing scheme worth mentioning.
 */
export function openInDisabledReason(
  editor: EditorId,
  location: OpenInLocation,
): string | null {
  // The same sentence the git group uses, because it is the same fact: no
  // issue, no worktree, and nothing downstream is worth saying.
  if (!location.linked) return OPEN_IN_SENTENCES.noIssue;
  if (!location.local) return remoteSentence(location.hostname);
  if (location.worktreePath === null || location.worktreePath.trim().length === 0) {
    return OPEN_IN_SENTENCES.noWorktreePath;
  }
  // Last, and only when everything else would have been enabled: the file
  // manager has no registered scheme a page can hand a path to.
  if (editorOpenUrl(editor, location.worktreePath) === null) {
    return OPEN_IN_SENTENCES.fileManager;
  }
  return null;
}

/**
 * Where the open workspace's worktree is, as the picker reads it.
 *
 * A hook rather than a plain function only because the page's own host has to
 * be read from the document, and a non-browser build (the vitest node
 * environment, a server render) has no `location`. An unknown page host is
 * treated as remote, which is the same conservative answer the loopback rule
 * gives.
 */
export function useOpenInLocation(input: {
  readonly worktreePath: string | null;
  readonly hostname: string | null;
  readonly linked: boolean;
}): OpenInLocation {
  const { worktreePath, hostname, linked } = input;
  return React.useMemo(() => {
    const page = globalThis.location?.hostname ?? "";
    return {
      worktreePath,
      hostname,
      linked,
      local: page === "" ? false : isLocalRunner(page, hostname),
    };
  }, [hostname, linked, worktreePath]);
}
