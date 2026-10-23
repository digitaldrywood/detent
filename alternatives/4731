// The exec stream (decisions.md §18.12): output frames accumulate in arrival
// order, `exited 0` settles succeeded, `exited 3` settles failed, a truncated
// span is marked, and every teardown path writes a terminal status.
//
// Driven through a substituted transport, the way `tests/workspaceRelay.test
// .ts` drives the files client without a socket.
import { describe, expect, it } from "vitest";

import {
  createActionRunStream,
  decodeOutputSpan,
  outputText,
  reasonSentence,
  snapshotFromRecord,
  type ActionRunSnapshot,
  type ActionRunStream,
} from "../src/app/adapters/actionRuns.ts";
import {
  inferChunkedType,
  RelayFrameAssembler,
  type RelayHandlers,
  type RelayTransport,
} from "../src/app/adapters/workspaceRelay.ts";
import type { ActionRun } from "../src/contracts/work.ts";

const flush = () => new Promise((resolve) => setTimeout(resolve, 0));

interface FakeSocket {
  readonly url: string;
  readonly sent: string[];
  readonly handlers: RelayHandlers;
  closed: boolean;
}

interface Harness {
  readonly stream: ActionRunStream;
  readonly sockets: FakeSocket[];
  readonly latest: () => FakeSocket;
  readonly frames: () => Array<Record<string, unknown>>;
  readonly deliver: (frame: unknown) => void;
  readonly snapshots: ActionRunSnapshot[];
}

function harness(): Harness {
  const sockets: FakeSocket[] = [];
  const snapshots: ActionRunSnapshot[] = [];
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
  const stream = createActionRunStream({
    url: (ticket) => `wss://hub.invalid/relay?ticket=${ticket}`,
    mintTicket: async () => "ticket-1",
    transport,
    ackThreshold: 2,
    ackIntervalMs: 10_000,
    run: {
      runId: "run_1",
      actionId: "action_1",
      name: "Run tests",
      command: "bun test",
    },
    onChange: (snapshot) => snapshots.push(snapshot),
  });
  const latest = () => sockets[sockets.length - 1]!;
  return {
    stream,
    sockets,
    latest,
    snapshots,
    frames: () => latest().sent.map((text) => JSON.parse(text) as Record<string, unknown>),
    deliver: (frame) => latest().handlers.onMessage(JSON.stringify(frame)),
  };
}

function output(data: string, extra: Record<string, unknown> = {}): unknown {
  return {
    channel: "exec",
    stream: "stream_1",
    type: "output",
    seq: 1,
    payload: { data, ...extra },
  };
}

describe("the exec stream", () => {
  it("sends one `run` with no stream, and acknowledges on §18.2's cadence", async () => {
    const h = harness();
    await flush();
    expect(h.frames()).toHaveLength(1);
    expect(h.frames()[0]).toEqual({
      channel: "exec",
      type: "run",
      seq: 1,
      payload: { run_id: "run_1", action_id: "action_1", command: "bun test" },
    });
    expect(h.stream.snapshot.status).toBe("queued");

    h.deliver(output("a"));
    h.deliver({ ...(output("b") as Record<string, unknown>), seq: 2 });
    // Two inbound frames at a threshold of two: one ack, through the highest
    // sequence seen, on the stream the first answer named.
    const ack = h.frames().find((frame) => frame.type === "ack");
    expect(ack).toEqual({
      channel: "exec",
      stream: "stream_1",
      type: "ack",
      payload: { through: 2 },
    });
  });

  it("accumulates output frames in arrival order", async () => {
    const h = harness();
    await flush();
    h.deliver(output("first\n"));
    h.deliver(output("second\n"));
    h.deliver(output("third"));
    expect(h.stream.snapshot.status).toBe("running");
    expect(outputText(h.stream.snapshot)).toBe("first\nsecond\nthird");
    expect(h.stream.snapshot.output).toHaveLength(3);
  });

  it("settles succeeded on `exited 0`", async () => {
    const h = harness();
    await flush();
    h.deliver(output("ok\n"));
    h.deliver({ channel: "exec", stream: "stream_1", type: "exited", payload: { code: 0 } });
    expect(h.stream.snapshot.status).toBe("succeeded");
    expect(h.stream.snapshot.exitCode).toBe(0);
    expect(h.stream.snapshot.error).toBeNull();

    // The runner follows `exited` with `closed`, and that must not rewrite the
    // outcome: §18.12's "every teardown path writes a terminal status" is
    // once, not repeatedly.
    h.deliver({ channel: "exec", stream: "stream_1", type: "closed" });
    expect(h.stream.snapshot.status).toBe("succeeded");
  });

  it("settles failed on `exited 3`, keeping the code", async () => {
    const h = harness();
    await flush();
    h.deliver({ channel: "exec", stream: "stream_1", type: "exited", payload: { code: 3 } });
    expect(h.stream.snapshot.status).toBe("failed");
    expect(h.stream.snapshot.exitCode).toBe(3);
  });

  it("names the signal for a killed process, which has no exit code", async () => {
    const h = harness();
    await flush();
    h.deliver({
      channel: "exec",
      stream: "stream_1",
      type: "exited",
      payload: { signal: "SIGKILL" },
    });
    expect(h.stream.snapshot.status).toBe("failed");
    expect(h.stream.snapshot.exitCode).toBeNull();
    expect(h.stream.snapshot.error).toBe("The command was killed by SIGKILL.");
  });

  // §18.12: the runner emits the marker once, as its own span, and keeps
  // draining the pipe so the exit code still means what it says.
  it("marks a truncated span and keeps the outcome", async () => {
    const h = harness();
    await flush();
    h.deliver(output("a lot of output"));
    expect(h.stream.snapshot.truncated).toBe(false);
    h.deliver(output("[output truncated at 1 MiB]", { truncated: true }));
    expect(h.stream.snapshot.truncated).toBe(true);
    expect(h.stream.snapshot.output.at(-1)).toEqual({
      text: "[output truncated at 1 MiB]",
      truncated: true,
    });
    h.deliver({ channel: "exec", stream: "stream_1", type: "exited", payload: { code: 0 } });
    expect(h.stream.snapshot.status).toBe("succeeded");
  });

  it("fails a run whose stream closes before the command exits", async () => {
    const h = harness();
    await flush();
    h.deliver(output("half"));
    h.deliver({ channel: "exec", stream: "stream_1", type: "closed" });
    expect(h.stream.snapshot.status).toBe("failed");
    expect(h.stream.snapshot.error).toBe("The run's stream closed before the command exited.");
  });

  it("fails a run whose socket drops, rather than resuming it", async () => {
    const h = harness();
    await flush();
    h.latest().handlers.onClose("The relay connection failed.");
    expect(h.stream.snapshot.status).toBe("failed");
    expect(h.stream.snapshot.error).toBe("The relay connection failed.");
    // One socket only: a dropped exec stream is not resumed, because the
    // runner kills the process group when the socket dies.
    expect(h.sockets).toHaveLength(1);
  });

  it("reports a relay error in the relay's own words", async () => {
    const h = harness();
    await flush();
    h.deliver({
      channel: "exec",
      stream: "stream_1",
      type: "error",
      payload: { code: "read_only" },
    });
    expect(h.stream.snapshot.status).toBe("failed");
    expect(h.stream.snapshot.error).toBe("This workspace is read-only.");
  });

  it("closes the stream with a `close` frame and a terminal status", async () => {
    const h = harness();
    await flush();
    h.deliver(output("partial"));
    h.stream.close();
    expect(h.frames().at(-1)).toEqual({
      channel: "exec",
      stream: "stream_1",
      type: "close",
    });
    expect(h.latest().closed).toBe(true);
    expect(h.stream.snapshot.status).toBe("failed");
    expect(h.stream.snapshot.error).toBe("This run was stopped before the command exited.");
  });

  it("drops a frame from another channel", async () => {
    const h = harness();
    await flush();
    h.deliver({ channel: "files", stream: "stream_1", type: "content", payload: { data: "x" } });
    expect(h.stream.snapshot.output).toHaveLength(0);
  });
});

describe("output decoding", () => {
  it("honours §18.2's base64 encoding", () => {
    expect(decodeOutputSpan({ data: "aGVsbG8=", encoding: "base64" })).toEqual({
      text: "hello",
      truncated: false,
    });
    expect(decodeOutputSpan({ data: "plain" })).toEqual({ text: "plain", truncated: false });
    expect(decodeOutputSpan({})).toBeNull();
  });

  // A chunked exec payload carries `data`, which is also what a files
  // `content` payload carries — so `inferChunkedType` has to be told the
  // channel or a chunked span of output is read as a file read.
  it("reassembles a chunked output payload as `output`, not `content`", () => {
    const assembler = new RelayFrameAssembler();
    const whole = JSON.stringify({ data: "chunky" });
    const half = Math.ceil(whole.length / 2);
    expect(
      assembler.push(
        JSON.stringify({
          channel: "exec",
          stream: "s",
          type: "chunk",
          payload: { part: 1, parts: 2, data: whole.slice(0, half) },
        }),
      ),
    ).toHaveLength(0);
    const frames = assembler.push(
      JSON.stringify({
        channel: "exec",
        stream: "s",
        type: "chunk",
        payload: { part: 2, parts: 2, data: whole.slice(half) },
      }),
    );
    expect(frames).toHaveLength(1);
    expect(frames[0]?.type).toBe("output");
    expect(inferChunkedType({ code: 0 }, "exec")).toBe("exited");
    expect(inferChunkedType({ code: "denied" }, "exec")).toBe("error");
    // The files rules are unchanged for every other channel.
    expect(inferChunkedType({ data: "x" })).toBe("content");
  });
});

describe("a run read back from the hub", () => {
  it("keeps the record's own status, code and truncation", () => {
    const record = {
      id: "run_9",
      action_id: "action_1",
      workspace_id: "ws_1",
      command: "bun test",
      status: "failed",
      exit_code: null,
      reason: "lease_lost",
      started_at: "2026-09-12T10:00:00Z",
      finished_at: "2026-09-12T10:00:05Z",
      output_bytes: 12,
      truncated: true,
      revision: 3,
      created_at: "2026-09-12T10:00:00Z",
      updated_at: "2026-09-12T10:00:05Z",
    } as ActionRun;
    expect(snapshotFromRecord(record, "Run tests")).toEqual({
      runId: "run_9",
      actionId: "action_1",
      name: "Run tests",
      command: "bun test",
      status: "failed",
      output: [],
      truncated: true,
      exitCode: null,
      signal: null,
      error: "The runner lost its lease on the worktree while the command was running.",
    });
  });

  it("gives each §18.12 reason its own sentence", () => {
    expect(reasonSentence("stream_closed")).toBe(
      "The run's stream closed before the command exited.",
    );
    expect(reasonSentence("killed")).toBe("The command was killed.");
    expect(reasonSentence("workspace_closed")).toBe(
      "The workspace closed while the command was running.",
    );
    // A reason this build does not know is reported as-is rather than hidden.
    expect(reasonSentence("something_new")).toBe("something_new");
  });
});
