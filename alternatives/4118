import React from "react";
import { Button } from "../../components/ui/button.tsx";
import { NavigationMonogram } from "../../components/ui/navigation-list.tsx";
import { Popover, PopoverTrigger, PopoverPopup, PopoverTitle } from "../../components/ui/popover.tsx";
import { Sheet, SheetTrigger, SheetPopup, SheetTitle } from "../../components/ui/sheet.tsx";
import type { OrganizationLogo } from "../../contracts/account.ts";
import { useMediaQuery } from "../../hooks/useMediaQuery.ts";
import { useRefreshBootstrap } from "../client.ts";
import { OrganizationImage } from "../components/OrganizationImage.tsx";
import { SettingsRow } from "../settings/settingsLayout.tsx";
import { AccountError } from "./api.ts";
import { useAccountApi } from "./context.ts";
import { IconUploadView } from "./IconUploadView.tsx";
import { newKey } from "./idempotency.ts";
import { prepareIconUpload, type IconUpload } from "./projectIconUpload.ts";

export function OrganizationLogoPickerRow({ name, logo, canManage, onSave }: {
  readonly name: string;
  readonly logo: OrganizationLogo | null;
  readonly canManage: boolean;
  readonly onSave: (content: string | undefined, remove: boolean) => Promise<void>;
}) {
  const phone = useMediaQuery("max-sm");
  const [open, setOpen] = React.useState(false);
  const [upload, setUpload] = React.useState<IconUpload | null>(null);
  const [remove, setRemove] = React.useState(false);
  const [reading, setReading] = React.useState(false);
  const [pending, setPending] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);
  const [saveError, setSaveError] = React.useState<string | null>(null);
  const generation = React.useRef(0);
  React.useEffect(() => () => { generation.current++; }, []);
  function changeOpen(next: boolean) {
    if (pending) return;
    generation.current++;
    setOpen(next); setUpload(null); setRemove(false); setError(null); setSaveError(null); setReading(false);
  }
  async function read(file: File | undefined) {
    if (!file || pending) return;
    const current = ++generation.current;
    setUpload(null); setRemove(false); setError(null); setReading(true);
    try {
      const prepared = await prepareIconUpload(file);
      if (generation.current === current) setUpload(prepared);
    } catch (cause) {
      if (generation.current === current) setError(cause instanceof Error ? cause.message : "The image could not be read.");
    } finally {
      if (generation.current === current) setReading(false);
    }
  }
  const tile = <OrganizationImage name={name} url={logo?.url} className="size-8" fallback={<span className="flex size-8 items-center justify-center overflow-hidden rounded-lg"><NavigationMonogram name={name} /></span>} />;
  const trigger = <Button variant="outline" size="icon-lg" aria-label="Choose organization logo">{tile}</Button>;
  const content = <>
    <div className="space-y-4 p-4">
      {phone ? <SheetTitle>Organization logo</SheetTitle> : <PopoverTitle>Organization logo</PopoverTitle>}
      <IconUploadView label="Upload organization logo" upload={upload} error={error} reading={reading} pending={pending} onRead={read} />
      {remove ? <p role="status" className="text-xs text-muted-foreground">The generated monogram will be used.</p> : null}
      {saveError ? <p role="alert" className="text-sm text-destructive">{saveError}</p> : null}
    </div>
    <div className="flex flex-col-reverse items-stretch gap-3 border-t border-border p-3 sm:flex-row sm:items-center sm:justify-between">
      <Button size="sm" variant="ghost" disabled={pending || (!logo?.url && !upload)} onClick={() => { generation.current++; setReading(false); setRemove(true); setUpload(null); setError(null); }}>Remove logo</Button>
      <div className="flex justify-end gap-2">
        <Button size={phone ? "lg" : "sm"} className="flex-1 sm:flex-none" variant="outline" disabled={pending} onClick={() => changeOpen(false)}>Cancel</Button>
        <Button size={phone ? "lg" : "sm"} className="flex-1 sm:flex-none" disabled={pending || reading || error !== null || (!upload && !(remove && logo?.url))} onClick={async () => {
          setPending(true); setSaveError(null);
          try { await onSave(upload?.content, remove); setOpen(false); }
          catch (cause) { setSaveError(cause instanceof Error ? cause.message : "The logo could not be saved."); }
          finally { setPending(false); }
        }}>{pending ? "Saving…" : "Save"}</Button>
      </div>
    </div>
  </>;
  return <SettingsRow title="Logo" control={!canManage ? <span aria-label="Organization logo">{tile}</span> : phone ?
    <Sheet open={open} onOpenChange={changeOpen}><SheetTrigger render={trigger} /><SheetPopup side="bottom" showCloseButton={false}>{content}</SheetPopup></Sheet> :
    <Popover open={open} onOpenChange={changeOpen}><PopoverTrigger render={trigger} /><PopoverPopup width="lg" padding="none" align="end">{content}</PopoverPopup></Popover>} />;
}

export function OrganizationLogoSettings({ name, logo, canManage }: { readonly name: string; readonly logo: OrganizationLogo | null; readonly canManage: boolean }) {
  const api = useAccountApi();
  const refresh = useRefreshBootstrap();
  const [current, setCurrent] = React.useState(logo);
  React.useEffect(() => setCurrent(logo), [logo]);
  return <OrganizationLogoPickerRow name={name} logo={current} canManage={canManage} onSave={async (content, remove) => {
    try {
      const revision = current?.revision ?? (await api.organizationLogo()).revision;
      setCurrent(await api.setOrganizationLogo({ ...(content ? { content } : {}), remove, revision, key: newKey() }));
      await refresh();
    } catch (cause) {
      if (cause instanceof AccountError && cause.isConflict) {
        setCurrent(await api.organizationLogo());
        throw new Error("Changed by someone else. Reloaded; choose a logo and save again.");
      }
      throw cause;
    }
  }} />;
}
