import { XIcon } from "lucide-react";
import React from "react";

export function ComposerContextAttachment({
  label,
  title,
  onRemove,
  disabled = false,
}: {
  readonly label: string;
  readonly title?: string;
  readonly onRemove?: () => void;
  readonly disabled?: boolean;
}): React.ReactElement {
  return (
    <span
      data-testid="composer-context-attachment"
      title={title}
      className="mb-2 inline-flex max-w-full items-center gap-1 rounded-md border border-border bg-muted px-2 py-0.5 text-xs"
    >
      <span className="truncate">{label}</span>
      {onRemove === undefined ? null : (
        <button
          type="button"
          aria-label={`Remove ${label} context`}
          disabled={disabled}
          onClick={onRemove}
          className="shrink-0 rounded-sm p-0.5 hover:bg-background focus-visible:ring-2 focus-visible:ring-ring disabled:opacity-50"
        >
          <XIcon className="size-3" />
        </button>
      )}
    </span>
  );
}
