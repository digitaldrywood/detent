// The workspace relay's wire behaviour (decisions.md §18.2, §18.4): frame
// assembly and chunk reassembly, stream allocation, the acknowledgement
// cadence, resume across a reconnect, and the error codes a caller acts on.
//
// Every test drives the client through a substituted transport, the way
// `sse.test.ts` drives the parser without a socket.
import { describe, expect, it, vi } from "vitest";

import {
  createWorkspaceRelay,
  decodeContent,
  inferChunkedType,
  RelayError,
  relayErrorMessage,
  RelayFrameAssembler,
  type RelayChannel,
  type RelayHandlers,
  type RelayTransport,
  type WorkspaceRelay,
} from "../src/app/adapters/workspaceRelay.ts";

const flush = () => new Promise((resolve) => setTimeout(resolve, 0));

interface FakeSocket {
  readonly url: string;
  readonly sent: string[];
  readonly handlers: RelayHandlers;
  closed: boolean;
}

interface Harness {
  readonly relay: WorkspaceRelay;
  readonly sockets: FakeSocket[];
  readonly latest: () => FakeSocket;
  readonly frames: () => Array<Record<string, unknown>>;
  readonly deliver: (frame: unknown) => void;
}

function harness(
  options: { ackThreshold?: number; ackIntervalMs?: number; channel?: RelayChannel } = {},
): Harness {
  const sockets: FakeSocket[] = [];
  const transport: RelayTransport = (url, handlers) => {
    const socket: FakeSocket = { url, sent: [], handlers, closed: false };
    sockets.push(socket);
    // A real socket opens asynchronously; so does this one, so nothing in the
    // client is allowed to depend on `onOpen` running inside `transport`.
    queueMicrotask(() => handlers.onOpen());
    return {
      send: (data) => socket.sent.push(data),
      close: () => {
        socket.closed = true;
      },
    };
  };
  let minted = 0;
  const relay = createWorkspaceRelay({
    url: (ticket) => `wss://hub.invalid/relay?ticket=${ticket}`,
    mintTicket: async () => {
      minted += 1;
      return `ticket-${minted}`;
    },
    transport,
    ...options,
  });
  const latest = () => {
    const socket = sockets.at(-1);
    if (socket === undefined) throw new Error("no socket was opened");
    return socket;
  };
  return {
    relay,
    sockets,
    latest,
    frames: () =>
      latest().sent.map((raw) => JSON.parse(raw) as Record<string, unknown>),
    deliver: (frame) => latest().handlers.onMessage(JSON.stringify(frame)),
  };
}

describe("RelayFrameAssembler", () => {
  it("parses one whole frame", () => {
    const assembler = new RelayFrameAssembler();
    expect(
      assembler.push(
        JSON.stringify({ channel: "files", stream: "c1:1", type: "listed", seq: 1, payload: { path: "" } }),
      ),
    ).toEqual([
      { channel: "files", stream: "c1:1", type: "listed", seq: 1, payload: { path: "" } },
    ]);
  });

  it("drops a message that is not a frame rather than failing", () => {
    const assembler = new RelayFrameAssembler();
    expect(assembler.push("not json")).toEqual([]);
    expect(assembler.push(JSON.stringify({ channel: "files" }))).toEqual([]);
    expect(assembler.push(JSON.stringify({ type: "listed" }))).toEqual([]);
  });

  it("reassembles a chunked payload and emits nothing until the last part", () => {
    const assembler = new RelayFrameAssembler();
    const payload = { path: "a.txt", mime: "text/plain", size: 4, offset: 0, data: "abcd", truncated: false };
    const whole = JSON.stringify(payload);
    const half = Math.ceil(whole.length / 2);
    expect(
      assembler.push(
        JSON.stringify({
          channel: "files",
          stream: "c1:1",
          type: "chunk",
          payload: { part: 1, parts: 2, data: whole.slice(0, half) },
        }),
      ),
    ).toEqual([]);
    const frames = assembler.push(
      JSON.stringify({
        channel: "files",
        stream: "c1:1",
        type: "chunk",
        seq: 9,
        payload: { part: 2, parts: 2, data: whole.slice(half) },
      }),
    );
    expect(frames).toEqual([
      { channel: "files", stream: "c1:1", type: "content", seq: 9, payload },
    ]);
  });

  it("reassembles parts in index order, not arrival order", () => {
    const assembler = new RelayFrameAssembler();
    const whole = JSON.stringify({ entries: [{ name: "a" }] });
    const half = Math.ceil(whole.length / 2);
    const chunk = (part: number, data: string) =>
      JSON.stringify({ channel: "files", stream: "s", type: "chunk", payload: { part, parts: 2, data } });
    expect(assembler.push(chunk(2, whole.slice(half)))).toEqual([]);
    const [frame] = assembler.push(chunk(1, whole.slice(0, half)));
    expect(frame?.type).toBe("listed");
    expect(frame?.payload).toEqual({ entries: [{ name: "a" }] });
  });

  it("prefers the type the chunk names over the inferred one", () => {
    const assembler = new RelayFrameAssembler();
    const whole = JSON.stringify({ path: "a", kind: "file", mime: "text/plain" });
    const [frame] = assembler.push(
      JSON.stringify({
        channel: "files",
        type: "chunk",
        payload: { part: 1, parts: 1, data: whole, type: "stat" },
      }),
    );
    expect(frame?.type).toBe("stat");
  });

  it("infers each files answer from its own fields", () => {
    expect(inferChunkedType({ entries: [] })).toBe("listed");
    expect(inferChunkedType({ code: "too_large" })).toBe("error");
    expect(inferChunkedType({ data: "x" })).toBe("content");
    expect(inferChunkedType({ kind: "file", mime: "text/plain" })).toBe("stat");
    expect(inferChunkedType("nonsense")).toBe("content");
  });
});

describe("error codes", () => {
  it("gives every code its own sentence and keeps the code on the error", () => {
    const error = new RelayError("too_large");
    expect(error.code).toBe("too_large");
    expect(error.message).toBe("This file is larger than the 2 MB read limit.");
    expect(relayErrorMessage("denied")).toBe("This path is on the workspace's denylist.");
    expect(relayErrorMessage("not_found")).toBe("That path is no longer in the worktree.");
    expect(relayErrorMessage("weather")).toBe("The relay reported an error.");
  });

  it("keeps a hub-supplied message in preference to the stock sentence", () => {
    expect(new RelayError("forbidden", "nope").message).toBe("nope");
  });
});

describe("decodeContent", () => {
  it("returns plain data untouched and decodes base64", () => {
    const base = { path: "a", mime: "text/plain", size: 2, offset: 0, truncated: false } as const;
    expect(decodeContent({ ...base, data: "hi" })).toBe("hi");
    expect(decodeContent({ ...base, data: "aGk=", encoding: "base64" })).toBe("hi");
  });
});

describe("the workspace relay", () => {
  it("sends the first request with no stream and reuses the id the hub answers with", async () => {
    const relay = harness();
    const listed = relay.relay.list("");
    await flush();

    const [first] = relay.frames();
    expect(first).toMatchObject({ channel: "files", type: "list", seq: 1, payload: { path: "" } });
    expect(first).not.toHaveProperty("stream");
    expect(relay.latest().url).toBe("wss://hub.invalid/relay?ticket=ticket-1");

    relay.deliver({
      channel: "files",
      stream: "conn-7:1",
      type: "listed",
      seq: 1,
      payload: { path: "", entries: [{ name: "a.txt", kind: "file", size: 1, modified_at: "", ignored: false, denied: false }] },
    });
    await expect(listed).resolves.toMatchObject({ path: "" });

    void relay.relay.stat("a.txt");
    await flush();
    expect(relay.frames().at(-1)).toMatchObject({
      channel: "files",
      stream: "conn-7:1",
      type: "stat",
      seq: 2,
    });
  });

  it("holds a second request until the hub has named the stream", async () => {
    const relay = harness();
    void relay.relay.list("");
    void relay.relay.list("src");
    await flush();
    // Only the allocating request goes out; the other waits for the id.
    expect(relay.frames().filter((frame) => frame.type === "list")).toHaveLength(1);

    relay.deliver({ channel: "files", stream: "c:1", type: "listed", seq: 1, payload: { path: "", entries: [] } });
    await flush();
    const lists = relay.frames().filter((frame) => frame.type === "list");
    expect(lists).toHaveLength(2);
    expect(lists[1]).toMatchObject({ stream: "c:1", seq: 2, payload: { path: "src" } });
  });

  // The sixth dogfood run's stream_limit, from the client's side. A client
  // that sends every `list` with no `stream` makes the hub allocate a stream
  // per request, and the ninth is refused with `stream_limit` on a connection
  // that can then never list again. §18.2's rule is one stream per channel per
  // connection, so exactly one request in a run of nine may omit the id.
  it("keeps every later request on the one stream the hub allocated", async () => {
    const relay = harness();
    const rounds = 9;
    for (let round = 1; round <= rounds; round += 1) {
      const listed = relay.relay.list(".");
      await flush();
      relay.deliver({
        channel: "files",
        stream: "conn-5:1",
        type: "listed",
        seq: round,
        payload: { path: ".", entries: [] },
      });
      await expect(listed).resolves.toMatchObject({ path: "." });
    }
    const lists = relay.frames().filter((frame) => frame.type === "list");
    expect(lists).toHaveLength(rounds);
    expect(lists.filter((frame) => frame.stream === undefined)).toHaveLength(1);
    expect(lists.slice(1).map((frame) => frame.stream)).toEqual(
      Array.from({ length: rounds - 1 }, () => "conn-5:1"),
    );
    // seq is per stream per direction from 1, so a reused stream counts up.
    expect(lists.map((frame) => frame.seq)).toEqual(
      Array.from({ length: rounds }, (_value, index) => index + 1),
    );
  });

  it("routes each answer to the request of its kind", async () => {
    const relay = harness();
    const listed = relay.relay.list("");
    await flush();
    relay.deliver({ channel: "files", stream: "c:1", type: "listed", seq: 1, payload: { path: "", entries: [] } });
    await listed;

    const content = relay.relay.read("a.txt");
    const stat = relay.relay.stat("a.txt");
    await flush();
    // Answered out of order on purpose: correlation is by kind, not arrival.
    relay.deliver({ channel: "files", stream: "c:1", type: "stat", seq: 2, payload: { path: "a.txt", kind: "file" } });
    relay.deliver({
      channel: "files",
      stream: "c:1",
      type: "content",
      seq: 3,
      payload: { path: "a.txt", mime: "text/plain", size: 1, offset: 0, data: "x", truncated: false },
    });
    await expect(stat).resolves.toMatchObject({ kind: "file" });
    await expect(content).resolves.toMatchObject({ data: "x" });
  });

  it("fails the request an error names by seq, and leaves the others waiting", async () => {
    const relay = harness();
    const first = relay.relay.list("");
    await flush();
    relay.deliver({ channel: "files", stream: "c:1", type: "listed", seq: 1, payload: { path: "", entries: [] } });
    await first;

    // The list took outbound seq 1, so these are 2 and 3.
    const tooLarge = relay.relay.read("huge.bin");
    const fine = relay.relay.read("a.txt");
    await flush();
    relay.deliver({
      channel: "files",
      stream: "c:1",
      type: "error",
      seq: 4,
      payload: { code: "too_large", seq: 2 },
    });
    await expect(tooLarge).rejects.toMatchObject({ code: "too_large" });

    relay.deliver({
      channel: "files",
      stream: "c:1",
      type: "content",
      seq: 5,
      payload: { path: "a.txt", mime: "text/plain", size: 1, offset: 0, data: "x", truncated: false },
    });
    await expect(fine).resolves.toMatchObject({ path: "a.txt" });
  });

  it("acknowledges after 32 frames without waiting for the timer", async () => {
    const relay = harness({ ackIntervalMs: 60_000 });
    const listed = relay.relay.list("");
    await flush();
    relay.deliver({ channel: "files", stream: "c:1", type: "listed", seq: 1, payload: { path: "", entries: [] } });
    await listed;
    expect(relay.frames().filter((frame) => frame.type === "ack")).toHaveLength(0);

    for (let seq = 2; seq <= 32; seq += 1) {
      relay.deliver({ channel: "files", stream: "c:1", type: "changed", seq, payload: { path: "a", kind: "modified" } });
    }
    const acks = relay.frames().filter((frame) => frame.type === "ack");
    expect(acks).toHaveLength(1);
    expect(acks[0]).toMatchObject({ channel: "files", stream: "c:1", payload: { through: 32 } });
  });

  it("acknowledges on the interval when fewer than 32 frames arrive", async () => {
    const relay = harness({ ackIntervalMs: 10 });
    const listed = relay.relay.list("");
    await flush();
    relay.deliver({ channel: "files", stream: "c:1", type: "listed", seq: 1, payload: { path: "", entries: [] } });
    await listed;
    relay.deliver({ channel: "files", stream: "c:1", type: "changed", seq: 2, payload: { path: "a", kind: "modified" } });
    expect(relay.frames().filter((frame) => frame.type === "ack")).toHaveLength(0);

    await new Promise((resolve) => setTimeout(resolve, 30));
    expect(relay.frames().filter((frame) => frame.type === "ack")).toEqual([
      { channel: "files", stream: "c:1", type: "ack", payload: { through: 2 } },
    ]);
  });

  it("mints a new ticket and resumes from the last seq after a reconnect", async () => {
    const relay = harness();
    const listed = relay.relay.list("");
    await flush();
    relay.deliver({ channel: "files", stream: "c:1", type: "listed", seq: 4, payload: { path: "", entries: [] } });
    await listed;

    relay.latest().handlers.onClose("socket lost");
    const resumed = relay.relay.list("src");
    await flush();

    expect(relay.sockets).toHaveLength(2);
    expect(relay.latest().url).toBe("wss://hub.invalid/relay?ticket=ticket-2");
    expect(relay.frames()[0]).toEqual({
      channel: "files",
      type: "resume",
      payload: { stream: "c:1", last_seq: 4 },
    });
    // Nothing else goes out until the hub says the stream survived.
    expect(relay.frames().filter((frame) => frame.type === "list")).toHaveLength(0);

    relay.deliver({ channel: "files", stream: "c:1", type: "resumed" });
    await flush();
    expect(relay.frames().at(-1)).toMatchObject({ stream: "c:1", type: "list", payload: { path: "src" } });

    relay.deliver({ channel: "files", stream: "c:1", type: "listed", seq: 5, payload: { path: "src", entries: [] } });
    await expect(resumed).resolves.toMatchObject({ path: "src" });
  });

  it("starts a fresh stream when the hub refuses the resume", async () => {
    const relay = harness();
    const listed = relay.relay.list("");
    await flush();
    relay.deliver({ channel: "files", stream: "c:1", type: "listed", seq: 2, payload: { path: "", entries: [] } });
    await listed;

    relay.latest().handlers.onClose("socket lost");
    const retried = relay.relay.list("src");
    await flush();
    relay.deliver({ channel: "files", type: "error", payload: { code: "resume_failed" } });
    await flush();

    const lists = relay.frames().filter((frame) => frame.type === "list");
    expect(lists).toHaveLength(1);
    // The re-sent request allocates again: no stream, and the counter restarted.
    expect(lists[0]).not.toHaveProperty("stream");
    expect(lists[0]).toMatchObject({ seq: 1, payload: { path: "src" } });

    relay.deliver({ channel: "files", stream: "c:2", type: "listed", seq: 1, payload: { path: "src", entries: [] } });
    await expect(retried).resolves.toMatchObject({ path: "src" });
  });

  it("delivers changed frames to a watcher and reports an unsupported watch", async () => {
    const relay = harness();
    const listed = relay.relay.list("");
    await flush();
    relay.deliver({ channel: "files", stream: "c:1", type: "listed", seq: 1, payload: { path: "", entries: [] } });
    await listed;

    const changes: string[] = [];
    relay.relay.watch("", (event) => changes.push(event.path));
    await flush();
    expect(relay.frames().at(-1)).toMatchObject({ type: "watch", stream: "c:1" });
    relay.deliver({ channel: "files", stream: "c:1", type: "changed", seq: 2, payload: { path: "a.txt", kind: "modified" } });
    expect(changes).toEqual(["a.txt"]);

    const unavailable = vi.fn();
    relay.relay.watch("src", () => {}, unavailable);
    await flush();
    const watchSeq = (relay.frames().at(-1) as { seq: number }).seq;
    relay.deliver({ channel: "files", stream: "c:1", type: "error", seq: 3, payload: { code: "unsupported", seq: watchSeq } });
    await flush();
    expect(unavailable).toHaveBeenCalledWith(expect.objectContaining({ code: "unsupported" }));
  });

  it("closes the stream and fails everything in flight on close()", async () => {
    const relay = harness();
    const listed = relay.relay.list("");
    await flush();
    relay.deliver({ channel: "files", stream: "c:1", type: "listed", seq: 1, payload: { path: "", entries: [] } });
    await listed;

    const pending = relay.relay.read("a.txt");
    await flush();
    relay.relay.close();
    expect(relay.frames().at(-1)).toEqual({ channel: "files", stream: "c:1", type: "close" });
    await expect(pending).rejects.toMatchObject({ code: "workspace_closed" });
    expect(relay.relay.state).toBe("closed");
    await expect(relay.relay.list("src")).rejects.toMatchObject({ code: "workspace_closed" });
  });
});

// The `git` channel (decisions.md §18.13). Same stream allocation, same
// correlation rule, three more answer kinds — and one error code that carries
// the tool's own output, which is the only actionable thing about a refusal.
describe("the git channel", () => {
  it("allocates a stream on the first status request and resolves from the answer", async () => {
    const relay = harness({ channel: "git" });
    const status = relay.relay.gitStatus();
    await flush();
    expect(relay.frames()).toEqual([
      // The very first request carries no `stream`: the hub allocates one.
      { channel: "git", type: "status", seq: 1, payload: {} },
    ]);
    relay.deliver({
      channel: "git",
      stream: "w1:1",
      type: "status",
      seq: 1,
      payload: {
        branch: "detent/acme_widgets_42",
        detached: false,
        remote: "origin",
        upstream: true,
        ahead: 2,
        behind: 0,
        dirty_file_count: 3,
        head_sha: "a".repeat(40),
      },
    });
    await expect(status).resolves.toMatchObject({ branch: "detent/acme_widgets_42", ahead: 2 });
  });

  it("reuses the named stream for every later request", async () => {
    const relay = harness({ channel: "git" });
    const first = relay.relay.gitStatus();
    await flush();
    relay.deliver({
      channel: "git",
      stream: "w1:1",
      type: "status",
      seq: 1,
      payload: {
        branch: "main",
        detached: false,
        remote: "origin",
        upstream: true,
        ahead: 0,
        behind: 0,
        dirty_file_count: 0,
        head_sha: "b".repeat(40),
      },
    });
    await first;
    const committed = relay.relay.gitCommit("fix(header): commit from the group");
    await flush();
    expect(relay.frames().at(-1)).toEqual({
      channel: "git",
      stream: "w1:1",
      type: "commit",
      seq: 2,
      payload: { message: "fix(header): commit from the group" },
    });
    relay.deliver({
      channel: "git",
      stream: "w1:1",
      type: "committed",
      seq: 2,
      payload: { commit: "c".repeat(40), branch: "main", files: 3, excluded: [".env"] },
    });
    await expect(committed).resolves.toMatchObject({ files: 3, excluded: [".env"] });
  });

  it("resolves a push from its own answer kind", async () => {
    const relay = harness({ channel: "git" });
    const pushed = relay.relay.gitPush();
    await flush();
    expect(relay.frames().at(-1)).toMatchObject({ channel: "git", type: "push" });
    relay.deliver({
      channel: "git",
      stream: "w1:1",
      type: "pushed",
      seq: 1,
      payload: { branch: "main", remote: "origin", commit: "d".repeat(40) },
    });
    await expect(pushed).resolves.toMatchObject({ remote: "origin" });
  });

  it("rejects a git_failed with the code and the tool's own output intact", async () => {
    const relay = harness({ channel: "git" });
    const pushed = relay.relay.gitPush();
    await flush();
    const seq = (relay.frames().at(-1) as { seq: number }).seq;
    relay.deliver({
      channel: "git",
      stream: "w1:1",
      type: "error",
      seq: 1,
      payload: {
        code: "git_failed",
        seq,
        stderr: "Enumerating objects: 5, done.\n! [rejected]        main -> main (fetch first)\n",
      },
    });
    await expect(pushed).rejects.toMatchObject({
      code: "git_failed",
      stderr: expect.stringContaining("(fetch first)"),
    });
  });

  it("gives a refused command its own sentence", () => {
    expect(relayErrorMessage("git_failed")).toBe(
      "The runner's version control refused the command.",
    );
  });
});
