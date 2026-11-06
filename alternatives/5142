import React from "react";
import { UploadIcon } from "lucide-react";
import { Button } from "../../components/ui/button.tsx";
import type { IconUpload } from "./projectIconUpload.ts";

export function IconUploadView({ label, upload, error, reading, pending, onRead }: {
  readonly label: string;
  readonly upload: IconUpload | null;
  readonly error: string | null;
  readonly reading: boolean;
  readonly pending: boolean;
  readonly onRead: (file: File | undefined) => Promise<void>;
}) {
  const input = React.useRef<HTMLInputElement>(null);
  return <div className="space-y-4">
    <div hidden={upload !== null} className="flex flex-col items-center gap-3 rounded-lg border border-dashed border-border p-6 text-center" onDragOver={(event) => event.preventDefault()} onDrop={(event) => { event.preventDefault(); void onRead(event.dataTransfer.files[0]); }}>
      <UploadIcon className="size-6 text-muted-foreground" />
      <p className="text-sm">Drop an image here or <Button size="sm" variant="link" disabled={reading || pending} onClick={() => input.current?.click()}>browse files</Button></p>
      <p className="text-xs text-muted-foreground">PNG, JPEG or WebP · Up to 256 KB<br />64–1024 px · Square within 10%</p>
    </div>
    <input ref={input} type="file" accept="image/png,image/jpeg,image/webp" aria-label={label} className="sr-only" disabled={pending} onChange={(event) => { void onRead(event.currentTarget.files?.[0]); event.currentTarget.value = ""; }} />
    {reading ? <p role="status" className="text-sm text-muted-foreground">Checking image…</p> : null}
    {error ? <p role="alert" className="text-sm text-destructive">{error}</p> : null}
    {upload ? <>
      <div className="flex items-end justify-center gap-6 rounded-lg bg-muted p-6" aria-label="Image preview">{[64, 32, 16].map((size) => <div key={size} className="space-y-2 text-center text-xs text-muted-foreground"><img src={upload.preview} width={size} height={size} alt={`${size} px preview`} className="rounded-sm object-cover" /><span>{size} px</span></div>)}</div>
      <p className="break-words text-center text-xs text-muted-foreground">{upload.name} · {upload.width} × {upload.height} · {(upload.size / 1024).toFixed(1)} KB</p>
      <Button size="sm" variant="ghost" disabled={pending} onClick={() => input.current?.click()}>Choose another</Button>
    </> : null}
  </div>;
}
