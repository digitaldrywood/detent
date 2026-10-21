// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import React from "react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { Composer } from "../../src/app/components/Composer.tsx";
import { composerBanners } from "../../src/app/adapters/composerBanners.tsx";
import { DEFAULT_TURN_PREFERENCES } from "../../src/contracts/index.ts";
import { execution } from "./builders.ts";
import { focusComposer } from "./composerInput.ts";

afterEach(cleanup);

function mount(overrides: Partial<React.ComponentProps<typeof Composer>> = {}) {
  const onChange = vi.fn();
  render(
    <Composer
      value=""
      onChange={onChange}
      onSend={() => {}}
      sending={false}
      streaming={false}
      preferences={DEFAULT_TURN_PREFERENCES}
      onPreferencesChange={() => {}}
      {...overrides}
    />,
  );
  return { onChange };
}

describe("the banner rail Detent builds for the stack", () => {
  it("puts the execution strip on the rail as the activity row", () => {
    const items = composerBanners({
      execution: {
        execution: execution({ status: "running" }),
        controls: [],
        stale: false,
        onDismissStale: () => {},
        onInterrupt: () => {},
        onContinue: () => {},
        onRetryControl: () => {},
        onDiscardControl: () => {},
      },
    });
    expect(items).toHaveLength(1);
    // `activity` is what keeps a row attached to the card rather than behind
    // the peek cap; every other row stacks behind it.
    expect(items[0]?.priority).toBe("activity");
    expect(items[0]?.id).toBe("execution");
  });

  it("builds a thread error as an urgent row with a dismiss", () => {
    const onDismiss = vi.fn();
    const [row] = composerBanners({
      threadError: { message: "The runner died.", onDismiss },
    });
    expect(row?.priority).toBe("urgent");
    expect(row && "onDismiss" in row ? row.onDismiss : undefined).toBe(onDismiss);
  });

  it("builds nothing at all when there is nothing to say", () => {
    expect(composerBanners({})).toEqual([]);
  });

  it("keeps the rail out of the DOM when it is empty", () => {
    mount();
    expect(document.querySelector("[data-composer-banner-drawer]")).toBeNull();
  });

  it("renders the execution strip through the rail, test ids intact", () => {
    mount({
      banners: composerBanners({
        execution: {
          execution: execution({ status: "running" }),
          controls: [],
          stale: false,
          onDismissStale: () => {},
          onInterrupt: () => {},
          onContinue: () => {},
          onRetryControl: () => {},
          onDiscardControl: () => {},
        },
      }),
    });
    expect(screen.getByTestId("execution-strip")).toBeTruthy();
    expect(screen.getByTestId("execution-copy").textContent).toContain("Running");
  });

  // §10.11: the reader who may never send is told why, once, on the rail.
  it("states a blocked reason on the rail without the caller asking", () => {
    mount({ blockedReason: "You can read this chat but not send messages" });
    expect(screen.getByText("You can read this chat but not send messages")).toBeTruthy();
  });
});

describe("up-arrow prompt recall", () => {
  const history = [
    { id: "m1", role: "user", text: "first thing" },
    { id: "m2", role: "assistant", text: "a reply" },
    { id: "m3", role: "user", text: "second thing" },
  ];

  it("recalls the newest prompt into an empty composer", async () => {
    const { onChange } = mount({ history });
    await focusComposer(screen.getByTestId("composer-editor"));
    fireEvent.keyDown(screen.getByTestId("composer-editor"), { key: "ArrowUp" });
    expect(onChange).toHaveBeenLastCalledWith("second thing");
  });

  it("leaves a composer that already has text alone", async () => {
    const { onChange } = mount({ history, value: "half a sentence" });
    await focusComposer(screen.getByTestId("composer-editor"));
    fireEvent.keyDown(screen.getByTestId("composer-editor"), { key: "ArrowUp" });
    expect(onChange).not.toHaveBeenCalled();
  });

  it("skips the assistant's turns: only prompts the user typed are history", async () => {
    const { onChange } = mount({ history });
    const editor = screen.getByTestId("composer-editor");
    await focusComposer(editor);
    fireEvent.keyDown(editor, { key: "ArrowUp" });
    expect(onChange).toHaveBeenLastCalledWith("second thing");
    expect(onChange.mock.calls.flat()).not.toContain("a reply");
  });

  it("does not browse while a question is open: the prompt is the answer", async () => {
    const { onChange } = mount({
      history,
      pendingQuestion: {
        pending: { requestId: "q1", createdAt: "", questions: [], dismissible: true },
        respondingRequestIds: [],
        answers: {},
        questionIndex: 0,
        onToggleOption: () => {},
        customAnswer: "",
        onCustomAnswerChange: () => {},
        placeholder: "Type your own answer",
        acceptsCustomAnswer: true,
        onAdvance: () => {},
        onPrevious: () => {},
        onDismiss: () => {},
        action: {
          questionIndex: 0,
          isLastQuestion: true,
          canAdvance: false,
          isResponding: false,
          isComplete: false,
        },
        receipt: null,
        lockedSentence: null,
        retry: null,
      },
    });
    await focusComposer(screen.getByTestId("composer-editor"));
    fireEvent.keyDown(screen.getByTestId("composer-editor"), { key: "ArrowUp" });
    expect(onChange).not.toHaveBeenCalled();
  });
});

describe("collapse on scroll", () => {
  it("hides the footer control row when the transcript is scrolled back", () => {
    const scroller = document.createElement("div");
    document.body.append(scroller);
    Object.defineProperty(scroller, "scrollHeight", { value: 4000, configurable: true });
    Object.defineProperty(scroller, "clientHeight", { value: 600, configurable: true });
    scroller.scrollTop = 2000;
    const ref = { current: scroller as HTMLElement | null };

    mount({ scrollRef: ref });
    const chips = screen.getByTestId("composer-scope-chips");
    expect(chips.className).not.toContain("invisible");

    fireEvent.wheel(scroller, { deltaY: -40 });
    expect(screen.getByTestId("composer-scope-chips").className).toContain("invisible");

    fireEvent.focus(screen.getByTestId("composer-editor"), { bubbles: true });
    fireEvent.focusIn(screen.getByTestId("composer-editor"));
    expect(screen.getByTestId("composer-scope-chips").className).not.toContain("invisible");
    scroller.remove();
  });
});
