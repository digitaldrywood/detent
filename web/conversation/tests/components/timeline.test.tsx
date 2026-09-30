// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import React from "react";

import { Timeline } from "../../src/app/components/Timeline.tsx";
import type { Message } from "../../src/contracts/index.ts";
import {
  assistantMessage,
  attentionMessage,
  conversation,
  detail,
  issueMessage,
  proposalMessage,
  question,
  userMessage,
} from "./builders.ts";

/** An unlinked chat: answered by a coordinator turn, with no issue behind it. */
const unlinked = { conversation: conversation({ work_item_id: null, work_item: null }) };

afterEach(cleanup);

function renderTimeline(
  overrides: Parameters<typeof detail>[0] = {},
  props: Partial<React.ComponentProps<typeof Timeline>> = {},
) {
  const onLoadOlder = vi.fn();
  const onRetry = vi.fn();
  const onDismiss = vi.fn();
  const utils = render(
    <Timeline
      detail={detail(overrides)}
      onLoadOlder={onLoadOlder}
      onRetry={onRetry}
      onDismiss={onDismiss}
      {...props}
    />,
  );
  return { ...utils, onLoadOlder, onRetry, onDismiss };
}

describe("Timeline", () => {
  it("renders a user bubble and an assistant turn", () => {
    renderTimeline();
    expect(screen.getAllByTestId("user-turn")).toHaveLength(1);
    expect(screen.getAllByTestId("assistant-turn")).toHaveLength(1);
  });

  it("shows the delivery state of every user message with an explanation", () => {
    renderTimeline({ messages: [userMessage({ delivery: "queued" })] });
    expect(screen.getByTestId("delivery-label").textContent).toBe("queued");
    expect(
      screen.getByTitle("Waiting for a bound runner to pick the message up."),
    ).toBeTruthy();
  });

  it("appends streaming delta text to the message the snapshot carried", () => {
    const streaming = assistantMessage({ id: "msg_stream", text: "" });
    const { container } = render(
      <Timeline
        detail={detail({
          messages: [streaming],
          deltas: { msg_stream: { parts: { 2: "world", 1: "hello " } } },
        })}
        onLoadOlder={vi.fn()}
        onRetry={vi.fn()}
        onDismiss={vi.fn()}
      />,
    );
    expect(container.textContent).toContain("hello world");
    expect(screen.getByTestId("assistant-turn")).toBeTruthy();
  });

  it("keeps an unknown send visible with a consequence sentence, not a failure", () => {
    renderTimeline({
      messages: [],
      pending: [
        {
          key: "cmd_1",
          text: "Did this land?",
          attachments: null,
          createdAt: "2026-09-09T10:00:00Z",
          messageId: null,
          expected: null,
          receiptStatus: null,
          status: "unknown",
          error: null,
          errorCode: null,
          retryable: false,
        },
      ],
    });
    expect(screen.getByTestId("delivery-label").textContent).toBe("unknown");
    expect(
      screen.getByText(
        "Delivery is uncertain. Recover history before deciding whether to send again.",
      ),
    ).toBeTruthy();
  });

  it("offers a retry that re-queues the message rather than sending a new one", () => {
    const { onRetry } = renderTimeline({
      pending: [
        {
          key: "cmd_same",
          text: "Retry me",
          attachments: null,
          createdAt: "2026-09-09T10:00:00Z",
          messageId: null,
          expected: null,
          receiptStatus: null,
          status: "failed",
          error: "The control queue is full.",
          errorCode: null,
          retryable: true,
        },
      ],
    });
    fireEvent.click(screen.getByText("Retry"));
    expect(onRetry).toHaveBeenCalledWith(expect.objectContaining({ key: "cmd_same" }));
  });

  it("offers to load older history only when there is more", () => {
    const { onLoadOlder } = renderTimeline({
      page: { hasMore: true, loadingOlder: false, oldestSeq: 7 },
    });
    fireEvent.click(screen.getByText("Load earlier messages"));
    expect(onLoadOlder).toHaveBeenCalledTimes(1);
  });

  it("renders assistant markdown as elements and never as raw HTML", () => {
    const { container } = render(
      <Timeline
        detail={detail({
          messages: [
            assistantMessage({
              text: "**bold** and <img src=x onerror=alert(1)> and [link](https://example.test)",
            }),
          ],
        })}
        onLoadOlder={vi.fn()}
        onRetry={vi.fn()}
        onDismiss={vi.fn()}
      />,
    );
    expect(container.querySelector("strong")?.textContent).toBe("bold");
    expect(container.querySelector("img")).toBeNull();
    const link = container.querySelector("a");
    expect(link?.getAttribute("rel")).toBe("noopener noreferrer");
  });

  // A runner posts its proposal as an `item` turn event, which the hub stores
  // as `role: system, kind: status` (decisions.md §9.3, §9.4). The card is
  // chosen by kind and data, so who wrote it never changes what the reader sees.
  it("renders a runner-posted proposal that arrives as a system status message", () => {
    const onCreateIssue = vi.fn();
    render(
      <Timeline
        detail={detail({
          ...unlinked,
          messages: [
            proposalMessage({
              role: "system",
              actor: { kind: "runner", principal_id: "tok_runner" },
              attempt_id: "att_1",
            }),
          ],
        })}
        onLoadOlder={vi.fn()}
        onRetry={vi.fn()}
        onDismiss={vi.fn()}
        onCreateIssue={onCreateIssue}
      />,
    );
    expect(screen.getByTestId("proposal-card")).toBeTruthy();
    expect(screen.queryByTestId("status-row")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Create linked issue" }));
    expect(onCreateIssue).toHaveBeenCalledWith(
      expect.objectContaining({ project_id: "proj_parable" }),
    );
  });

  it("reads a queued message on an unlinked chat as waiting, not as a fault", () => {
    renderTimeline({ ...unlinked, messages: [userMessage({ delivery: "queued" })] });
    expect(screen.getByTestId("delivery-label").textContent).toBe("Queued for a runner");
    expect(
      screen.getByTitle(
        "The hub is holding this until a runner picks the chat up. Nothing has failed.",
      ),
    ).toBeTruthy();
    // Nothing failed, so the chip carries no error tone.
    expect(document.querySelector(".dc-delivery-chip .dc-err")).toBeNull();
  });

  it("names a missing coordinator instead of reporting a bare failure", () => {
    renderTimeline({
      ...unlinked,
      messages: [],
      pending: [
        {
          key: "cmd_no_coordinator",
          text: "Anyone home?",
          attachments: null,
          createdAt: "2026-09-09T10:00:00Z",
          messageId: null,
          expected: null,
          receiptStatus: null,
          status: "failed",
          error: "No coordinator backend is configured.",
          errorCode: "coordinator_unavailable",
          retryable: false,
        },
      ],
    });
    expect(screen.getByTestId("delivery-label").textContent).toBe(
      "No coordinator is available for this project",
    );
    expect(
      screen.getByTitle("Enrol a runner for this project so it can answer chats."),
    ).toBeTruthy();
  });

  it("renders a coordinator proposal as a card with the handoff action", () => {
    const onCreateIssue = vi.fn();
    render(
      <Timeline
        detail={detail({ messages: [proposalMessage()] })}
        onLoadOlder={vi.fn()}
        onRetry={vi.fn()}
        onDismiss={vi.fn()}
        onCreateIssue={onCreateIssue}
      />,
    );
    expect(screen.getByTestId("proposal-card")).toBeTruthy();
    expect(
      screen.getByText("Checkout lock renewal waits on a healthy handoff"),
    ).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Create linked issue" }));
    expect(onCreateIssue).toHaveBeenCalledWith(
      expect.objectContaining({ project_id: "proj_parable" }),
    );
  });

  it("renders the issue result card with its lane and runner state", () => {
    const onOpenIssue = vi.fn();
    render(
      <Timeline
        detail={detail({ messages: [issueMessage()] })}
        onLoadOlder={vi.fn()}
        onRetry={vi.fn()}
        onDismiss={vi.fn()}
        onOpenIssue={onOpenIssue}
      />,
    );
    const card = screen.getByTestId("issue-card");
    expect(card.textContent).toContain("parable#3363");
    expect(card.textContent).toContain("Todo");
    expect(card.textContent).toContain("Waiting for a runner");
    fireEvent.click(screen.getByRole("button", { name: "Open issue" }));
    expect(onOpenIssue).toHaveBeenCalledWith("wi_3363");
  });

  it("renders attention items as issue links and nowhere else", () => {
    const onOpenIssue = vi.fn();
    const { container } = render(
      <Timeline
        detail={detail({ messages: [attentionMessage()] })}
        onLoadOlder={vi.fn()}
        onRetry={vi.fn()}
        onDismiss={vi.fn()}
        onOpenIssue={onOpenIssue}
      />,
    );
    const links = container.querySelectorAll("[data-testid='attention-list'] a");
    expect(links).toHaveLength(2);
    expect(links[0]?.getAttribute("href")).toBe("/chat/issues/wi_3363");
    expect(screen.getByText("The runner asked which renewal window to use.")).toBeTruthy();
    // One canonical rendering: the list is the message, not a second summary.
    expect(container.querySelectorAll("[data-testid='attention-list']")).toHaveLength(1);
    fireEvent.click(links[1]!);
    expect(onOpenIssue).toHaveBeenCalledWith("wi_3401");
  });

  it("anchors a question card to the message that opened it", () => {
    const anchored = question({ message_id: "msg_anchor" });
    render(
      <Timeline
        detail={detail({
          messages: [assistantMessage({ id: "msg_anchor" })],
          questions: [anchored],
        })}
        onLoadOlder={vi.fn()}
        onRetry={vi.fn()}
        onDismiss={vi.fn()}
        renderQuestion={(value) => <div data-testid="rendered-question">{value.id}</div>}
      />,
    );
    expect(screen.getByTestId("rendered-question").textContent).toBe(anchored.id);
  });

  it("continues a streamed reply from the text a snapshot already carried", () => {
    const { container } = render(
      <Timeline
        detail={detail({
          messages: [
            assistantMessage({ id: "msg_partial", text: "Reading the lease. ", delivery: "responding" }),
          ],
          deltas: { msg_partial: { parts: { 4: "Moving the renewal." } } },
        })}
        onLoadOlder={vi.fn()}
        onRetry={vi.fn()}
        onDismiss={vi.fn()}
      />,
    );
    expect(container.textContent).toContain("Reading the lease. Moving the renewal.");
  });

  it("says what an unknown send means for the reader", () => {
    renderTimeline({
      messages: [],
      pending: [
        {
          key: "cmd_2",
          text: "Did this land?",
          attachments: null,
          createdAt: "2026-09-09T10:00:00Z",
          messageId: null,
          expected: null,
          receiptStatus: null,
          status: "unknown",
          error: null,
          errorCode: null,
          retryable: false,
        },
      ],
    });
    // The promise the retry makes is the §10.3 one, in the row and on the chip.
    expect(
      screen.getByText("Retry re-queues this message once; nothing is duplicated."),
    ).toBeTruthy();
    expect(
      screen.getByTestId("delivery-label").closest(".dc-delivery-chip")?.getAttribute("title"),
    ).toContain("Retry re-queues this message once; nothing is duplicated.");
  });

  // The renderer strips the scheme rather than dropping the anchor, so the
  // property to hold is that nothing in a reply can ever carry a script URL.
  it("refuses to link a javascript: URL", () => {
    const { container } = render(
      <Timeline
        detail={detail({
          // eslint-disable-next-line no-script-url
          messages: [assistantMessage({ text: "[click](javascript:alert(1))" })],
        })}
        onLoadOlder={vi.fn()}
        onRetry={vi.fn()}
        onDismiss={vi.fn()}
      />,
    );
    for (const anchor of container.querySelectorAll("a")) {
      expect(anchor.getAttribute("href") ?? "").not.toContain("javascript");
    }

    const href = container.querySelector("a")?.getAttribute("href") ?? "";
    expect(href).toBe("");
  });

  // §10.3: the retry affordance is on hub-persisted messages too, not only on
  // the client's own outbox.
  it("offers a retry on a persisted message whose delivery was lost", () => {
    const onRetryMessage = vi.fn();
    const lost = userMessage({ id: "msg_lost", delivery: "unknown" });
    renderTimeline({ messages: [lost] }, { onRetryMessage });
    expect(screen.getByTestId("message-retry-note").textContent).toBe(
      "Retry re-queues this message once; nothing is duplicated.",
    );
    fireEvent.click(screen.getByTestId("message-retry"));
    expect(onRetryMessage).toHaveBeenCalledWith(expect.objectContaining({ id: "msg_lost" }));
  });

  it.each(["failed", "rejected"] as const)(
    "offers a retry on a %s message",
    (delivery) => {
      renderTimeline({ messages: [userMessage({ delivery })] }, { onRetryMessage: vi.fn() });
      expect(screen.getByTestId("message-retry")).toBeTruthy();
    },
  );

  it("offers no retry on a message the hub is still delivering", () => {
    renderTimeline({ messages: [userMessage({ delivery: "sent" })] }, { onRetryMessage: vi.fn() });
    expect(screen.queryByTestId("message-retry")).toBeNull();
  });

  it("offers no retry to a reader who cannot write", () => {
    renderTimeline({ messages: [userMessage({ delivery: "unknown" })] });
    expect(screen.queryByTestId("message-retry")).toBeNull();
  });

  // A streamed reply is never announced character by character; the end of it
  // is what a screen-reader user needs.
  it("announces a reply once it has finished streaming", () => {
    const streaming = assistantMessage({ id: "msg_stream", text: "", delivery: "responding" });
    const { rerender } = render(
      <Timeline
        detail={detail({ messages: [streaming], deltas: { msg_stream: { parts: { 1: "half" } } } })}
        onLoadOlder={vi.fn()}
        onRetry={vi.fn()}
        onDismiss={vi.fn()}
      />,
    );
    expect(screen.getByTestId("reply-announcement").textContent).toBe("");
    rerender(
      <Timeline
        detail={detail({
          messages: [assistantMessage({ id: "msg_stream", text: "half", delivery: "completed" })],
          deltas: {},
        })}
        onLoadOlder={vi.fn()}
        onRetry={vi.fn()}
        onDismiss={vi.fn()}
      />,
    );
    expect(screen.getByTestId("reply-announcement").textContent).toBe("Reply finished");
  });

  // §10.4: the hub's transcript-recovery notice is history, and it is a
  // sentence, not a card.
  it("renders a system status message as plain text with no card", () => {
    renderTimeline({
      messages: [
        userMessage(),
        {
          ...assistantMessage(),
          id: "msg_status",
          role: "system",
          kind: "status",
          data: {},
          text: "Provider history was not available on this runner; continuing from a transcript of the last 20 messages",
        },
      ],
    });

    const trigger = screen.getByTestId("work-log").querySelector("button");
    expect(trigger?.textContent).toContain("Worked for");
    fireEvent.click(trigger as HTMLElement);
    expect(document.body.textContent).toContain(
      "Provider history was not available on this runner",
    );
    expect(screen.queryByTestId("issue-card")).toBeNull();
    expect(screen.queryByTestId("proposal-card")).toBeNull();
  });

  describe("attachments on a user turn", () => {
    const withAttachments = (attachments: Message["attachments"]) =>
      renderTimeline({ messages: [userMessage({ attachments })] });

    it("draws an image attachment as a thumbnail in the grid", () => {
      withAttachments([
        {
          id: "att_1",
          name: "screenshot.png",
          mime: "image/png",
          size: 2048,
          url: "https://hub.test/a/att_1",
        },
      ]);
      const image = screen.getByRole("img", { name: /screenshot\.png/ });
      expect(image.getAttribute("src")).toBe("https://hub.test/a/att_1");
    });

    it("draws a file attachment as a download row named for the file", () => {
      withAttachments([
        {
          id: "att_2",
          name: "plan.md",
          mime: "text/markdown",
          size: 128,
          url: "https://hub.test/a/att_2",
        },
      ]);
      const link = screen.getByRole("link", { name: /plan\.md/ });
      expect(link.getAttribute("href")).toBe("https://hub.test/a/att_2");
      expect(link.getAttribute("download")).toBe("plan.md");
    });

    it("opens the expanded image dialog from a thumbnail", async () => {
      withAttachments([
        {
          id: "att_3",
          name: "diagram.png",
          mime: "image/png",
          size: 4096,
          url: "https://hub.test/a/att_3",
        },
      ]);
      expect(screen.queryByRole("dialog")).toBeNull();
      fireEvent.click(screen.getByRole("img", { name: /diagram\.png/ }));
      const dialog = await screen.findByRole("dialog");
      expect(within(dialog).getByRole("img").getAttribute("src")).toBe(
        "https://hub.test/a/att_3",
      );
    });
  });
});
