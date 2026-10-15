// Small builders for component tests, seeded from the shared contract
// fixtures so the tests exercise realistic payloads.
import conversationFixture from "../../src/contracts/fixtures/conversation.json";
import assistantFixture from "../../src/contracts/fixtures/message-assistant.json";
import attentionFixture from "../../src/contracts/fixtures/message-attention.json";
import issueFixture from "../../src/contracts/fixtures/message-issue.json";
import proposalFixture from "../../src/contracts/fixtures/message-proposal.json";
import questionFixture from "../../src/contracts/fixtures/question.json";
import multipleQuestionFixture from "../../src/contracts/fixtures/question-multiple.json";
import userFixture from "../../src/contracts/fixtures/message-user.json";
import type { Conversation, Execution, Message, Question } from "../../src/contracts/index.ts";
import type { ConversationDetail } from "../../src/runtime/state/conversationState.ts";

export function conversation(overrides: Partial<Conversation> = {}): Conversation {
  return { ...(conversationFixture as Conversation), ...overrides };
}

export function userMessage(overrides: Partial<Message> = {}): Message {
  return { ...(userFixture as Message), ...overrides };
}

export function assistantMessage(overrides: Partial<Message> = {}): Message {
  return { ...(assistantFixture as Message), ...overrides };
}

export function proposalMessage(overrides: Partial<Message> = {}): Message {
  return { ...(proposalFixture as Message), ...overrides };
}

export function issueMessage(overrides: Partial<Message> = {}): Message {
  return { ...(issueFixture as Message), ...overrides };
}

export function attentionMessage(overrides: Partial<Message> = {}): Message {
  return { ...(attentionFixture as Message), ...overrides };
}

export function question(overrides: Partial<Question> = {}): Question {
  return { ...(questionFixture as Question), ...overrides };
}

export function multiQuestion(overrides: Partial<Question> = {}): Question {
  return { ...(multipleQuestionFixture as Question), ...overrides };
}

export function execution(overrides: Partial<Execution> = {}): Execution {
  return { ...(conversationFixture as Conversation).execution, ...overrides };
}

export function detail(overrides: Partial<ConversationDetail> = {}): ConversationDetail {
  return {
    conversation: conversation(),
    messages: [userMessage(), assistantMessage()],
    deltas: {},
    questions: [],
    receipts: {},
    pending: [],
    controls: [],
    staleExecution: false,
    page: { hasMore: false, loadingOlder: false, oldestSeq: 7 },
    ...overrides,
  };
}
