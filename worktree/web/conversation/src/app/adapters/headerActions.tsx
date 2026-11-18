import React from "react";

import type { HeaderGit } from "./headerGit.ts";
import type { OpenInLocation } from "./openIn.ts";
import { GIT_POLICY_SENTENCES } from "./gitStatus.ts";

/** Where the header's `Open` menu can send the reader besides an editor. */
export interface HeaderOpenTarget {
  readonly id: string;
  readonly label: string;
  /** In-app navigation, or an absolute URL opened in a new tab. */
  readonly run: () => void;
  readonly external: boolean;
}

/**
 * The git group's state and its two runners, or null.
 *
 * Null is the everything-disabled value: a surface that has no git group at
 * all (the default below, and the tests that render one control in isolation)
 * supplies none rather than a stub whose `run` does nothing.
 */
export type HeaderGitActions = HeaderGit | null;

export interface HeaderActions {

  readonly openTargets: readonly HeaderOpenTarget[];

  readonly openLocation: OpenInLocation;
  /** The git group, or null where there is none. */
  readonly git: HeaderGitActions;

  readonly openPullRequest: (() => void) | null;

  readonly projectId: string | null;
}

export const DEFAULT_HEADER_ACTIONS: HeaderActions = {
  openTargets: [],
  openLocation: { worktreePath: null, hostname: null, local: false, linked: false },
  git: null,
  openPullRequest: null,
  projectId: null,
};

/** The sentence every control falls back to with no git group at all. */
export const NO_GIT_GROUP_REASON = GIT_POLICY_SENTENCES.noIssue;

const HeaderActionsContext = React.createContext<HeaderActions>(DEFAULT_HEADER_ACTIONS);

export const HeaderActionsProvider = HeaderActionsContext.Provider;

export function useHeaderActions(): HeaderActions {
  return React.use(HeaderActionsContext);
}
