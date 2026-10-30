// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { Composer } from "../../src/app/components/Composer.tsx";
import { Timeline } from "../../src/app/components/Timeline.tsx";
import { followCreatedConversation, useConversationSpeech, type ConversationSpeech } from "../../src/app/hooks/useConversationSpeech.ts";
import { createVoiceActivity } from "../../src/app/lib/voiceActivity.ts";
import type { RealTimeVADOptions } from "@ricky0123/vad-web";

vi.mock("../../src/app/lib/voiceActivity.ts", () => ({ createVoiceActivity: vi.fn() }));

import { DEFAULT_TURN_PREFERENCES } from "../../src/contracts/index.ts";
import type { ConversationDetail } from "../../src/runtime/state/conversationState.ts";
import { assistantMessage, conversation, detail, execution, userMessage } from "./builders.ts";

const speak = vi.fn();
const cancel = vi.fn();
let voice: ConversationSpeech;

function Harness({ data, id = "chat", value = "Next message", onVoiceSend, sending = false, streaming = false, disabled = false, blockedReason = null }: {
  data: ConversationDetail; id?: string; value?: string; onVoiceSend?: (text: string) => void | Promise<void>;
  sending?: boolean; streaming?: boolean; disabled?: boolean; blockedReason?: string | null;
}) {
  const speech = useConversationSpeech(id, data);
  voice = speech;
  return <>
    <Timeline detail={data} speech={speech} onLoadOlder={() => {}} onRetry={() => {}} onDismiss={() => {}} />
    <Composer value={value} onChange={() => {}} onSend={speech.prepareToSend}
      onVoiceSend={onVoiceSend === undefined ? undefined : (text) => { speech.prepareToSend(); return onVoiceSend(text); }}
      sending={sending} streaming={streaming} disabled={disabled} blockedReason={blockedReason} preferences={DEFAULT_TURN_PREFERENCES}
      onPreferencesChange={() => {}} speech={speech} />
  </>;
}

function reply(text: string, streaming = true, overrides = {}) {
  return detail({
    conversation: conversation({ execution: execution({ status: streaming ? "running" : "idle", turn_id: "turn_voice" }) }),
    messages: [userMessage(), assistantMessage({ id: "reply", seq: 20, text, turn_id: "turn_voice", delivery: streaming ? "responding" : "completed", ...overrides })],
  });
}

function said(): string[] {
  return speak.mock.calls.map(([utterance]) => utterance.text).filter((text) => text.trim());
}

beforeEach(() => {
  localStorage.clear();
  speak.mockReset();
  cancel.mockReset();
  vi.stubGlobal("speechSynthesis", { speak, cancel });
  vi.stubGlobal("SpeechSynthesisUtterance", class {
    constructor(public text: string) {}
    volume = 1;
    onend: (() => void) | null = null;
    onerror: (() => void) | null = null;
  });
});
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

function start() {
  const view = render(<Harness data={detail()} />);
  expect(screen.getByRole("button", { name: "Voice" }).getAttribute("aria-pressed")).toBe("false");
  fireEvent.click(screen.getByRole("button", { name: "Voice" }));
  expect(speak.mock.calls[0]?.[0].volume).toBe(0);
  return view;
}

describe("conversation speech", () => {
  it("speaks new sentence deltas once and flushes a final fragment without reading history or tools", () => {
    const view = start();
    view.rerender(<Harness data={reply("First sentence. Next")} />);
    expect(said()).toEqual(["First sentence."]);
    expect(screen.getByText("Reading aloud")).toBeTruthy();
    view.rerender(<Harness data={detail({
      messages: [userMessage(), assistantMessage({ id: "reply", seq: 20, text: "First sentence. ", delivery: "responding" })],
      deltas: { reply: { parts: { 1: "Next sentence! ", 2: "Last" } } },
    })} />);
    expect(said()).toEqual(["First sentence.", "Next sentence!"]);
    view.rerender(<Harness data={reply("First sentence. Next sentence! Last", false)} />);
    expect(said()).toEqual(["First sentence.", "Next sentence!", "Last"]);
    act(() => { for (const [utterance] of speak.mock.calls) utterance.onend?.(); });
    expect(screen.queryByText("Reading aloud")).toBeNull();
    view.rerender(<Harness data={reply("Tool output.", false, { seq: 21, id: "tool", kind: "tool" })} />);
    expect(said()).toEqual(["First sentence.", "Next sentence!", "Last"]);
    view.rerender(<Harness data={detail({ messages: [assistantMessage({ seq: 1, text: "Older loaded history." })] })} />);
    expect(said()).toHaveLength(3);
  });

  it("holds incomplete code and links, summarizes code blocks, and skips URL destinations", async () => {
    const view = start();
    await act(async () => { view.rerender(<Harness data={reply("Read this.\n```js\nsecret.call();")} />); });
    expect(said()).toEqual(["Read this."]);
    await act(async () => { view.rerender(<Harness data={reply("Read this.\n```js\nsecret.call();\n```\nVisit [the guide](https://example.test/path?q=private). See https://example.test/a.b?q=private now. `secret()` Done.", false)} />); });
    expect(said()).toEqual(["Read this.", "code block", "Visit the guide.", "See now.", "code Done."]);
    await act(async () => { view.rerender(<Harness data={reply("An [unfinished", true, { seq: 21, id: "next" })} />); });
    expect(said()).toHaveLength(5);
    await act(async () => { view.rerender(<Harness data={reply("An [unfinished link](https://example.test). Ready. ", true, { seq: 21, id: "next" })} />); });
    expect(said().slice(-2)).toEqual(["An unfinished link.", "Ready."]);
    const destinations = [
      "https://example.test/foo(bar)/private",
      "https://example.test/foo(bar(baz))/private",
      "https://example.test/foo\\(bar/private",
      "https://example.test/foo\\)bar/private",
    ];
    for (const [index, destination] of destinations.entries()) {
      const before = said();
      const overrides = { seq: 22 + index, id: `link-${index}` };
      const opening = `Visit [guide](${destination}`;
      await act(async () => { view.rerender(<Harness data={reply(opening, true, overrides)} />); });
      expect(said()).toEqual(before);
      await act(async () => { view.rerender(<Harness data={reply(`${opening}). Ready. `, true, overrides)} />); });
      expect(said()).toEqual([...before, "Visit guide.", "Ready."]);
      await act(async () => { view.rerender(<Harness data={reply(`${opening}). Ready.`, false, overrides)} />); });
      expect(said()).toEqual([...before, "Visit guide.", "Ready."]);
      await act(async () => { view.rerender(<Harness data={reply(`${opening}).`, false, { ...overrides, id: `complete-link-${index}` })} />); });
      expect(said()).toEqual([...before, "Visit guide.", "Ready.", "Visit guide."]);
    }
    const containers = [
      ["> ~~~js", "> console.log(123);", "> ~~~"],
      ["> ```js", "> console.log(123);", "> ```"],
      ["> > ~~~js", "> > console.log(123);", "> > ~~~"],
      ["- ~~~js", "  console.log(123);", "  ~~~"],
      ["> 10. ~~~js", ">     console.log(123);", ">     ~~~"],
      ["~~~js", "> ~~~\nconsole.log(123);", "~~~"],
      ["> ~~~js", "> > ~~~\n> console.log(123);", "> ~~~"],
    ];
    for (const [index, lines] of containers.entries()) {
      const before = said();
      const overrides = { seq: 30 + index, id: `block-${index}` };
      const opening = `Read this.\n${lines[0]}\n${lines[1]}`;
      await act(async () => { view.rerender(<Harness data={reply(opening, true, overrides)} />); });
      expect(said()).toEqual([...before, "Read this."]);
      await act(async () => { view.rerender(<Harness data={reply(`${opening}\n${lines[2]}\nSafe sentence.`, false, overrides)} />); });
      expect(said()).toEqual([...before, "Read this.", "code block", "Safe sentence."]);
    }
  });

  it("skips indented code and resumes new replies after Stop without disabling voice", async () => {
    const view = start();
    await act(async () => { view.rerender(<Harness data={reply("    private.run();\n    more.code();\nSafe sentence. ")} />); });
    expect(said()).toEqual(["code block", "Safe sentence."]);
    fireEvent.click(screen.getByRole("button", { name: "Stop reading aloud" }));
    expect(screen.getByRole("button", { name: "Voice" }).getAttribute("aria-pressed")).toBe("true");
    view.rerender(<Harness data={reply("Another reply.", false, { seq: 21, id: "another" })} />);
    expect(said().at(-1)).toBe("Another reply.");
  });

  it.each(["stop", "toggle", "send", "leave", "pagehide"])("cancels queued speech on %s and ignores late callbacks", (action) => {
    const view = start();
    view.rerender(<Harness data={reply("First. Second. ")} />);
    const lateEnd = speak.mock.calls[1]![0].onend;
    const before = cancel.mock.calls.length;
    if (action === "stop") fireEvent.click(screen.getByRole("button", { name: "Stop reading aloud" }));
    if (action === "toggle") fireEvent.click(screen.getByRole("button", { name: "Voice" }));
    if (action === "send") fireEvent.click(screen.getByRole("button", { name: "Send message" }));
    if (action === "leave") view.rerender(<Harness data={detail()} id="other" />);
    if (action === "pagehide") act(() => { window.dispatchEvent(new Event("pagehide")); });
    expect(cancel.mock.calls.length).toBeGreaterThan(before);
    expect(screen.queryByText("Reading aloud")).toBeNull();
    act(() => lateEnd?.());
    if (action !== "leave") view.rerender(<Harness data={reply("First. Second. Third.", false)} />);
    expect(said()).toEqual(["First.", "Second."]);
  });

  it("reads a fast first reply after creating a chat, but never replays it on a later visit", () => {
    const view = start();
    act(() => followCreatedConversation("created"));
    view.rerender(<Harness data={reply("Already finished.", false)} id="created" />);
    expect(said()).toEqual(["Already finished."]);
    view.unmount();
    render(<Harness data={reply("Already finished.", false)} id="created" />);
    expect(said()).toEqual(["Already finished."]);
  });

  it("remembers the browser preference across mounts and tolerates unavailable storage", () => {
    const view = start();
    expect(localStorage.getItem("detent.chat.voice")).toBe("on");
    view.unmount();
    render(<Harness data={detail()} />);
    expect(screen.getByRole("button", { name: "Voice" }).getAttribute("aria-pressed")).toBe("true");
    expect(said()).toEqual([]);
    fireEvent.click(screen.getByRole("button", { name: "Voice" }));
    expect(localStorage.getItem("detent.chat.voice")).toBe("off");
    vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => { throw new Error("Unavailable"); });
    fireEvent.click(screen.getByRole("button", { name: "Voice" }));
    expect(voice.enabled).toBe(true);
    vi.restoreAllMocks();
  });

  it("hides Voice when browser speech is unavailable", () => {
    vi.stubGlobal("speechSynthesis", undefined);
    render(<Harness data={detail()} />);
    expect(screen.queryByRole("button", { name: "Voice" })).toBeNull();
  });
});


describe("hands-free conversation", () => {
  const stopTrack = vi.fn();
  const track = { enabled: true, stop: stopTrack };
  const stream = { getTracks: () => [track], getAudioTracks: () => [track] };
  const getUserMedia = vi.fn();
  const closeContext = vi.fn();
  const destroyVad = vi.fn();
  let callbacks: Pick<RealTimeVADOptions, "onSpeechRealStart" | "onSpeechEnd">;
  const workers: FakeWorker[] = [];

  class FakeWorker {
    onmessage: ((event: { data: { type: string; text?: string } }) => void) | null = null;
    onerror: (() => void) | null = null;
    terminate = vi.fn();
    postMessage = vi.fn((data: { audio?: Float32Array }) => {
      if (!data.audio) queueMicrotask(() => this.onmessage?.({ data: { type: "ready" } }));
    });
    constructor() { workers.push(this); }
  }

  beforeEach(() => {
    workers.length = 0;
    stopTrack.mockReset();
    closeContext.mockReset();
    destroyVad.mockReset().mockResolvedValue(undefined);
    getUserMedia.mockReset().mockResolvedValue(stream);
    vi.stubGlobal("isSecureContext", true);
    vi.stubGlobal("AudioWorkletNode", class {});
    vi.stubGlobal("AudioContext", class {
      state = "running";
      resume = async () => {};
      close = closeContext;
    });
    vi.stubGlobal("Worker", FakeWorker);
    Object.defineProperty(navigator, "mediaDevices", { configurable: true, value: { getUserMedia } });
    vi.mocked(createVoiceActivity).mockReset().mockImplementation(async (_context, _stream, handlers) => {
      callbacks = handlers;
      return { destroy: destroyVad } as unknown as Awaited<ReturnType<typeof createVoiceActivity>>;
    });
  });

  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
    delete (navigator as unknown as { mediaDevices?: unknown }).mediaDevices;
  });

  async function startHandsFree() {
    fireEvent.click(screen.getByRole("button", { name: "Start hands-free voice" }));
    await screen.findByText("Listening");
  }

  async function finishPhrase(text: string) {
    act(() => { callbacks.onSpeechRealStart(); callbacks.onSpeechEnd(new Float32Array(16000).fill(0.1)); });
    await waitFor(() => expect(workers[0]!.postMessage).toHaveBeenCalledWith(expect.objectContaining({ audio: expect.any(Float32Array) }), expect.any(Array)));
    expect(screen.getByText("Transcribing…")).toBeTruthy();
    await act(async () => { workers[0]!.onmessage?.({ data: { type: "result", text } }); });
  }

  it("listens, sends one local transcript, speaks the reply and listens for the next message", async () => {
    let finishSend!: () => void;
    const onVoiceSend = vi.fn().mockReturnValueOnce(new Promise<void>((resolve) => { finishSend = resolve; }));
    const view = render(<Harness data={detail()} value="" onVoiceSend={onVoiceSend} />);
    expect(getUserMedia).not.toHaveBeenCalled();
    await startHandsFree();
    expect(getUserMedia).toHaveBeenCalledWith({ audio: expect.objectContaining({ echoCancellation: true }) });
    expect(voice.enabled).toBe(true);
    expect(localStorage.getItem("detent.chat.voice")).toBeNull();
    expect(screen.getByRole<HTMLButtonElement>("button", { name: "Start voice input" }).disabled).toBe(true);
    await finishPhrase("Please review this");
    expect(onVoiceSend).toHaveBeenCalledExactlyOnceWith("Please review this");
    expect(screen.getByText("Waiting for reply…")).toBeTruthy();
    const transcriptions = workers[0]!.postMessage.mock.calls.length;
    await act(async () => {
      callbacks.onSpeechRealStart();
      callbacks.onSpeechEnd(new Float32Array(16000).fill(0.1));
    });
    expect(workers[0]!.postMessage).toHaveBeenCalledTimes(transcriptions);
    expect(screen.getByText("Waiting for reply…")).toBeTruthy();
    await act(async () => { finishSend(); });
    view.rerender(<Harness data={reply("Here is the answer.", false)} value="" onVoiceSend={onVoiceSend} />);
    expect(said()).toEqual(["Here is the answer."]);
    expect(screen.getByText("Speaking")).toBeTruthy();
    act(() => { speak.mock.calls.at(-1)![0].onend(); });
    expect(screen.getByText("Listening")).toBeTruthy();
    await finishPhrase("One more question");
    expect(onVoiceSend).toHaveBeenLastCalledWith("One more question");
    expect(onVoiceSend).toHaveBeenCalledTimes(2);
  });

  it("interrupts playback on speech and suppresses remaining streamed sentences before sending the new message", async () => {
    const onVoiceSend = vi.fn();
    const view = render(<Harness data={detail()} value="" onVoiceSend={onVoiceSend} />);
    await startHandsFree();
    view.rerender(<Harness data={reply("First. Second. ")} value="" streaming onVoiceSend={onVoiceSend} />);
    const lateEnd = speak.mock.calls.at(-1)![0].onend;
    const before = cancel.mock.calls.length;
    act(() => { callbacks.onSpeechRealStart(); });
    expect(cancel.mock.calls.length).toBeGreaterThan(before);
    expect(screen.queryByText("Speaking")).toBeNull();
    view.rerender(<Harness data={reply("First. Second. Third.", false)} value="" onVoiceSend={onVoiceSend} />);
    expect(said()).toEqual(["First.", "Second."]);
    act(() => lateEnd?.());
    await finishPhrase("Let me clarify");
    expect(onVoiceSend).toHaveBeenCalledExactlyOnceWith("Let me clarify");
    view.rerender(<Harness data={reply("Understood.", false, { id: "next", seq: 21 })} value="" onVoiceSend={onVoiceSend} />);
    expect(said().at(-1)).toBe("Understood.");
  });

  it("ends hands-free and releases capture when sending the transcript fails", async () => {
    const onVoiceSend = vi.fn().mockRejectedValue(new Error("Send failed"));
    render(<Harness data={detail()} value="" onVoiceSend={onVoiceSend} />);
    await startHandsFree();
    await finishPhrase("Please review this");
    expect(screen.getByRole("alert").textContent).toContain("The voice message could not send");
    expect(screen.queryByRole("button", { name: "End hands-free voice" })).toBeNull();
    expect(stopTrack).toHaveBeenCalledOnce();
    expect(destroyVad).toHaveBeenCalledOnce();
    expect(workers[0]!.terminate).toHaveBeenCalledOnce();
    expect(voice.enabled).toBe(false);
  });

  it.each(["end", "hidden", "pagehide", "leave", "disabled", "blocked", "draft", "unmount"])("releases microphone, model and playback and ignores late transcription on %s", async (reason) => {
    const onVoiceSend = vi.fn();
    const view = render(<Harness data={detail()} value="" onVoiceSend={onVoiceSend} />);
    await startHandsFree();
    act(() => { callbacks.onSpeechEnd(new Float32Array(16000).fill(0.1)); });
    await waitFor(() => expect(workers[0]!.postMessage.mock.calls.length).toBe(2));
    if (reason === "end") fireEvent.click(screen.getByRole("button", { name: "End hands-free voice" }));
    if (reason === "hidden") {
      vi.spyOn(document, "hidden", "get").mockReturnValue(true);
      act(() => document.dispatchEvent(new Event("visibilitychange")));
    }
    if (reason === "pagehide") act(() => window.dispatchEvent(new Event("pagehide")));
    if (reason === "unmount") view.unmount();
    if (["leave", "disabled", "blocked", "draft"].includes(reason)) view.rerender(<Harness data={detail()}
      id={reason === "leave" ? "other" : "chat"} disabled={reason === "disabled"}
      blockedReason={reason === "blocked" ? "Connection lost" : null}
      value={reason === "draft" ? "Typed draft" : ""} onVoiceSend={onVoiceSend} />);
    expect(stopTrack).toHaveBeenCalled();
    expect(closeContext).toHaveBeenCalled();
    expect(destroyVad).toHaveBeenCalled();
    expect(workers[0]!.terminate).toHaveBeenCalled();
    await act(async () => {
      workers[0]!.onmessage?.({ data: { type: "result", text: "Late words" } });
      callbacks.onSpeechEnd(new Float32Array(16000));
    });
    expect(onVoiceSend).not.toHaveBeenCalled();
    expect(screen.queryByRole("button", { name: "End hands-free voice" })).toBeNull();
    if (reason !== "unmount") expect(voice.enabled).toBe(false);
  });

  it.each(["microphone", "model"])("ends mode after a %s failure and leaves ordinary input usable", async (failure) => {
    if (failure === "microphone") getUserMedia.mockRejectedValue(new DOMException("Denied", "NotAllowedError"));
    else vi.mocked(createVoiceActivity).mockRejectedValue(new Error("Unsupported model"));
    render(<Harness data={detail()} value="" onVoiceSend={vi.fn()} />);
    fireEvent.click(screen.getByRole("button", { name: "Start hands-free voice" }));
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain(failure === "microphone" ? "Microphone access is blocked" : "Hands-free voice could not start");
    expect(screen.getByRole<HTMLButtonElement>("button", { name: "Start voice input" }).disabled).toBe(false);
    expect(voice.enabled).toBe(false);
    expect(closeContext).toHaveBeenCalled();
    if (failure === "model") expect(stopTrack).toHaveBeenCalled();
  });

  it("stops capture granted after mode ended without starting the models", async () => {
    let grant!: (stream: MediaStream) => void;
    getUserMedia.mockReturnValue(new Promise((resolve) => { grant = resolve; }));
    render(<Harness data={detail()} value="" onVoiceSend={vi.fn()} />);
    fireEvent.click(screen.getByRole("button", { name: "Start hands-free voice" }));
    fireEvent.click(screen.getByRole("button", { name: "End hands-free voice" }));
    await act(async () => { grant(stream as unknown as MediaStream); });
    expect(stopTrack).toHaveBeenCalledOnce();
    expect(createVoiceActivity).not.toHaveBeenCalled();
    expect(workers).toHaveLength(0);
  });

  it("ends during model preparation and destroys a VAD that finishes loading later", async () => {
    let finishLoad!: (vad: Awaited<ReturnType<typeof createVoiceActivity>>) => void;
    vi.mocked(createVoiceActivity).mockReturnValue(new Promise((resolve) => { finishLoad = resolve; }));
    render(<Harness data={detail()} value="" onVoiceSend={vi.fn()} />);
    fireEvent.click(screen.getByRole("button", { name: "Start hands-free voice" }));
    await waitFor(() => expect(createVoiceActivity).toHaveBeenCalledOnce());
    expect(track.enabled).toBe(false);
    fireEvent.click(screen.getByRole("button", { name: "End hands-free voice" }));
    expect(stopTrack).toHaveBeenCalledOnce();
    expect(workers[0]!.terminate).toHaveBeenCalledOnce();
    await act(async () => { finishLoad({ destroy: destroyVad } as unknown as Awaited<ReturnType<typeof createVoiceActivity>>); });
    expect(destroyVad).toHaveBeenCalledOnce();
    expect(screen.queryByText("Listening")).toBeNull();
  });

  it("restores the saved spoken-reply preference when hands-free ends", async () => {
    localStorage.setItem("detent.chat.voice", "on");
    render(<Harness data={detail()} value="" onVoiceSend={vi.fn()} />);
    await startHandsFree();
    fireEvent.click(screen.getByRole("button", { name: "End hands-free voice" }));
    expect(voice.enabled).toBe(true);
    expect(localStorage.getItem("detent.chat.voice")).toBe("on");
  });

  it.each(["speech", "worklet", "wasm", "simd", "worker", "microphone", "insecure"])("hides hands-free without required %s support", (missing) => {
    if (missing === "speech") vi.stubGlobal("speechSynthesis", undefined);
    if (missing === "worklet") vi.stubGlobal("AudioWorkletNode", undefined);
    if (missing === "wasm") vi.stubGlobal("WebAssembly", undefined);
    if (missing === "simd") vi.stubGlobal("WebAssembly", { validate: () => false });
    if (missing === "worker") vi.stubGlobal("Worker", undefined);
    if (missing === "microphone") delete (navigator as unknown as { mediaDevices?: unknown }).mediaDevices;
    if (missing === "insecure") vi.stubGlobal("isSecureContext", false);
    render(<Harness data={detail()} value="" onVoiceSend={vi.fn()} />);
    expect(screen.queryByRole("button", { name: "Start hands-free voice" })).toBeNull();
    expect(getUserMedia).not.toHaveBeenCalled();
  });
});
