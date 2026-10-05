import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import * as Effect from "effect/Effect";
import { afterEach, beforeEach, expect, it, vi } from "vitest";

import { composerText, typeInComposer } from "./composerInput.ts";

import fixture from "../../src/contracts/fixtures/conversation-private.json";
import type {
  Conversation,
  ConversationListResponse,
} from "../../src/contracts/conversation.ts";
import {
  IssueAskPanel,
  useIssueAsk,
} from "../../src/app/work/IssueAsk.tsx";

const mock = vi.hoisted(() => ({
  create: vi.fn(),
  refresh: vi.fn(),
  list: vi.fn(),
  open: vi.fn(),
  navigate: vi.fn(),
}));
const client = {
  createConversation: mock.create,
  refreshList: mock.refresh,
  http: { listProjectConversations: mock.list },
};
vi.mock("@effect/atom-react", () => ({ useAtomSet: (atom: unknown) => atom }));
vi.mock("../../src/app/client.ts", async (original) => ({
  ...(await original<typeof import("../../src/app/client.ts")>()),
  useClient: () => client,
}));
vi.mock("../../src/app/components/ChatWorkspace.tsx", () => ({
  useWorkspacePanel: () => ({ openAsk: mock.open }),
}));
vi.mock("@tanstack/react-router", () => ({ useNavigate: () => mock.navigate }));
vi.mock("../../src/app/App.tsx", () => ({
  ConversationView: ({ conversationId }: { conversationId: string }) => (
    <div data-testid="selected-chat">{conversationId}</div>
  ),
}));

const thread = (id: string, created_at: string): Conversation =>
  ({
    ...fixture,
    id,
    created_at,
    subject_work_item_id: "wi_subject",
  }) as Conversation;
function View() {
  const ask = useIssueAsk("project", "wi_subject");
  return <IssueAskPanel ask={ask} projectId="project" identifier="#28" canWrite />;
}
afterEach(cleanup);
beforeEach(() => {
  vi.clearAllMocks();
  mock.list.mockImplementation(() =>
    Effect.succeed({ conversations: [], next_cursor: null }),
  );
  mock.create.mockResolvedValue({
    _tag: "Success",
    success: { conversation: thread("conv_new", "2026-10-02T00:00:00Z") },
  });
  mock.refresh.mockResolvedValue(undefined);
});

it("shows suggestions only before the first question and sends the split prompt", async () => {
  render(<View />);
  await waitFor(() => expect(screen.queryByRole("status")).toBeNull());
  const suggestions = within(screen.getByTestId("issue-ask-panel"));
  const labels = [
    "Why is this Blocked?",
    "Summarize the history",
    "What is left before it can start?",
    "Split into smaller issues",
  ];
  expect(
    suggestions.getAllByRole("button")
      .filter((button) => button.textContent !== "Ask" && button.textContent !== "New question")
      .map((button) => button.textContent),
  ).toEqual(labels);
  fireEvent.click(
    suggestions.getByRole("button", { name: "Split into smaller issues" }),
  );
  await screen.findByTestId("selected-chat");
  expect(mock.create).toHaveBeenCalledOnce();
  expect(mock.create.mock.calls[0]?.[0]).toMatchObject({
    projectId: "project",
    subjectWorkItemId: "wi_subject",
    firstMessage: {
      text: "Use the split-issue skill to break this issue into smaller issues that can each land on their own. Wire up the dependencies so independent pieces can run in parallel, and show me the whole split as one proposal so I can confirm it once.",
    },
  });
  expect(screen.queryByRole("button", { name: "Split into smaller issues" })).toBeNull();
});

it("keeps the question and create keys on retry and sends the subject with the first message", async () => {
  mock.create.mockResolvedValueOnce({
    _tag: "Failure",
    failure: { message: "Try again" },
  });
  render(<View />);
  await waitFor(() => expect(screen.queryByRole("status")).toBeNull());
  const input = screen.getByRole("textbox", { name: "Ask message" });
  await typeInComposer(input, "Why is this Blocked?");
  fireEvent.submit(input.closest("form")!);
  await screen.findByRole("alert");
  expect(composerText(input)).toBe("Why is this Blocked?");
  fireEvent.submit(input.closest("form")!);
  await screen.findByTestId("selected-chat");
  const [first, second] = mock.create.mock.calls.map((call) => call[0]);
  expect(first).toEqual(second);
  expect(second.subjectWorkItemId).toBe("wi_subject");
  expect(second.firstMessage.text).toBe("Why is this Blocked?");
  expect(mock.refresh).toHaveBeenCalledOnce();
});

it("sorts all subject pages newest first and opens the same thread in Chat", async () => {
  const older = thread("conv_older", "2026-09-01T00:00:00Z");
  const newer = thread("conv_newer", "2026-10-01T00:00:00Z");
  mock.list.mockImplementation(({ cursor }: { cursor?: string }) =>
    Effect.succeed({
      conversations: cursor == null ? [older] : [newer],
      next_cursor: cursor == null ? "second" : null,
    } satisfies ConversationListResponse),
  );
  render(<View />);
  await waitFor(() =>
    expect(screen.getByTestId("selected-chat").textContent).toBe(newer.id),
  );
  const options = within(
    screen.getByRole("combobox", { name: "Issue chats" }),
  ).getAllByRole("option");
  expect(
    options.slice(1).map((option) => option.getAttribute("value")),
  ).toEqual([newer.id, older.id]);
  expect(mock.list.mock.calls[0]?.[0].subjectWorkItemId).toBe("wi_subject");
  fireEvent.click(screen.getByRole("button", { name: "Open in Chat" }));
  expect(mock.navigate).toHaveBeenCalledWith({
    to: "/chat/c/$conversationId",
    params: { conversationId: newer.id },
  });
});

it("starts a fresh subject thread without suggestions while keeping prior threads selectable", async () => {
  mock.list.mockImplementation(() =>
    Effect.succeed({
      conversations: [thread("conv_old", "2026-09-01T00:00:00Z")],
      next_cursor: null,
    }),
  );
  render(<View />);
  await screen.findByTestId("selected-chat");
  fireEvent.click(screen.getByRole("button", { name: "New question" }));
  const panel = screen.getByTestId("issue-ask-panel");
  expect(within(panel).queryByRole("button", { name: "Summarize the history" })).toBeNull();
  const input = within(panel).getByRole("textbox", { name: "Ask message" });
  await typeInComposer(input, "Summarize the history");
  fireEvent.submit(input.closest("form")!);
  await waitFor(() =>
    expect(screen.getByTestId("selected-chat").textContent).toBe("conv_new"),
  );
  expect(mock.create.mock.calls[0]?.[0].firstMessage.text).toBe(
    "Summarize the history",
  );
  expect(
    within(screen.getByRole("combobox", { name: "Issue chats" })).getAllByRole(
      "option",
    ),
  ).toHaveLength(3);
});
