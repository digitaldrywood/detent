// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { OutputSurface, runStatusLabel } from "../../src/app/components/surfaces/OutputSurface.tsx";
import type { ActionRunSnapshot } from "../../src/app/adapters/actionRuns.ts";

afterEach(cleanup);

function run(overrides: Partial<ActionRunSnapshot> = {}): ActionRunSnapshot {
  return {
    runId: "run_1",
    actionId: "action_1",
    name: "Run tests",
    command: "bun test",
    status: "succeeded",
    output: [{ text: "2 passed\n", truncated: false }],
    truncated: false,
    exitCode: 0,
    signal: null,
    error: null,
    ...overrides,
  };
}

function mount(overrides: Partial<React.ComponentProps<typeof OutputSurface>> = {}) {
  return render(
    <OutputSurface
      runs={[]}
      activeRunId={null}
      onSelectRun={vi.fn()}
      state="ready"
      reason={null}
      error={null}
      loading={false}
      onRetry={vi.fn()}
      {...overrides}
    />,
  );
}

describe("the Output surface", () => {
  it("says where output comes from before any run", () => {
    mount();
    expect(screen.getByTestId("output-empty").textContent).toContain(
      "Run a project action from the header",
    );
  });

  it("says what the workspace is doing while it comes up", () => {
    mount({ state: "starting", session: { runnerId: "rnr_01J" } });
    expect(screen.getByTestId("output-starting").textContent).toContain(
      "Checking out the worktree on rnr_01J",
    );
  });

  // The exec capability is what this surface waits on, so the sentence says
  // commands rather than files (decisions.md §18.12).
  it("says it is waiting for a runner that can run commands, and for how long", () => {
    mount({
      state: "requested",
      session: { requestedAt: new Date(Date.now() - 80_000).toISOString() },
    });
    expect(screen.getByTestId("output-waiting").textContent).toContain(
      "Waiting for a runner that can run commands for this project",
    );
    expect(screen.getByTestId("output-elapsed").textContent).toBe("1m 20s elapsed");
  });

  it("names the exec capability nobody reported, and offers a retry", async () => {
    const onRetry = vi.fn();
    mount({ state: "failed", reason: "no_runner", onRetry });
    expect(screen.getByTestId("output-unavailable").textContent).toContain(
      "No runner reported the exec capability",
    );
    await userEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(onRetry).toHaveBeenCalledTimes(1);
  });

  it("draws the output in the code-block chrome, with their wrap and copy", () => {
    const { container } = mount({ runs: [run()], activeRunId: "run_1" });
    // The chrome, and the `.chat-markdown` scope its CSS lives under.
    expect(container.querySelector(".chat-markdown .chat-markdown-codeblock")).toBeTruthy();
    expect(screen.getByLabelText("Wrap lines")).toBeTruthy();
    expect(screen.getByLabelText("Copy code")).toBeTruthy();
    // The command names the block, and the bytes are in it.
    expect(screen.getByText("bun test")).toBeTruthy();
    expect(screen.getByTestId("output-run").textContent).toContain("2 passed");
    expect(screen.getByTestId("output-run-status").textContent).toBe("Exited 0");
  });

  // §18.12 makes "exited 0" and "never exited" different facts, so the chip
  // carries the code rather than only the outcome.
  it("names the exit code, or says the run never produced one", () => {
    expect(runStatusLabel(run({ status: "failed", exitCode: 3 }))).toBe("Exited 3");
    expect(runStatusLabel(run({ status: "failed", exitCode: null }))).toBe("Failed");
    expect(runStatusLabel(run({ status: "running", exitCode: null }))).toBe("Running");
    expect(runStatusLabel(run({ status: "queued", exitCode: null }))).toBe("Queued");
  });

  it("says a cut log was cut", () => {
    mount({
      runs: [
        run({
          truncated: true,
          output: [{ text: "[output truncated at 1 MiB]", truncated: true }],
        }),
      ],
      activeRunId: "run_1",
    });
    expect(screen.getByTestId("output-truncated").textContent).toContain(
      "[output truncated at 1 MiB]",
    );
  });

  it("reports a run that failed without exiting", () => {
    mount({
      runs: [
        run({
          status: "failed",
          exitCode: null,
          output: [],
          error: "The runner lost its lease on the worktree while the command was running.",
        }),
      ],
      activeRunId: "run_1",
    });
    expect(screen.getByTestId("output-run-error").textContent).toContain("lost its lease");
    // A finished run with nothing printed says so rather than showing nothing.
    expect(screen.getByTestId("output-run").textContent).toContain("(no output)");
  });

  it("offers a picker once there is more than one run", () => {
    mount({
      runs: [run(), run({ runId: "run_2", name: "Lint", command: "bun lint" })],
      activeRunId: "run_1",
    });
    expect(screen.getByTestId("output-run-picker")).toBeTruthy();
  });

  it("shows a recorded run even though the workspace has closed", () => {
    mount({ runs: [run()], activeRunId: "run_1", state: "closed", reason: "expired" });
    expect(screen.getByTestId("output-run")).toBeTruthy();
    expect(screen.queryByTestId("output-unavailable")).toBeNull();
  });

  // §18.12: a run the hub dispatched to the runner itself has no exec stream in
  // this tab at all — its whole record is the row and the bytes behind
  // `output_artifact`, read back by `loadRecordedRuns`. The surface has to draw
  // that the same way it draws a run it watched, or a headless run stays
  // invisible however completely the hub recorded it.
  it("draws a run read back from the hub with no stream behind it", () => {
    mount({
      runs: [
        run({
          runId: "actionrun_headless",
          name: "Greet",
          command: "echo hello",
          // One span, which is what the output endpoint's whole body becomes.
          output: [{ text: "hello\n", truncated: false }],
        }),
      ],
      activeRunId: "actionrun_headless",
      // No live workspace: nobody asked for a worktree in this tab.
      state: null,
    });
    expect(screen.getByTestId("output-run").textContent).toContain("hello");
    expect(screen.getByTestId("output-run-status").textContent).toContain(
      runStatusLabel(run({ status: "succeeded" })),
    );
    expect(screen.queryByTestId("output-empty")).toBeNull();
  });
});
