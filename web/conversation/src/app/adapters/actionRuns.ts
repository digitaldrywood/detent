// One run of one project action, over the relay's `exec` channel
// (decisions.md §18.12).
//
// **Why this is beside `workspaceRelay.ts` and not inside it.** That module is
// a request/response client, and honestly so: `Pending` is a closed union of
// the four answers the files channel gives, `settle` removes the oldest
// request an answer can belong to, and one request settles exactly one
// promise. `exec` is not that conversation. One `run` frame is answered by an
// unbounded number of `output` frames, then one `exited`, then `closed`, and
// nothing about it is a promise: the surface has to see the output arrive, not
// receive it at the end. Widening `Pending["answer"]` to carry a "settles many
// times, then finishes" mode would leave that module describing two protocols
// and claiming to describe one, and every future reader of `settle` would have
// to work out which of the two they were looking at. So the streaming shape is
// its own client, and what is shared is shared as code rather than copied:
// `RelayFrameAssembler` does the framing and the chunk reassembly (its
// `inferChunkedType` grew the one exec case it needed), `RelayError` and
// `relayErrorMessage` do the errors, `webSocketTransport` is the same
// transport seam, and §18.2's acknowledgement cadence is the same 32 frames or
// one second.
//
// What is genuinely different, and is why the file is short: there is no
// stream reuse and no resume. §18.12 gives every run its own stream, as every
// terminal `open` gets its own PTY, so this client opens a socket, allocates
// one stream, sends one `run`, and is finished when the run is. A reconnect
// would resume a stream whose process the runner has already killed — it kills
// the process group when the socket dies — so a dropped socket is a failed
// run, reported as one, and not something to paper over.
import React from "react";

import {
  RelayError,
  RelayFrameAssembler,
  webSocketTransport,
  type RelayFrame,
  type RelaySocket,
  type RelayTransport,
} from "./workspaceRelay.ts";
import { newWorkKey } from "../work/lib/workHttp.ts";
import type { WorkHttp } from "../work/lib/workHttp.ts";
import { useWorkHttp } from "../work/lib/useWork.ts";
import {
  ACTION_RUN_EVENT_TYPES,
  type ActionList,
  type ActionRun,
  type ActionRunList,
} from "../../contracts/work.ts";

const CHANNEL = "exec";

/** §18.2's ack cadence, the same two numbers the files client uses. */
const ACK_THRESHOLD = 32;
const ACK_INTERVAL_MS = 1_000;

/** §18.12: the marker the runner emits once, as its own span, at the cap. */
export const OUTPUT_TRUNCATED_MARKER = "[output truncated at 1 MiB]";

/**
 * One span of a run's output.
 *
 * A span rather than a flat string, because §18.12's truncation marker is
 * "its own span" and a reader has to be able to see that the log was cut
 * rather than finished. `truncated` is the runner's own flag on the frame.
 */
export interface ActionRunOutputSpan {
  readonly text: string;
  readonly truncated: boolean;
}

/**
 * What the Output surface draws.
 *
 * `status` mirrors the hub's four (§18.12) so a run watched over the stream
 * and a run read back from `GET …/runs/:run` are the same shape:
 * `connecting` is the client's own fifth state, before the socket is open,
 * and it is deliberately not one of the hub's — the row is already `queued`
 * there, which is what a reader is told.
 */
export interface ActionRunSnapshot {
  readonly runId: string;
  readonly actionId: string;
  /** The action's name, for the surface's header. */
  readonly name: string;
  readonly command: string;
  readonly status: "connecting" | "queued" | "running" | "succeeded" | "failed";
  readonly output: readonly ActionRunOutputSpan[];
  /** True once any span carried the runner's truncation flag. */
  readonly truncated: boolean;
  readonly exitCode: number | null;
  readonly signal: string | null;
  /** Why it failed without exiting, as a sentence. Null while it is fine. */
  readonly error: string | null;
}

export interface ActionRunStreamOptions {
  /** Same-origin relay path; the ticket is appended by `mintTicket`'s caller. */
  readonly url: (ticket: string) => string;
  readonly mintTicket: () => Promise<string>;
  readonly transport?: RelayTransport;
  readonly ackIntervalMs?: number;
  readonly ackThreshold?: number;
  /** The run the hub has already recorded as `queued`. */
  readonly run: {
    readonly runId: string;
    readonly actionId: string;
    readonly name: string;
    readonly command: string;
  };
  /** Called on every change, with the whole snapshot. */
  readonly onChange: (snapshot: ActionRunSnapshot) => void;
}

export interface ActionRunStream {
  /** Closes the stream. A run still in flight is marked failed, not forgotten. */
  close(): void;
  readonly snapshot: ActionRunSnapshot;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null;
}

/** Decodes an `output` payload, honouring §18.2's base64 encoding. */
export function decodeOutputSpan(payload: unknown): ActionRunOutputSpan | null {
  if (!isRecord(payload)) return null;
  const data = typeof payload.data === "string" ? payload.data : null;
  if (data === null) return null;
  const truncated = payload.truncated === true;
  if (payload.encoding !== "base64") return { text: data, truncated };
  try {
    const binary = globalThis.atob(data);
    const bytes = Uint8Array.from(binary, (character) => character.charCodeAt(0));
    return { text: new TextDecoder().decode(bytes), truncated };
  } catch {
    // An undecodable span is reported as empty rather than dropped silently:
    // the truncation flag on it still matters.
    return { text: "", truncated };
  }
}

/** The whole output as one string, in arrival order, for a copy or a read. */
export function outputText(snapshot: ActionRunSnapshot): string {
  return snapshot.output.map((span) => span.text).join("");
}

export function createActionRunStream(options: ActionRunStreamOptions): ActionRunStream {
  const transport = options.transport ?? webSocketTransport;
  const ackThreshold = options.ackThreshold ?? ACK_THRESHOLD;
  const ackIntervalMs = options.ackIntervalMs ?? ACK_INTERVAL_MS;
  const assembler = new RelayFrameAssembler();

  let snapshot: ActionRunSnapshot = {
    runId: options.run.runId,
    actionId: options.run.actionId,
    name: options.run.name,
    command: options.run.command,
    status: "connecting",
    output: [],
    truncated: false,
    exitCode: null,
    signal: null,
    error: null,
  };

  let socket: RelaySocket | null = null;
  let streamId: string | null = null;
  let sentRun = false;
  let settled = false;
  let disposed = false;
  let inboundSeq = 0;
  let unacked = 0;
  let ackTimer: ReturnType<typeof setTimeout> | undefined;

  function publish(next: Partial<ActionRunSnapshot>): void {
    snapshot = { ...snapshot, ...next };
    options.onChange(snapshot);
  }

  function clearAckTimer(): void {
    if (ackTimer === undefined) return;
    clearTimeout(ackTimer);
    ackTimer = undefined;
  }

  function flushAck(): void {
    clearAckTimer();
    if (unacked === 0 || streamId === null || socket === null) return;
    unacked = 0;
    socket.send(
      JSON.stringify({
        channel: CHANNEL,
        stream: streamId,
        type: "ack",
        payload: { through: inboundSeq },
      }),
    );
  }

  function noteInbound(seq: number | undefined): void {
    if (seq === undefined) return;
    if (seq > inboundSeq) inboundSeq = seq;
    unacked += 1;
    if (unacked >= ackThreshold) {
      flushAck();
      return;
    }
    if (ackTimer === undefined) ackTimer = setTimeout(flushAck, ackIntervalMs);
  }

  /**
   * A terminal status, written exactly once.
   *
   * §18.12: "every teardown path writes a terminal status", because a run left
   * running forever is the bug that rule exists to prevent. `settled` is what
   * makes the second path a no-op rather than a second answer — the runner
   * follows `exited` with `closed`, so the ordinary happy path takes two of
   * these.
   */
  function finish(next: Partial<ActionRunSnapshot> & { status: "succeeded" | "failed" }): void {
    if (settled) return;
    settled = true;
    clearAckTimer();
    publish(next);
  }

  function onFrame(frame: RelayFrame): void {
    if (frame.channel !== CHANNEL) return;
    // The first answer names the stream every later frame of this run reuses
    // (§18.2). There is only ever one run on it (§18.12).
    if (streamId === null && frame.stream !== undefined) streamId = frame.stream;
    noteInbound(frame.seq);
    switch (frame.type) {
      case "output": {
        const span = decodeOutputSpan(frame.payload);
        if (span === null) return;
        publish({
          status: settled ? snapshot.status : "running",
          output: [...snapshot.output, span],
          truncated: snapshot.truncated || span.truncated,
        });
        return;
      }
      case "exited": {
        const payload = isRecord(frame.payload) ? frame.payload : {};
        const code = typeof payload.code === "number" ? payload.code : null;
        const signal = typeof payload.signal === "string" ? payload.signal : null;
        // §18.12: `succeeded` is exit 0 and `failed` is everything else. A
        // process killed by a signal exits with no code of its own, so it is
        // a failure that names the signal rather than an unexplained one.
        finish({
          status: code === 0 ? "succeeded" : "failed",
          exitCode: code,
          signal,
          error:
            code === 0
              ? null
              : signal !== null
                ? `The command was killed by ${signal}.`
                : code === null
                  ? "The command ended without an exit code."
                  : null,
        });
        return;
      }
      case "closed":
        // The runner follows `exited` with `closed`, so by the time this
        // arrives the run is normally already settled and this is a no-op.
        // When it is not, the stream ended before the process did.
        finish({
          status: "failed",
          error: "The run's stream closed before the command exited.",
        });
        return;
      case "error": {
        const payload = isRecord(frame.payload) ? frame.payload : {};
        const code = typeof payload.code === "string" ? payload.code : "invalid_frame";
        const message = typeof payload.message === "string" ? payload.message : undefined;
        finish({ status: "failed", error: new RelayError(code, message).message });
        return;
      }
      default:
        // §18.2: an unknown type is dropped rather than acted on.
        return;
    }
  }

  function sendRun(opened: RelaySocket): void {
    if (sentRun) return;
    sentRun = true;
    publish({ status: "queued" });
    opened.send(
      JSON.stringify({
        channel: CHANNEL,
        // The very first frame carries no `stream`: the hub allocates one and
        // names it on the answer (§18.2).
        type: "run",
        seq: 1,
        payload: {
          run_id: options.run.runId,
          action_id: options.run.actionId,
          command: options.run.command,
        },
      }),
    );
  }

  void options
    .mintTicket()
    .then((ticket) => {
      if (disposed) return;
      const opened = transport(options.url(ticket), {
        onOpen: () => sendRun(opened),
        onMessage: (data) => {
          for (const frame of assembler.push(data)) onFrame(frame);
        },
        onClose: (detail) => {
          socket = null;
          clearAckTimer();
          if (disposed) return;
          // A dropped socket is a failed run, not something to resume: the
          // runner kills the process group when the socket dies (§18.12).
          finish({ status: "failed", error: detail });
        },
      });
      socket = opened;
    })
    .catch((cause: unknown) => {
      if (disposed) return;
      finish({
        status: "failed",
        error: cause instanceof Error ? cause.message : String(cause),
      });
    });

  return {
    close: () => {
      disposed = true;
      clearAckTimer();
      if (socket !== null && streamId !== null) {
        socket.send(JSON.stringify({ channel: CHANNEL, stream: streamId, type: "close" }));
      }
      socket?.close();
      socket = null;
      finish({
        status: "failed",
        error: "This run was stopped before the command exited.",
      });
    },
    get snapshot() {
      return snapshot;
    },
  };
}

// --- The surface's view of a project's runs ---------------------------------

/** What a run record from the hub looks like once the surface has it. */
export function snapshotFromRecord(
  record: ActionRun,
  name: string,
  output: readonly ActionRunOutputSpan[] = [],
): ActionRunSnapshot {
  return {
    runId: record.id,
    actionId: record.action_id,
    name,
    command: record.command,
    status: record.status,
    output,
    truncated: record.truncated,
    exitCode: record.exit_code,
    signal: null,
    error: record.reason === undefined || record.reason.length === 0 ? null : reasonSentence(record.reason),
  };
}

/** The four `reason` values §18.12 names, as sentences a reader is owed. */
export function reasonSentence(reason: string): string {
  switch (reason) {
    case "lease_lost":
      return "The runner lost its lease on the worktree while the command was running.";
    case "stream_closed":
      return "The run's stream closed before the command exited.";
    case "killed":
      return "The command was killed.";
    case "workspace_closed":
      return "The workspace closed while the command was running.";
    default:
      return reason;
  }
}

export interface StartRunInput {
  readonly actionId: string;
  readonly name: string;
  readonly command: string;
}

/**
 * The three reads that recover a run nobody in this tab watched (§18.12).
 *
 * It is a narrow interface rather than `WorkHttp` so the load can be tested on
 * its own: `WorkHttp` satisfies it structurally, and a test that had to build
 * the whole client to assert three calls would be testing the fake.
 */
export interface RecordedRunReader {
  listActions(projectId: string): Promise<ActionList>;
  listActionRuns(projectId: string, actionId: string): Promise<ActionRunList>;
  readActionRunOutput(projectId: string, actionId: string, runId: string): Promise<string>;
}

/**
 * Reads a run's recorded output back, or an empty span list when there is
 * nothing to read.
 *
 * `output_bytes` is what decides, not the status: a command that printed
 * nothing and exited zero has no output, and asking for it would spend a
 * request to be handed an empty body. A run still `queued` or `running` has
 * nothing stored either — §18.12 accumulates the output in memory and writes
 * it once, on completion — so only a terminal run is read.
 */
async function recordedOutput(
  reader: RecordedRunReader,
  projectId: string,
  record: ActionRun,
): Promise<readonly ActionRunOutputSpan[]> {
  if (record.status !== "succeeded" && record.status !== "failed") return [];
  if (record.output_bytes <= 0) return [];
  try {
    const text = await reader.readActionRunOutput(projectId, record.action_id, record.id);
    if (text.length === 0) return [];
    return [{ text, truncated: record.truncated }];
  } catch {
    // The run itself is still worth showing. A reader who can see that the
    // command failed, and why, is better served than one shown nothing
    // because the log could not be fetched.
    return [];
  }
}

/**
 * Every run the hub has recorded for a project, newest first.
 *
 * This is what makes a run nobody watched visible (§18.12). Until now the
 * Output surface only ever held runs this tab started over the exec channel,
 * so a run queued through the API — which is every run a headless caller
 * makes, and now every run the hub dispatches to the runner itself — was
 * invisible to the panel even though the hub had the whole thing on disk.
 *
 * The listing is per action because that is the shape the hub serves
 * (`GET …/actions/:id/runs`), and the set of actions is capped at 50 per
 * project, so the fan-out is bounded by the same rule that bounds the header
 * menu. Each action's own list is already newest first; the merge sorts across
 * them by `created_at` so the panel's order is the project's rather than the
 * order the requests happened to answer in.
 */
export async function loadRecordedRuns(
  reader: RecordedRunReader,
  projectId: string,
): Promise<readonly ActionRunSnapshot[]> {
  const actions = await reader.listActions(projectId);
  const listings = await Promise.all(
    actions.items.map(async (action) => {
      try {
        const runs = await reader.listActionRuns(projectId, action.id);
        return runs.items.map((record) => ({ record, name: action.name }));
      } catch {
        // One action's runs failing is not the whole surface failing.
        return [];
      }
    }),
  );
  const flattened = listings.flat();
  flattened.sort((left, right) => right.record.created_at.localeCompare(left.record.created_at));
  return Promise.all(
    flattened.map(async ({ record, name }) =>
      snapshotFromRecord(record, name, await recordedOutput(reader, projectId, record)),
    ),
  );
}

export interface ActionRunsHandle {
  /** Newest first, which is the order §18.12 lists an action's runs in. */
  readonly runs: readonly ActionRunSnapshot[];
  readonly activeRunId: string | null;
  readonly select: (runId: string) => void;
  /**
   * Starts a run. A no-op without a live workspace: `RightPanel.tsx` asks for
   * one first, and the surface says what it is waiting for until it arrives.
   */
  readonly start: (input: StartRunInput) => void;
  /** True while a start is waiting for a workspace rather than for a runner. */
  readonly pending: boolean;
}

export interface UseActionRunsInput {
  readonly projectId: string | null;
  readonly workspaceId: string | null;
  /** False until the workspace is `ready` or `idle`; a start waits for it. */
  readonly workspaceLive: boolean;
  readonly relayUrl: ((ticket: string) => string) | null;
  readonly mintTicket: (() => Promise<string>) | null;
  /**
   * True while the Output surface is on the panel, which is when the recorded
   * runs are worth fetching (§18.12).
   *
   * It is a flag rather than an unconditional load because the read is a
   * listing per action: a panel that never opens Output should not spend it,
   * and a panel that does should not have to wait for a person to start a run
   * before it can show the ones the hub already has.
   */
  readonly showRecorded: boolean;
}

/**
 * The runs this tab has started or been told about.
 *
 * Two sources, and both are needed. A run this tab started is followed over
 * its own exec stream, because output only exists on the stream while it is
 * arriving. A run this tab did *not* start — a run-on-worktree-creation run,
 * or one another tab began — arrives as an `action_run.<status>` frame on the
 * project event stream (§18.12: "that is how a client observes a run it did
 * not start ... by the same subscription and never by polling"), and its
 * output is read back from `output_artifact` once it is terminal.
 */
export function useActionRuns(input: UseActionRunsInput): ActionRunsHandle {
  const http = useWorkHttp();
  const [runs, setRuns] = React.useState<readonly ActionRunSnapshot[]>([]);
  const [activeRunId, setActiveRunId] = React.useState<string | null>(null);
  const [queue, setQueue] = React.useState<readonly StartRunInput[]>([]);
  const streams = React.useRef(new Map<string, ActionRunStream>());

  const upsert = React.useCallback((snapshot: ActionRunSnapshot) => {
    setRuns((current) => {
      const index = current.findIndex((entry) => entry.runId === snapshot.runId);
      if (index < 0) return [snapshot, ...current];
      const next = [...current];
      next[index] = snapshot;
      return next;
    });
  }, []);

  // Every stream this tab opened is closed on unmount, which writes a terminal
  // status on each — §18.12's rule that no teardown path leaves a run running.
  React.useEffect(
    () => () => {
      for (const stream of streams.current.values()) stream.close();
      streams.current.clear();
    },
    [],
  );

  const { projectId, workspaceId, workspaceLive, relayUrl, mintTicket, showRecorded } = input;

  // A run this tab is following over its own stream is never replaced by a
  // recorded copy of itself: the stream carries output the record does not have
  // yet, and the record would blank it.
  const mergeRecorded = React.useCallback((loaded: readonly ActionRunSnapshot[]) => {
    setRuns((current) => {
      const known = new Set(current.map((entry) => entry.runId));
      const added = loaded.filter((entry) => !known.has(entry.runId));
      if (added.length === 0) return current;
      // The recorded runs go after what this tab already holds, which is what
      // it started or watched this session -- newer than anything on disk.
      return [...current, ...added];
    });
  }, []);

  // The queue drains once there is a live workspace to run in. Running an
  // action is what asks for the worktree (`RightPanel.tsx`), so the gap
  // between the reader's click and a `ready` workspace is the normal case
  // rather than an error.
  React.useEffect(() => {
    if (queue.length === 0) return;
    if (
      projectId === null ||
      workspaceId === null ||
      !workspaceLive ||
      relayUrl === null ||
      mintTicket === null
    ) {
      return;
    }
    const starting = queue;
    setQueue([]);
    for (const request of starting) {
      void startOne(http, {
        projectId,
        workspaceId,
        relayUrl,
        mintTicket,
        request,
        onSnapshot: upsert,
        onStream: (runId, stream) => {
          streams.current.set(runId, stream);
          setActiveRunId(runId);
        },
      });
    }
  }, [http, mintTicket, projectId, queue, relayUrl, upsert, workspaceId, workspaceLive]);

  // Runs this tab did not start (§18.12). The stream is the project's, so a
  // frame for another project's action is dropped; a frame for a run this tab
  // is already following is dropped too, because the stream it is following
  // carries the output and this frame does not.
  const actionNames = React.useRef(new Map<string, string>());

  // The runs the hub has already recorded (§18.12). Without this the panel only
  // ever held what this tab started, so a run requested through the API — the
  // headless caller's run, and now every run the hub hands to the runner itself
  // — was invisible here however completely the hub had recorded it.
  React.useEffect(() => {
    if (!showRecorded || projectId === null) return;
    let live = true;
    void (async () => {
      let loaded: readonly ActionRunSnapshot[];
      try {
        loaded = await loadRecordedRuns(http, projectId);
      } catch {
        // The surface still draws whatever this tab is following. A listing
        // that failed is not a reason to throw away a run in flight.
        return;
      }
      if (!live) return;
      for (const entry of loaded) actionNames.current.set(entry.actionId, entry.name);
      mergeRecorded(loaded);
    })();
    return () => {
      live = false;
    };
  }, [http, mergeRecorded, projectId, showRecorded]);

  React.useEffect(() => {
    if (projectId === null) return;
    if (typeof globalThis.EventSource !== "function") return;
    const source = new globalThis.EventSource(http.eventsUrl(projectId), {
      withCredentials: true,
    });
    const onRunEvent = (event: MessageEvent<string>) => {
      let parsed: unknown;
      try {
        parsed = JSON.parse(event.data);
      } catch {
        return;
      }
      if (!isRecord(parsed)) return;
      const runId = typeof parsed.id === "string" ? parsed.id : null;
      const actionId = typeof parsed.action_id === "string" ? parsed.action_id : null;
      if (runId === null || actionId === null) return;
      if (streams.current.has(runId)) return;
      const record = parsed as unknown as ActionRun;
      const name = actionNames.current.get(actionId) ?? "Action";
      upsert(snapshotFromRecord(record, name, []));
      // The event carries the row and never the bytes, so a run this tab only
      // heard about has to read its own output back once it is finished. That
      // is the whole difference between watching a run the hub dispatched and
      // watching an empty box say "succeeded".
      void (async () => {
        const output = await recordedOutput(http, projectId, record);
        if (output.length === 0) return;
        upsert(snapshotFromRecord(record, name, output));
      })();
    };
    for (const type of ACTION_RUN_EVENT_TYPES) {
      source.addEventListener(type, onRunEvent as EventListener);
    }
    return () => {
      for (const type of ACTION_RUN_EVENT_TYPES) {
        source.removeEventListener(type, onRunEvent as EventListener);
      }
      source.close();
    };
  }, [http, projectId, upsert]);

  const start = React.useCallback((request: StartRunInput) => {
    actionNames.current.set(request.actionId, request.name);
    setQueue((current) => [...current, request]);
  }, []);

  return React.useMemo<ActionRunsHandle>(
    () => ({
      runs,
      activeRunId: activeRunId ?? runs[0]?.runId ?? null,
      select: setActiveRunId,
      start,
      pending: queue.length > 0,
    }),
    [activeRunId, queue.length, runs, start],
  );
}

/** Records the run, then follows it. The 202 exists before the first frame. */
async function startOne(
  http: WorkHttp,
  input: {
    readonly projectId: string;
    readonly workspaceId: string;
    readonly relayUrl: (ticket: string) => string;
    readonly mintTicket: () => Promise<string>;
    readonly request: StartRunInput;
    readonly onSnapshot: (snapshot: ActionRunSnapshot) => void;
    readonly onStream: (runId: string, stream: ActionRunStream) => void;
  },
): Promise<void> {
  let runId: string;
  try {
    const accepted = await http.startActionRun({
      projectId: input.projectId,
      actionId: input.request.actionId,
      key: newWorkKey("action-run"),
      workspaceId: input.workspaceId,
    });
    runId = accepted.run_id;
  } catch (cause) {
    // The run was refused, so there is no record to show — but the reader
    // pressed something, and a refusal that leaves the surface empty is the
    // failure §5 is about. A synthetic id keyed on the action is enough for
    // the surface to draw one row saying why.
    input.onSnapshot({
      runId: `refused:${input.request.actionId}`,
      actionId: input.request.actionId,
      name: input.request.name,
      command: input.request.command,
      status: "failed",
      output: [],
      truncated: false,
      exitCode: null,
      signal: null,
      error: cause instanceof Error ? cause.message : String(cause),
    });
    return;
  }
  const stream = createActionRunStream({
    url: input.relayUrl,
    mintTicket: input.mintTicket,
    run: {
      runId,
      actionId: input.request.actionId,
      name: input.request.name,
      command: input.request.command,
    },
    onChange: input.onSnapshot,
  });
  input.onStream(runId, stream);
  input.onSnapshot(stream.snapshot);
}
