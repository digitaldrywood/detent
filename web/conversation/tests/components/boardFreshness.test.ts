// What the board's connection chip decides, and the sentence it shows for it.
//
// Michael found the board's toolbar reading "● Not streaming · data as of
// 9:02 AM · Reload" while the sidebar footer read "● Live", and asked what
// each word meant. These cases are the answer in one place: the board's chip
// follows the app's own connection state, adds the one fact only this surface
// knows — whether its project event streams are connected — and offers Reload
// exactly where the stream is down and nowhere else.
import { describe, expect, it, vi } from "vitest";

import { boardConnectionChip } from "../../src/app/work/lib/freshness.ts";
import {
  chipStamp,
  LIVE_TOOLTIP,
  LOADING_TOOLTIP,
} from "../../src/app/lib/connectionTooltips.ts";
import { connectionChip } from "../../src/app/App.tsx";

// The moment the data on screen was read, and the stamp this machine prints
// for it. The stamp is read from the same helper the chips use rather than
// written out, because the locale and the zone are the reader's and a literal
// here would only assert the machine the suite happens to run on.
const AT = new Date(2026, 8, 12, 9, 2, 0).getTime();
const STAMP = chipStamp(AT);

const live = connectionChip("live", AT, () => {});
const reconnecting = connectionChip("synchronizing", AT, () => {});
const offline = connectionChip("cached", AT, () => {});

function chip(overrides: Partial<Parameters<typeof boardConnectionChip>[0]> = {}) {
  return boardConnectionChip({
    app: live,
    streaming: true,
    loading: false,
    asOf: AT,
    onReload: () => {},
    ...overrides,
  });
}

describe("the board's connection chip", () => {
  it("reads Live, with nothing to press, while the app and the stream are both up", () => {
    const result = chip();
    expect(result.label).toBe("Live");
    expect(result.tone).toBe("dc-ok");
    expect(result.action).toBeNull();
    expect(result.tooltip).toBe(LIVE_TOOLTIP);
  });

  // The sidebar's chip says Live from the same source, so the two agree by
  // construction rather than by two functions happening to match.
  it("says exactly what the sidebar chip says about a live client", () => {
    expect(chip().label).toBe(live.label);
    expect(chip().tooltip).toBe(live.tooltip);
  });

  it("claims nothing while the first read is in flight", () => {
    const result = chip({ loading: true });
    expect(result.label).toBe("Loading");
    expect(result.action).toBeNull();
    expect(result.tooltip).toBe(LOADING_TOOLTIP);
  });

  it("says Not streaming and offers Reload once the stream is down", () => {
    const onReload = vi.fn();
    const result = chip({ streaming: false, onReload });
    expect(result.label).toBe("Not streaming");
    expect(result.tone).toBe("dc-warn");
    expect(result.action?.label).toBe("Reload");
    expect(result.tooltip).toBe(
      `Not streaming: showing data from ${STAMP}, Reload fetches the board again.`,
    );
    result.action?.onClick();
    expect(onReload).toHaveBeenCalledTimes(1);
  });

  // The board cannot be fresher than the client it is inside, so while the app
  // is reconnecting the board borrows its word rather than inventing one.
  it("borrows the app's own word while the client is not live", () => {
    const result = chip({ app: reconnecting, streaming: true });
    expect(result.label).toBe("Reconnecting");
    expect(result.tone).toBe("dc-warn");
    expect(result.action?.label).toBe("Reload");
    expect(result.tooltip).toBe(
      `Reconnecting: showing data from ${STAMP}, Reload fetches the board again.`,
    );
  });

  it("keeps the app's severity when the client is offline", () => {
    const result = chip({ app: offline, streaming: false });
    expect(result.label).toBe("Offline");
    expect(result.tone).toBe("dc-err");
    expect(result.action?.label).toBe("Reload");
  });

  // A client that has read nothing has no "from" to name, and the sentence
  // says what is missing instead of printing an empty time.
  it("names no time before the first read lands", () => {
    const result = chip({ streaming: false, asOf: null });
    expect(result.detail).toBe("no activity stream");
    expect(result.tooltip).toBe(
      "Not streaming: no live updates are arriving, Reload fetches the board again.",
    );
  });
});
