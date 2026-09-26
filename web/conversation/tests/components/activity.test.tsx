// @vitest-environment jsdom
//
// The issue page's activity feed: the merge, the fold and what the rows draw.
import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import attemptsFixture from "../../src/contracts/fixtures/work-attempt-list.json";
import commentsFixture from "../../src/contracts/fixtures/work-comment-list.json";
import historyFixture from "../../src/contracts/fixtures/work-history.json";
import type {
  CollaborationEvent,
  NativeAttempt,
  NativeComment,
} from "../../src/contracts/work.ts";
import {
  actorLabel,
  foldActivity,
  foldLabel,
  historySentence,
  mergeActivity,
  type ActivityRow,
} from "../../src/app/work/lib/activity.ts";
import { ActivityFeed, LiveRow } from "../../src/app/work/components/ActivityFeed.tsx";

afterEach(cleanup);

const HISTORY = (historyFixture as { items: unknown[] }).items as unknown as CollaborationEvent[];
const ATTEMPTS = (attemptsFixture as { items: unknown[] }).items as unknown as NativeAttempt[];
const COMMENTS = (commentsFixture as { items: unknown[] }).items as unknown as NativeComment[];

function row(overrides: Partial<ActivityRow> = {}): ActivityRow {
  return {
    key: "k",
    kind: "event",
    icon: "edited",
    at: 1,
    actor: "someone",
    sentence: "did a thing",
    lowValue: false,
    ...overrides,
  };
}

describe("the activity merge", () => {
  it("writes one sentence per history type and folds the ones that say nothing", () => {
    const cases = [
      { type: "issue.created", sentence: "created the issue", lowValue: false },
      { type: "change.created", sentence: "opened a change request", lowValue: false },
      { type: "run.checkpointed", sentence: "checkpointed the attempt", lowValue: true },
      { type: "comment.edited", sentence: "edited a comment", lowValue: true },
      // The log is append-only and the hub grows it: an unknown type is still
      // a row, named by its own type, rather than a dropped page.
      { type: "nothing.we.know", sentence: "recorded nothing.we.know", lowValue: true },
    ] as const;
    for (const testCase of cases) {
      const written = historySentence({
        ...(HISTORY[0] as CollaborationEvent),
        type: testCase.type,
        data: {},
      });
      expect(written.sentence, testCase.type).toBe(testCase.sentence);
      expect(written.lowValue, testCase.type).toBe(testCase.lowValue);
    }
  });

  it("names the transition it recorded", () => {
    const moved = HISTORY.find((event) => event.type === "workflow.transitioned");
    expect(historySentence(moved as CollaborationEvent).sentence).toBe(
      "moved it from Todo to In Progress",
    );
  });

  it("merges every source into one list, oldest first", () => {
    const rows = mergeActivity({
      history: HISTORY,
      attempts: ATTEMPTS,
      comments: COMMENTS,
      conversation: null,
      viewerPrincipalId: "tok_7b21",
    });
    const times = rows.map((entry) => entry.at);
    expect(times).toEqual([...times].toSorted((left, right) => left - right));

    // One claim row per attempt, and one ending row per attempt that ended.
    expect(rows.filter((entry) => entry.key.endsWith(":claimed"))).toHaveLength(ATTEMPTS.length);
    expect(
      rows.find((entry) => entry.sentence.includes("attempt 1 was interrupted")),
    ).toBeDefined();

    // Comments are cards, and the reader's own rows say "You".
    const comment = rows.find((entry) => entry.kind === "comment");
    expect(comment?.actor).toBe("You");
    expect(comment?.comment?.body).toContain("Reproduced on the mac-studio runner");
  });

  it("drops the log's row for a comment it is already drawing as a card", () => {
    const comment = COMMENTS[0] as NativeComment;
    const event = {
      ...(HISTORY[0] as CollaborationEvent),
      event_id: "evt_comment",
      type: "comment.created",
      data: { comment_id: comment.comment_id },
    };
    const withCard = mergeActivity({
      history: [event],
      attempts: [],
      comments: [comment],
      conversation: null,
      viewerPrincipalId: null,
    });
    expect(withCard.map((entry) => entry.key)).toEqual([`comment:${comment.comment_id}`]);

    // Without the comment itself — a page the reader cannot see, or a comment
    // past the page that was read — the log's row is all there is, and it stays.
    const withoutCard = mergeActivity({
      history: [event],
      attempts: [],
      comments: [],
      conversation: null,
      viewerPrincipalId: null,
    });
    expect(withoutCard.map((entry) => entry.key)).toEqual(["history:evt_comment"]);
  });

  it("reads the linked conversation's questions, steering and attachments", () => {
    const rows = mergeActivity({
      history: [],
      attempts: [],
      comments: [],
      conversation: {
        questions: [
          {
            id: "q_1",
            conversation_id: "conv_1",
            message_id: "msg_1",
            status: "answered",
            owner: { attempt_id: "att_1", turn_id: "turn_1" },
            questions: [
              { id: "p1", header: "Approach", question: "Blue or Green?", options: [], free_text: false },
            ],
            answers: {},
            answered_by: "tok_7b21",
            expires_at: null,
            created_at: "2026-09-09T10:10:00Z",
            updated_at: "2026-09-09T10:11:00Z",
          },
        ],
        messages: [
          {
            id: "msg_2",
            conversation_id: "conv_1",
            seq: 2,
            role: "user",
            kind: "text",
            text: "use the second approach",
            data: {},
            delivery: "delivered",
            attempt_id: "att_1",
            turn_id: "turn_1",
            provider_item_id: null,
            actor: { kind: "human", principal_id: "tok_7b21" },
            command_key: null,
            references: [],
            attachments: [
              { id: "a1", name: "notes.md", mime: "text/markdown", size: 10, url: "/a1" },
            ],
            created_at: "2026-09-09T10:12:00Z",
            updated_at: "2026-09-09T10:12:00Z",
          },
        ],
      },
      viewerPrincipalId: "tok_7b21",
    });
    const sentences = rows.map((entry) => entry.sentence);
    expect(sentences).toContain("asked: Blue or Green?");
    expect(sentences).toContain("answered the question");
    expect(sentences).toContain("sent the runner: use the second approach");
    expect(sentences).toContain("attached notes.md");
  });

  it("names a runner by its own id and a stranger by a shortened principal", () => {
    expect(actorLabel({ kind: "runner", principal_id: "tok_r" }, null, "rnr_mac")).toBe("rnr_mac");
    expect(actorLabel({ kind: "runner", principal_id: "tok_r" }, null, null)).toBe("The runner");
    expect(actorLabel({ kind: "human", principal_id: "me" }, "me", null)).toBe("You");
    expect(
      actorLabel({ kind: "human", principal_id: "0".repeat(40) }, null, null),
    ).toBe(`${"0".repeat(18)}…`);
  });
});

describe("the fold", () => {
  it("folds a run of low-value rows and leaves a lone one alone", () => {
    const groups = foldActivity([
      row({ key: "a" }),
      row({ key: "b", lowValue: true }),
      row({ key: "c", lowValue: true }),
      row({ key: "d" }),
      row({ key: "e", lowValue: true }),
    ]);
    expect(groups.map((group) => group.kind)).toEqual(["row", "fold", "row", "row"]);
    expect(foldLabel([row(), row()])).toBe("Show 2 events…");
    expect(foldLabel([row()])).toBe("Show 1 event…");
  });

  it("never folds a comment, however quiet its neighbours are", () => {
    const groups = foldActivity([
      row({ key: "a", lowValue: true, kind: "comment" }),
      row({ key: "b", lowValue: true }),
    ]);
    expect(groups.map((group) => group.kind)).toEqual(["row", "row"]);
  });
});

describe("the feed", () => {
  it("draws one line per event and a card with a reply for a comment", () => {
    const onReply = vi.fn();
    render(
      <ActivityFeed
        rows={[
          row({ key: "a", actor: "michael", sentence: "created the issue", at: 10 }),
          row({
            key: "c",
            kind: "comment",
            actor: "michael",
            sentence: "commented",
            at: 20,
            comment: { id: "c1", body: "Add a test too.", actor: "michael", at: "" },
          }),
        ]}
        live={null}
        liveAt={Number.MAX_SAFE_INTEGER}
        onReply={onReply}
        posting={false}
      />,
    );
    expect(screen.getAllByTestId("issue-activity-row")).toHaveLength(1);
    const card = screen.getByTestId("issue-comment");
    expect(within(card).getByTestId("issue-comment-body").textContent).toContain("Add a test too.");
    expect(within(card).getByTestId("issue-comment-reply")).not.toBeNull();
  });

  it("gives a read-only reader the comment without the reply", () => {
    render(
      <ActivityFeed
        rows={[
          row({
            key: "c",
            kind: "comment",
            at: 20,
            comment: { id: "c1", body: "Read only.", actor: "michael", at: "" },
          }),
        ]}
        live={null}
        liveAt={Number.MAX_SAFE_INTEGER}
        onReply={null}
        posting={false}
      />,
    );
    expect(screen.queryByTestId("issue-comment-reply")).toBeNull();
  });
});

describe("the live row", () => {
  it("pulses, names the runner's sentence and offers Interrupt while one runs", () => {
    const onInterrupt = vi.fn();
    render(
      <ul>
        <LiveRow
          running
          sentence="Changed only main.go."
          detail="rnr_mac · codex gpt-6-astra · attempt 2"
          elapsed="1m 08s"
          onInterrupt={onInterrupt}
          onOpenConversation={vi.fn()}
        />
      </ul>,
    );
    const live = screen.getByTestId("issue-live-row");
    expect(live.getAttribute("data-live")).toBe("true");
    expect(live.querySelector('[class*="animate-status-pulse"]')).not.toBeNull();
    expect(screen.getByTestId("live-row-elapsed").textContent).toBe("1m 08s");
    expect(screen.getByTestId("live-row-sentence").textContent).toBe("Changed only main.go.");
    expect(screen.getByTestId("live-row-interrupt")).not.toBeNull();
  });

  it("is the last turn's summary with the same link when nothing runs", () => {
    const onOpen = vi.fn();
    render(
      <ul>
        <LiveRow
          running={false}
          sentence="The issue is queued. Nothing is running yet."
          detail=""
          elapsed=""
          onInterrupt={null}
          onOpenConversation={onOpen}
        />
      </ul>,
    );
    expect(screen.getByTestId("issue-live-row").getAttribute("data-live")).toBe("false");
    expect(screen.queryByTestId("live-row-interrupt")).toBeNull();
    screen.getByTestId("view-conversation").click();
    expect(onOpen).toHaveBeenCalledTimes(1);
  });
});
