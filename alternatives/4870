import { describe, expect, it } from "vitest";

import { runnerDisplay, shortRunnerId, type RunnerNames } from "../src/app/work/lib/runnerNames.ts";
import { actorLabel, runnerLabel } from "../src/app/work/lib/activity.ts";
import { toAttemptView } from "../src/app/work/lib/fromWire.ts";
import attemptsFixture from "../src/contracts/fixtures/work-attempt-list.json";
import type { NativeAttempt } from "../src/contracts/work.ts";

const ATTEMPTS = (attemptsFixture as { items: unknown[] }).items as unknown as NativeAttempt[];
const NAMES: RunnerNames = new Map([
  ["runner_be4aaeec1c424bb6afbe94aea6d605d6", { display: "Mac Studio", host: "mac-studio.local" }],
  ["rnr_mac_studio", { display: "Mac Studio", host: "mac-studio.local" }],
]);

describe("runner names", () => {
  it("shortens a generated id and keeps a readable one", () => {
    expect(shortRunnerId("runner_be4aaeec1c424bb6afbe94aea6d605d6")).toBe("runner_be4aaeec");
    expect(shortRunnerId("machine_847830833ad440379a52ff5d026bcdc4")).toBe("machine_84783083");
    expect(shortRunnerId("rnr_mac_studio")).toBe("rnr_mac_studio");
  });

  it("prefers the fleet's display name and falls back to the short id", () => {
    expect(runnerDisplay(NAMES, "runner_be4aaeec1c424bb6afbe94aea6d605d6")).toBe("Mac Studio");
    expect(runnerDisplay(NAMES, "runner_0000000000000000000000000000dead")).toBe("runner_00000000");
    expect(runnerDisplay(undefined, "runner_0000000000000000000000000000dead")).toBe("runner_00000000");
    expect(runnerDisplay(NAMES, null)).toBeNull();
  });

  it("names a runner actor through the fleet before the attempt's runner", () => {
    const actor = { kind: "runner" as const, principal_id: "runner_be4aaeec1c424bb6afbe94aea6d605d6" };
    expect(actorLabel(actor, null, "rnr_other", NAMES)).toBe("Mac Studio");
    expect(actorLabel({ kind: "runner", principal_id: "tok_r" }, null, "rnr_other", NAMES)).toBe("rnr_other");
    expect(
      actorLabel({ kind: "runner", principal_id: "runner_0000000000000000000000000000dead" }, null, null, NAMES),
    ).toBe("runner_00000000");
    expect(actorLabel({ kind: "human", principal_id: "hosted_1" }, null, null, NAMES)).toBe("hosted_1");
  });

  it("labels attempts and the worker strip by display name", () => {
    const attempt = { ...ATTEMPTS[0]!, runner_id: "rnr_mac_studio" };
    expect(runnerLabel(attempt, NAMES)).toBe("Mac Studio");
    expect(runnerLabel(attempt)).toBe("rnr_mac_studio");
    expect(toAttemptView([attempt], NAMES)?.runner).toBe("Mac Studio");
    expect(toAttemptView([{ ...attempt, runner_id: "runner_be4aaeec1c424bb6afbe94aea6d605d6" }])?.runner).toBe("runner_be4aaeec");
  });
});
