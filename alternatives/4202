import { useCallback, useEffect, useRef, useState } from "react";

import { HandsFreeInput, supportsHandsFreeInput } from "../lib/handsFreeInput.ts";
import type { ConversationSpeech } from "./useConversationSpeech.ts";

type Phase = "idle" | "starting" | "listening" | "transcribing" | "waiting";

export function useHandsFreeConversation(options: {
  speech: ConversationSpeech | undefined;
  unavailable: boolean;
  sending: boolean;
  streaming: boolean;
  onSend: ((text: string) => Promise<void> | void) | undefined;
}) {
  const [phase, setPhase] = useState<Phase>("idle");
  const [error, setError] = useState<string | null>(null);
  const input = useRef<HandsFreeInput | null>(null);
  const speaking = useRef(false);
  const latest = useRef(options);
  latest.current = options;
  const supported = options.speech?.supported === true && options.onSend !== undefined && supportsHandsFreeInput();

  const end = useCallback(() => {
    if (!input.current) return;
    input.current.dispose();
    input.current = null;
    speaking.current = false;
    latest.current.speech?.endHandsFree();
    setPhase("idle");
  }, []);

  useEffect(() => {
    const hide = () => { if (document.hidden) end(); };
    document.addEventListener("visibilitychange", hide);
    window.addEventListener("pagehide", end);
    return () => {
      document.removeEventListener("visibilitychange", hide);
      window.removeEventListener("pagehide", end);
      end();
    };
  }, [options.speech?.conversationId, end]);

  useEffect(() => {
    if (options.unavailable || !supported) end();
  }, [options.unavailable, supported, end]);

  useEffect(() => {
    if (speaking.current && options.speech?.readingMessageId) options.speech.stop();
  }, [options.speech]);

  const toggle = () => {
    if (input.current) { end(); return; }
    if (!supported || latest.current.unavailable || latest.current.sending || document.hidden) return;
    setError(null);
    setPhase("starting");
    latest.current.speech?.startHandsFree();
    const capture = new HandsFreeInput({
      onReady: () => { if (input.current === capture) setPhase("listening"); },
      onSpeechStart: () => {
        if (input.current !== capture) return;
        speaking.current = true;
        latest.current.speech?.stop();
        setPhase("listening");
      },
      onTranscribing: () => {
        if (input.current !== capture) return;
        speaking.current = false;
        setPhase("transcribing");
      },
      onTranscript: async (text) => {
        if (input.current !== capture) return;
        const current = latest.current;
        if (current.unavailable) { end(); return; }
        setPhase("waiting");
        try {
          await current.onSend?.(text);
          if (input.current === capture) setPhase("listening");
        } catch {
          if (input.current !== capture) return;
          end();
          setError("The voice message could not send. Type your message or start hands-free again.");
        }
      },
      onError: (message) => {
        if (input.current !== capture) return;
        end();
        setError(message);
      },
    });
    input.current = capture;
    void capture.start();
  };

  const status = phase === "idle" ? null : phase === "starting" ? "Preparing hands-free voice…" :
    phase === "transcribing" ? "Transcribing…" : options.speech?.readingMessageId ? "Speaking" :
      speaking.current ? "Listening" : phase === "waiting" || options.sending || options.streaming ? "Waiting for reply…" : "Listening";
  return { active: phase !== "idle", supported, error, status, toggle, end };
}
