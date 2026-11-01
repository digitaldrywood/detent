import React from "react";
import { CheckIcon, CopyIcon } from "lucide-react";
import { Button } from "../../components/ui/button.tsx";
import { Input } from "../../components/ui/input.tsx";
import { useCopyToClipboard } from "../../hooks/useCopyToClipboard.ts";
import { ControlError } from "../account/controls.tsx";
import {
  Dialog,
  DialogClose,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogPanel,
  DialogPopup,
  DialogTitle,
} from "../../components/ui/dialog.tsx";
import { SettingsRow } from "./settingsLayout.tsx";

export function ConnectionValue({
  label,
  value,
  inline = false,
}: {
  readonly label: string;
  readonly value: string;
  readonly inline?: boolean;
}) {
  const [failed, setFailed] = React.useState(false);
  const { copyToClipboard, isCopied } = useCopyToClipboard({
    target: label,
    onCopy: () => setFailed(false),
    onError: () => setFailed(true),
  });
  const input = (
    <Input
      size="sm"
      readOnly
      value={value}
      aria-label={`${label} value`}
      className="min-w-0 font-mono"
    />
  );
  const status = (
    <span
      role="status"
      aria-label={`${label} copy status`}
      className={failed ? undefined : "sr-only"}
    >
      {failed
        ? "Could not copy. Select the text and copy it manually."
        : isCopied
          ? `${label} copied.`
          : ""}
    </span>
  );
  const control = (
    <Button
      size="icon-sm"
      variant="ghost"
      aria-label={`Copy ${label}`}
      onClick={() => copyToClipboard(value)}
    >
      {isCopied ? (
        <CheckIcon aria-hidden="true" />
      ) : (
        <CopyIcon aria-hidden="true" />
      )}
    </Button>
  );
  return inline ? (
    <div className="min-w-0 flex-1 basis-full sm:basis-0">
      <div className="flex items-center gap-1">
        {input}
        {control}
      </div>
      {status}
    </div>
  ) : (
    <SettingsRow
      title={label}
      description={input}
      status={status}
      control={control}
    />
  );
}

export function SetupPromptControl({
  label,
  value,
  copyLabel = `Copy ${label}`,
  preview = false,
  inlinePreview = false,
}: {
  readonly label: string;
  readonly value: string;
  readonly copyLabel?: string;
  readonly preview?: boolean;
  readonly inlinePreview?: boolean;
}) {
  const [open, setOpen] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);
  const { copyToClipboard, isCopied } = useCopyToClipboard({
    target: label,
    onCopy: () => setError(null),
    onError: () =>
      setError("Could not copy. Preview the prompt and copy it manually."),
  });
  return (
    <>
      {preview && !inlinePreview && (
        <Button
          size="sm"
          variant="ghost"
          aria-label={`Preview ${label}`}
          onClick={() => setOpen(true)}
        >
          Preview
        </Button>
      )}
      <Button
        size={inlinePreview ? "sm-multiline" : preview ? "sm" : "default"}
        variant="outline"
        aria-label={copyLabel}
        onClick={() => copyToClipboard(value)}
      >
        {isCopied ? (
          <CheckIcon aria-hidden="true" />
        ) : (
          <CopyIcon aria-hidden="true" />
        )}
        {isCopied ? "Copied" : preview && !inlinePreview ? "Copy" : copyLabel}
      </Button>
      {inlinePreview && (
        <Button
          size="sm"
          variant="ghost"
          aria-label={`Preview ${label}`}
          aria-expanded={open}
          onClick={() => setOpen(!open)}
        >
          {open ? "Hide prompt" : "Preview"}
        </Button>
      )}
      <span role="status" className="sr-only">
        {isCopied ? `${label} copied.` : ""}
      </span>
      <ControlError message={error} />
      {inlinePreview && open && (
        <pre
          aria-label={`${label} preview`}
          tabIndex={0}
          className="max-h-48 w-full overflow-y-auto rounded-lg border border-border bg-background p-3 font-mono text-xs leading-relaxed whitespace-pre-wrap [overflow-wrap:anywhere]"
        >
          {value}
        </pre>
      )}
      <Dialog open={!inlinePreview && open} onOpenChange={setOpen}>
        <DialogPopup className="max-w-2xl">
          <DialogHeader>
            <DialogTitle>{label}</DialogTitle>
            <DialogDescription>
              This prompt references your private DETENT_API_KEY environment
              variable. It contains no key.
            </DialogDescription>
          </DialogHeader>
          <DialogPanel>
            <pre
              tabIndex={0}
              className="font-mono text-xs leading-relaxed whitespace-pre-wrap [overflow-wrap:anywhere]"
            >
              {value}
            </pre>
          </DialogPanel>
          <DialogFooter>
            <DialogClose render={<Button variant="outline">Close</Button>} />
            <Button onClick={() => copyToClipboard(value)}>
              {isCopied ? "Copied" : copyLabel}
            </Button>
          </DialogFooter>
        </DialogPopup>
      </Dialog>
    </>
  );
}
