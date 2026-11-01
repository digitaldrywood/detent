/** Both chips' live sentence. There is one event stream and one meaning. */
export const LIVE_TOOLTIP = "Live: updates arrive over the project event stream.";

/** The board's first read, before it can claim anything about freshness. */
export const LOADING_TOOLTIP = "Loading: reading the board from the hub.";

/**
 * A chip that is not live, where the action re-reads the board.
 *
 * `stamp` is the clock label of the data on screen, or empty where this client
 * has not read anything yet — in which case there is no "from" to name and the
 * sentence says what is missing instead.
 */
export function staleBoardTooltip(label: string, stamp: string): string {
  return stamp.length > 0
    ? `${label}: showing data from ${stamp}, Reload fetches the board again.`
    : `${label}: no live updates are arriving, Reload fetches the board again.`;
}

/** A chip that is not live, where the action re-reads the conversation list. */
export function staleClientTooltip(label: string, stamp: string, missing: string): string {
  return stamp.length > 0
    ? `${label}: showing data from ${stamp}, Retry now re-reads it.`
    : `${label}: ${missing}; Retry now re-reads it.`;
}

/** The clock stamp every chip prints, in the reader's own locale. */
export function chipStamp(asOf: number | null): string {
  if (asOf === null) return "";
  return new Date(asOf).toLocaleTimeString(undefined, { hour: "numeric", minute: "2-digit" });
}
