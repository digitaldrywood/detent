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
