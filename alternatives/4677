// Server-sent event transport for the conversation stream.
//
// Written for this repository. Two transports implement the same contract:
// `EventSource` in the browser (cookie authenticated, same-origin), and a
// `fetch` reader everywhere `EventSource` is not a global — Node still does
// not expose one by default, and the runtime integration tests drive the
// client outside a DOM.
//
// The browser transport closes the `EventSource` on its first error instead
// of letting it retry: reconnection is the supervisor's job, and it has to
// resume from the cursor this client tracked, not from `Last-Event-ID`.

export interface SseFrame {
  /** Value of the `id:` field. Heartbeats carry no id (decisions.md §5). */
  readonly id: string | null;
  readonly event: string;
  readonly data: string;
}

export interface SseHandlers {
  readonly onFrame: (frame: SseFrame) => void;
  readonly onError: (detail: string) => void;
}

export type SseTransport = (url: string, handlers: SseHandlers) => () => void;

/**
 * Incremental parser for the `text/event-stream` wire format. Split out so
 * frame assembly across chunk boundaries is testable without a socket.
 */
export class SseParser {
  private buffer = "";
  private id: string | null = null;
  private event = "message";
  private data: string[] = [];

  push(chunk: string): SseFrame[] {
    this.buffer += chunk;
    const frames: SseFrame[] = [];
    let newline = this.buffer.indexOf("\n");
    while (newline >= 0) {
      const raw = this.buffer.slice(0, newline).replace(/\r$/, "");
      this.buffer = this.buffer.slice(newline + 1);
      const frame = this.line(raw);
      if (frame !== null) frames.push(frame);
      newline = this.buffer.indexOf("\n");
    }
    return frames;
  }

  private line(raw: string): SseFrame | null {
    if (raw === "") {
      if (this.data.length === 0 && this.event === "message") {
        this.reset();
        return null;
      }
      const frame: SseFrame = {
        id: this.id,
        event: this.event,
        data: this.data.join("\n"),
      };
      this.reset();
      return frame;
    }
    if (raw.startsWith(":")) return null;
    const colon = raw.indexOf(":");
    const field = colon < 0 ? raw : raw.slice(0, colon);
    let value = colon < 0 ? "" : raw.slice(colon + 1);
    if (value.startsWith(" ")) value = value.slice(1);
    if (field === "id") this.id = value;
    else if (field === "event") this.event = value;
    else if (field === "data") this.data.push(value);
    return null;
  }

  private reset(): void {
    this.id = null;
    this.event = "message";
    this.data = [];
  }
}

/** Browser transport. Reconnection is disabled on purpose (see file header). */
export const eventSourceTransport: SseTransport = (url, handlers) => {
  const source = new EventSource(url, { withCredentials: true });
  let disposed = false;
  const dispose = () => {
    if (disposed) return;
    disposed = true;
    source.close();
  };
  const forward = (type: string) => (raw: Event) => {
    const message = raw as MessageEvent<string>;
    handlers.onFrame({
      id: message.lastEventId === "" ? null : message.lastEventId,
      event: type,
      data: message.data,
    });
  };
  for (const type of [
    "conversation.updated",
    "message.accepted",
    "message.delta",
    "message.updated",
    "question.opened",
    "question.updated",
    "execution.updated",
    "command.receipt",
    "heartbeat",
    "closed",
  ]) {
    source.addEventListener(type, forward(type));
  }
  source.addEventListener("message", forward("message"));
  source.addEventListener("error", () => {
    dispose();
    handlers.onError("The event stream closed.");
  });
  return dispose;
};

/** Non-browser transport used by Node and by the runtime integration tests. */
export function fetchEventStreamTransport(
  fetchImpl: typeof globalThis.fetch = globalThis.fetch,
): SseTransport {
  return (url, handlers) => {
    const controller = new AbortController();
    let disposed = false;
    const dispose = () => {
      if (disposed) return;
      disposed = true;
      controller.abort();
    };
    void (async () => {
      try {
        const response = await fetchImpl(url, {
          method: "GET",
          credentials: "same-origin",
          headers: { Accept: "text/event-stream" },
          signal: controller.signal,
        });
        if (!response.ok || response.body === null) {
          if (!disposed) handlers.onError(`The event stream failed (${response.status}).`);
          return;
        }
        const parser = new SseParser();
        const reader = response.body.getReader();
        const decoder = new TextDecoder();
        for (;;) {
          const { done, value } = await reader.read();
          if (done) break;
          for (const frame of parser.push(decoder.decode(value, { stream: true }))) {
            if (disposed) return;
            handlers.onFrame(frame);
          }
        }
        if (!disposed) handlers.onError("The event stream ended.");
      } catch (cause) {
        if (disposed) return;
        handlers.onError(cause instanceof Error ? cause.message : String(cause));
      }
    })();
    return dispose;
  };
}

export function defaultSseTransport(): SseTransport {
  return typeof globalThis.EventSource === "function"
    ? eventSourceTransport
    : fetchEventStreamTransport();
}

/** No frame — not even a heartbeat — for this long means the stream is dead. */
export const HEARTBEAT_TIMEOUT_MS = 45_000;
