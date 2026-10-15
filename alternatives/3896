// Auto-follow for the transcript.
//
// The threshold and the "stop following once the user scrolls away" rule come
// from the conversation POC (`src/app/main.tsx`, `follow.current`), which the
// design inventory names as the source for A.10. The return-to-latest button
// is new: the POC tracked the state but showed no control.
import React from "react";

import type { ConversationDetail } from "../../runtime/state/conversationState.ts";

export const FOLLOW_THRESHOLD_PX = 100;

/**
 * What the transcript is showing, as one number: when it changes, a reader who
 * is at the bottom is scrolled back to the bottom.
 *
 * It counts characters, not messages. Counting open delta buffers instead
 * pinned this at one for the whole of a streaming reply, so the view stopped
 * following exactly when there was something new to follow.
 */
export function transcriptSignature(detail: ConversationDetail): number {
  let total = detail.messages.length + detail.pending.length;
  for (const message of detail.messages) total += message.text.length;
  for (const buffer of Object.values(detail.deltas)) {
    for (const part of Object.values(buffer.parts)) total += part.length;
  }
  return total;
}

export function isNearBottom(
  element: Pick<HTMLElement, "scrollHeight" | "scrollTop" | "clientHeight">,
  threshold = FOLLOW_THRESHOLD_PX,
): boolean {
  return element.scrollHeight - element.scrollTop - element.clientHeight <= threshold;
}

export function useAutoFollow(
  ref: React.RefObject<HTMLElement | null>,
  signature: unknown,
): { readonly following: boolean; readonly jumpToLatest: () => void } {
  const [following, setFollowing] = React.useState(true);

  React.useEffect(() => {
    const element = ref.current;
    if (element === null) return;
    const onScroll = () => setFollowing(isNearBottom(element));
    element.addEventListener("scroll", onScroll, { passive: true });
    return () => element.removeEventListener("scroll", onScroll);
  }, [ref]);

  React.useEffect(() => {
    const element = ref.current;
    if (element === null || !following) return;
    element.scrollTop = element.scrollHeight;
  }, [ref, following, signature]);

  const jumpToLatest = React.useCallback(() => {
    const element = ref.current;
    if (element === null) return;
    element.scrollTop = element.scrollHeight;
    setFollowing(true);
  }, [ref]);

  return { following, jumpToLatest };
}
