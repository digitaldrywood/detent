import {
  GitMergeIcon,
  GitPullRequestClosedIcon,
  GitPullRequestDraftIcon,
  GitPullRequestIcon,
  TriangleAlertIcon,
} from "lucide-react";

import { cn } from "../../lib/utils.ts";
import type {
  PullRequestActor,
  PullRequestMergeability,
  PullRequestState,
} from "../../contracts/ui.ts";

interface StatePresentation {
  readonly label: string;
  readonly toneClassName: string;
  readonly Icon: typeof GitPullRequestIcon;
}

/**
 * How a pull request's state reads on this page. Open, closed, merged, and draft use the same
 * ink as the thread badge in `ThreadStatusIndicators`, so one pull request cannot look like two
 * different things in two places.
 *
 * Draft outranks conflicts: a draft is not heading for a merge yet, so conflicts only surface
 * once it is real work.
 */
export function resolvePullRequestState(input: {
  readonly state: PullRequestState;
  readonly isDraft: boolean;
  readonly mergeability?: PullRequestMergeability;
  readonly baseBranch?: string;
}): StatePresentation {
  if (input.state === "merged") {
    return {
      label: "Merged",
      toneClassName: "text-violet-600 dark:text-violet-300/90",
      Icon: GitMergeIcon,
    };
  }
  if (input.state === "closed") {
    return {
      label: "Closed",
      toneClassName: "text-red-600 dark:text-red-300/90",
      Icon: GitPullRequestClosedIcon,
    };
  }
  if (input.isDraft) {
    return {
      label: "Draft",
      toneClassName: "text-zinc-500 dark:text-zinc-400/80",
      Icon: GitPullRequestDraftIcon,
    };
  }
  if (input.mergeability === "conflicting") {
    return {
      // "Has conflicts" leaves out the one thing a reader wants when the warning triangle catches
      // their eye, so name the branch it collides with wherever the caller knows it.
      label: input.baseBranch ? `Conflicts with ${input.baseBranch}` : "Has conflicts",
      toneClassName: "text-destructive",
      Icon: TriangleAlertIcon,
    };
  }
  return {
    label: "Open",
    toneClassName: "text-emerald-600 dark:text-emerald-300/90",
    Icon: GitPullRequestIcon,
  };
}

export function PullRequestActorAvatar({
  actor,
  className,
}: {
  actor: PullRequestActor | null;
  className?: string;
}) {
  const login = actor?.login ?? "ghost";
  const avatarUrl = actor?.avatarUrl ?? null;
  return avatarUrl === null ? (
    // Not every host reports an avatar, so the initial stands in where none arrives.
    <span
      aria-hidden
      className={cn(
        "flex size-4 shrink-0 items-center justify-center rounded-full bg-muted text-[8px] font-medium text-muted-foreground",
        className,
      )}
    >
      {login.slice(0, 1).toUpperCase()}
    </span>
  ) : (
    <img
      aria-hidden
      alt=""
      src={avatarUrl}
      loading="lazy"
      className={cn("size-4 shrink-0 rounded-full bg-muted object-cover", className)}
    />
  );
}
