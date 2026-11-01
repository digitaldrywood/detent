// @vitest-environment jsdom
import { InlineActionCard } from "../../src/app/components/InlineActionCard.tsx";
import { ClientContext } from "../../src/app/client.ts";
import type { ConversationClient } from "../../src/runtime/bootstrap.ts";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import React from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { Blob as NodeBlob } from "node:buffer";

import { DEFAULT_TURN_PREFERENCES } from "../../src/contracts/index.ts";
import { Composer } from "../../src/app/components/Composer.tsx";
import { composerCaret, composerText, focusComposer, typeInComposer, setComposerText } from "./composerInput.ts";

afterEach(cleanup);

function setup(overrides: Partial<React.ComponentProps<typeof Composer>> = {}) {
  const onSend = vi.fn();
  const onChange = vi.fn();
  const onStop = vi.fn();
  const onPreferencesChange = vi.fn();
  const component = (changes: Partial<React.ComponentProps<typeof Composer>> = {}) => (
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
      {...changes}
    />
  );
  const view = render(component());
  return {
    update: (changes: Partial<React.ComponentProps<typeof Composer>>) => view.rerender(component(changes)),
    unmount: view.unmount,
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

  it.each([
    { field: true, focusRequest: undefined, caret: null },
    { field: true, focusRequest: 1, caret: { offset: 5, collapsed: true } },
    { field: false, focusRequest: undefined, caret: { offset: 5, collapsed: true } },
  ])("preserves editing focus and navigation autofocus ($field, $focusRequest)", async ({ field, focusRequest, caret }) => {
    render(field ? <input aria-label="Search threads" /> : <div tabIndex={0} aria-label="Open a surface" />);
    const opener = screen.getByLabelText(field ? "Search threads" : "Open a surface", { exact: true });
    opener.focus();
    const { textarea } = setup({ autoFocus: true, focusRequest });
    await act(async () => {});
    if (caret === null) expect(document.activeElement).toBe(opener);
    expect(composerCaret(textarea)).toEqual(caret);
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

  // The reader keeps the transcript and loses the keystrokes.
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


describe("composer slash help and skills", () => {
  it.each(["pickers", "none"] as const)("help lists the resolved commands on a %s surface", async (footerControls) => {
    const { textarea, onSend } = setup({ value: "", footerControls, attachControl: "none", slashCommands: [{ name: "custom", description: "Surface command", run: vi.fn() }] });
    await setComposerText(textarea, "/");
    const menu = screen.getByRole("listbox", { name: "Slash commands" });
    const names = within(menu).getAllByRole("option").map((option) => within(option).getByText(/^\//).textContent);
    fireEvent.click(within(menu).getByRole("option", { name: /\/help/ }));
    const dialog = await screen.findByRole("dialog");
    for (const name of names) expect(within(dialog).getByText(name!)).toBeTruthy();
    expect(within(dialog).queryByText("/attach")).toBeNull();
    expect(within(dialog).queryByText("/stop")).toBeNull();
    if (footerControls === "none") expect(within(dialog).queryByText("/model")).toBeNull();
    expect(within(dialog).getByText("Shift+Enter")).toBeTruthy();
    expect(onSend).not.toHaveBeenCalled();
  });
  it("shows invocable project skills, inserts their native form, and reserves command names", async () => {
    const skill = { name: "review", description: "Review the work", path: "/repo/.agents/skills/review/SKILL.md", enabled: true, invocation: "$review" };
    const { textarea, onChange, onSend } = setup({ value: "", skills: [skill, { ...skill, name: "hidden", userInvocable: false }, { ...skill, name: "disabled", enabled: false }, { ...skill, name: "MODEL" }, { ...skill, name: "plan" }], slashCommands: [{ name: "plan", description: "Plan this work", run: vi.fn() }] });
    await setComposerText(textarea, "/");
    const menu = screen.getByRole("listbox", { name: "Slash commands" });
    expect(within(menu).queryByRole("option", { name: /\/hidden/ })).toBeNull();
    expect(within(menu).queryByRole("option", { name: /\/disabled/ })).toBeNull();
    expect(within(menu).getAllByRole("option", { name: /\/model/ })).toHaveLength(1);
    expect(within(menu).getAllByRole("option", { name: /\/plan/ })).toHaveLength(1);
    expect(screen.queryByText("Runner skills and prompts appear here once the hub serves them")).toBeNull();
    fireEvent.click(within(menu).getByRole("option", { name: /\/review/ }));
    await waitFor(() => expect(onChange).toHaveBeenLastCalledWith("$review "));
    expect(onSend).not.toHaveBeenCalled();
  });
});


it("requires a click to apply a slash proposal even when automatic chat confirmation is enabled", async () => {
  const preference = "detent:chat-confirmation:slash-test";
  localStorage.setItem(preference, "off");
  const fetch = vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response("{}"));
  try {
    const client = { bootstrap: { actor: { principal_id: "slash-test" }, api_base: "/api", csrf_token: "test" } } as unknown as ConversationClient;
    render(<ClientContext value={client}><InlineActionCard text="Move the item" proposal={{ conversation_id: "chat", action: { request_id: "slash", project_id: "project", kind: "move_item", requires_confirmation: true, arguments: { state: "Todo" } } }} /></ClientContext>);
    expect(fetch).not.toHaveBeenCalled();
    expect((screen.getByRole("checkbox", { name: "Ask me to confirm chat changes" }) as HTMLInputElement).disabled).toBe(true);
    fireEvent.click(screen.getByRole("button", { name: "Approve" }));
    await waitFor(() => expect(screen.getByRole("status").textContent).toBe("Change completed."));
    expect(fetch).toHaveBeenCalledOnce();
  } finally {
    localStorage.removeItem(preference);
    localStorage.removeItem(`${preference}:slash`);
    fetch.mockRestore();
  }
});

describe("Composer voice input", () => {
  const stopTrack = vi.fn();
  const track = { enabled: true, stop: stopTrack };
  const stream = { getTracks: () => [track], getAudioTracks: () => [track] };
  const getUserMedia = vi.fn();
  const recognitions: FakeRecognition[] = [];
  const workers: FakeWorker[] = [];
  let failModel = false;
  let silent = false;
  let delayModel = false;

  class FakeRecognition {
    onstart: (() => void) | null = null;
    onend: (() => void) | null = null;
    onerror: ((event: { error: string }) => void) | null = null;
    onresult: ((event: { results: { isFinal: boolean; 0: { transcript: string } }[] }) => void) | null = null;
    abort = vi.fn();
    stop = vi.fn();
    constructor() { recognitions.push(this); }
    start() { this.onstart?.(); }
    result(text: string, isFinal = false) { this.onresult?.({ results: [{ isFinal, 0: { transcript: text } }] }); }
  }

  class FakeWorker {
    onmessage: ((event: { data: { type: string; text?: string } }) => void) | null = null;
    onerror: (() => void) | null = null;
    terminate = vi.fn();
    postMessage = vi.fn((data: { audio?: Float32Array }) => {
      if (delayModel && !data.audio) return;
      queueMicrotask(() => this.onmessage?.({ data: failModel ? { type: "error" } :
        data.audio ? { type: "result", text: "Local transcript" } : { type: "ready" } }));
    });
    constructor() { workers.push(this); }
  }

  class FakeAudioContext {
    state = "running";
    resume = async () => {};
    close = vi.fn(async () => { this.state = "closed"; });
    createMediaStreamSource = () => ({ connect: vi.fn() });
    createAnalyser = () => ({ fftSize: 256, getFloatTimeDomainData: (data: Float32Array) => data.fill(0.1) });
    decodeAudioData = async () => ({ duration: 1 });
  }

  class FakeRecorder {
    state = "inactive";
    mimeType = "audio/webm";
    ondataavailable: ((event: { data: Blob }) => void) | null = null;
    onstop: (() => void) | null = null;
    onerror: (() => void) | null = null;
    start() { this.state = "recording"; }
    stop() {
      this.state = "inactive";
      this.ondataavailable?.({ data: new Blob(["audio"]) });
      this.onstop?.();
    }
  }

  beforeEach(() => {
    getUserMedia.mockReset().mockResolvedValue(stream);
    stopTrack.mockReset();
    recognitions.length = 0;
    workers.length = 0;
    failModel = false;
    silent = false;
    delayModel = false;
    localStorage.removeItem("detent.voice-model-downloaded");
    vi.stubGlobal("AudioContext", FakeAudioContext);
    vi.stubGlobal("SpeechRecognition", FakeRecognition);
    vi.stubGlobal("webkitSpeechRecognition", undefined);
    vi.stubGlobal("Worker", FakeWorker);
    vi.stubGlobal("MediaRecorder", FakeRecorder);
    vi.stubGlobal("Blob", NodeBlob);
    vi.stubGlobal("OfflineAudioContext", class {
      destination = {};
      createBufferSource = () => ({ buffer: null, connect: vi.fn(), start: vi.fn() });
      startRendering = async () => ({ getChannelData: () => new Float32Array(16000).fill(silent ? 0 : 0.1) });
    });
    Object.defineProperty(navigator, "mediaDevices", { configurable: true, value: { getUserMedia } });
  });

  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
    delete (navigator as unknown as { mediaDevices?: unknown }).mediaDevices;
    localStorage.removeItem("detent.voice-model-downloaded");
  });

  async function start() {
    fireEvent.click(screen.getByRole("button", { name: "Start voice input" }));
    await screen.findByRole("button", { name: "Stop recording" });
  }

  it.each(["standard", "WebKit on iOS"])("requests the microphone only on click, previews speech and preserves edits without sending with %s recognition", async (browser) => {
    if (browser !== "standard") {
      vi.stubGlobal("SpeechRecognition", undefined);
      vi.stubGlobal("webkitSpeechRecognition", FakeRecognition);
    }
    if (browser === "WebKit on iOS") vi.spyOn(navigator, "userAgent", "get").mockReturnValue("Mozilla/5.0 (iPhone) AppleWebKit Safari");
    const { onChange, onSend, textarea, update } = setup();
    expect(getUserMedia).not.toHaveBeenCalled();
    await start();
    expect(getUserMedia).toHaveBeenCalledOnce();
    expect(recognitions).toHaveLength(1);
    expect(workers).toHaveLength(0);
    expect(screen.getByText("Recording")).toBeTruthy();
    expect(screen.getByText("0:00")).toBeTruthy();
    expect(screen.getByRole<HTMLButtonElement>("button", { name: "Finish voice input before sending" }).disabled).toBe(true);
    act(() => recognitions[0]!.result("Review this"));
    expect(screen.getByText("Review this").className).toContain("text-muted-foreground");
    expect(onChange).not.toHaveBeenCalled();
    update({ value: "Edited draft" });
    fireEvent.keyDown(textarea, { key: "Enter" });
    expect(onSend).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("button", { name: "Stop recording" }));
    expect(stopTrack).toHaveBeenCalled();
    expect(recognitions[0]!.stop).toHaveBeenCalledOnce();
    act(() => {
      recognitions[0]!.result("Review this.", true);
      recognitions[0]!.onend?.();
    });
    expect(onChange).toHaveBeenLastCalledWith("Edited draft Review this.");
    expect(onSend).not.toHaveBeenCalled();
    fireEvent.keyDown(textarea, { key: "Enter" });
    expect(onSend).toHaveBeenCalledOnce();
  });

  it.each(["permission", "no speech"])("leaves typing and sending usable after %s", async (failure) => {
    if (failure === "permission") getUserMedia.mockRejectedValue(new DOMException("denied", "NotAllowedError"));
    const { onSend, onChange, textarea } = setup();
    fireEvent.click(screen.getByRole("button", { name: "Start voice input" }));
    if (failure === "no speech") {
      await screen.findByText("Recording");
      act(() => recognitions[0]!.onerror?.({ error: "no-speech" }));
    }
    expect((await screen.findByRole("alert")).textContent).toContain(failure === "permission" ? "Microphone access is blocked" : "No speech detected");
    await typeInComposer(textarea, " typed");
    expect(onChange).toHaveBeenCalled();
    fireEvent.keyDown(textarea, { key: "Enter" });
    expect(onSend).toHaveBeenCalledOnce();
  });

  it.each(["unsupported", "native failure"])("transcribes locally for %s and downloads only on first use", async (browser) => {
    if (browser === "unsupported") vi.stubGlobal("SpeechRecognition", undefined);
    const { onChange, onSend } = setup();
    fireEvent.click(screen.getByRole("button", { name: "Start voice input" }));
    if (browser === "native failure") {
      await screen.findByText("Recording");
      act(() => recognitions[0]!.onerror?.({ error: "network" }));
    }
    await screen.findByRole("button", { name: "Stop recording" });
    fireEvent.click(screen.getByRole("button", { name: "Stop recording" }));
    await waitFor(() => expect(onChange).toHaveBeenCalledWith("Hello Local transcript"));
    expect(onSend).not.toHaveBeenCalled();
    expect(workers[0]!.postMessage.mock.calls[1]![0].audio).toBeInstanceOf(Float32Array);
    expect(stopTrack).toHaveBeenCalled();
    expect(localStorage.getItem("detent.voice-model-downloaded")).toBe("true");
    await start();
    expect(workers).toHaveLength(1);
    expect(screen.queryByText(/Downloading the speech model/)).toBeNull();
  });

  it("reports a failed model download and permits a later retry", async () => {
    vi.stubGlobal("SpeechRecognition", undefined);
    failModel = true;
    const { textarea, onSend } = setup();
    fireEvent.click(screen.getByRole("button", { name: "Start voice input" }));
    expect((await screen.findByRole("alert")).textContent).toContain("speech model could not load");
    expect(stopTrack).toHaveBeenCalled();
    await screen.findByRole("button", { name: "Start voice input" });
    fireEvent.keyDown(textarea, { key: "Enter" });
    expect(onSend).toHaveBeenCalledOnce();
    failModel = false;
    await start();
    expect(workers).toHaveLength(2);
  });

  it("shows the first-use download notice and lets the user cancel preparation", async () => {
    vi.stubGlobal("SpeechRecognition", undefined);
    delayModel = true;
    setup();
    fireEvent.click(screen.getByRole("button", { name: "Start voice input" }));
    await screen.findByText(/Downloading the speech model/);
    expect(track.enabled).toBe(false);
    fireEvent.click(screen.getByRole("button", { name: "Cancel voice input" }));
    expect(stopTrack).toHaveBeenCalled();
    await act(async () => { workers[0]!.onmessage?.({ data: { type: "ready" } }); });
    expect(screen.queryByText("Recording")).toBeNull();
    expect(screen.getByRole("button", { name: "Start voice input" })).toBeTruthy();
  });

  it("rejects silent fallback audio instead of inserting a Whisper hallucination", async () => {
    vi.stubGlobal("SpeechRecognition", undefined);
    silent = true;
    const { onChange } = setup();
    await start();
    fireEvent.click(screen.getByRole("button", { name: "Stop recording" }));
    expect((await screen.findByRole("alert")).textContent).toContain("No speech detected");
    expect(onChange).not.toHaveBeenCalled();
    expect(workers[0]!.postMessage).toHaveBeenCalledOnce();
  });

  it.each(["unmount", "disabled", "hidden"])("releases capture and ignores late results on %s", async (reason) => {
    const { unmount, update, onChange } = setup();
    await start();
    const lateResult = recognitions[0]!.onresult;
    if (reason === "unmount") unmount();
    if (reason === "disabled") update({ disabled: true });
    if (reason === "hidden") {
      vi.spyOn(document, "hidden", "get").mockReturnValue(true);
      act(() => document.dispatchEvent(new Event("visibilitychange")));
    }
    expect(stopTrack).toHaveBeenCalled();
    expect(recognitions[0]!.abort).toHaveBeenCalled();
    act(() => lateResult?.({ results: [{ isFinal: true, 0: { transcript: "late words" } }] }));
    expect(onChange).not.toHaveBeenCalled();
  });

  it("stops a microphone granted after capture was cancelled", async () => {
    let grant!: (value: typeof stream) => void;
    getUserMedia.mockReturnValue(new Promise((resolve) => { grant = resolve; }));
    const { onChange } = setup();
    fireEvent.click(screen.getByRole("button", { name: "Start voice input" }));
    fireEvent.click(screen.getByRole("button", { name: "Cancel voice input" }));
    await act(async () => { grant(stream); });
    expect(stopTrack).toHaveBeenCalledOnce();
    expect(recognitions).toHaveLength(0);
    expect(onChange).not.toHaveBeenCalled();
  });
});
