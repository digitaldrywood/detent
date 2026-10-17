// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { Composer } from "../../src/app/components/Composer.tsx";
import { Timeline } from "../../src/app/components/Timeline.tsx";
import { useConversationSpeech, type ConversationSpeech } from "../../src/app/hooks/useConversationSpeech.ts";
import { DEFAULT_TURN_PREFERENCES } from "../../src/contracts/index.ts";
import type { ConversationDetail } from "../../src/runtime/state/conversationState.ts";
import { assistantMessage, conversation, detail, execution, userMessage } from "./builders.ts";

const speak = vi.fn();
const cancel = vi.fn();
let voice: ConversationSpeech;

function Harness({ data, id = "chat" }: { data: ConversationDetail; id?: string }) {
  const speech = useConversationSpeech(id, data);
  voice = speech;
  return <>
    <Timeline detail={data} speech={speech} onLoadOlder={() => {}} onRetry={() => {}} onDismiss={() => {}} />
    <Composer value="Next message" onChange={() => {}} onSend={speech.prepareToSend}
      sending={false} streaming={false} preferences={DEFAULT_TURN_PREFERENCES}
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
    act(() => voice.followCreatedConversation("created"));
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
