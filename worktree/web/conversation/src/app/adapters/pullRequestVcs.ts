import type {
  PullRequestState,
  SourceControlProviderKind,
  VcsStatusResult,
} from "../../contracts/ui.ts";

export interface PullRequestSummary {
  readonly number: number;
  readonly title: string;
  readonly url: string;
  readonly baseBranch: string;
  readonly headBranch: string;
  readonly state: PullRequestState;
  readonly isDraft?: boolean | undefined;
  readonly updatedAt: string;
  readonly provider: SourceControlProviderKind;
}

export type PullRequestDetail = PullRequestSummary;

export interface PullRequestRef {
  readonly projectId: string;
  readonly repository: string;
  readonly number: number;
}

export function pullRequestDetailToVcsStatus(
  detail: PullRequestDetail | PullRequestSummary,
): NonNullable<VcsStatusResult["pr"]> {
  return {
    number: detail.number,
    title: detail.title,
    url: detail.url,
    baseRef: detail.baseBranch,
    headRef: detail.headBranch,
    state: detail.state,
    ...(detail.isDraft === true ? { isDraft: true } : {}),
    updatedAt: detail.updatedAt,
  };
}
