import { useEffect, useRef, useState } from "react";
import { idleVoiceState, VoiceInput } from "../lib/voiceInput.ts";

export function useVoiceInput(disabled: boolean, onTranscript: (text: string) => void) {
  const [state, setState] = useState(idleVoiceState);
  const transcriptRef = useRef(onTranscript);
  transcriptRef.current = onTranscript;
  const inputRef = useRef<VoiceInput | null>(null);

  useEffect(() => {
    const input = new VoiceInput(setState, (text) => transcriptRef.current(text));
    inputRef.current = input;
    const hide = () => { if (document.hidden) input.cancel(); };
    document.addEventListener("visibilitychange", hide);
    return () => {
      document.removeEventListener("visibilitychange", hide);
      input.dispose();
      inputRef.current = null;
    };
  }, []);

  useEffect(() => { if (disabled) inputRef.current?.cancel(); }, [disabled]);

  return {
    ...state,
    busy: state.phase !== "idle",
    toggle: () => {
      if (disabled) return;
      if (state.phase === "idle") void inputRef.current?.start();
      else inputRef.current?.stop();
    },
  };
}
