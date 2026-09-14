// @vitest-environment jsdom
//
// The issue page's composer (decisions.md §19.5).
//
// What is asserted here is the shape Michael asked for: the issue page's
// composer is a tracker's comment box and nothing else. There is no runner
// mode any more (his review of September 12) — a turn is steered from the
// conversation surface the Activity feed's live row opens — so nothing on this
// card configures a turn, takes a file, or chooses what the send does.
import { cleanup, render, screen } from "@testing-library/react";
import React from "react";
import { afterEach, describe, expect, it, vi } from "vitest";

import {
  IssueComposer,
  issueComposerSlashCommands,
} from "../../src/app/work/components/IssueComposer.tsx";

afterEach(cleanup);

function setup(overrides: Partial<React.ComponentProps<typeof IssueComposer>> = {}) {
  const onComment = vi.fn(async () => {});
  render(<IssueComposer canWrite onComment={onComment} {...overrides} />);
  return { onComment };
}

describe("the issue composer", () => {
  // Linear's box: a prompt and a send. A comment has no model, no reasoning
  // effort and no runtime access to choose.
  it("shows no turn pickers", () => {
    setup();
    for (const label of ["Model", "Reasoning effort", "Runtime access"]) {
      expect(screen.queryByLabelText(label)).toBeNull();
    }
    expect(screen.getByLabelText("Comment").getAttribute("aria-placeholder")).toBe(
      "Leave a comment…",
    );
  });

  // The strip under the card, the shortcut line under that and the mode toggle
  // in the footer were each the weight this page asked to lose.
  it("carries no context strip, no hint line, no mode toggle and no scope sentence", () => {
    setup();
    expect(document.querySelector("[data-slot='composer-context-strip']")).toBeNull();
    expect(document.getElementById("dc-issue-composer-hint")).toBeNull();
    expect(screen.queryByTestId("issue-composer-mode")).toBeNull();
    expect(screen.queryByTestId("mode-comment")).toBeNull();
    expect(screen.queryByTestId("mode-runner")).toBeNull();
    expect(screen.queryByTestId("issue-composer-scope")).toBeNull();
    // An `aria-describedby` pointing at a hint that is no longer there is
    // worse than none at all.
    expect(screen.getByLabelText("Comment").getAttribute("aria-describedby")).toBeNull();
  });

  // The hub's comment endpoint takes a body and nothing else, so a paperclip
  // here — even a disabled one — would promise a feature that is not coming.
  it("offers no attach control", () => {
    setup();
    expect(screen.queryByTestId("composer-attach")).toBeNull();
    expect(screen.queryByTestId("composer-attach-input")).toBeNull();
  });

  it("says why a reader who cannot write may not comment", () => {
    setup({ canWrite: false });
    expect(screen.getByLabelText("Comment").getAttribute("aria-disabled")).toBe("true");
  });
});

// The list itself needs layout jsdom has not got
// (`tests/visual/conversation.spec.js` drives it in a browser). What the page
// puts *in* the menu is checkable here.
describe("the issue composer's slash commands", () => {
  it("offers the shortcuts panel when the page can open it", () => {
    const onOpenShortcuts = vi.fn();
    const commands = issueComposerSlashCommands({ onOpenShortcuts });
    expect(commands.map((command) => command.name)).toEqual(["shortcuts"]);
    commands[0]?.run();
    expect(onOpenShortcuts).toHaveBeenCalledTimes(1);
  });

  // No mode to switch and no runner to send to: the page adds nothing, and
  // `/clear` — the one built-in a comment box has — comes from `Composer`.
  it("adds nothing when the page cannot open the shortcuts", () => {
    expect(issueComposerSlashCommands({ onOpenShortcuts: null })).toEqual([]);
  });
});
