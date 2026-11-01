import React from "react";

import type { ExpandedImagePreview } from "../../../components/chat/ExpandedImagePreview.tsx";
import type { WorkAttachment as Attachment } from "../../../contracts/workAttachments.ts";
import { attachmentMarkdown } from "../../../contracts/workAttachments.ts";
import { formatAttachmentSize } from "../../../runtime/state/attachments.ts";
import { useAccountApi } from "../../account/context.ts";

export function WorkAttachment({ id, projectId, image, label, expand }: {
  readonly id: string;
  readonly projectId: string;
  readonly image: boolean;
  readonly label: React.ReactNode;
  readonly expand?: ((preview: ExpandedImagePreview) => void) | undefined;
}): React.ReactElement {
  const api = useAccountApi();
  const [result, setResult] = React.useState<{ projectId: string; id: string; file: Attachment | null } | null>(null);
  const [failedUrl, setFailedUrl] = React.useState<string | null>(null);
  React.useEffect(() => {
    let active = true;
    void api.attachment(projectId, id).then(
      (file) => { if (active) setResult({ projectId, id, file: file.id === id && file.project_id === projectId ? file : null }); },
      () => { if (active) setResult({ projectId, id, file: null }); },
    );
    return () => { active = false; };
  }, [api, projectId, id]);
  const current = result?.projectId === projectId && result.id === id ? result : null;
  if (current === null) return <span role="status">Loading attachment…</span>;
  const file = current.file;
  const url = api.attachmentUrl(projectId, id);
  if (file === null || failedUrl === url) return <span data-testid="attachment-unavailable" className="text-xs text-muted-foreground">Attachment unavailable</span>;
  if (image && file.content_type.startsWith("image/")) {
    const alt = typeof label === "string" ? label : file.name;
    const open = () => expand?.({ images: [{ src: url, name: file.name }], index: 0 });
    return <img src={url} alt={alt} width={file.width || undefined} height={file.height || undefined}
      data-markdown-copy={attachmentMarkdown({ ...file, name: alt })}
      data-testid="issue-attachment-image" className="block h-auto max-w-full cursor-zoom-in"
      style={{ width: file.width || undefined, height: "auto", maxWidth: "100%" }}
      role={expand ? "button" : undefined} tabIndex={expand ? 0 : undefined}
      aria-label={expand ? `Preview ${alt}` : undefined} onClick={open}
      onKeyDown={(event) => { if (event.key === "Enter" || event.key === " ") { event.preventDefault(); open(); } }}
      onError={() => setFailedUrl(url)} />;
  }
  return <a href={url} download={file.name} data-testid="issue-attachment-file"
    data-markdown-copy={attachmentMarkdown(file)}
    className="inline-flex items-center gap-2 rounded-md border border-border px-2 py-1 text-sm">
    <span>{file.name}</span><span className="text-xs text-muted-foreground">{formatAttachmentSize(file.size)}</span>
  </a>;
}
