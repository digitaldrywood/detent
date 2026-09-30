// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import React from "react";
import { afterEach, describe, expect, it, vi } from "vitest";

import GitActionsControl from "../../src/components/GitActionsControl.tsx";
import { OpenInPicker } from "../../src/components/chat/OpenInPicker.tsx";
import {
  HeaderActionsProvider,
  DEFAULT_HEADER_ACTIONS,
  type HeaderActions,
} from "../../src/app/adapters/headerActions.tsx";
import type { HeaderGit } from "../../src/app/adapters/headerGit.ts";
import { toVcsStatus, type GitActionPolicy } from "../../src/app/adapters/gitStatus.ts";

afterEach(() => {
  // Dismiss anything still open before unmounting.
  //
  // Base UI's menus and popovers attach their dismissal machinery to the
  // document, not to their own subtree, and a popup that is still open when
  // React unmounts it does not get to take that machinery back down. Every
  // test that left a menu open therefore added another set of document
  // listeners for the rest of the file, and each `fireEvent` after it had to
  // walk all of them: twenty tests in this file were taking four minutes, with
  // the cost landing on whichever test came next rather than on the one that
  // opened the menu. Escape is how a reader closes them, so it is how a test
  // closes them too.
  fireEvent.keyDown(document.activeElement ?? document.body, { key: "Escape" });
  cleanup();
});

function withActions(actions: Partial<HeaderActions>, node: React.ReactNode) {
  return render(
    <HeaderActionsProvider value={{ ...DEFAULT_HEADER_ACTIONS, ...actions }}>
      {node}
    </HeaderActionsProvider>,
  );
}

function headerGit(
  overrides: {
    policy?: Partial<GitActionPolicy>;
    status?: HeaderGit["status"] | null;
    connector?: { baseUrl: string } | null;
  } = {},
): HeaderGit & { run: ReturnType<typeof vi.fn>; refresh: ReturnType<typeof vi.fn> } {
  const policy: GitActionPolicy = {
    workItemId: "wi_1",
    canWrite: true,
    hasConnector: true,
    workspaceState: "ready",
    workspaceReadOnly: false,
    workspaceError: null,
    gitCapable: true,
    busy: false,
    ...overrides.policy,
  };
  const connector =
    overrides.connector === undefined ? { baseUrl: "https://github.com" } : overrides.connector;
  const status =
    overrides.status === null
      ? null
      : (overrides.status ??
        toVcsStatus({
          status: {
            branch: "detent/parable_3363",
            detached: false,
            remote: "origin",
            upstream: true,
            ahead: 1,
            behind: 0,
            dirty_file_count: 2,
            head_sha: "a".repeat(40),
          },
          pullRequest: null,
          connector,
        }));
  const run = vi.fn();
  const refresh = vi.fn();
  return {
    policy,
    status,
    statusError: null,
    busy: false,
    worktreePath: "/Users/runner/code/widgets",
    machineHostname: "mac-studio",
    run,
    refresh,
  };
}

describe("the header's Open split button", () => {
  it("refuses on a chat with no linked issue, in the git group's own words", () => {
    withActions({}, <OpenInPicker />);
    expect((screen.getByTestId("header-open") as HTMLButtonElement).disabled).toBe(true);
    expect(screen.getByTestId("header-open").getAttribute("data-disabled-reason")).toBe(
      "Link an issue to get a worktree.",
    );
  });

  // The regression this file exists to prevent a second time.
  //
  // On Detent Cloud the runner is normally somewhere else, so an editor is
  // normally unreachable. A primary that only ever launched an editor was
  // therefore dead in the ordinary case — and it had been opening the linked
  // issue since the port began, which a browser spec has asserted all along.
  // Making the editors its only meaning quietly took a working control away.
  it("falls back to the linked issue when no editor can reach the worktree", () => {
    const run = vi.fn();
    withActions(
      {
        openLocation: {
          worktreePath: "/Users/runner/code/widgets",
          hostname: "mac-studio",
          local: false,
          linked: true,
        },
        openTargets: [{ id: "issue", label: "Open parable#3363 in Work", run, external: false }],
      },
      <OpenInPicker />,
    );
    const primary = screen.getByTestId("header-open") as HTMLButtonElement;
    expect(primary.disabled).toBe(false);
    // Enabled means no refusal to show: the rows below still each say why they
    // cannot run, so nothing is being hidden here.
    expect(primary.getAttribute("data-disabled-reason")).toBeNull();
    fireEvent.click(primary);
    expect(run).toHaveBeenCalledTimes(1);
  });

  it("refuses only when neither an editor nor a destination can be reached", () => {
    withActions(
      {
        openLocation: {
          worktreePath: "/Users/runner/code/widgets",
          hostname: "mac-studio",
          local: false,
          linked: true,
        },
        openTargets: [],
      },
      <OpenInPicker />,
    );
    const primary = screen.getByTestId("header-open") as HTMLButtonElement;
    expect(primary.disabled).toBe(true);
    expect(primary.getAttribute("data-disabled-reason")).toBe("This worktree is on mac-studio.");
  });

});

describe("the header's Commit, push & PR control", () => {

  it("refuses on a chat with no linked issue, with the reason on it", () => {
    withActions({}, <GitActionsControl />);
    const button = screen.getByTestId("header-commit-push-pr");
    expect(button.getAttribute("aria-disabled")).toBe("true");
    expect(button.className).toContain("cursor-not-allowed");
    expect(button.getAttribute("data-disabled-reason")).toBe("Link an issue to get a worktree.");
  });

  it("is pressable before any workspace exists, because pressing is what asks for one", () => {
    const git = headerGit({ status: null });
    withActions({ git }, <GitActionsControl />);
    const button = screen.getByTestId("header-commit-push-pr") as HTMLButtonElement;
    expect(button.disabled).toBe(false);
    expect(button.getAttribute("data-disabled-reason")).toBeNull();
    expect(button.textContent).toContain("Commit, push & PR");
  });

  it("shows a policy refusal in place of the state reason", () => {
    withActions(
      { git: headerGit({ policy: { workspaceReadOnly: true } }) },
      <GitActionsControl />,
    );
    expect(
      screen.getByTestId("header-commit-push-pr").getAttribute("data-disabled-reason"),
    ).toBe("This worktree is read-only while the attempt is still running.");
  });

  it("opens T3's commit dialog for an action that commits, and runs it on submit", () => {
    const git = headerGit();
    withActions({ git }, <GitActionsControl />);
    fireEvent.click(screen.getByTestId("header-commit-push-pr"));
    expect(screen.getByText("Commit changes")).toBeTruthy();
    const field = screen.getByRole("textbox", { name: /message/i });
    fireEvent.change(field, { target: { value: "feat(header): the git group" } });
    fireEvent.click(screen.getByRole("button", { name: /^Commit/ }));
    expect(git.run).toHaveBeenCalledWith("commit_push_pr", "feat(header): the git group");
  });
});
