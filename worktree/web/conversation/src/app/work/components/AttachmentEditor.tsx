import React from "react";

import { Button } from "../../../components/ui/button.tsx";
import { Textarea, type TextareaProps } from "../../../components/ui/textarea.tsx";
import { attachmentMarkdown } from "../../../contracts/workAttachments.ts";
import { makeAccountApi } from "../../account/api.ts";
import { ClientContext } from "../../client.ts";

export function useAttachmentDraft(projectId: string, setValue: React.Dispatch<React.SetStateAction<string>>) {
  const client = React.useContext(ClientContext);
  const api = React.useMemo(() => client === null ? null : makeAccountApi({ origin: client.http.origin, apiBase: client.http.apiBase, csrfToken: client.http.csrfToken }), [client]);
  const [pending, setPending] = React.useState(0);
  const [errors, setErrors] = React.useState<readonly string[]>([]);
  const scope = React.useRef({ projectId, active: true, placeholders: new Set<string>() });
  React.useEffect(() => {
    const current = { projectId, active: true, placeholders: new Set<string>() };
    scope.current = current;
    setPending(0);
    setErrors([]);
    return () => {
      current.active = false;
      const placeholders = [...current.placeholders];
      if (placeholders.length > 0) setValue((text) => {
        for (const placeholder of placeholders) text = text.replace(placeholder, "");
        return text;
      });
    };
  }, [projectId, setValue]);
  const addFiles = (files: readonly File[]) => {
    if (api === null || projectId === "") return;
    setErrors([]);
    const current = scope.current;
    for (const file of files) {
      const placeholder = `[Uploading ${file.name.replace(/[\[\]\r\n]/g, " ")}…](upload:${crypto.randomUUID()})`;
      current.placeholders.add(placeholder);
      setValue((text) => `${text}${text.length > 0 ? "\n\n" : ""}${placeholder}\n`);
      setPending((count) => count + 1);
      void api.uploadWorkAttachment(projectId, file).then(
        (uploaded) => {
          if (current.active && current === scope.current) setValue((text) => text.replace(placeholder, attachmentMarkdown(uploaded)));
        },
        (cause: unknown) => {
          if (!current.active || current !== scope.current) return;
          setValue((text) => text.replace(placeholder, ""));
          setErrors((entries) => [...entries, `${file.name}: ${cause instanceof Error ? cause.message : String(cause)}`]);
        },
      ).finally(() => {
        current.placeholders.delete(placeholder);
        if (current.active && current === scope.current) setPending((count) => count - 1);
      });
    }
  };
  return { addFiles, uploading: pending > 0, errors };
}

export function attachmentInputHandlers(addFiles: (files: readonly File[]) => void, disabled = false) {
  return {
    onPasteCapture: (event: React.ClipboardEvent) => {
      if (disabled || event.clipboardData.files.length === 0) return;
      event.preventDefault();
      event.stopPropagation();
      addFiles(Array.from(event.clipboardData.files));
    },
    onDragOver: (event: React.DragEvent) => {
      if (!disabled && Array.from(event.dataTransfer.types).includes("Files")) event.preventDefault();
    },
    onDropCapture: (event: React.DragEvent) => {
      if (disabled || event.dataTransfer.files.length === 0) return;
      event.preventDefault();
      event.stopPropagation();
      addFiles(Array.from(event.dataTransfer.files));
    },
  };
}

export function AttachmentEditor({ projectId, value, onChange, disabled, onUploadingChange, ...props }: Omit<TextareaProps, "value" | "onChange"> & {
  readonly projectId: string;
  readonly value: string;
  readonly onChange: React.Dispatch<React.SetStateAction<string>>;
  readonly onUploadingChange: (uploading: boolean) => void;
}): React.ReactElement {
  const upload = useAttachmentDraft(projectId, onChange);
  const input = React.useRef<HTMLInputElement>(null);
  React.useEffect(() => onUploadingChange(upload.uploading), [upload.uploading, onUploadingChange]);
  return <div {...attachmentInputHandlers(upload.addFiles, disabled)} className="flex flex-col gap-1.5">
    <Textarea {...props} value={value} disabled={disabled} onChange={(event) => onChange(event.currentTarget.value)} />
    <input ref={input} type="file" multiple hidden data-testid="attachment-input" disabled={disabled}
      onChange={(event) => { upload.addFiles(Array.from(event.currentTarget.files ?? [])); event.currentTarget.value = ""; }} />
    <Button type="button" size="xs" variant="ghost" className="w-fit" disabled={disabled} onClick={() => input.current?.click()}>Attach files</Button>
    {upload.errors.map((error, index) => <p key={index} role="alert" className="text-sm text-destructive">{error}</p>)}
  </div>;
}
