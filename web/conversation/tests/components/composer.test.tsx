// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import React from "react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { DEFAULT_TURN_PREFERENCES } from "../../src/contracts/index.ts";
import { Composer } from "../../src/app/components/Composer.tsx";
import { composerCaret, composerText, focusComposer, typeInComposer } from "./composerInput.ts";

afterEach(cleanup);

function setup(overrides: Partial<React.ComponentProps<typeof Composer>> = {}) {
  const onSend = vi.fn();
  const onChange = vi.fn();
  const onStop = vi.fn();
  const onPreferencesChange = vi.fn();
  render(
    <Composer
      value="Hello"
      onChange={onChange}
      onSend={onSend}
      onStop={onStop}
      sending={false}
      streaming={false}
      preferences={DEFAULT_TURN_PREFERENCES}
      onPreferencesChange={onPreferencesChange}
      {...overrides}
    />,
  );
  return {
    onSend,
    onChange,
    onStop,
    onPreferencesChange,
    textarea: screen.getByLabelText<HTMLElement>("Message"),
  };
}

describe("Composer", () => {

  it("offers model, effort and access pickers, all on Auto", () => {
    setup();
    for (const label of ["Model", "Reasoning effort", "Runtime access"]) {
      const trigger = screen.getByLabelText(label);
      expect(trigger.textContent).toContain("Auto");
      // A real select trigger, not a disabled chip wearing a chevron.
      expect(trigger.getAttribute("aria-disabled")).not.toBe("true");
      expect(trigger.getAttribute("aria-haspopup")).toBe("listbox");
    }
  });

  it("offers what the bootstrap published, and keeps Auto first", () => {
    setup({
      preferenceChoices: {
        models: [{ id: "gpt-6-astra", label: "Codex Astra", default: true }],
        efforts: [{ id: "low", label: "Low", default: true }],
        access: [{ id: "read_only", label: "Read only", default: true }],
      },
    });
    // Opening a Base UI select needs layout and animation jsdom does not have,
    // so the options themselves are asserted in the browser spec
    // (`tests/visual/conversation.spec.js`, "the composer's pickers"). What is
    // checkable here is that each trigger is a real combobox on "Auto".
    for (const label of ["Model", "Reasoning effort", "Runtime access"]) {
      expect(screen.getByLabelText(label).getAttribute("aria-haspopup")).toBe("listbox");
      expect(screen.getByLabelText(label).textContent).toContain("Auto");
    }
  });

  it("shows a value the hub no longer offers rather than silently changing it", () => {
    setup({
      preferences: { model: "retired-model", reasoning_effort: "auto", access: "auto" },
      preferenceChoices: {
        models: [{ id: "gpt-6-astra", label: "Codex Astra", default: true }],
        efforts: [],
        access: [],
      },
    });
    expect(screen.getByLabelText("Model").textContent).toContain("retired-model");
  });

  // The chip row used to take a tab stop of its own, which drew a focus ring
  // around the whole row; every chip in it is focusable now, so it must not.
  it("puts no focus ring around the chip row itself", () => {
    setup();
    const row = screen.getByTestId("composer-scope-chips");
    expect(row.getAttribute("tabindex")).toBeNull();
    expect(row.getAttribute("role")).toBeNull();
  });

  it("sends on Enter", () => {
    const { onSend, textarea } = setup();
    fireEvent.keyDown(textarea, { key: "Enter" });
    expect(onSend).toHaveBeenCalledTimes(1);
  });

  it("inserts a newline on Shift+Enter instead of sending", () => {
    const { onSend, textarea } = setup();
    fireEvent.keyDown(textarea, { key: "Enter", shiftKey: true });
    expect(onSend).not.toHaveBeenCalled();
  });

  // The issue page mounts two composers — the conversation panel's Message box
  // and the comment box under the issue — and the second attaches after the
  // first, once the issue body's data lands. A composer whose initial editor
  // state carried a selection took the caret at that moment: Lexical
  // reconciles the state's selection into the document when it attaches the
  // root element, and a DOM selection inside a `contenteditable` moves the
  // browser's focus into it, with no `focus()` call anywhere. Keystrokes
  // dispatched before the focus came back landed in the wrong box, which is
  // how a draft lost a run of characters out of its middle.
  it("claims no caret when it mounts, so a late composer cannot steal one", () => {
    const { textarea } = setup();
    expect(composerCaret(textarea)).toBeNull();
  });

  // The caret is asked for, not assumed: `autoFocus` is the ask, and it lands
  // where a click at the end of the prompt would.
  it("takes the caret when it is asked to, at the end of the prompt", async () => {
    const { textarea } = setup({ autoFocus: true, value: "Hello" });
    // `AutoFocusPlugin` focuses in a passive effect and Lexical commits the
    // selection on the next tick, so the caret is read after both have run.
    await act(async () => {});
    expect(composerCaret(textarea)).toEqual({ offset: 5, collapsed: true });
  });

  it("does not send while an IME composition is active", async () => {
    const { onSend, textarea } = setup();
    await focusComposer(textarea);
    fireEvent.compositionStart(textarea);
    fireEvent.keyDown(textarea, { key: "Enter" });
    expect(onSend).not.toHaveBeenCalled();
    fireEvent.compositionEnd(textarea);
    fireEvent.keyDown(textarea, { key: "Enter" });
    expect(onSend).toHaveBeenCalledTimes(1);
  });

  it("does not send when the key event reports isComposing", async () => {
    const { onSend, textarea } = setup();
    await focusComposer(textarea);
    fireEvent.keyDown(textarea, { key: "Enter", isComposing: true });
    expect(onSend).not.toHaveBeenCalled();
  });

  it("disables the send button while a send is in flight and keeps the draft", () => {
    setup({ sending: true });
    const send = screen.getByLabelText<HTMLButtonElement>("Sending");
    expect(send.disabled).toBe(true);
    expect(composerText(screen.getByLabelText<HTMLElement>("Message"))).toBe("Hello");
  });

  it("disables the send button on an empty draft", () => {
    setup({ value: "   " });
    expect(screen.getByLabelText<HTMLButtonElement>("Send message").disabled).toBe(true);
  });

  it("offers stop beside send while a turn is streaming", () => {
    const { onStop } = setup({ streaming: true });
    fireEvent.click(screen.getByLabelText("Stop generation"));
    expect(onStop).toHaveBeenCalledTimes(1);
    expect(screen.getByLabelText("Send message")).toBeTruthy();
  });

  it("states why sending is unavailable rather than hiding the control", () => {
    const { onSend, textarea } = setup({ blockedReason: "Read-only project" });
    expect(screen.getByText("Read-only project")).toBeTruthy();
    expect(screen.getByLabelText<HTMLButtonElement>("Read-only project").disabled).toBe(true);
    fireEvent.keyDown(textarea, { key: "Enter" });
    expect(onSend).not.toHaveBeenCalled();
  });

  // A paste and a keystroke both reach the editor as plain-text insertion, so
  // this is the path that has to keep the line breaks.
  it("keeps a multiline paste intact", async () => {
    const { onChange, textarea } = setup({ value: "" });
    await typeInComposer(textarea, "one\ntwo\nthree");
    expect(onChange).toHaveBeenLastCalledWith("one\ntwo\nthree");
  });

  // §10.11: the reader keeps the transcript and loses the keystrokes.
  it("disables the textarea and states why when the viewer cannot write", () => {
    const { onSend, textarea } = setup({
      disabled: true,
      blockedReason: "You can read this chat but not send messages",
    });
    expect(textarea.getAttribute("contenteditable")).toBe("false");
    fireEvent.keyDown(textarea, { key: "Enter" });
    expect(onSend).not.toHaveBeenCalled();
    expect(screen.getByText("You can read this chat but not send messages")).toBeTruthy();

    expect(
      screen.getByLabelText<HTMLButtonElement>("Environment disconnected").disabled,
    ).toBe(true);
  });

  it("offers no stop button while it is disabled", () => {
    setup({ disabled: true, streaming: true, blockedReason: "This chat is settled" });
    expect(screen.queryByLabelText("Stop generation")).toBeNull();
  });
});
