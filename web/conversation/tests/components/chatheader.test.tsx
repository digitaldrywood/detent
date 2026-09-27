// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { connectionChip } from "../../src/app/App.tsx";
import ProjectScriptsControl from "../../src/components/ProjectScriptsControl.tsx";
import { PanelLayoutControls } from "../../src/components/chat/PanelLayoutControls.tsx";
import {
  ConversationActionsProvider,
  conversationActions,
  type ConversationAction,
} from "../../src/app/adapters/conversationActions.tsx";
import { detentKeybindings } from "../../src/app/adapters/keybindings.ts";

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

const NO_SCRIPTS: never[] = [];

function renderControl(
  actions: readonly ConversationAction[],
  run: (action: ConversationAction) => void,
) {
  return render(
    <ConversationActionsProvider value={{ actions, run }}>
      <ProjectScriptsControl
        scripts={NO_SCRIPTS}
        keybindings={detentKeybindings}
        onRunScript={vi.fn()}
        onAddScript={vi.fn()}
        onUpdateScript={vi.fn()}
        onDeleteScript={vi.fn()}
      />
    </ConversationActionsProvider>,
  );
}

describe("the header's conversation actions", () => {
  it("offers the linked-issue handoff on an unlinked chat, and a link to copy", () => {
    const actions = conversationActions({ canCreateIssue: true, issueIdentifier: null });
    expect(actions.map((action) => action.name)).toEqual(["Create linked issue", "Copy link"]);
  });

  it("offers the issue once the chat is linked, and no second handoff", () => {
    const actions = conversationActions({
      canCreateIssue: false,
      issueIdentifier: "parable#3363",
    });
    expect(actions.map((action) => action.name)).toEqual([
      "Open issue parable#3363",
      "Copy link",
    ]);
  });

  it("lists the conversation group and the Add action row, and runs what is picked", () => {
    const run = vi.fn();
    const actions = conversationActions({ canCreateIssue: true, issueIdentifier: null });
    renderControl(actions, run);
    fireEvent.click(screen.getByLabelText("Project actions"));
    expect(screen.getByText("This conversation")).toBeTruthy();

    expect(screen.getByRole("menuitem", { name: "Add action" })).toBeTruthy();
    fireEvent.click(screen.getByTestId("header-action-create-issue"));
    expect(run).toHaveBeenCalledTimes(1);
    expect(run.mock.calls[0]![0].id).toBe("create-issue");
  });
});

describe("the header's panel toggles", () => {
  it("names the right panel toggle with its shortcut and its live agent count", () => {
    render(
      <PanelLayoutControls
        terminalAvailable={false}
        terminalOpen={false}
        terminalShortcutLabel="Ctrl+J"
        rightPanelAvailable
        rightPanelOpen={false}
        rightPanelShortcutLabel="Ctrl+Alt+B"
        liveAgentCount={2}
        onToggleTerminal={vi.fn()}
        onToggleRightPanel={vi.fn()}
      />,
    );
    expect(screen.getByLabelText("Toggle right panel, 2 agents working")).toBeTruthy();
    expect((screen.getByLabelText("Toggle terminal drawer") as HTMLButtonElement).disabled).toBe(
      true,
    );
  });

  it("toggles the right panel", () => {
    const onToggleRightPanel = vi.fn();
    render(
      <PanelLayoutControls
        terminalAvailable={false}
        terminalOpen={false}
        terminalShortcutLabel={null}
        rightPanelAvailable
        rightPanelOpen={false}
        rightPanelShortcutLabel={null}
        liveAgentCount={0}
        onToggleTerminal={vi.fn()}
        onToggleRightPanel={onToggleRightPanel}
      />,
    );
    fireEvent.click(screen.getByLabelText("Toggle right panel"));
    expect(onToggleRightPanel).toHaveBeenCalledTimes(1);
  });
});

describe("connectionChip", () => {
  const at = Date.parse("2026-09-09T10:18:00Z");

  it("says the data is current while live", () => {
    const chip = connectionChip("live", at, vi.fn());
    expect(chip.label).toBe("Live");
    expect(chip.detail).toContain("data current");
    expect(chip.action).toBeNull();
  });

  it("says how old the data is while reconnecting, and offers a retry", () => {
    const chip = connectionChip("synchronizing", at, vi.fn());
    expect(chip.label).toBe("Reconnecting");
    expect(chip.detail).toContain("data as of");
    expect(chip.action?.label).toBe("Retry now");
  });

  it("names the same fact when offline, in the error tone", () => {
    const chip = connectionChip("cached", at, vi.fn());
    expect(chip.label).toBe("Offline");
    expect(chip.tone).toBe("dc-err");
    expect(chip.detail).toContain("data as of");
  });
});

