// @vitest-environment jsdom
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { AsyncResult } from "effect/unstable/reactivity";

import {
  EMPTY_PROJECT_SCRIPT_INPUT,
  PREVIEW_UNAVAILABLE_REASON,
  ProjectScriptEditorDialog,
  editorRequestForScript,
  type NewProjectScriptInput,
} from "../../src/components/projectScriptEditor.tsx";
import { detentKeybindings } from "../../src/app/adapters/keybindings.ts";
import type { ProjectScript } from "../../src/contracts/ui.ts";

afterEach(cleanup);

const SCRIPT: ProjectScript = {
  id: "run-tests",
  name: "Run tests",
  command: "bun test",
  icon: "test",
  runOnWorktreeCreate: false,
};

function mount(
  overrides: {
    readonly editing?: boolean;
    readonly onSubmit?: (
      scriptId: string | null,
      input: NewProjectScriptInput,
    ) => Promise<ReturnType<typeof AsyncResult.success>>;
    readonly onDelete?: (scriptId: string) => void;
    readonly onClose?: () => void;
  } = {},
) {
  const onSubmit =
    overrides.onSubmit ?? vi.fn(async () => AsyncResult.success(undefined as void));
  const onDelete = overrides.onDelete ?? vi.fn();
  const onClose = overrides.onClose ?? vi.fn();
  const request =
    overrides.editing === true
      ? editorRequestForScript(SCRIPT, detentKeybindings)
      : { scriptId: null, initial: EMPTY_PROJECT_SCRIPT_INPUT };
  render(
    <ProjectScriptEditorDialog
      request={request}
      scripts={[SCRIPT]}
      onSubmit={onSubmit as never}
      onDelete={onDelete}
      onClose={onClose}
    />,
  );
  return { onSubmit, onDelete, onClose };
}

function nameField(): HTMLInputElement {
  return screen.getByLabelText("Name") as HTMLInputElement;
}

function keybindingField(): HTMLInputElement {
  return screen.getByLabelText("Keybinding") as HTMLInputElement;
}

describe("the Add Action dialog", () => {
  it("is the dialog, with their title, description and placeholders", () => {
    mount();
    expect(screen.getByText("Add Action")).toBeTruthy();
    expect(
      screen.getByText(
        "Actions are project-scoped commands you can run from the top bar or keybindings.",
      ),
    ).toBeTruthy();
    expect(keybindingField().placeholder).toBe("Press shortcut");
    expect(keybindingField().readOnly).toBe(true);
    expect((screen.getByLabelText("Command") as HTMLTextAreaElement).placeholder).toBe("bun test");
    expect(screen.getByText("Preview URL (optional)")).toBeTruthy();
    expect(screen.getByText("Run automatically on worktree creation")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Save action" })).toBeTruthy();
  });

  it("refuses an empty name", async () => {
    const user = userEvent.setup();
    const { onSubmit } = mount();
    await user.click(screen.getByRole("button", { name: "Save action" }));
    await waitFor(() => expect(screen.getByText("Name is required.")).toBeTruthy());
    expect(onSubmit).not.toHaveBeenCalled();
  });

  it("refuses an empty command", async () => {
    const user = userEvent.setup();
    const { onSubmit } = mount();
    await user.type(nameField(), "Run tests");
    await user.click(screen.getByRole("button", { name: "Save action" }));
    await waitFor(() => expect(screen.getByText("Command is required.")).toBeTruthy());
    expect(onSubmit).not.toHaveBeenCalled();
  });

  it("captures a chord, and Backspace clears it", async () => {
    const user = userEvent.setup();
    mount();
    keybindingField().focus();
    await user.keyboard("{Control>}{Shift>}t{/Shift}{/Control}");
    await waitFor(() => expect(keybindingField().value).toBe("mod+shift+t"));
    await user.keyboard("{Backspace}");
    await waitFor(() => expect(keybindingField().value).toBe(""));
  });

  // §18.7: the Browser surface is snapshot-first and the live preview is
  // deferred, so the switch stays present and disabled with the reason —
  // §16's rule — rather than being cut.
  it("disables the Open-preview switch and carries the reason", () => {
    mount();
    // The switch is disabled whatever the URL field says, which is where
    // Detent differs from upstream: theirs is disabled only while it is empty.
    const toggle = screen.getByRole("switch", {
      name: /Open preview automatically when this action runs/,
    });
    expect(toggle.getAttribute("data-disabled")).not.toBeNull();

    const reason = screen.getByText(PREVIEW_UNAVAILABLE_REASON);
    expect(toggle.getAttribute("aria-describedby")).toBe(reason.id);
    // The URL field itself stays live: the hub stores and echoes it (§18.12).
    expect((screen.getByLabelText("Preview URL (optional)") as HTMLInputElement).disabled).toBe(
      false,
    );
  });

  it("submits the normalized payload", async () => {
    const user = userEvent.setup();
    const { onSubmit, onClose } = mount();
    await user.type(nameField(), "  Run tests  ");
    await user.type(screen.getByLabelText("Command"), "  bun test  ");
    await user.type(screen.getByLabelText("Preview URL (optional)"), " http://localhost:5173 ");
    keybindingField().focus();
    await user.keyboard("{Control>}{Shift>}t{/Shift}{/Control}");
    await user.click(screen.getByRole("button", { name: "Save action" }));
    await waitFor(() => expect(onSubmit).toHaveBeenCalledTimes(1));
    expect(onSubmit).toHaveBeenCalledWith(null, {
      name: "Run tests",
      command: "bun test",
      icon: "play",
      runOnWorktreeCreate: false,
      keybinding: "mod+shift+t",
      previewUrl: "http://localhost:5173",
      // Still false: §18.7's switch is disabled, so nothing can turn it on.
      autoOpenPreview: false,
    });
    expect(onClose).toHaveBeenCalled();
  });

  it("reports a failed write through the validation line", async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn(async () =>
      AsyncResult.failure(
        // The shape `app/adapters/projectActions.tsx`'s `settlePromise`
        // produces for a refused mutation.
        (await import("effect/Cause")).die(new Error("A project may hold at most 50 actions")),
      ),
    );
    mount({ onSubmit: onSubmit as never });
    await user.type(nameField(), "Run tests");
    await user.type(screen.getByLabelText("Command"), "bun test");
    await user.click(screen.getByRole("button", { name: "Save action" }));
    await waitFor(() =>
      expect(screen.getByText("A project may hold at most 50 actions")).toBeTruthy(),
    );
  });
});

describe("the Edit Action dialog", () => {
  it("says Save changes, offers Delete, and prefills the action", () => {
    mount({ editing: true });
    expect(screen.getByText("Edit Action")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Save changes" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Delete" })).toBeTruthy();
    expect(nameField().value).toBe("Run tests");
    expect((screen.getByLabelText("Command") as HTMLTextAreaElement).value).toBe("bun test");
  });

  it("confirms a delete in two steps", async () => {
    const user = userEvent.setup();
    const { onDelete } = mount({ editing: true });
    await user.click(screen.getByRole("button", { name: "Delete" }));
    await waitFor(() => expect(screen.getByText('Delete action "Run tests"?')).toBeTruthy());
    expect(onDelete).not.toHaveBeenCalled();
    await user.click(screen.getByRole("button", { name: "Delete action" }));
    await waitFor(() => expect(onDelete).toHaveBeenCalledWith("run-tests"));
  });
});
