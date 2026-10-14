// @vitest-environment jsdom
import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import React from "react";

import {
  ALL_KEYBINDING_COMMANDS,
  DEFAULT_BINDINGS,
  commandLabel,
  keybindingCatalogue,
  registerProjectActionKeybindings,
  type KeybindingCommand,
} from "../../src/app/adapters/keybindings.ts";
import {
  useKeybindingActions,
  type KeybindingActions,
} from "../../src/app/adapters/keybindingActions.ts";
import { KeybindingsSettings } from "../../src/app/settings/Settings.tsx";

afterEach(cleanup);

/**
 * Presses the chord `DEFAULT_BINDINGS` holds for a command.
 *
 * jsdom reports no platform, so `Mod` resolves to Control exactly as it does
 * for a reader who is not on a Mac — which is the branch worth driving here,
 * since the Mac branch is the one `formatShortcutLabel` is tested on below.
 * Returns false when a listener claimed the event.
 */
function pressChordFor(command: KeybindingCommand): boolean {
  const shortcut = DEFAULT_BINDINGS[command];
  const letter = /^[a-z]$/.test(shortcut.key) ? { code: `Key${shortcut.key.toUpperCase()}` } : {};
  return fireEvent.keyDown(window, {
    key: shortcut.key,
    ...letter,
    metaKey: shortcut.metaKey,
    ctrlKey: shortcut.ctrlKey || shortcut.modKey,
    shiftKey: shortcut.shiftKey,
    altKey: shortcut.altKey,
  });
}

function Harness({ actions }: { actions: KeybindingActions }): React.ReactElement {
  useKeybindingActions(actions);
  return <div>bound</div>;
}

describe("the keybinding table", () => {
  // Every command Detent binds, driven through the chord the table actually
  // resolves — so a chord that changes without its handler shows up here.
  const bound: readonly KeybindingCommand[] = [
    "chat.new",
    "chat.newLocal",
    "rightPanel.toggle",
    "rightPanel.close",
    "rightPanel.toggleMaximized",
    "diff.toggle",
    "thread.copyReference",
    "thread.stop",
    // The issue page's property pickers (decisions.md §19.1). These are the
    // only bare letters the table binds.
    "issue.status",
    "issue.priority",
    "issue.assignee",
    "issue.labels",
    "issue.related",
  ];

  for (const command of bound) {
    it(`runs the handler for ${command}`, () => {
      const action = vi.fn();
      render(<Harness actions={{ [command]: action } as KeybindingActions} />);
      pressChordFor(command);
      expect(action).toHaveBeenCalledTimes(1);
    });
  }

  it("leaves the keystroke alone when the command has no handler", () => {
    render(<Harness actions={{}} />);
    // `fireEvent` returns false when a listener called preventDefault.
    expect(pressChordFor("diff.toggle")).toBe(true);
  });

  // A command can be bound and still unavailable — `thread.stop` while no turn
  // is running. Declining must not swallow the keystroke, or the reader loses
  // whatever the browser would have done with it.
  it("leaves the keystroke alone when the handler declines", () => {
    const action = vi.fn(() => false);
    render(<Harness actions={{ "thread.stop": action }} />);
    expect(pressChordFor("thread.stop")).toBe(true);
    expect(action).toHaveBeenCalledTimes(1);
  });

  it("claims the keystroke when the handler acts", () => {
    render(<Harness actions={{ "thread.stop": () => true }} />);
    expect(pressChordFor("thread.stop")).toBe(false);
  });

  it("ignores a chord typed inside a keybinding capture", () => {
    const action = vi.fn();
    render(
      <div data-keybinding-capture="">
        <Harness actions={{ "diff.toggle": action }} />
        <input aria-label="capture" />
      </div>,
    );
    fireEvent.keyDown(screen.getByLabelText("capture"), {
      key: "d",
      code: "KeyD",
      metaKey: true,
    });
    expect(action).not.toHaveBeenCalled();
  });

  describe("a bare letter typed into a field", () => {
    for (const test of [
      { name: "a text input", tag: "input" as const, type: "text" },
      { name: "a search input", tag: "input" as const, type: "search" },
      { name: "a textarea", tag: "textarea" as const },
      { name: "a rich-text editor", tag: "editor" as const },
    ]) {
      it(`does not run ${test.name}'s picker shortcut`, () => {
        const bare = vi.fn();
        const modified = vi.fn();
        render(
          <div>
            <Harness actions={{ "issue.status": bare, "diff.toggle": modified }} />
            {test.tag === "textarea" ? (
              <textarea aria-label="field" />
            ) : test.tag === "editor" ? (
              // eslint-disable-next-line jsx-a11y/no-noninteractive-element-interactions
              <div aria-label="field" contentEditable suppressContentEditableWarning role="textbox" />
            ) : (
              <input aria-label="field" type={test.type} />
            )}
          </div>,
        );
        const field = screen.getByLabelText("field");
        fireEvent.keyDown(field, { key: "s", code: "KeyS" });
        expect(bare).not.toHaveBeenCalled();
        // The modified chord still reaches its handler from the same field.
        fireEvent.keyDown(field, { key: "d", code: "KeyD", ctrlKey: true });
        expect(modified).toHaveBeenCalledTimes(1);
      });
    }

    it("runs it from a checkbox, which no letter types into", () => {
      const action = vi.fn();
      render(
        <div>
          <Harness actions={{ "issue.status": action }} />
          <input aria-label="tick" type="checkbox" />
        </div>,
      );
      fireEvent.keyDown(screen.getByLabelText("tick"), { key: "s", code: "KeyS" });
      expect(action).toHaveBeenCalledTimes(1);
    });
  });

  // One project action's chord (decisions.md §18.12). The command is
  // `script.<slug>.run`, which is not a member of the closed record, so the
  // listener parses the slug out and routes it to `runProjectAction` — see
  // that member's own comment for why it is not an index signature.
  describe("a project action's chord", () => {
    afterEach(() => registerProjectActionKeybindings([]));

    function pressRegistered(): boolean {
      return fireEvent.keyDown(window, {
        key: "t",
        code: "KeyT",
        metaKey: false,
        ctrlKey: true,
        shiftKey: true,
        altKey: false,
      });
    }

    it("reaches the run handler with the action's slug", () => {
      registerProjectActionKeybindings([
        { command: "script.run-tests.run", chord: "mod+shift+t" },
      ]);
      const runProjectAction = vi.fn();
      render(<Harness actions={{ runProjectAction }} />);
      expect(pressRegistered()).toBe(false);
      expect(runProjectAction).toHaveBeenCalledWith("run-tests");
    });

    it("leaves the keystroke alone with no handler, and when the handler declines", () => {
      registerProjectActionKeybindings([
        { command: "script.run-tests.run", chord: "mod+shift+t" },
      ]);
      render(<Harness actions={{}} />);
      expect(pressRegistered()).toBe(true);
      cleanup();

      const declining = vi.fn(() => false);
      render(<Harness actions={{ runProjectAction: declining }} />);
      expect(pressRegistered()).toBe(true);
      expect(declining).toHaveBeenCalledTimes(1);
    });

    it("does nothing once the action is gone", () => {
      const runProjectAction = vi.fn();
      render(<Harness actions={{ runProjectAction }} />);
      expect(pressRegistered()).toBe(true);
      expect(runProjectAction).not.toHaveBeenCalled();
    });
  });
});

describe("the keybinding catalogue", () => {
  it("accounts for every shared UI command exactly once", () => {
    const rows = keybindingCatalogue("MacIntel");
    expect(rows).toHaveLength(ALL_KEYBINDING_COMMANDS.length);
    expect(new Set(rows.map((row) => row.command)).size).toBe(rows.length);
  });

  it("gives every row either a chord or a reason, never both and never neither", () => {
    for (const row of keybindingCatalogue("MacIntel")) {
      expect(row.chord === null).toBe(row.reason !== null);
    }
  });

  it("labels a command", () => {
    expect(commandLabel("rightPanel.toggleMaximized")).toBe("Right Panel: Toggle Maximized");
    expect(commandLabel("thread.next")).toBe("Thread: Next");
  });

  it("binds the three terminal commands the surface serves", () => {

    const rows = keybindingCatalogue("MacIntel");
    expect(rows.find((entry) => entry.command === "terminal.toggle")?.chord).toBe("⌘J");
    expect(rows.find((entry) => entry.command === "terminal.new")?.chord).toBe("⌥⌘T");
    expect(rows.find((entry) => entry.command === "terminal.close")?.chord).toBe("⌥⇧⌘W");
  });

  it("names the reason a terminal split is unbound", () => {

    const row = keybindingCatalogue("MacIntel").find((entry) => entry.command === "terminal.split");
    expect(row?.chord).toBeNull();
    expect(row?.reason).toContain("one shell at a time");
  });

  it("formats a bound chord for the reader's platform", () => {
    const mac = keybindingCatalogue("MacIntel").find((row) => row.command === "diff.toggle");
    const other = keybindingCatalogue("Win32").find((row) => row.command === "diff.toggle");
    expect(mac?.chord).toBe("⌘D");
    expect(other?.chord).toBe("Ctrl+D");
  });
});

function renderKeybindingsSection(): void {
  const root = createRootRoute();
  const section = createRoute({
    getParentRoute: () => root,
    path: "/settings/$section",
    component: () => <KeybindingsSettings />,
  });
  const router = createRouter({
    routeTree: root.addChildren([section]),
    history: createMemoryHistory({ initialEntries: ["/settings/keybindings"] }),
  });
  render(<RouterProvider router={router as never} />);
}

describe("the Keybindings settings section", () => {
  it("lists every shared UI command, and marks the unbound ones with their reason", async () => {
    renderKeybindingsSection();
    const rows = [
      ...(await screen.findAllByTestId("keybinding-bound")),
      ...screen.getAllByTestId("keybinding-unbound"),
    ];
    expect(rows).toHaveLength(ALL_KEYBINDING_COMMANDS.length);

    const unbound = screen.getAllByTestId("keybinding-unbound");
    expect(unbound.length).toBeGreaterThan(0);
    for (const row of unbound) {
      expect(within(row).getByText("Unbound")).toBeTruthy();
      expect(row.textContent?.length ?? 0).toBeGreaterThan("Unbound".length);
    }
  });

  it("shows a bound command's chord rather than the word Unbound", async () => {
    renderKeybindingsSection();
    const diff = (await screen.findAllByTestId("keybinding-bound"))
      .find((row) => row.getAttribute("data-command") === "diff.toggle");
    expect(diff).toBeTruthy();
    expect(within(diff as HTMLElement).queryByText("Unbound")).toBeNull();
  });

  it("keeps the terminal splits listed with the reason they are unbound", async () => {
    renderKeybindingsSection();
    const terminal = (await screen.findAllByTestId("keybinding-unbound"))
      .find((row) => row.getAttribute("data-command") === "terminal.split");
    expect(terminal?.textContent).toContain("one shell at a time");
  });
});
