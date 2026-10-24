import React from "react";
import { CheckIcon, UploadIcon } from "lucide-react";
import { DynamicIcon, iconNames, type IconName } from "lucide-react/dynamic";
import { ProjectFavicon } from "../../components/ProjectFavicon.tsx";
import { Button } from "../../components/ui/button.tsx";
import { Input } from "../../components/ui/input.tsx";
import { Popover, PopoverTrigger, PopoverPopup, PopoverTitle } from "../../components/ui/popover.tsx";
import { Sheet, SheetTrigger, SheetPopup, SheetTitle } from "../../components/ui/sheet.tsx";
import { Tabs, TabsList, TabsTab, TabsPanel } from "../../components/ui/tabs.tsx";
import { PROJECT_ICON_COLORS, projectIconColorClassName } from "../../projectIconColors.ts";
import type { ProjectIcon, ProjectIconChoice } from "../../contracts/projectIcon.ts";
import type { ProjectIntegration } from "../../contracts/account.ts";
import { HUB_ENVIRONMENT_ID } from "../../contracts/index.ts";
import { useMediaQuery } from "../../hooks/useMediaQuery.ts";
import { cn } from "../../lib/utils.ts";
import { useRefreshBootstrap } from "../client.ts";
import { SettingsRow } from "../settings/settingsLayout.tsx";
import { useProjectSettingsDirty } from "../settings/projectSettingsDirty.ts";
import { useAccountApi } from "./context.ts";
import { AccountError } from "./api.ts";
import { newKey } from "./idempotency.ts";
import { prepareIconUpload, type IconUpload } from "./projectIconUpload.ts";

const COMMON_ICONS: readonly IconName[] = ["rocket", "bot", "book-open", "cloud", "database", "monitor", "gamepad-2", "image", "smartphone", "music", "package", "shield-check", "server", "shopping-bag", "terminal", "flask-conical", "video", "globe", "code", "braces", "circuit-board", "folder-code", "layers", "zap", "heart", "star", "wrench", "leaf", "mail", "chart-no-axes-combined", "lock", "cpu"];
const EMOJI = [
  ["🚀", "rocket launch space"], ["🤖", "robot bot ai"], ["📚", "books docs"], ["☁️", "cloud"], ["💾", "disk database save"], ["💻", "computer laptop code"], ["🎮", "game controller"], ["🖼️", "image picture"],
  ["📱", "phone mobile"], ["🎵", "music note"], ["📦", "package box"], ["🛡️", "shield security"], ["🌐", "globe web"], ["🛒", "shopping cart"], ["⚡", "lightning zap fast"], ["🧪", "test science"],
  ["🎬", "movie video"], ["🌍", "earth world"], ["🔧", "wrench tools"], ["🧩", "puzzle plugin"], ["🔌", "plug circuit"], ["📁", "folder files"], ["🗂️", "folders layers"], ["✨", "sparkles magic"],
  ["❤️", "heart love"], ["⭐", "star"], ["⚙️", "gear settings"], ["🌿", "leaf nature"], ["✉️", "mail email"], ["📊", "chart analytics"], ["🔒", "lock security"], ["🧠", "brain ai"],
  ["😀", "happy smile face"], ["😎", "cool sunglasses"], ["🔥", "fire"], ["🎯", "target goal"], ["🎨", "art palette design"], ["🏠", "home house"], ["🏗️", "building construction"], ["🦊", "fox"],
  ["🐱", "cat"], ["🐶", "dog"], ["🐸", "frog"], ["🐳", "whale"], ["🦋", "butterfly"], ["🌻", "sunflower flower"], ["🌈", "rainbow"], ["🌙", "moon night"],
  ["☀️", "sun day"], ["🌲", "tree forest"], ["🏔️", "mountain"], ["🌊", "wave water ocean"], ["🍎", "apple fruit"], ["🍋", "lemon"], ["☕", "coffee"], ["🍕", "pizza"],
  ["🎉", "party celebration"], ["🏆", "trophy award"], ["💡", "idea light bulb"], ["💎", "diamond gem"], ["🧭", "compass navigate"], ["🔬", "microscope science"], ["📷", "camera photo"], ["🎧", "headphones audio"],
] as const;

function choiceOf(icon: ProjectIcon | null): ProjectIconChoice {
  return icon?.kind === "image" ? { kind: "image" } : icon ?? { kind: "automatic" };
}

export function ProjectIconPickerRow({ projectId, projectName, icon, canManage, onSave }: {
  readonly projectId: string;
  readonly projectName: string;
  readonly icon: ProjectIcon | null;
  readonly canManage: boolean;
  readonly onSave: (choice: ProjectIconChoice, content?: string) => Promise<void>;
}) {
  const phone = useMediaQuery("max-sm");
  const [open, setOpen] = React.useState(false);
  const [tab, setTab] = React.useState("icon");
  const [search, setSearch] = React.useState("");
  const [limit, setLimit] = React.useState(80);
  const [choice, setChoice] = React.useState<ProjectIconChoice>(() => choiceOf(icon));
  const [upload, setUpload] = React.useState<IconUpload | null>(null);
  const [uploadError, setUploadError] = React.useState<string | null>(null);
  const [saveError, setSaveError] = React.useState<string | null>(null);
  const [pending, setPending] = React.useState(false);
  const [reading, setReading] = React.useState(false);
  const generation = React.useRef(0);
  const fileInput = React.useRef<HTMLInputElement>(null);
  const changed = upload !== null || JSON.stringify(choice) !== JSON.stringify(choiceOf(icon));
  useProjectSettingsDirty(open && changed);
  React.useEffect(() => () => { generation.current++; }, []);
  function changeOpen(next: boolean) {
    if (pending) return;
    generation.current++;
    setReading(false);
    setOpen(next);
    setChoice(choiceOf(icon));
    setSearch(""); setLimit(80); setTab(icon?.kind === "emoji" ? "emoji" : icon?.kind === "image" ? "upload" : "icon");
    setUpload(null); setUploadError(null); setSaveError(null);
  }
  async function read(file: File | undefined) {
    if (!file || pending) return;
    const current = ++generation.current;
    setUpload(null); setUploadError(null); setReading(true); setChoice({ kind: "image" });
    try {
      const prepared = await prepareIconUpload(file);
      if (generation.current === current) setUpload(prepared);
    } catch (cause) {
      if (generation.current === current) setUploadError(cause instanceof Error ? cause.message : "The image could not be read.");
    } finally {
      if (generation.current === current) setReading(false);
    }
  }
  const name = choice.kind === "lucide" ? choice.name : "rocket";
  const color = choice.kind === "lucide" ? choice.color : "emerald";
  const filtered = search.trim() ? iconNames.filter((name) => name.includes(search.trim().toLowerCase().replaceAll(" ", "-"))) : COMMON_ICONS;
  const invalidEmoji = choice.kind === "emoji" && (!choice.emoji.trim() || [...new Intl.Segmenter(undefined, { granularity: "grapheme" }).segment(choice.emoji)].length !== 1);
  const invalidUpload = choice.kind === "image" && (uploadError !== null || reading || (upload === null && icon?.kind !== "image"));
  const tile = <ProjectFavicon project={{ environmentId: HUB_ENVIRONMENT_ID, workspaceRoot: projectId, title: projectName, faviconPath: null, projectIcon: icon }} className="size-6" />;
  const trigger = <Button size="icon" variant="outline" aria-label="Choose project icon">{tile}</Button>;
  const content = <>
    <div className="space-y-4 p-4">
      {phone ? <SheetTitle>Project icon</SheetTitle> : <PopoverTitle className="sr-only">Project icon</PopoverTitle>}
      <Tabs variant="segmented" value={tab} onValueChange={(value) => { setTab(String(value)); setSearch(""); setLimit(80); }}>
        <TabsList aria-label="Icon type" className="grid w-full grid-cols-3"><TabsTab className="justify-center" value="icon">Icon</TabsTab><TabsTab className="justify-center" value="emoji">Emoji</TabsTab><TabsTab className="justify-center" value="upload">Upload</TabsTab></TabsList>
        <TabsPanel value="icon" className="space-y-4">
          <Input aria-label="Search icons" placeholder="Search icons" value={search} onChange={(event) => { setSearch(event.currentTarget.value); setLimit(80); }} />
          <div className="max-h-64 overflow-y-auto" aria-label="Lucide icons">
            <div className="grid grid-cols-6 gap-1 sm:grid-cols-8">
              {filtered.slice(0, limit).map((item) => <Button key={item} size={phone ? "icon-xl" : "icon-lg"} variant={choice.kind === "lucide" && choice.name === item ? "outline" : "ghost"} aria-label={item} aria-pressed={choice.kind === "lucide" && choice.name === item} onClick={() => setChoice({ kind: "lucide", name: item, color })}><DynamicIcon name={item} /></Button>)}
            </div>
            {filtered.length === 0 ? <p className="py-4 text-sm text-muted-foreground">No icons found.</p> : null}
            {filtered.length > limit ? <Button variant="ghost" size="sm" onClick={() => setLimit(limit + 80)}>Show more icons</Button> : null}
          </div>
          <div className="flex flex-wrap gap-2" aria-label="Icon color">
            {PROJECT_ICON_COLORS.map((item) => <button key={item.value} type="button" aria-label={item.label} aria-pressed={choice.kind === "lucide" && color === item.value} className={cn("flex size-6 items-center justify-center rounded-full outline-none ring-offset-2 ring-offset-popover focus-visible:ring-2 focus-visible:ring-ring", item.swatchClassName, choice.kind === "lucide" && color === item.value && "ring-2 ring-foreground")} onClick={() => setChoice({ kind: "lucide", name, color: item.value })}>{choice.kind === "lucide" && color === item.value ? <CheckIcon className="size-3 text-white" /> : null}</button>)}
          </div>
          {choice.kind === "lucide" ? <div className="flex items-center gap-2 text-xs text-muted-foreground"><DynamicIcon name={choice.name as IconName} className={cn("size-4", projectIconColorClassName(choice.color))} />{choice.name}</div> : null}
        </TabsPanel>
        <TabsPanel value="emoji" className="space-y-3">
          <Input aria-label="Search emoji" placeholder="Search emoji" value={search} onChange={(event) => setSearch(event.currentTarget.value)} />
          <div className="grid max-h-64 grid-cols-8 gap-1 overflow-y-auto" aria-label="Emoji choices">
            {EMOJI.filter(([, label]) => label.includes(search.toLowerCase())).map(([emoji, label]) => <Button key={emoji} size={phone ? "icon-xl" : "icon-lg"} variant={choice.kind === "emoji" && choice.emoji === emoji ? "outline" : "ghost"} aria-label={label} aria-pressed={choice.kind === "emoji" && choice.emoji === emoji} onClick={() => setChoice({ kind: "emoji", emoji })}><span className="text-xl">{emoji}</span></Button>)}
          </div>
          <Input aria-label="Emoji" placeholder="Or type any emoji" value={choice.kind === "emoji" ? choice.emoji : ""} onChange={(event) => setChoice({ kind: "emoji", emoji: event.currentTarget.value })} />
          {invalidEmoji ? <p role="status" className="text-sm text-destructive">Choose one emoji.</p> : null}
        </TabsPanel>
        <TabsPanel value="upload" className="space-y-4">
          <div hidden={upload !== null} className="flex flex-col items-center gap-3 rounded-lg border border-dashed border-border p-6 text-center" onDragOver={(event) => event.preventDefault()} onDrop={(event) => { event.preventDefault(); void read(event.dataTransfer.files[0]); }}>
            <UploadIcon className="size-6 text-muted-foreground" />
            <p className="text-sm">Drop an image here or <Button size="sm" variant="link" disabled={reading || pending} onClick={() => fileInput.current?.click()}>browse files</Button></p>
            <p className="text-xs text-muted-foreground">PNG, JPEG or WebP · Up to 256 KB<br />64–1024 px · Square within 10%</p>
            </div>
 <input ref={fileInput} type="file" accept="image/png,image/jpeg,image/webp" aria-label="Upload project icon" className="sr-only" disabled={pending} onChange={(event) => { void read(event.currentTarget.files?.[0]); event.currentTarget.value = ""; }} />
          {reading ? <p role="status" className="text-sm text-muted-foreground">Checking image…</p> : null}
          {uploadError ? <p role="alert" className="text-sm text-destructive">{uploadError}</p> : null}
          {upload ? <>
            <div className="flex items-end justify-center gap-6 rounded-lg bg-muted p-6" aria-label="Image preview">{[64, 32, 16].map((size) => <div key={size} className="space-y-2 text-center text-xs text-muted-foreground"><img src={upload.preview} width={size} height={size} alt={`${size} px preview`} className="rounded-sm object-cover" /><span>{size} px</span></div>)}</div>
            <p className="break-words text-center text-xs text-muted-foreground">{upload.name} · {upload.width} × {upload.height} · {(upload.size / 1024).toFixed(1)} KB</p>
 <Button size="sm" variant="ghost" disabled={pending} onClick={() => fileInput.current?.click()}>Choose another</Button>
          </> : null}
        </TabsPanel>
      </Tabs>
      {choice.kind === "automatic" ? <p role="status" className="text-xs text-muted-foreground">The icon is chosen from the project name.</p> : null}
      {saveError ? <p role="alert" className="text-sm text-destructive">{saveError}</p> : null}
    </div>
    <div className="flex flex-col-reverse items-stretch gap-3 border-t border-border p-3 sm:flex-row sm:items-center sm:justify-between">
      <Button size="sm" variant="ghost" disabled={pending} onClick={() => { generation.current++; setReading(false); setChoice({ kind: "automatic" }); setUpload(null); setUploadError(null); }}>Use automatic</Button>
      <div className="flex justify-end gap-2"><Button className="flex-1 sm:flex-none" size={phone ? "lg" : "sm"} variant="outline" disabled={pending} onClick={() => changeOpen(false)}>Cancel</Button><Button className="flex-1 sm:flex-none" size={phone ? "lg" : "sm"} disabled={pending || !changed || invalidEmoji || invalidUpload} onClick={async () => {
        setPending(true); setSaveError(null);
        try { await onSave(choice, choice.kind === "image" ? upload?.content : undefined); setOpen(false); }
        catch (cause) { setSaveError(cause instanceof Error ? cause.message : "The icon could not be saved."); }
        finally { setPending(false); }
      }}>{pending ? "Saving…" : "Save"}</Button></div>
    </div>
  </>;
  return <SettingsRow title="Icon" control={!canManage ? <span aria-label="Project icon" className="inline-flex size-9 items-center justify-center rounded-md border border-border">{tile}</span> : phone ?
    <Sheet open={open} onOpenChange={changeOpen}><SheetTrigger render={trigger} /><SheetPopup side="bottom" showCloseButton={false}>{content}</SheetPopup></Sheet> :
    <Popover open={open} onOpenChange={changeOpen}><PopoverTrigger render={trigger} /><PopoverPopup width="lg" padding="none" align="end">{content}</PopoverPopup></Popover>} />;
}

export function ProjectIconSettings({ projectId, projectName, integration, canManage, onSaved, onReload }: {
  readonly projectId: string;
  readonly projectName: string;
  readonly integration: ProjectIntegration;
  readonly canManage: boolean;
  readonly onSaved: (value: ProjectIntegration) => void;
  readonly onReload: (value: ProjectIntegration) => void;
}) {
  const api = useAccountApi();
  const refresh = useRefreshBootstrap();
  return <ProjectIconPickerRow projectId={projectId} projectName={projectName} icon={integration.project_icon ?? null} canManage={canManage} onSave={async (icon, content) => {
    try {
      onSaved(await api.setProjectIcon({ projectId, icon, ...(content ? { content } : {}), revision: integration.revision, key: newKey() }));
      await refresh();
    } catch (cause) {
      if (cause instanceof AccountError && cause.isConflict) {
        onReload(await api.integration(projectId));
        throw new Error("Changed by someone else. Reloaded; choose an icon and save again.");
      }
      throw cause;
    }
  }} />;
}
