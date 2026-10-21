import React from "react";

import { deltaText, type ConversationDetail } from "../../runtime/state/conversationState.ts";

const STORAGE_KEY = "detent.chat.voice";
let createdConversation: string | null = null;

export function followCreatedConversation(id: string): void {
  createdConversation = id;
}

export interface ConversationSpeech {
  readonly conversationId: string;
  readonly startHandsFree: () => void;
  readonly endHandsFree: () => void;
  readonly supported: boolean;
  readonly enabled: boolean;
  readonly readingMessageId: string | null;
  readonly toggle: () => void;
  readonly stop: () => void;
  readonly prepareToSend: () => void;
}

export function useConversationSpeech(
  conversationId: string,
  detail?: ConversationDetail,
): ConversationSpeech {
  const supported = typeof window !== "undefined" &&
    typeof window.speechSynthesis?.speak === "function" &&
    typeof window.SpeechSynthesisUtterance === "function";
  const [enabled, setEnabled] = React.useState(() => {
    try { return supported && localStorage.getItem(STORAGE_KEY) === "on"; }
    catch { return false; }
  });
  const [handsFree, setHandsFree] = React.useState(false);
  const speakingEnabled = enabled || handsFree;
  const [readingMessageId, setReadingMessageId] = React.useState<string | null>(null);
  const queue = React.useRef<Array<{ id: string; utterance: SpeechSynthesisUtterance }>>([]);
  const cursor = React.useRef(new Map<string, number>());
  const suppressed = React.useRef(new Set<string>());
  const baseline = React.useRef<number | null>(null);
  const latest = React.useRef(detail);
  latest.current = detail;

  const cancel = React.useCallback(() => {
    for (const { utterance } of queue.current) {
      utterance.onend = null;
      utterance.onerror = null;
    }
    queue.current = [];
    if (supported) window.speechSynthesis.cancel();
    setReadingMessageId(null);
  }, [supported]);

  const stop = React.useCallback(() => {
    for (const message of latest.current?.messages ?? []) suppressed.current.add(message.id);
    cancel();
  }, [cancel]);

  const unlock = React.useCallback(() => {
    if (!supported) return;
    const utterance = new SpeechSynthesisUtterance(" ");
    utterance.volume = 0;
    window.speechSynthesis.speak(utterance);
  }, [supported]);

  const toggle = React.useCallback(() => {
    stop();
    const next = !enabled;
    if (next) unlock();
    setEnabled(next);
    try { localStorage.setItem(STORAGE_KEY, next ? "on" : "off"); } catch {}
  }, [enabled, stop, unlock]);

  const prepareToSend = React.useCallback(() => {
    stop();
    if (speakingEnabled) unlock();
  }, [speakingEnabled, stop, unlock]);

  const startHandsFree = React.useCallback(() => {
    stop();
    unlock();
    setHandsFree(true);
  }, [stop, unlock]);
  const endHandsFree = React.useCallback(() => {
    stop();
    setHandsFree(false);
  }, [stop]);

  React.useEffect(() => {
    setHandsFree(false);
    baseline.current = null;
    cursor.current.clear();
    suppressed.current.clear();
    const leave = () => stop();
    window.addEventListener("pagehide", leave);
    return () => {
      window.removeEventListener("pagehide", leave);
      cancel();
    };
  }, [conversationId, cancel, stop]);

  React.useEffect(() => {
    if (detail === undefined) return;
    if (baseline.current === null) {
      baseline.current = createdConversation === conversationId ? 0 :
        Math.max(0, ...detail.messages.map((message) => message.seq));
      if (createdConversation === conversationId) createdConversation = null;
    }
    for (const message of detail.messages) {
      const text = message.text + deltaText(detail.deltas[message.id]);
      if (!speakingEnabled || !supported || message.seq <= baseline.current ||
        suppressed.current.has(message.id) || message.role !== "assistant" || message.kind !== "text") {
        cursor.current.set(message.id, text.length);
        continue;
      }
      const start = cursor.current.get(message.id) ?? 0;
      const complete = message.delivery !== "responding" && detail.deltas[message.id] === undefined;
      for (const segment of speechSegments(text.slice(start), complete)) {
        cursor.current.set(message.id, start + segment.end);
        if (!segment.text) continue;
        const utterance = new SpeechSynthesisUtterance(segment.text);
        queue.current.push({ id: message.id, utterance });
        const finish = () => {
          queue.current = queue.current.filter((entry) => entry.utterance !== utterance);
          setReadingMessageId(queue.current[0]?.id ?? null);
        };
        utterance.onend = finish;
        utterance.onerror = (event) => {
          if (event.error === "canceled" || event.error === "interrupted") finish();
          else stop();
        };
        setReadingMessageId(queue.current[0]?.id ?? null);
        window.speechSynthesis.speak(utterance);
      }
    }
  }, [conversationId, detail, speakingEnabled, supported, stop]);

  return React.useMemo(() => ({
    conversationId, startHandsFree, endHandsFree, supported, enabled: speakingEnabled, readingMessageId, toggle, stop, prepareToSend,
  }), [conversationId, startHandsFree, endHandsFree, supported, speakingEnabled, readingMessageId, toggle, stop, prepareToSend]);
}

function speechSegments(text: string, complete: boolean): Array<{ text: string; end: number }> {
  const segments: Array<{ text: string; end: number }> = [];
  let spoken = "";
  const emit = (end: number) => {
    segments.push({ text: spoken.replace(/[*_#>~|]/g, "").replace(/\s+/g, " ").trim(), end });
    spoken = "";
  };
  let index = 0;
  while (index < text.length) {
    const rest = text.slice(index);
    const lineStart = index === 0 || text[index - 1] === "\n";
    const container = lineStart ? speechContainerContent(rest) : { content: rest, quoteDepth: 0 };
    const fence = lineStart ? /^ {0,3}(`{3,}|~{3,})[^\n]*(?:\n|$)/.exec(container.content) : null;
    if (fence) {
      const marker = fence[1]!;
      const openingEnd = rest.length - container.content.length + fence[0].length;
      const closing = new RegExp(`^[\\t ]*${marker[0]}{${marker.length},}[^\\S\\n]*(?:\\n|$)`);
      let closingEnd: number | null = null;
      for (const line of rest.slice(openingEnd).matchAll(/[^\n]*(?:\n|$)/g)) {
        const candidate = speechContainerContent(line[0], false);
        if (candidate.quoteDepth === container.quoteDepth && closing.test(candidate.content)) {
          closingEnd = openingEnd + line.index + line[0].length;
          break;
        }
      }
      if (closingEnd === null && !complete) break;
      if (spoken.trim()) emit(index);
      index += closingEnd ?? rest.length;
      spoken = "code block";
      emit(index);
      continue;
    }
    const indented = lineStart ? /^(?:(?: {4}|\t)[^\n]*(?:\n|$))+/.exec(rest) : null;
    if (indented) {
      if (indented[0].length === rest.length && !complete) break;
      if (spoken.trim()) emit(index);
      index += indented[0].length;
      spoken = "code block";
      emit(index);
      continue;
    }
    const code = /^(`+)/.exec(rest);
    if (code) {
      const end = rest.indexOf(code[1]!, code[1]!.length);
      if (end < 0 && !complete) break;
      index += end < 0 ? rest.length : end + code[1]!.length;
      spoken += " code ";
      continue;
    }
    if (/^!?\[/.test(rest)) {
      const link = /^(!?)\[([^\]]*)\]\(/.exec(rest);
      if (link) {
        let end = link[0].length;
        let depth = 1;
        while (end < rest.length && depth > 0) {
          const char = rest[end]!;
          end += 1;
          if (char === "\\") end = Math.min(end + 1, rest.length);
          else if (char === "(") depth += 1;
          else if (char === ")") depth -= 1;
        }
        if (depth === 0) {
          if (!link[1]) spoken += link[2]!.replace(/(?:https?:\/\/|www\.)\S+/gi, "");
          index += end;
          continue;
        }
      }
      if (!complete) break;
    }
    const url = /^(?:[a-z][a-z\d+.-]*:\/\/|www\.)[^\s<>]*/i.exec(rest);
    if (url) {
      if (url[0].length === rest.length && !complete) break;
      spoken += " ";
      index += url[0].length;
      continue;
    }
    if (rest[0] === "<") {
      const end = rest.indexOf(">");
      if (end < 0 && !complete) break;
      index += end < 0 ? rest.length : end + 1;
      continue;
    }
    const char = text[index]!;
    spoken += char;
    index += 1;
    if (/[.!?。！？]/.test(char) && (index === text.length ? complete : /\s/.test(text[index]!))) emit(index);
  }
  if (complete && index === text.length) emit(index);
  return segments;
}

function speechContainerContent(text: string, lists = true): { content: string; quoteDepth: number } {
  let content = text;
  let quoteDepth = 0;
  while (true) {
    const quote = /^ {0,3}>[\t ]?/.exec(content);
    const prefix = quote ?? (lists ? /^ {0,3}(?:[-+*]|\d{1,9}[.)])[\t ]+/.exec(content) : null);
    if (prefix === null) return { content, quoteDepth };
    if (quote !== null) quoteDepth += 1;
    content = content.slice(prefix[0].length);
  }
}
