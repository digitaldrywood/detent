import { AsyncResult, Atom } from "effect/unstable/reactivity";

import type { PullRequestActor, PullRequestState } from "../contracts/ui.ts";
import type { PullRequestRef, PullRequestSummary } from "../app/adapters/pullRequestVcs.ts";

export interface PullRequestDetail {
  readonly state: PullRequestState;
  readonly isDraft: boolean;
  readonly url: string;
  readonly repository: string;
  readonly number: number;
  readonly title: string;
  readonly author: PullRequestActor | null;
  readonly createdAt: string;
}

const UNRESOLVED = Atom.make(AsyncResult.initial<PullRequestDetail, never>(false)).pipe(
  Atom.withLabel("detent-pull-request-detail:seeded"),
);

export const pullRequestEnvironment = {
  detail(_input: {
    environmentId: string;
    input: { projectId: string; repository: string; number: number };
  }): Atom.Atom<AsyncResult.AsyncResult<PullRequestDetail, never>> {
    return UNRESOLVED;
  },
};

/**
 * The two names the copied `ThreadStatusIndicators.tsx` reaches for when a
 * thread carries a linked change request.
 *
 * A hosted conversation never carries one — `app/adapters/sidebarThreads.ts`
 * leaves both pull-request fields null, because a conversation has no checkout
 * for a branch to belong to — so `useLinkedThreadPullRequest` returns null
 * before either is consulted. They exist so that file keeps its upstream
 * imports (decisions.md §16). A milestone that links a change request to a
 * conversation fills these two in and the copied row lights up unchanged.
 */
const UNRESOLVED_SUMMARY = Atom.make(
  AsyncResult.initial<LinkedPullRequestSummary, never>(false),
).pipe(Atom.withLabel("detent-linked-pull-request:absent"));

export type LinkedPullRequestSummary = PullRequestSummary;

export function linkedPullRequestDetailAtom(_input: {
  environmentId: string;
  input: { projectId: string; repository: string; number: number };
}): Atom.Atom<AsyncResult.AsyncResult<LinkedPullRequestSummary, never>> {
  return UNRESOLVED_SUMMARY;
}

export function useSharedPullRequestSummary(
  _environmentId: string | null,
  _reference: PullRequestRef | null,
  current: PullRequestSummary | null,
): PullRequestSummary | null {
  return current;
}
