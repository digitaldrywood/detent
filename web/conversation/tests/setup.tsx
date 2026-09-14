// Test environment shims.
//
import type { ReactNode, Ref } from "react";
import { vi } from "vitest";

import type { LegendListRef } from "@legendapp/list/react";

vi.mock("@legendapp/list/react", () => {
  const LegendList = (props: {
    data: Array<{ id: string }>;
    keyExtractor: (item: { id: string }) => string;
    renderItem: (args: { item: { id: string } }) => ReactNode;
    ListHeaderComponent?: ReactNode;
    ListFooterComponent?: ReactNode;
    anchoredEndSpace?: { anchorIndex: number; onReady?: (info: { anchorIndex: number }) => void };
    className?: string;
    ref?: Ref<LegendListRef>;
  }) => {
    props.anchoredEndSpace?.onReady?.({ anchorIndex: props.anchoredEndSpace.anchorIndex });
    return (
      <div data-testid="legend-list" className={props.className}>
        {props.ListHeaderComponent}
        {props.data.map((item) => (
          <div key={props.keyExtractor(item)}>{props.renderItem({ item })}</div>
        ))}
        {props.ListFooterComponent}
      </div>
    );
  };
  return { LegendList };
});

if (!("cookieStore" in globalThis)) {
  Object.defineProperty(globalThis, "cookieStore", {
    configurable: true,
    value: {
      get: async () => null,
      set: async () => undefined,
      delete: async () => undefined,
    },
  });
}

if (typeof window !== "undefined" && typeof window.matchMedia !== "function") {
  Object.defineProperty(window, "matchMedia", {
    configurable: true,
    writable: true,
    value: (query: string) => ({
      matches: false,
      media: query,
      onchange: null,
      addEventListener: () => {},
      removeEventListener: () => {},
      addListener: () => {},
      removeListener: () => {},
      dispatchEvent: () => false,
    }),
  });
}

/**
 * jsdom implements `requestAnimationFrame` with a 60 Hz `setInterval`, and the
 * interval outlives the frame it was scheduled for.
 *
 * That is a real problem here rather than a curiosity. Base UI's popups ask
 * their transition scheduler for a frame when they mount
 * (`@base-ui/react/internals/useTransitionStatus` →
 * `@base-ui/utils/useAnimationFrame`), so every menu, popover and tooltip that
 * opens in a test leaves one 16.7 ms interval running in the worker — React's
 * unmount cancels the frame, but jsdom's `cancelAnimationFrame` does not stop
 * the interval behind it. They accumulate for the lifetime of the file: with a
 * dozen popups opened, a test that renders a bare `<div>` was taking over
 * twenty seconds, and the worker eventually missed vitest's `onTaskUpdate`
 * heartbeat and failed the run with an unhandled RPC timeout while every test
 * passed.
 *
 * A one-shot timer cannot do that: it fires once and is finished whether or not
 * anybody cancels it. The contract callers depend on is "call me back
 * asynchronously, once, with a timestamp", which this keeps.
 *
 * The delay stays at a frame's length rather than dropping to zero, and that
 * matters more than it looks. Base UI's scheduler re-requests a frame while it
 * waits for a transition to settle, so a zero-delay timer turns a 60 Hz poll
 * into a hot loop: measured, it made this file's twenty tests slower than the
 * leak did (243 s against 175 s). Keeping the cadence and dropping only the
 * *repeat* is what fixes the leak without paying for it elsewhere.
 *
 * The shim lives here, with the other jsdom gaps, rather than in the copied
 * components: it is the environment that is wrong, and the components are
 * byte-identical upstream files (decisions.md §16).
 */
const FRAME_MS = 16;
if (typeof globalThis.requestAnimationFrame === "function") {
  const frames = new Map<number, ReturnType<typeof setTimeout>>();
  let nextHandle = 1;
  Object.defineProperty(globalThis, "requestAnimationFrame", {
    configurable: true,
    writable: true,
    value: (callback: FrameRequestCallback): number => {
      const handle = nextHandle;
      nextHandle += 1;
      frames.set(
        handle,
        setTimeout(() => {
          frames.delete(handle);
          callback(performance.now());
        }, FRAME_MS),
      );
      return handle;
    },
  });
  Object.defineProperty(globalThis, "cancelAnimationFrame", {
    configurable: true,
    writable: true,
    value: (handle: number): void => {
      const timer = frames.get(handle);
      if (timer === undefined) return;
      clearTimeout(timer);
      frames.delete(handle);
    },
  });
}

if (typeof globalThis.ResizeObserver !== "function") {
  Object.defineProperty(globalThis, "ResizeObserver", {
    configurable: true,
    writable: true,
    value: class {
      observe(): void {}
      unobserve(): void {}
      disconnect(): void {}
    },
  });
}

if (typeof Element !== "undefined") {
  if (typeof Element.prototype.animate !== "function") {
    Object.defineProperty(Element.prototype, "animate", {
      configurable: true,
      writable: true,
      value: () => ({ cancel: () => {}, finished: Promise.resolve() }),
    });
  }
  if (typeof Element.prototype.getAnimations !== "function") {
    // Base UI's popups wait for their own exit animations to finish before
    // unmounting. jsdom has no Web Animations API, so an empty list tells them
    // there is nothing to wait for.
    Object.defineProperty(Element.prototype, "getAnimations", {
      configurable: true,
      writable: true,
      value: () => [],
    });
  }
  if (typeof Element.prototype.scrollIntoView !== "function") {
    Object.defineProperty(Element.prototype, "scrollIntoView", {
      configurable: true,
      writable: true,
      value: () => {},
    });
  }
}

if (typeof window !== "undefined" && typeof window.scrollTo === "function") {
  Object.defineProperty(window, "scrollTo", {
    configurable: true,
    writable: true,
    value: () => {},
  });
}

/**
 * jsdom's `Blob` predates the file-reading half of the spec: its prototype has
 * only `slice`, `size` and `type`, so `File.arrayBuffer()` does not exist.
 * Every browser has it, and the attachment upload
 * (`src/runtime/rpc/http.ts`, `encodeAttachmentUpload`) reads a dropped file
 * through it. Node's own `Blob` and `File` are the spec ones, so they stand in
 * here; nothing in the client is weakened for a gap only jsdom has.
 */
if (typeof Blob !== "undefined" && typeof Blob.prototype.arrayBuffer !== "function") {
  const { Blob: NodeBlob, File: NodeFile } = await import("node:buffer");
  Object.defineProperty(globalThis, "Blob", {
    configurable: true,
    writable: true,
    value: NodeBlob,
  });
  Object.defineProperty(globalThis, "File", {
    configurable: true,
    writable: true,
    value: NodeFile,
  });
}

export {};
