import React from "react";

import { DEFAULT_TURN_PREFERENCES } from "../../../contracts/index.ts";
import type { SlashCommand } from "../../adapters/slashCommands.ts";
import { Composer } from "../../components/Composer.tsx";
import { attachmentInputHandlers, useAttachmentDraft } from "./AttachmentEditor.tsx";
import { Textarea } from "../../../components/ui/textarea.tsx";
import { Button } from "../../../components/ui/button.tsx";

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
  readonly variant?: "rich" | "simple";
  readonly projectId?: string | undefined;
  /** False for a reader who may not write: the composer is unavailable. */
  readonly canWrite: boolean;
  readonly onComment: (body: string) => Promise<void>;
  /** Opens the keybindings panel, for the composer's `/shortcuts`. */
  readonly onOpenShortcuts?: (() => void) | undefined;
}

export function IssueComposer(props: IssueComposerProps): React.ReactElement {
  const [draft, setDraft] = React.useState("");
  const [sending, setSending] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);
  const upload = useAttachmentDraft(props.projectId ?? "", setDraft);

  const send = async () => {
    const text = draft.trim();
    if (text.length === 0 || sending || upload.uploading) return;
    setSending(true);
    setError(null);
    try {
      await props.onComment(text);
      setDraft("");
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : String(cause));
    } finally {
      setSending(false);
    }
  };

  const onOpenShortcuts = props.onOpenShortcuts;
  const slashCommands = React.useMemo(
    () => issueComposerSlashCommands({ onOpenShortcuts: onOpenShortcuts ?? null }),
    [onOpenShortcuts],
  );

  if (props.variant === "simple") return <form className="space-y-2" {...attachmentInputHandlers(upload.addFiles, !props.canWrite || sending)} onSubmit={(event) => { event.preventDefault(); void send(); }}>
    <Textarea aria-label="Reply or add details" placeholder="Reply or add details" autoGrow={false} rows={2} value={draft} disabled={!props.canWrite || sending} onChange={(event) => setDraft(event.currentTarget.value)} />
    {[...upload.errors, ...(error === null ? [] : [error])].map((message) => <p key={message} role="alert" className="text-sm text-destructive-foreground">{message}</p>)}
    <div className="flex justify-end"><Button type="submit" size="sm" variant="outline" disabled={!props.canWrite || sending || upload.uploading || !draft.trim()}>Comment</Button></div>
  </form>;

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
      attachControl={props.projectId === undefined ? "none" : "attach"}
      onFiles={props.projectId === undefined ? undefined : upload.addFiles}
      uploadingFiles={upload.uploading}
      attachmentErrors={[...upload.errors, ...(error === null ? [] : [error])]}
      slashCommands={slashCommands}
      // No hint line under this card, so nothing for the editor to point at.
      describedBy={null}
      domId="dc-issue-composer"
    />
  );
}
