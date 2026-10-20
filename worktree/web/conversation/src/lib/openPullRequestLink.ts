import type { MouseEvent } from "react";
import { useCallback } from "react";

import type {
  EnvironmentId,
  ScopedThreadRef,
  SourceControlProviderKind,
  ThreadLinkedPullRequest,
} from "@t3tools/contracts";
import { pullRequestHostOf } from "@t3tools/contracts";
import type { EnvironmentProject } from "@t3tools/client-runtime/state/shell";

export function useOpenPrLink(
  _threadRef?: ScopedThreadRef,
): (
  event: MouseEvent<HTMLElement>,
  prUrl: string,
  targetThreadRef?: ScopedThreadRef,
) => boolean {
  return useCallback(() => false, []);
}

/**
 * A change request the page can open, named the way the page names one: the host below which the
 * repository is addressed, the repository path as that host writes it, and the number.
 *
 * The two strings are what `pullRequestHostOf` and the project's `repositoryIdentity` produce
 * from a git remote — lower case, no port, the full path below the host — because the page matches
 * a link against those. Anything else opens nothing.
 */
export interface ChangeRequestLink {
  readonly host: string;
  readonly repository: string;
  readonly number: number;
}

/** The host itself, one of its subdomains, or an install named after the provider. */
function isHostOf(hostname: string, apex: string, label?: string): boolean {
  if (hostname === apex || hostname.endsWith(`.${apex}`)) return true;
  return label !== undefined && hostname.startsWith(`${label}.`);
}

/**
 * The repository and number behind a change request URL on a host the page can read, or null for
 * anything else — an issue, a commit, a repository root, a host this cannot tell apart from an
 * ordinary link. Null means the system browser, so a doubtful match is worse than no match: it
 * takes the reader out of their browser and into a page that cannot find the change request.
 *
 * Each host is recognised by the path shape it alone uses, guarded by a hostname it could
 * plausibly be served from, since self-hosted installs are named whatever their admin chose:
 * GitLab's `/-/` marker is unique enough to trust on any hostname, while `/pull/` is generic
 * enough that it is only believed from a GitHub-ish host.
 */
export function parseChangeRequestUrl(targetUrl: string): ChangeRequestLink | null {
  let url: URL;
  try {
    url = new URL(targetUrl);
  } catch {
    return null;
  }
  // `javascript:`, `mailto:` and friends have no host to speak of and nothing to open.
  if (url.protocol !== "https:" && url.protocol !== "http:") return null;
  // Nothing here tries to tell a lookalike hostname from a real one — `github.com.evil.test`,
  // `github.com-evil.test` and the rest are an open set, and blocking spellings of it costs real
  // hosts (`gitlab.com.br` is a registrable domain, not a disguise). What a claim is worth is
  // decided where it is used: only a link matching a repository this workspace has checked out
  // opens the page, and everything else stays the ordinary link it was.
  const host = url.hostname.toLowerCase();

  // GitHub, and any Enterprise install: /{owner}/{repo}/pull/{n}
  if (isHostOf(host, "github.com", "github")) {
    const match = /^\/([^/]+\/[^/]+)\/pull\/(\d+)(?:\/|$)/u.exec(url.pathname);
    return claim(host, match);
  }
  // GitLab, self-hosted included: /{group}/[{subgroup}/...]{repo}/-/merge_requests/{n}. The `/-/`
  // separator is GitLab's own, so the hostname is not asked about.
  const gitlab = /^\/([^/]+(?:\/[^/]+)+)\/-\/merge_requests\/(\d+)(?:\/|$)/u.exec(url.pathname);
  if (gitlab) return claim(host, gitlab);
  // Bitbucket Cloud: /{workspace}/{repo}/pull-requests/{n}
  if (isHostOf(host, "bitbucket.org", "bitbucket")) {
    const match = /^\/([^/]+\/[^/]+)\/pull-requests\/(\d+)(?:\/|$)/u.exec(url.pathname);
    return claim(host, match);
  }
  // Azure DevOps, both the current host and the per-organisation one it replaced. `_git` is part
  // of the repository path there, as it is in the remote URL the identity is read from.
  if (isHostOf(host, "dev.azure.com") || host.endsWith(".visualstudio.com")) {
    const match = /^\/((?:[^/]+\/)*_git\/[^/]+)\/pullrequest\/(\d+)(?:\/|$)/u.exec(url.pathname);
    return claim(host, match);
  }
  return null;
}

/**
 * The pull-request URL a GitHub-style `#123` autolink might name. GitHub writes every bare
 * reference through `/issues/`, including pull requests, so this only builds a candidate: the
 * caller must successfully read it as a pull request before treating it as one.
 */
export function pullRequestCandidateUrlFromReferenceAutolink(targetUrl: string): string | null {
  let url: URL;
  try {
    url = new URL(targetUrl);
  } catch {
    return null;
  }
  if (
    (url.protocol !== "https:" && url.protocol !== "http:") ||
    !isHostOf(url.hostname.toLowerCase(), "github.com", "github")
  ) {
    return null;
  }
  const match = /^\/([^/]+\/[^/]+)\/issues\/(\d+)(?:\/|$)/u.exec(url.pathname);
  if (match?.[1] === undefined || match[2] === undefined) return null;
  url.pathname = `/${match[1]}/pull/${match[2]}`;
  return url.toString();
}

/** Match a stored PR without requiring its project to remain available. */
export function matchesLinkedPullRequestUrl(
  linkedPullRequest: ThreadLinkedPullRequest,
  targetUrl: string,
): boolean {
  const linked = parseChangeRequestUrl(linkedPullRequest.url);
  const target = parseChangeRequestUrl(targetUrl);
  return (
    linked !== null &&
    target !== null &&
    linked.host === target.host &&
    linked.repository === target.repository &&
    linked.number === target.number
  );
}

function claim(host: string, match: RegExpExecArray | null): ChangeRequestLink | null {
  const repository = match?.[1];
  const number = Number(match?.[2]);
  return repository && Number.isSafeInteger(number) && number > 0
    ? { host, repository: repository.toLowerCase(), number }
    : null;
}

/**
 * The project a link belongs to, or nothing. Matched the way the server matches: the repository
 * identity is the full path below the host where one was recorded — which is what nested GitLab
 * groups and Azure project paths need — and the host is the first segment of the canonical
 * remote, so github.com and an Enterprise install stay apart.
 */
export function findProjectForChangeRequest(
  projects: ReadonlyArray<EnvironmentProject>,
  link: ChangeRequestLink,
): EnvironmentProject | undefined {
  return projects.find((project) => {
    const identity = project.repositoryIdentity;
    if (!identity) return false;
    const kind = identity.provider as SourceControlProviderKind | undefined;
    if (kind === undefined) return false;
    const repository =
      identity.displayName ??
      (identity.owner && identity.name ? `${identity.owner}/${identity.name}` : null);
    return (
      repository !== null &&
      repository.toLowerCase() === link.repository.toLowerCase() &&
      pullRequestHostOf(identity, kind) === link.host.toLowerCase()
    );
  });
}

export function shouldOpenPullRequestExternally(
  event: Pick<MouseEvent<HTMLElement>, "metaKey" | "ctrlKey">,
): boolean {
  return event.metaKey || event.ctrlKey;
}

/**
 * Following a change-request link, at the data boundary.
 *
 * Upstream this hook is the whole reason the parsers above exist: having
 * recognised a URL and found the project it belongs to, it opens the change
 * request *inside* the app — in the thread's right panel beside the reader, or
 * on the pull requests page — so a reader following a link the agent wrote is
 * still reading the thread afterwards.
 *
 * Detent has neither surface in this milestone: there is no pull-requests
 * route, and `app/adapters/rightPanel.ts` opens no change-request tab. The
 * honest answer is therefore upstream's own "this is not ours to handle"
 * return — `false` — which leaves the anchor the copied `ChatMarkdown.tsx`
 * renders to do what it already does: follow its `href` in a new tab, to the
 * change on its own host. That is the same answer `useOpenPrLink` above gives,
 * and the same one upstream gives for a repository nothing here is checked out
 * from (decisions.md §16, §18.6).
 *
 * The parsers are still live: the copied file uses them to decide whether a
 * link *is* a change request, which is what decorates it and offers to link it
 * to the conversation. Only the in-app open is missing.
 */
export function useOpenChangeRequestLink(
  _threadRef?: ScopedThreadRef,
  _panelRef?: ScopedThreadRef,
): (
  event: Pick<
    MouseEvent<HTMLElement>,
    "preventDefault" | "stopPropagation" | "metaKey" | "ctrlKey"
  >,
  targetUrl: string,
  targetThreadRef?: ScopedThreadRef,
  targetEnvironmentId?: EnvironmentId,
) => boolean {
  return useCallback(() => false, []);
}
