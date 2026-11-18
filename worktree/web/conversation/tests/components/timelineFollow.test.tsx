// @vitest-environment jsdom
import { cleanup, fireEvent, render } from "@testing-library/react";
import React from "react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { Timeline } from "../../src/app/components/Timeline.tsx";
import { assistantMessage, detail, userMessage } from "./builders.ts";

interface ListState {
  contentLength: number;
  scroll: number;
  scrollLength: number;
}

const list = vi.hoisted(() => ({
  state: { contentLength: 0, scroll: 0, scrollLength: 0 } as ListState,
  props: {} as Record<string, unknown>,
  scrollToEnd: null as unknown as ReturnType<typeof vi.fn>,
}));

vi.mock("@legendapp/list/react", async () => {
  const { useImperativeHandle } = await import("react");
  const { vi: mocks } = await import("vitest");
  list.scrollToEnd = mocks.fn(() => {
    list.state.scroll = list.state.contentLength - list.state.scrollLength;
    return Promise.resolve();
  });
  const LegendList = (props: Record<string, unknown> & {
    data: Array<{ id: string }>;
    keyExtractor: (item: { id: string }) => string;
    renderItem: (args: { item: { id: string } }) => React.ReactNode;
    ref?: React.Ref<unknown>;
  }) => {
    list.props = props;
    useImperativeHandle(props.ref, () => ({
      getState: () => ({ ...list.state }),
      scrollToEnd: list.scrollToEnd,
      getScrollableNode: () => null,
    }));
    return (
      <div data-testid="legend-list">
        {props.data.map((item) => (
          <div key={props.keyExtractor(item)}>{props.renderItem({ item })}</div>
        ))}
      </div>
    );
  };
  return { LegendList };
});

afterEach(cleanup);

const noop = () => {};

function renderAt(state: ListState) {
  list.state = { ...state };
  const first = detail({
    messages: [userMessage({ id: "msg_user_1" }), assistantMessage({ id: "msg_reply_1" })],
  });
  const utils = render(
    <Timeline detail={first} onLoadOlder={noop} onRetry={noop} onDismiss={noop} />,
  );
  (list.props.onScroll as () => void)();
  list.scrollToEnd.mockClear();
  const send = () =>
    utils.rerender(
      <Timeline
        detail={detail({
          messages: [
            userMessage({ id: "msg_user_1" }),
            assistantMessage({ id: "msg_reply_1" }),
            userMessage({ id: "msg_user_2", text: "Inspect workflow policy again." }),
            assistantMessage({ id: "msg_reply_2", text: "", delivery: "responding" }),
          ],
        })}
        onLoadOlder={noop}
        onRetry={noop}
        onDismiss={noop}
      />,
    );
  return { ...utils, send };
}

function streamReply(growth: number) {
  list.state.contentLength += growth;
  (list.props.onItemSizeChanged as () => void)();
}

const atEnd = { contentLength: 1200, scroll: 600, scrollLength: 600 };

describe("Timeline own-turn follow", () => {
  it("keeps a reader who sent from the end pinned through a long streamed reply", () => {
    const { send } = renderAt(atEnd);
    send();
    expect(list.scrollToEnd).toHaveBeenCalled();
    expect(list.props.maintainVisibleContentPosition).toBe(false);
    for (const growth of [400, 2500, 900]) {
      list.scrollToEnd.mockClear();
      streamReply(growth);
      expect(list.scrollToEnd).toHaveBeenCalledTimes(1);
      expect(list.state.contentLength - list.state.scroll - list.state.scrollLength).toBe(0);
    }
  });

  it("stops following once the reader scrolls up by hand", () => {
    const { send, getByTestId } = renderAt(atEnd);
    send();
    const viewport = getByTestId("legend-list").closest("[data-assistant-citation-viewport]");
    fireEvent.wheel(viewport as Element, { deltaY: -120 });
    list.state.scroll -= 300;
    list.scrollToEnd.mockClear();
    streamReply(1500);
    expect(list.scrollToEnd).not.toHaveBeenCalled();
    expect(list.props.maintainVisibleContentPosition).not.toBe(false);
  });

  it("leaves a reader who was reading history where they are", () => {
    const { send } = renderAt({ contentLength: 6000, scroll: 0, scrollLength: 600 });
    send();
    streamReply(2000);
    expect(list.scrollToEnd).not.toHaveBeenCalled();
    expect(list.props.maintainVisibleContentPosition).not.toBe(false);
  });
});
