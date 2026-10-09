// Small formatters shared by the board, the list and the issue page.
//
// Every one of them is pure and takes `now` explicitly, so a test never has to
// freeze the clock and a card rendered twice in the same tick cannot disagree
// with itself.

/**
 * The artifact's age column: `3m`, `4h`, `2d`, `6w`. Coarse on purpose — the
 * card answers "is this stale", not "how many seconds".
 */
export function ageLabel(at: string | null | undefined, now: number): string {
  if (at === undefined || at === null || at === "") return "";
  const then = Date.parse(at);
  if (Number.isNaN(then)) return "";
  const seconds = Math.max(0, Math.round((now - then) / 1000));
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.round(seconds / 60);
  if (minutes < 60) return `${minutes}m`;
  const hours = Math.round(minutes / 60);
  if (hours < 24) return `${hours}h`;
  const days = Math.round(hours / 24);
  if (days < 14) return `${days}d`;
  return `${Math.round(days / 7)}w`;
}

/**
 * The worker strip's elapsed time: `1m 12s` under an hour, `2h 04m` above it.
 * A running attempt is watched second by second, so this one is not coarse.
 */
export function elapsedLabel(startedAt: string | null | undefined, now: number): string {
  if (startedAt === undefined || startedAt === null || startedAt === "") return "";
  const started = Date.parse(startedAt);
  if (Number.isNaN(started)) return "";
  const total = Math.max(0, Math.round((now - started) / 1000));
  const hours = Math.floor(total / 3600);
  const minutes = Math.floor((total % 3600) / 60);
  const seconds = total % 60;
  if (hours > 0) return `${hours}h ${String(minutes).padStart(2, "0")}m`;
  if (minutes > 0) return `${minutes}m ${String(seconds).padStart(2, "0")}s`;
  return `${seconds}s`;
}

/** `1.2M tok`, `312k tok`, `84 tok`. Empty for an unknown or zero count. */
export function tokenLabel(tokens: number | null | undefined): string {
  if (tokens === undefined || tokens === null || tokens <= 0) return "";
  if (tokens >= 1_000_000) return `${(tokens / 1_000_000).toFixed(1)}M tok`;
  if (tokens >= 1_000) return `${Math.round(tokens / 1_000)}k tok`;
  return `${tokens} tok`;
}

/** `$28.77`. Empty rather than `$0.00` for an absent figure. */
export function moneyLabel(usd: number | null | undefined): string {
  if (usd === undefined || usd === null) return "";
  return `$${usd.toFixed(2)}`;
}

/** `Sep 7`, for the issue card's "opened" pill. */
export function dayLabel(at: string | null | undefined): string {
  if (at === undefined || at === null || at === "") return "";
  const stamp = Date.parse(at);
  if (Number.isNaN(stamp)) return "";
  return new Date(stamp).toLocaleDateString(undefined, { month: "short", day: "numeric" });
}

/**
 * The project colour dot. The hub serves no colour, and inventing a random one
 * per render would make the same project a different colour on every tick, so
 * the hue is a stable hash of the project id and the dot is only ever a
 * recognition aid (A.1: it renders in the all-projects scope only).
 */
export function projectHue(projectId: string): number {
  let hash = 0;
  for (let index = 0; index < projectId.length; index += 1) {
    hash = (hash * 31 + projectId.charCodeAt(index)) % 360;
  }
  return hash;
}

/** `#3363` from whatever identifier shape the tracker uses. */
export function issueNumber(identifier: string | null | undefined, number?: number | null): string {
  if (number !== undefined && number !== null && number > 0) return `#${number}`;
  if (identifier === undefined || identifier === null || identifier === "") return "";
  const match = identifier.match(/#?(\d+)\s*$/);
  return match === null ? identifier : `#${match[1]}`;
}
