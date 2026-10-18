// Folding the transcript into turns and work logs.
import { describe, expect, it } from "vitest";

import type { Message } from "../../src/contracts/index.ts";
import {
  foldTimeline,
  formatWorkDuration,
  isWorkMessage,
  workLogLabel,
} from "../../src/app/lib/worklog.ts";
import { assistantMessage, proposalMessage, userMessage } from "./builders.ts";

function step(id: string, at: string, text = "reading files"): Message {
  return {
    ...assistantMessage(),
    id,
    role: "system",
    kind: "status",
    data: {},
    text,
    created_at: at,
  };
}

describe("formatWorkDuration", () => {
  it.each([
    [0, "0s"],
    [22_400, "22s"],
    [112_000, "1m 52s"],
    [3_600_000, "1h 0m"],
    [5_460_000, "1h 31m"],
  ])("formats %ims as %s", (ms, label) => {
    expect(formatWorkDuration(ms)).toBe(label);
  });
});

describe("isWorkMessage", () => {
  it("folds a plain status or tool message", () => {
    expect(isWorkMessage(step("msg_1", "2026-09-09T10:00:00Z"))).toBe(true);
  });

  it("never folds a spoken turn", () => {
    expect(isWorkMessage(userMessage())).toBe(false);
    expect(isWorkMessage(assistantMessage())).toBe(false);
  });

  // The cards are the answer, not the working out (B.8.6).
  it("never folds a card-bearing status message", () => {
    expect(isWorkMessage(proposalMessage())).toBe(false);
  });
});

describe("foldTimeline", () => {
  it("folds a run of steps into one entry and times it from the user's turn", () => {
    const entries = foldTimeline([
      { ...userMessage(), id: "msg_u", created_at: "2026-09-09T10:00:00Z" },
      step("msg_s1", "2026-09-09T10:00:30Z"),
      step("msg_s2", "2026-09-09T10:01:20Z"),
      { ...assistantMessage(), id: "msg_a", created_at: "2026-09-09T10:01:52Z" },
    ]);
    expect(entries.map((entry) => entry.kind)).toEqual(["message", "work", "message"]);
    const work = entries[1];
    if (work?.kind !== "work") throw new Error("expected a work entry");
    expect(work.messages).toHaveLength(2);
    // From the user's message to the reply, not from the first step.
    expect(workLogLabel(work)).toBe("Worked for 1m 52s");
  });

  it("closes an unfinished run on its own last step", () => {
    const entries = foldTimeline([
      { ...userMessage(), id: "msg_u", created_at: "2026-09-09T10:00:00Z" },
      step("msg_s1", "2026-09-09T10:00:08Z"),
    ]);
    const work = entries[1];
    if (work?.kind !== "work") throw new Error("expected a work entry");
    expect(workLogLabel(work)).toBe("Worked for 8s");
  });

  it("names a run it cannot time by its size instead of guessing", () => {
    const entries = foldTimeline([step("msg_s1", "not a date"), step("msg_s2", "not a date")]);
    const work = entries[0];
    if (work?.kind !== "work") throw new Error("expected a work entry");
    expect(workLogLabel(work)).toBe("2 steps");
  });

  it("keeps separate runs separate", () => {
    const entries = foldTimeline([
      step("msg_s1", "2026-09-09T10:00:00Z"),
      { ...assistantMessage(), id: "msg_a1", created_at: "2026-09-09T10:00:10Z" },
      step("msg_s2", "2026-09-09T10:00:20Z"),
      { ...assistantMessage(), id: "msg_a2", created_at: "2026-09-09T10:00:30Z" },
    ]);
    expect(entries.map((entry) => entry.kind)).toEqual([
      "work",
      "message",
      "work",
      "message",
    ]);
  });
});
