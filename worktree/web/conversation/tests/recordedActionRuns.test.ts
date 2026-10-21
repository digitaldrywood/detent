// Reading a recorded run back (decisions.md §18.12).
//
// The eighth dogfood run queued a run through the API, got its 202, and the row
// sat `queued` for three minutes and forty seconds because nothing but a
// browser's exec stream ever started one. The hub now hands such a run to the
// runner itself — and the half that makes it visible is this one: the Output
// surface only ever held runs *this tab* started over the exec channel, so a
// run nobody watched was invisible here however completely the hub had recorded
// it.
//
// `loadRecordedRuns` is what closes that. It is tested against a narrow reader
// rather than the whole `WorkHttp` client, because what has to be right is
// which three reads it makes and what it does with the answers.
import { describe, expect, it } from "vitest";

import { loadRecordedRuns, outputText, type RecordedRunReader } from "../src/app/adapters/actionRuns.ts";
import type { Action, ActionRun } from "../src/contracts/work.ts";

function action(overrides: Partial<Action> = {}): Action {
  return {
    id: "action_1",
    name: "Greet",
    command: "echo hello",
    icon: "play",
    open_preview: false,
    run_on_worktree_creation: false,
    created_by: "user_1",
    revision: 1,
    created_at: "2026-09-12T17:00:00Z",
    updated_at: "2026-09-12T17:00:00Z",
    ...overrides,
  } as Action;
}

function record(overrides: Partial<ActionRun> = {}): ActionRun {
  return {
    id: "actionrun_1",
    action_id: "action_1",
    workspace_id: "ws_1",
    command: "echo hello",
    status: "succeeded",
    exit_code: 0,
    started_at: "2026-09-12T17:00:01Z",
    finished_at: "2026-09-12T17:00:02Z",
    output_bytes: 6,
    truncated: false,
    revision: 3,
    created_at: "2026-09-12T17:00:00Z",
    updated_at: "2026-09-12T17:00:02Z",
    ...overrides,
  } as ActionRun;
}

interface Calls {
  readonly actions: string[];
  readonly runs: string[];
  readonly outputs: string[];
}

function reader(
  actions: readonly Action[],
  runsByAction: Readonly<Record<string, readonly ActionRun[]>>,
  outputs: Readonly<Record<string, string>> = {},
): { reader: RecordedRunReader; calls: Calls } {
  const calls: Calls = { actions: [], runs: [], outputs: [] };
  return {
    calls,
    reader: {
      async listActions(projectId) {
        calls.actions.push(projectId);
        return { items: actions };
      },
      async listActionRuns(_projectId, actionId) {
        calls.runs.push(actionId);
        const items = runsByAction[actionId];
        if (items === undefined) throw new Error(`no runs for ${actionId}`);
        return { items };
      },
      async readActionRunOutput(_projectId, _actionId, runId) {
        calls.outputs.push(runId);
        const text = outputs[runId];
        if (text === undefined) throw new Error(`no output for ${runId}`);
        return text;
      },
    },
  };
}

describe("loadRecordedRuns", () => {
  it("reads a finished run's recorded output back", async () => {
    const { reader: source, calls } = reader(
      [action()],
      { action_1: [record()] },
      { actionrun_1: "hello\n" },
    );

    const loaded = await loadRecordedRuns(source, "prj_1");

    expect(loaded).toHaveLength(1);
    // The run the hub recorded, with the action's own name rather than the
    // placeholder a run this tab never started used to get.
    expect(loaded[0]?.name).toBe("Greet");
    expect(loaded[0]?.status).toBe("succeeded");
    expect(outputText(loaded[0]!)).toBe("hello\n");
    expect(calls).toEqual({ actions: ["prj_1"], runs: ["action_1"], outputs: ["actionrun_1"] });
  });

  it("does not spend a request on a run with nothing to read", async () => {
    // §18.12 accumulates output in memory and writes it once on completion, so
    // a run still going has nothing stored; and a command that printed nothing
    // and exited zero has no bytes. Either way the read would be answered with
    // an empty body.
    const { reader: source, calls } = reader([action()], {
      action_1: [
        record({ id: "actionrun_running", status: "running", exit_code: null, output_bytes: 0 }),
        record({ id: "actionrun_silent", output_bytes: 0 }),
      ],
    });

    const loaded = await loadRecordedRuns(source, "prj_1");

    expect(loaded.map((entry) => entry.runId)).toEqual(["actionrun_running", "actionrun_silent"]);
    expect(loaded.every((entry) => entry.output.length === 0)).toBe(true);
    expect(calls.outputs).toEqual([]);
  });

  it("orders every action's runs together, newest first", async () => {
    // Each action's own listing is already newest first, but the panel shows
    // one list: without the merge the order would be whichever request
    // answered first.
    const { reader: source } = reader(
      [action(), action({ id: "action_2", name: "Build", command: "make" })],
      {
        action_1: [record({ id: "run_old", created_at: "2026-09-12T17:00:00Z", output_bytes: 0 })],
        action_2: [
          record({
            id: "run_new",
            action_id: "action_2",
            created_at: "2026-09-12T18:00:00Z",
            output_bytes: 0,
          }),
        ],
      },
    );

    const loaded = await loadRecordedRuns(source, "prj_1");

    expect(loaded.map((entry) => entry.runId)).toEqual(["run_new", "run_old"]);
    expect(loaded.map((entry) => entry.name)).toEqual(["Build", "Greet"]);
  });

  it("still shows a run whose output could not be fetched", async () => {
    // A reader who can see that the command failed, and why, is better served
    // than one shown nothing because the log read was refused.
    const { reader: source } = reader(
      [action()],
      {
        action_1: [
          record({ status: "failed", exit_code: null, reason: "lease_lost", output_bytes: 12 }),
        ],
      },
      {},
    );

    const loaded = await loadRecordedRuns(source, "prj_1");

    expect(loaded).toHaveLength(1);
    expect(loaded[0]?.status).toBe("failed");
    expect(loaded[0]?.output).toEqual([]);
    expect(loaded[0]?.error).toContain("lost its lease");
  });

  it("keeps the other actions when one listing fails", async () => {
    const { reader: source } = reader(
      [action(), action({ id: "action_missing", name: "Lint", command: "lint" })],
      { action_1: [record({ output_bytes: 0 })] },
    );

    const loaded = await loadRecordedRuns(source, "prj_1");

    expect(loaded.map((entry) => entry.runId)).toEqual(["actionrun_1"]);
  });
});
