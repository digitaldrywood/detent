// What the board's connection chip says, and why.
//
// Michael, on the Work board: the toolbar read "1 project · ● Not streaming ·
// data as of 9:02 AM · Reload" while the sidebar footer read "● Live", and he
// asked what Reload does and what Live represents. Two chips on one screen
// disagreeing about one client is a bug in the question, not only in the
// answer, so the board's chip is now derived from the app's own connection
// state — the same value the sidebar footer shows — and from one extra fact
// the sidebar has no view of: whether this board's project event streams are
// connected (`useWork.ts`).
//
// The decision is a pure function so it can be checked without a browser: the
// chip is the one thing on the board a reader trusts to tell them whether what
// they are looking at is still true.
import type { ConnectionChip } from "../../App.tsx";
import {
  chipStamp,
  LIVE_TOOLTIP,
  LOADING_TOOLTIP,
  staleBoardTooltip,
} from "../../lib/connectionTooltips.ts";

/** The chip, plus the one sentence that says what it means. */
export type BoardChip = ConnectionChip;

export interface BoardChipInput {
  /** The app's connection chip — exactly what the sidebar footer shows. */
  readonly app: ConnectionChip;
  /** True while every project event stream this board reads is connected. */
  readonly streaming: boolean;
  /** True while the board's read is in flight. */
  readonly loading: boolean;
  /** When the data on screen was read. */
  readonly asOf: number | null;
  /** Re-reads the board. Offered only where the stream is actually down. */
  readonly onReload: () => void;
}

/**
 * The board's chip.
 *
 * Three states, in the order they are decided:
 *
 *  1. **Loading.** The first read is in flight and no claim about freshness is
 *     available yet, so none is made and nothing is offered to press.
 *  2. **Whatever the app says**, while the app is not live. The sidebar's word
 *     — Reconnecting, Offline, Connecting — is the board's word too: one
 *     client has one connection state, and the board cannot be fresher than
 *     the client it is inside.
 *  3. **The board's own stream.** With the app live, the only remaining
 *     question is whether this board's project streams are connected. They
 *     are, and the chip says Live with nothing to press; they are not, and it
 *     says Not streaming and offers the one action that fixes it.
 */
export function boardConnectionChip(input: BoardChipInput): BoardChip {
  const stamp = chipStamp(input.asOf);
  if (input.loading) {
    return {
      tone: "",
      label: "Loading",
      detail: "reading the board",
      action: null,
      tooltip: LOADING_TOOLTIP,
    };
  }
  if (input.app.tone !== "dc-ok") {
    return {
      tone: input.app.tone,
      label: input.app.label,
      detail: stamp.length > 0 ? `data as of ${stamp}` : input.app.detail,
      action: { label: "Reload", onClick: input.onReload },
      tooltip: staleBoardTooltip(input.app.label, stamp),
    };
  }
  if (!input.streaming) {
    return {
      tone: "dc-warn",
      label: "Not streaming",
      detail: stamp.length > 0 ? `data as of ${stamp}` : "no activity stream",
      action: { label: "Reload", onClick: input.onReload },
      tooltip: staleBoardTooltip("Not streaming", stamp),
    };
  }
  return {
    tone: "dc-ok",
    label: "Live",
    detail: stamp.length > 0 ? `data current · ${stamp}` : "data current",
    action: null,
    tooltip: LIVE_TOOLTIP,
  };
}
