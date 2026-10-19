// @vitest-environment jsdom
//
// The issue page's activity feed: the merge, the fold and what the rows draw.
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

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
import { WorkApiError } from "../../src/app/work/lib/workHttp.ts";
import { useActivityCitation } from "../../src/app/work/lib/useActivityCitation.ts";
import { ActivityFeed, LiveRow } from "../../src/app/work/components/ActivityFeed.tsx";
import { ClientContext } from "../../src/app/client.ts";
import type { ConversationClient } from "../../src/runtime/bootstrap.ts";

beforeEach(() => {
  if (typeof globalThis.PointerEvent === "undefined") vi.stubGlobal("PointerEvent", globalThis.MouseEvent);
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); });

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
      { type: "issue.edited", data: { operation: "archive", fields: ["archived"], reason_detail: "Archived automatically after 30 days" }, sentence: "Archived automatically after 30 days", lowValue: false },
      { type: "issue.edited", data: { operation: "archive", fields: ["archived"] }, sentence: "archived the issue", lowValue: false },
      { type: "issue.edited", data: { operation: "restore", fields: ["archived"] }, sentence: "restored the issue", lowValue: false },
      // The log is append-only and the hub grows it: an unknown type is still
      // a row, named by its own type, rather than a dropped page.
      { type: "nothing.we.know", sentence: "recorded nothing.we.know", lowValue: true },
    ] as const;
    for (const testCase of cases) {
      const written = historySentence({
        ...(HISTORY[0] as CollaborationEvent),
        type: testCase.type,
        data: "data" in testCase ? testCase.data : {},
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
    expect(actorLabel({ kind: "integration", principal_id: "auto_archive" }, null, null)).toBe("Detent");
    expect(actorLabel({ kind: "runner", principal_id: "tok_r" }, null, null)).toBe("tok_r");
    expect(
      actorLabel({ kind: "runner", principal_id: "runner_be4aaeec1c424bb6afbe94aea6d605d6" }, null, null),
    ).toBe("runner_be4aaeec");
    expect(actorLabel({ kind: "human", principal_id: "me" }, "me", null)).toBe("You");
    expect(actorLabel({ kind: "human", principal_id: "me", agent: "Luna (gpt-6.1-sol)" }, "me", null)).toBe("You via Luna (gpt-6.1-sol)");
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
  async function openCommentAction(user: ReturnType<typeof userEvent.setup>, action: "Edit" | "Delete") {
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Comment actions" }));
    await user.keyboard("{Enter}");
    await waitFor(() => expect(document.activeElement).toBe(screen.getByRole("menuitem", { name: "Edit" })));
    await user.keyboard(action === "Edit" ? "{Home}" : "{End}");
    expect(document.activeElement).toBe(screen.getByRole("menuitem", { name: action }));
    await user.keyboard("{Enter}");
  }

  function commentRows() {
    return mergeActivity({ history: [], attempts: [], conversation: null, viewerPrincipalId: "viewer", comments: [
      { ...COMMENTS[0]!, comment_id: "own", actor: { kind: "human", principal_id: "viewer" }, edited_by: undefined, body: "Original comment", revision: "1" },
      { ...COMMENTS[0]!, comment_id: "reply", actor: { kind: "human", principal_id: "other" }, edited_by: undefined, body: "Another user's reply", revision: "1" },
    ] });
  }

  it("edits only owned comments by keyboard, cancels drafts, keeps failed edits, and returns focus", async () => {
    const user = userEvent.setup();
    const rows = commentRows();
    const onChange = vi.fn().mockRejectedValueOnce(new Error("Save unavailable")).mockRejectedValueOnce(new WorkApiError({ status: 409, code: "revision_conflict", message: "Comment changed in another session" })).mockResolvedValueOnce(undefined);
    const view = render(<ActivityFeed rows={rows} live={null} liveAt={Infinity} onReply={null} posting={false} onChangeComment={onChange} />);
    expect(screen.getAllByRole("button", { name: "Comment actions" })).toHaveLength(1);
    await user.tab();
    await openCommentAction(user, "Edit");
    expect(document.activeElement).toBe(screen.getByRole("textbox", { name: "Edit comment" }));
    expect((screen.getByRole("textbox", { name: "Edit comment" }) as HTMLTextAreaElement).value).toBe("Original comment");
    await user.keyboard(" cancelled");
    await user.tab();
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Attach files" }));
    await user.tab();
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Save" }));
    await user.tab();
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Cancel" }));
    await user.keyboard("{Enter}");
    expect(onChange).not.toHaveBeenCalled();
    expect(screen.getByText("Original comment")).toBeTruthy();
    await openCommentAction(user, "Edit");
    await user.keyboard("{Control>}a{/Control}Corrected comment");
    await user.tab();
    await user.tab();
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Save" }));
    await user.keyboard("{Enter}");
    expect((await screen.findByRole("alert")).textContent).toContain("Save unavailable");
    expect((screen.getByRole("textbox", { name: "Edit comment" }) as HTMLTextAreaElement).value).toBe("Corrected comment");
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Save" }));
    await user.keyboard("{Enter}");
    expect((await screen.findByRole("alert")).textContent).toContain("Comment changed in another session");
    expect(onChange).toHaveBeenLastCalledWith(rows[0]!.comment, "Corrected comment", false);
    const refreshed = rows.map((entry, index) => index === 0 ? { ...entry, comment: { ...entry.comment!, body: "Changed in another session", revision: "2" } } : entry);
    view.rerender(<ActivityFeed rows={refreshed} live={null} liveAt={Infinity} onReply={null} posting={false} onChangeComment={onChange} />);
    expect((screen.getByRole("textbox", { name: "Edit comment" }) as HTMLTextAreaElement).value).toBe("Corrected comment");
    expect(screen.getByRole("alert").textContent).toContain("Comment changed in another session");
    await user.keyboard("{Enter}");
    expect(onChange).toHaveBeenLastCalledWith(refreshed[0]!.comment, "Corrected comment", false);
    const edited = rows.map((entry, index) => index === 0 ? { ...entry, comment: { ...entry.comment!, body: "Corrected comment", edited: true } } : entry);
    view.rerender(<ActivityFeed rows={edited} live={null} liveAt={Infinity} onReply={null} posting={false} onChangeComment={onChange} />);
    expect(await screen.findByText("Corrected comment")).toBeTruthy();
    expect(screen.getByText(/· edited/)).toBeTruthy();
    expect(screen.getByText("Another user's reply")).toBeTruthy();
    await waitFor(() => expect(document.activeElement).toBe(screen.getByRole("button", { name: "Comment actions" })));
  });

  it("confirms deletion by keyboard, preserves cancelled or failed deletes, keeps later replies, and returns focus", async () => {
    const user = userEvent.setup();
    const rows = commentRows();
    const onChange = vi.fn().mockRejectedValueOnce(new Error("Delete unavailable")).mockRejectedValueOnce(new WorkApiError({ status: 409, code: "revision_conflict", message: "Comment changed in another session" })).mockResolvedValueOnce(undefined);
    const props = { rows, live: null, liveAt: Infinity, onReply: null, posting: false, onChangeComment: onChange };
    const view = render(<ActivityFeed {...props} />);
    await user.tab();
    await openCommentAction(user, "Delete");
    await waitFor(() => expect(document.activeElement).toBe(within(screen.getByRole("alertdialog")).getByRole("button", { name: "Cancel" })));
    await user.tab();
    expect(document.activeElement).toBe(within(screen.getByRole("alertdialog")).getByRole("button", { name: "Delete" }));
    await user.tab({ shift: true });
    expect(document.activeElement).toBe(within(screen.getByRole("alertdialog")).getByRole("button", { name: "Cancel" }));
    await user.keyboard("{Enter}");
    expect(onChange).not.toHaveBeenCalled();
    expect(screen.getByText("Original comment")).toBeTruthy();
    await waitFor(() => expect(document.activeElement).toBe(screen.getByRole("button", { name: "Comment actions" })));
    await openCommentAction(user, "Delete");
    await waitFor(() => expect(document.activeElement).toBe(within(screen.getByRole("alertdialog")).getByRole("button", { name: "Cancel" })));
    await user.tab();
    await user.keyboard("{Enter}");
    expect((await screen.findByRole("alert")).textContent).toContain("Delete unavailable");
    expect(screen.getByText("Original comment")).toBeTruthy();
    expect(document.activeElement).toBe(within(screen.getByRole("alertdialog")).getByRole("button", { name: "Delete" }));
    await user.keyboard("{Enter}");
    expect((await screen.findByRole("alert")).textContent).toContain("Comment changed in another session");
    expect(onChange).toHaveBeenLastCalledWith(rows[0]!.comment, "", true);
    const refreshed = rows.map((entry, index) => index === 0 ? { ...entry, comment: { ...entry.comment!, body: "Changed in another session", revision: "2" } } : entry);
    view.rerender(<ActivityFeed {...props} rows={refreshed} />);
    expect(screen.getByText("Changed in another session")).toBeTruthy();
    expect(screen.getByRole("alert").textContent).toContain("Comment changed in another session");
    await user.keyboard("{Enter}");
    expect(onChange).toHaveBeenLastCalledWith(refreshed[0]!.comment, "", true);
    view.rerender(<ActivityFeed {...props} rows={rows.map((entry, index) => index === 0 ? { ...entry, comment: { ...entry.comment!, body: "", deleted: true } } : entry)} />);
    expect(await screen.findByText("This comment was deleted")).toBeTruthy();
    expect(screen.queryByText("Original comment")).toBeNull();
    expect(screen.getByText("Another user's reply")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Comment actions" })).toBeNull();
    await waitFor(() => expect(document.activeElement).toBe(screen.getByText("This comment was deleted").closest("article")));
  });

  it("allows deletion after cancelling an edit with an attachment upload still pending", async () => {
    const user = userEvent.setup();
    let finishUpload: (response: Response) => void = () => {};
    const upload = new Promise<Response>((resolve) => { finishUpload = resolve; });
    const fetch = vi.spyOn(globalThis, "fetch").mockReturnValue(upload);
    const onChange = vi.fn().mockResolvedValue(undefined);
    const rows = commentRows();
    const client = { http: { origin: "http://example.test", apiBase: "/api/v2/organizations/org_example", csrfToken: "example" } } as ConversationClient;
    render(<ClientContext.Provider value={client}><ActivityFeed projectId="prj_example" rows={rows} live={null} liveAt={Infinity} onReply={null} posting={false} onChangeComment={onChange} /></ClientContext.Provider>);
    try {
      fireEvent.click(screen.getByRole("button", { name: "Comment actions" }));
      await user.click(screen.getByRole("menuitem", { name: "Edit" }));
      await user.upload(screen.getByTestId("attachment-input"), new File(["sample"], "sample.txt", { type: "text/plain" }));
      expect(fetch).toHaveBeenCalledTimes(1);
      expect((screen.getByRole("button", { name: "Save" }) as HTMLButtonElement).disabled).toBe(true);
      await user.click(screen.getByRole("button", { name: "Cancel" }));
      expect(screen.getByText("Original comment")).toBeTruthy();
      fireEvent.click(screen.getByRole("button", { name: "Comment actions" }));
      await user.click(screen.getByRole("menuitem", { name: "Delete" }));
      await user.click(within(screen.getByRole("alertdialog")).getByRole("button", { name: "Delete" }));
      await waitFor(() => expect(onChange).toHaveBeenCalledWith(rows[0]!.comment, "", true));
    } finally {
      await act(async () => finishUpload(Response.json({ id: "att_example", project_id: "prj_example", name: "sample.txt", content_type: "text/plain", size: 6, width: 0, height: 0 })));
    }
  });

  it("draws one line per event and a card with a reply for a comment", () => {
    const onReply = vi.fn();
    render(
      <ActivityFeed
        rows={[
          row({ key: "a", actor: "example-owner", sentence: "created the issue", at: 10 }),
          row({
            key: "c",
            kind: "comment",
            actor: "example-owner",
            sentence: "commented",
            at: 20,
            comment: { id: "c1", body: "Add a test too.", actor: "example-owner", at: "" },
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
            comment: { id: "c1", body: "Read only.", actor: "example-owner", at: "" },
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

it("reveals and highlights a cited folded event and leaves unrelated links alone", () => {
  function CitedFeed() {
    const highlightedId = useActivityCitation("wi_subject");
    return <><a href="#issue-history-evt_old">Lane history</a><a href="#unrelated">Other section</a><ActivityFeed
      rows={[row({ key: "history:evt_old", lowValue: true, reason: "approval missing" }), row({ key: "history:evt_next", lowValue: true })]}
      live={null} liveAt={Number.MAX_SAFE_INTEGER} onReply={null} posting={false} highlightedId={highlightedId}
    /></>;
  }
  window.history.replaceState(null, "", "/work/i/wi_subject");
  render(<CitedFeed />);
  expect(screen.getByTestId("activity-fold")).not.toBeNull();
  fireEvent.click(screen.getByRole("link", { name: "Lane history" }));
  const target = document.getElementById("issue-history-evt_old");
  expect(target?.getAttribute("data-citation-highlight")).toBe("true");
  expect(target?.textContent).toContain("approval missing");
  expect(screen.queryByTestId("activity-fold")).toBeNull();
  expect(screen.getByRole("link", { name: "Other section" }).getAttribute("href")).toBe("#unrelated");
  window.history.replaceState(null, "", "/");
});
