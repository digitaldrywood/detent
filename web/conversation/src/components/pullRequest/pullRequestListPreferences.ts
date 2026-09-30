export interface PullRequestListPreferences {
  readonly [key: string]: never;
}

const NONE: PullRequestListPreferences = {};

export function readPullRequestListPreferences(): PullRequestListPreferences {
  return NONE;
}
