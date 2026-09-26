// The terminal stream (decisions.md §18.3): one `open` with no stream, the
// output spans it answers with, the input and resize a person sends back, the
// `exit` that ends it — and §18.2's resume, which is the one thing this client
// does that neither of the other two may.
//
// Driven through a substituted transport, the way `tests/actionRuns.test.ts`
// and `tests/workspaceRelay.test.ts` drive theirs, so nothing here needs a
// socket.
import { describe, expect, it } from "vitest";

import {
  createTerminalStream,
  decodeTerminalOutput,
  type TerminalSnapshot,
  type TerminalStream,
} from "../src/app/adapters/terminalStream.ts";
import {
  inferChunkedType,
  RelayFrameAssembler,
  type RelayHandlers,
  type RelayTransport,
} from "../src/app/adapters/workspaceRelay.ts";
import {
  terminalBlockedReason,
  TERMINAL_POLICY_SENTENCES,
  type TerminalPolicy,
} from "../src/app/adapters/terminalPolicy.ts";

const flush = () => new Promise((resolve) => setTimeout(resolve, 0));

/** Encodes a span the way §18.3 puts one on the wire: always base64. */
function outputPayload(text: string): { data: string; encoding: string } {
  const bytes = new TextEncoder().encode(text);
  let binary = "";
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return { data: globalThis.btoa(binary), encoding: "base64" };
}

interface FakeSocket {
  readonly url: string;
  readonly sent: string[];
  readonly handlers: RelayHandlers;
  closed: boolean;
}

interface Harness {
  readonly stream: TerminalStream;
  readonly sockets: FakeSocket[];
  readonly latest: () => FakeSocket;
  readonly frames: () => Array<Record<string, unknown>>;
  readonly deliver: (frame: unknown) => void;
  readonly output: string[];
  readonly snapshots: TerminalSnapshot[];
}

function harness(options: { reconnectDelayMs?: number } = {}): Harness {
  const sockets: FakeSocket[] = [];
  const snapshots: TerminalSnapshot[] = [];
  const output: string[] = [];
  let tickets = 0;
  const transport: RelayTransport = (url, handlers) => {
    const socket: FakeSocket = { url, sent: [], handlers, closed: false };
    sockets.push(socket);
    queueMicrotask(() => handlers.onOpen());
    return {
      send: (data) => socket.sent.push(data),
      close: () => {
        socket.closed = true;
      },
    };
  };
  const stream = createTerminalStream({
    url: (ticket) => `wss://hub.invalid/relay?ticket=${ticket}`,
    // A new ticket per connection: each is a separate single-use intent
    // (§18.2), so replaying one would be wrong rather than idempotent.
    mintTicket: async () => {
      tickets += 1;
      return `ticket-${tickets}`;
    },
    transport,
    reconnectDelayMs: options.reconnectDelayMs ?? 0,
    cols: 80,
    rows: 24,
    onOutput: (data) => output.push(data),
    onChange: (snapshot) => snapshots.push(snapshot),
  });
  const latest = () => sockets[sockets.length - 1] as FakeSocket;
  return {
    stream,
    sockets,
    latest,
    frames: () =>
      latest().sent.map((entry) => JSON.parse(entry) as Record<string, unknown>),
    deliver: (frame) => latest().handlers.onMessage(JSON.stringify(frame)),
    output,
    snapshots,
  };
}

describe("the terminal stream", () => {
  it("opens with the window and no stream, then adopts the one the hub names", async () => {
    const h = harness();
    await flush();

    const [open] = h.frames();
    expect(open?.channel).toBe("terminal");
    expect(open?.type).toBe("open");
    // §18.2: the very first frame carries no `stream`, because the hub is what
    // allocates one — and every terminal open allocates, because every open is
    // its own PTY.
    expect(open?.stream).toBeUndefined();
    expect(open?.payload).toEqual({ cols: 80, rows: 24 });

    h.deliver({
      channel: "terminal",
      stream: "conn:1",
      type: "opened",
      seq: 1,
      payload: { pid: 4242, isolation: "container", cols: 80, rows: 24 },
    });
    expect(h.stream.snapshot.status).toBe("open");
    expect(h.stream.snapshot.pid).toBe(4242);
    expect(h.stream.snapshot.isolation).toBe("container");
  });

  it("hands the shell what the person typed and the window they measured", async () => {
    const h = harness();
    await flush();
    h.deliver({ channel: "terminal", stream: "conn:1", type: "opened", seq: 1, payload: { pid: 1 } });

    h.stream.write("echo hi\n");
    h.stream.resize(132, 43);
    const frames = h.frames();
    const input = frames.find((frame) => frame.type === "input");
    expect(input?.stream).toBe("conn:1");
    expect(input?.payload).toEqual({ data: "echo hi\n" });
    const resize = frames.find((frame) => frame.type === "resize");
    expect(resize?.payload).toEqual({ cols: 132, rows: 43 });
    // A resize to the size it already has is not a frame: §18.3 turns one into
    // an ioctl and a SIGWINCH, and a window that did not change must not raise
    // one.
    const before = h.latest().sent.length;
    h.stream.resize(132, 43);
    expect(h.latest().sent.length).toBe(before);
  });

  it("decodes the shell's output in arrival order", async () => {
    const h = harness();
    await flush();
    h.deliver({ channel: "terminal", stream: "conn:1", type: "opened", seq: 1, payload: { pid: 1 } });

    h.deliver({ channel: "terminal", stream: "conn:1", type: "output", seq: 2, payload: outputPayload("hi\r\n") });
    h.deliver({ channel: "terminal", stream: "conn:1", type: "output", seq: 3, payload: outputPayload("$ ") });
    expect(h.output.join("")).toBe("hi\r\n$ ");
  });

  it("ends on the exit frame and reports how", async () => {
    const h = harness();
    await flush();
    h.deliver({ channel: "terminal", stream: "conn:1", type: "opened", seq: 1, payload: { pid: 1 } });
    h.deliver({ channel: "terminal", stream: "conn:1", type: "exit", seq: 2, payload: { code: 7 } });

    expect(h.stream.snapshot.status).toBe("exited");
    expect(h.stream.snapshot.exitCode).toBe(7);
    // A shell that is over is over: the closed frame that gives the stream's
    // slot back does not overwrite the exit.
    h.deliver({ channel: "terminal", stream: "conn:1", type: "closed", seq: 3 });
    expect(h.stream.snapshot.exitCode).toBe(7);
  });

  it("names the signal that killed a shell with no exit code of its own", async () => {
    const h = harness();
    await flush();
    h.deliver({ channel: "terminal", stream: "conn:1", type: "opened", seq: 1, payload: { pid: 1 } });
    h.deliver({
      channel: "terminal",
      stream: "conn:1",
      type: "exit",
      seq: 2,
      payload: { code: -1, signal: "SIGHUP" },
    });
    expect(h.stream.snapshot.signal).toBe("SIGHUP");
  });

  it("acknowledges every thirty-two frames", async () => {
    const h = harness();
    await flush();
    h.deliver({ channel: "terminal", stream: "conn:1", type: "opened", seq: 1, payload: { pid: 1 } });
    for (let seq = 2; seq <= 33; seq += 1) {
      h.deliver({ channel: "terminal", stream: "conn:1", type: "output", seq, payload: outputPayload("x") });
    }
    // The opened frame counts as the first of the thirty-two, so the ack lands
    // one frame before the last output span rather than after it.
    const ack = h.frames().find((frame) => frame.type === "ack");
    expect(ack?.payload).toEqual({ through: 32 });
  });

  it("resumes the same stream on a new ticket when the socket drops", async () => {
    const h = harness();
    await flush();
    h.deliver({ channel: "terminal", stream: "conn:1", type: "opened", seq: 1, payload: { pid: 1 } });
    h.deliver({ channel: "terminal", stream: "conn:1", type: "output", seq: 2, payload: outputPayload("before") });

    h.latest().handlers.onClose("the relay connection closed.");
    expect(h.stream.snapshot.status).toBe("reconnecting");
    await flush();
    await flush();

    // A second socket, on a ticket of its own, carrying a resume for the same
    // stream from the last sequence the client saw (§18.2).
    expect(h.sockets.length).toBe(2);
    expect(h.latest().url).toContain("ticket-2");
    const resume = h.frames().find((frame) => frame.type === "resume");
    expect(resume?.payload).toEqual({ stream: "conn:1", last_seq: 2 });

    h.deliver({ channel: "terminal", stream: "conn:1", type: "resumed", seq: 3, payload: {} });
    expect(h.stream.snapshot.status).toBe("open");
    // And the shell is writable again, on the same stream it always was.
    h.stream.write("still here\n");
    expect(h.frames().find((frame) => frame.type === "input")?.stream).toBe("conn:1");
  });

  it("gives up when the resume is refused, because there is no shell to go back to", async () => {
    const h = harness();
    await flush();
    h.deliver({ channel: "terminal", stream: "conn:1", type: "opened", seq: 1, payload: { pid: 1 } });
    h.latest().handlers.onClose("closed");
    await flush();
    await flush();

    h.deliver({
      channel: "terminal",
      stream: "conn:1",
      type: "error",
      seq: 2,
      payload: { code: "resume_failed" },
    });
    expect(h.stream.snapshot.status).toBe("failed");
    expect(h.stream.snapshot.error).toContain("open a new one");
  });

  it("fails rather than reconnecting when the socket died before a stream existed", async () => {
    const h = harness();
    await flush();
    h.latest().handlers.onClose("The relay connection failed.");
    await flush();
    await flush();

    expect(h.stream.snapshot.status).toBe("failed");
    // Nothing to resume, so nothing redialled.
    expect(h.sockets.length).toBe(1);
  });

  it("drops an undelivered keystroke rather than replaying it", async () => {
    const h = harness();
    await flush();
    h.deliver({ channel: "terminal", stream: "conn:1", type: "opened", seq: 1, payload: { pid: 1 } });
    h.deliver({
      channel: "terminal",
      stream: "conn:1",
      type: "error",
      seq: 2,
      payload: { code: "unknown_delivery", seq: 4 },
    });
    // §18.2: a person-originated frame whose delivery cannot be established is
    // reported and dropped. The shell may already have run it, so the stream
    // stays open and nothing is re-sent.
    expect(h.stream.snapshot.status).toBe("open");
  });

  it("closes the stream and settles when the reader is done", async () => {
    const h = harness();
    await flush();
    h.deliver({ channel: "terminal", stream: "conn:1", type: "opened", seq: 1, payload: { pid: 1 } });

    h.stream.close();
    const close = h.frames().find((frame) => frame.type === "close");
    expect(close?.stream).toBe("conn:1");
    expect(h.latest().closed).toBe(true);
    expect(h.stream.snapshot.status).toBe("exited");
  });

  it("ignores a frame from another channel", async () => {
    const h = harness();
    await flush();
    h.deliver({ channel: "files", stream: "conn:1", type: "listed", seq: 1, payload: { entries: [] } });
    expect(h.stream.snapshot.status).toBe("connecting");
  });
});

describe("output decoding", () => {
  it("decodes base64, which is the only encoding a terminal span uses", () => {
    expect(decodeTerminalOutput(outputPayload("ok\r\n"))).toBe("ok\r\n");
  });

  it("keeps a rune split across two spans whole", () => {
    // A PTY chunks on bytes and knows nothing about runes, so the two halves
    // of one character routinely arrive in different frames.
    const bytes = new TextEncoder().encode("日");
    const half = (from: number, to: number): { data: string; encoding: string } => {
      let binary = "";
      for (const byte of bytes.slice(from, to)) binary += String.fromCharCode(byte);
      return { data: globalThis.btoa(binary), encoding: "base64" };
    };
    const first = decodeTerminalOutput(half(0, 2));
    const second = decodeTerminalOutput(half(2, 3));
    expect(`${first ?? ""}${second ?? ""}`).toBe("日");
  });

  it("honours a span that arrived as plain text", () => {
    expect(decodeTerminalOutput({ data: "plain" })).toBe("plain");
  });

  it("refuses a payload with no data", () => {
    expect(decodeTerminalOutput({ encoding: "base64" })).toBeNull();
    expect(decodeTerminalOutput(null)).toBeNull();
  });

  it("does not mistake a chunked terminal span for a file's content", () => {
    // `files.content` and `terminal.output` both carry `data: string`, so the
    // reassembler needs the channel to tell them apart — the same collision
    // §18.12 had to name for `exec`.
    expect(inferChunkedType({ data: "x", encoding: "base64" }, "terminal")).toBe("output");
    expect(inferChunkedType({ code: "forbidden" }, "terminal")).toBe("error");

    const assembler = new RelayFrameAssembler();
    const frames = [
      ...assembler.push(
        JSON.stringify({
          channel: "terminal",
          stream: "conn:1",
          type: "chunk",
          payload: { part: 1, parts: 2, data: '{"data":"aGk=",' },
        }),
      ),
      ...assembler.push(
        JSON.stringify({
          channel: "terminal",
          stream: "conn:1",
          type: "chunk",
          payload: { part: 2, parts: 2, data: '"encoding":"base64"}' },
        }),
      ),
    ];
    expect(frames.at(-1)?.type).toBe("output");
  });
});

describe("why a terminal is unavailable", () => {
  const base: TerminalPolicy = {
    workItemId: "wi_1",
    canWrite: true,
    canManageRunners: true,
    role: "member",
    workspaceReadOnly: false,
    workspaceState: "ready",
    workspaceReason: null,
    workspaceError: null,
    terminalCapable: true,
  };

  it("allows a member with write and the runners grant", () => {
    expect(terminalBlockedReason(base)).toBeNull();
  });

  it("names the most structural reason first", () => {
    // Every one of these would also be refused for a later reason; a reader has
    // to be told the cause rather than a consequence (§18.6, §18.13).
    expect(terminalBlockedReason({ ...base, workItemId: null })).toBe(
      TERMINAL_POLICY_SENTENCES.noIssue,
    );
    expect(terminalBlockedReason({ ...base, role: "viewer" })).toBe(
      TERMINAL_POLICY_SENTENCES.viewer,
    );
    expect(terminalBlockedReason({ ...base, canWrite: false })).toBe(
      TERMINAL_POLICY_SENTENCES.noWriteGrant,
    );
    expect(terminalBlockedReason({ ...base, canManageRunners: false })).toBe(
      TERMINAL_POLICY_SENTENCES.noRunnerGrant,
    );
    expect(terminalBlockedReason({ ...base, workspaceReadOnly: true })).toBe(
      TERMINAL_POLICY_SENTENCES.attemptRunning,
    );
    expect(terminalBlockedReason({ ...base, terminalCapable: false })).toBe(
      TERMINAL_POLICY_SENTENCES.noCapability,
    );
  });

  it("a viewer loses however generous the grant row is", () => {
    expect(
      terminalBlockedReason({ ...base, role: "viewer", canWrite: true, canManageRunners: true }),
    ).toBe(TERMINAL_POLICY_SENTENCES.viewer);
  });

  it("does not refuse a capability nobody has reported yet", () => {
    // §18.1's `enabled` flag exists so that merely rendering the tab spends no
    // workspace slot; a null is what the panel looks like before anything was
    // asked for, and pressing is what answers the question.
    expect(terminalBlockedReason({ ...base, terminalCapable: null, workspaceState: null })).toBeNull();
  });

  it("passes the hub's own sentence through when the request was refused", () => {
    expect(
      terminalBlockedReason({
        ...base,
        workspaceError: "Terminals are turned off for this organization",
      }),
    ).toBe("Terminals are turned off for this organization");
  });
});
