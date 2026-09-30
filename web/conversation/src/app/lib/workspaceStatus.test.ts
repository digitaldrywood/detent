// The words a surface shows for every workspace state and every reason
// (decisions.md §18.1). The point of the module is that a reader is never left
// with a skeleton and no sentence, so the cases below are exhaustive over both
// literal unions rather than a sample of them.
import { describe, expect, it } from "vitest";

import type { WorkspaceReason, WorkspaceState } from "../../contracts/work.ts";
import {
  describeWorkspaceStatus,
  formatElapsed,
  workspaceReasonSentence,
  workspaceRequestTimeoutPhrase,
  workspaceRunnerName,
  workspaceSessionFacts,
  type WorkspaceCapability,
} from "./workspaceStatus.ts";

const STATES: readonly WorkspaceState[] = [
  "requested",
  "starting",
  "ready",
  "idle",
  "unreachable",
  "closing",
  "closed",
  "failed",
];

const REASONS: readonly WorkspaceReason[] = [
  "no_runner",
  "checkout_failed",
  "worktree_missing",
  "runner_restarted",
  "hub_restarted",
  "lease_lost",
  "capacity",
  "closed_by_actor",
  "expired",
];

function status(
  state: WorkspaceState | null,
  overrides: Partial<Parameters<typeof describeWorkspaceStatus>[0]> = {},
) {
  return describeWorkspaceStatus({
    state,
    reason: null,
    capability: "files",
    ...overrides,
  });
}

describe("what a surface says while its workspace is not serving it", () => {
  it("says what it is waiting for, per capability, while the request is unclaimed", () => {
    expect(status("requested")?.sentence).toBe(
      "Waiting for a runner that can serve files for this project…",
    );
    expect(status("requested", { capability: "exec" })?.sentence).toBe(
      "Waiting for a runner that can run commands for this project…",
    );
  });

  it("counts from the request while it waits", () => {
    expect(status("requested", { session: { requestedAt: "2026-09-12T10:00:00Z" } })?.elapsedSince).toBe(
      "2026-09-12T10:00:00Z",
    );
    // Nothing to count from is not a reason to say nothing.
    expect(status("requested")?.elapsedSince).toBeNull();
    expect(status("starting")?.elapsedSince).toBeNull();
  });

  it("names the runner checking the worktree out, or says it plainly", () => {
    expect(status("starting", { session: { runnerId: "rnr_01J" } })?.sentence).toBe(
      "Checking out the worktree on rnr_01J…",
    );
    expect(status("starting", { session: { machineId: "mac_7" } })?.sentence).toBe(
      "Checking out the worktree on mac_7…",
    );
    expect(status("starting")?.sentence).toBe("Checking out the worktree on the runner…");
  });

  it("shows the workspace skeleton for `starting` and for nothing else", () => {
    for (const state of STATES) {
      expect(status(state)?.skeleton ?? false).toBe(state === "starting");
    }
    expect(status(null)?.skeleton).toBe(false);
  });

  it("says a live workspace needs no words, unless its socket is still opening", () => {
    expect(status("ready")).toBeNull();
    expect(status("idle")).toBeNull();
    expect(status("ready", { connected: true })).toBeNull();
    expect(status("ready", { connected: false })?.sentence).toBe("Connecting to the workspace…");
    expect(status("idle", { connected: false })?.testId).toBe("connecting");
  });

  it("says a runner that stopped answering may come back, per capability", () => {
    expect(status("unreachable")?.sentence).toBe(
      "The runner stopped answering. This workspace will serve files again if the runner comes back.",
    );
    expect(status("unreachable", { capability: "exec" })?.sentence).toBe(
      "The runner stopped answering. This workspace will run commands again if the runner comes back.",
    );
  });

  it("says a workspace that is closing is closing, and one the hub has not answered for is opening", () => {
    expect(status("closing")?.sentence).toBe("This workspace is closing.");
    expect(status(null)?.sentence).toBe("Opening a workspace for this conversation…");
    expect(status(null)?.testId).toBe("opening");
  });

  it("offers a retry on a failure and a fresh workspace on a close, and neither while waiting", () => {
    expect(status("failed")?.retryLabel).toBe("Retry");
    expect(status("closed")?.retryLabel).toBe("Open a new workspace");
    expect(status("requested")?.retryLabel).toBeNull();
    expect(status("starting")?.retryLabel).toBeNull();
    expect(status("unreachable")?.retryLabel).toBeNull();
    expect(status("closing")?.retryLabel).toBeNull();
    expect(status(null)?.retryLabel).toBeNull();
  });

  it("gives every state a sentence rather than a bare skeleton", () => {
    for (const state of STATES) {
      const presentation = status(state, { connected: false });
      expect(presentation).not.toBeNull();
      expect(presentation?.sentence.length ?? 0).toBeGreaterThan(0);
    }
  });
});

describe("the sentence for a workspace that failed or closed", () => {
  it("gives every reason its own", () => {
    const sentences: Record<WorkspaceReason, string> = {
      no_runner: "No runner reported the files capability before the request timed out.",
      checkout_failed: "The runner could not check the repository out.",
      worktree_missing: "The worktree this workspace was attached to is gone.",
      runner_restarted: "The runner restarted and released this worktree.",
      hub_restarted: "The hub restarted and this workspace was not re-bound in time.",
      lease_lost: "The runner stopped answering and lost its lease on the worktree.",
      capacity: "The runner had no capacity left for this workspace.",
      closed_by_actor: "This workspace was closed.",
      expired: "This workspace reached its idle timeout and closed.",
    };
    for (const reason of REASONS) {
      expect(status("failed", { reason })?.sentence).toBe(sentences[reason]);
    }
  });

  it("names the capability nobody reported, and how long it waited where that can be derived", () => {
    expect(
      status("failed", {
        reason: "no_runner",
        session: {
          requestedAt: "2026-09-12T10:00:00Z",
          endedAt: "2026-09-12T10:05:02Z",
        },
      })?.sentence,
    ).toBe("No runner reported the files capability within 5 minutes.");
    expect(
      status("failed", {
        reason: "no_runner",
        capability: "exec",
        session: { requestedAt: "2026-09-12T10:00:00Z", requestedExpiresAt: "2026-09-12T10:01:00Z" },
      })?.sentence,
    ).toBe("No runner reported the exec capability within 1 minute.");
  });

  it("falls back to the state when the reason is one this build does not know", () => {
    expect(workspaceReasonSentence("failed", null)).toBe("This workspace failed.");
    expect(workspaceReasonSentence("closed", null)).toBe("This workspace has closed.");
    // Called without a capability — the shape the workspace hook's callers use.
    expect(workspaceReasonSentence("failed", "no_runner")).toBe(
      "No runner reported the capabilities this workspace needs, so it timed out waiting.",
    );
  });
});

describe("the facts the sentences are built from", () => {
  it("counts elapsed time in the coarsest pair of units that still moves", () => {
    expect(formatElapsed(0)).toBe("0s");
    expect(formatElapsed(9_400)).toBe("9s");
    expect(formatElapsed(80_000)).toBe("1m 20s");
    expect(formatElapsed(59_999)).toBe("59s");
    expect(formatElapsed(60_000)).toBe("1m 0s");
    expect(formatElapsed(3_600_000)).toBe("1h 0m");
    expect(formatElapsed(7_380_000)).toBe("2h 3m");
    expect(formatElapsed(-1)).toBe("0s");
    expect(formatElapsed(Number.NaN)).toBe("0s");
  });

  it("prefers the stated deadline over the gap a failed request actually waited", () => {
    expect(
      workspaceRequestTimeoutPhrase({
        requestedAt: "2026-09-12T10:00:00Z",
        requestedExpiresAt: "2026-09-12T10:05:00Z",
        endedAt: "2026-09-12T11:00:00Z",
      }),
    ).toBe("5 minutes");
    expect(
      workspaceRequestTimeoutPhrase({
        requestedAt: "2026-09-12T10:00:00Z",
        endedAt: "2026-09-12T10:00:30Z",
      }),
    ).toBe("30 seconds");
  });

  it("names no number it cannot derive", () => {
    expect(workspaceRequestTimeoutPhrase(null)).toBeNull();
    expect(workspaceRequestTimeoutPhrase(undefined)).toBeNull();
    expect(workspaceRequestTimeoutPhrase({ requestedAt: "2026-09-12T10:00:00Z" })).toBeNull();
    expect(workspaceRequestTimeoutPhrase({ endedAt: "2026-09-12T10:05:00Z" })).toBeNull();
    expect(
      workspaceRequestTimeoutPhrase({ requestedAt: "not a time", endedAt: "2026-09-12T10:05:00Z" }),
    ).toBeNull();
    // A deadline before the request is not a duration.
    expect(
      workspaceRequestTimeoutPhrase({
        requestedAt: "2026-09-12T10:05:00Z",
        endedAt: "2026-09-12T10:00:00Z",
      }),
    ).toBeNull();
  });

  it("prefers the runner over the machine, and reports neither as none", () => {
    expect(workspaceRunnerName({ runnerId: "rnr_1", machineId: "mac_1" })).toBe("rnr_1");
    expect(workspaceRunnerName({ runnerId: "", machineId: "mac_1" })).toBe("mac_1");
    expect(workspaceRunnerName({ runnerId: null, machineId: null })).toBeNull();
    expect(workspaceRunnerName(null)).toBeNull();
  });

  it("reads the facts off a workspace resource", () => {
    expect(workspaceSessionFacts(null)).toBeNull();
    const facts = workspaceSessionFacts({
      runner_id: "rnr_2",
      machine_id: null,
      created_at: "2026-09-12T10:00:00Z",
      updated_at: "2026-09-12T10:05:00Z",
    } as unknown as Parameters<typeof workspaceSessionFacts>[0]);
    expect(facts).toEqual({
      runnerId: "rnr_2",
      machineId: null,
      requestedAt: "2026-09-12T10:00:00Z",
      // The hub does not serve `requested_expires_at`; the phrase is derived
      // from the failure instead, or left unsaid.
      requestedExpiresAt: null,
      endedAt: "2026-09-12T10:05:00Z",
    });
  });

  it("is typed over the two capabilities a surface can need", () => {
    const capabilities: readonly WorkspaceCapability[] = ["files", "exec"];
    for (const capability of capabilities) {
      expect(status("requested", { capability })?.kind).toBe("waiting");
    }
  });
});
