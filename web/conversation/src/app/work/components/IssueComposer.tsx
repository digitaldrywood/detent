import React from "react";

import { DEFAULT_TURN_PREFERENCES } from "../../../contracts/index.ts";
import type { SlashCommand } from "../../adapters/slashCommands.ts";
import { Composer } from "../../components/Composer.tsx";

/**
 * The slash commands this page adds to the composer's own.
 *
 * `Composer` supplies the rest from its own handles and offers each only
 * where this surface actually has it: a comment box has no pickers, no files
 * and no turn to interrupt, so `/clear` is the only built-in left.
 *
 * A function of its own, and exported, because "which commands does this page
 * offer, and what does each do" is a decision worth checking without a
 * browser: the list that draws them needs layout jsdom has not got.
 */
export function issueComposerSlashCommands(options: {
  readonly onOpenShortcuts: (() => void) | null;
}): readonly SlashCommand[] {
  if (options.onOpenShortcuts === null) return [];
  const openShortcuts = options.onOpenShortcuts;
  return [
    {
      name: "shortcuts",
      description: "Open the keyboard shortcuts",
      run: openShortcuts,
    },
  ];
}

export interface IssueComposerProps {
  /** False for a reader who may not write: the composer is unavailable. */
  readonly canWrite: boolean;
  readonly onComment: (body: string) => Promise<void>;
  /** Opens the keybindings panel, for the composer's `/shortcuts`. */
  readonly onOpenShortcuts?: (() => void) | undefined;
}

export function IssueComposer(props: IssueComposerProps): React.ReactElement {
  const [draft, setDraft] = React.useState("");
  const [sending, setSending] = React.useState(false);

  const send = async () => {
    const text = draft.trim();
    if (text.length === 0 || sending) return;
    setSending(true);
    try {
      await props.onComment(text);
      setDraft("");
    } finally {
      setSending(false);
    }
  };

  const onOpenShortcuts = props.onOpenShortcuts;
  const slashCommands = React.useMemo(
    () => issueComposerSlashCommands({ onOpenShortcuts: onOpenShortcuts ?? null }),
    [onOpenShortcuts],
  );

  return (
    <Composer
      value={draft}
      onChange={setDraft}
      onSend={() => void send()}
      sending={sending}
      streaming={false}
      disabled={!props.canWrite}
      blockedReason={props.canWrite ? null : "You can read this issue but not write to it"}
      label="Comment"
      placeholder="Leave a comment…"

      preferences={DEFAULT_TURN_PREFERENCES}
      onPreferencesChange={() => {}}
      footerControls="none"
      attachControl="none"
      slashCommands={slashCommands}
      // No hint line under this card, so nothing for the editor to point at.
      describedBy={null}
      domId="dc-issue-composer"
    />
  );
}
