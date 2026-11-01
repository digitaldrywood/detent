// Frame assembly for the event stream, including the chunk boundaries a real
// socket produces and the heartbeat frames that carry no id.
import { describe, expect, it } from "vitest";

import { SseParser } from "../src/runtime/rpc/sse.ts";

describe("SseParser", () => {
  it("assembles a frame delivered in one chunk", () => {
    const parser = new SseParser();
    expect(parser.push('id: 7\nevent: heartbeat\ndata: {"seq":7}\n\n')).toEqual([
      { id: "7", event: "heartbeat", data: '{"seq":7}' },
    ]);
  });

  it("assembles a frame split across chunk boundaries", () => {
    const parser = new SseParser();
    expect(parser.push("id: 8\nev")).toEqual([]);
    expect(parser.push("ent: message.delta\ndata: {")).toEqual([]);
    const frames = parser.push('"text":"hi"}\n\n');
    expect(frames).toEqual([{ id: "8", event: "message.delta", data: '{"text":"hi"}' }]);
  });

  it("keeps a null id for frames without one, such as heartbeats", () => {
    const parser = new SseParser();
    const [frame] = parser.push('event: heartbeat\ndata: {"seq":3}\n\n');
    expect(frame?.id).toBeNull();
  });

  it("joins multi-line data fields and ignores comments", () => {
    const parser = new SseParser();
    const frames = parser.push(": keep-alive\nevent: closed\ndata: {\ndata: }\n\n");
    expect(frames).toEqual([{ id: null, event: "closed", data: "{\n}" }]);
  });

  it("handles CRLF line endings", () => {
    const parser = new SseParser();
    expect(parser.push("id: 2\r\nevent: closed\r\ndata: {}\r\n\r\n")).toEqual([
      { id: "2", event: "closed", data: "{}" },
    ]);
  });

  it("emits several frames from one chunk in order", () => {
    const parser = new SseParser();
    const frames = parser.push("event: a\ndata: 1\n\nevent: b\ndata: 2\n\n");
    expect(frames.map((frame) => frame.event)).toEqual(["a", "b"]);
  });
});
