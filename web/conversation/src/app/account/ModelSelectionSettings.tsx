import { useProjectSettingsDirty } from "../settings/projectSettingsDirty.ts";
import React from "react";
import { DndContext, KeyboardSensor, PointerSensor, closestCenter, useSensor, useSensors } from "@dnd-kit/core";
import { SortableContext, arrayMove, rectSortingStrategy, sortableKeyboardCoordinates, useSortable } from "@dnd-kit/sortable";
import { CSS } from "@dnd-kit/utilities";
import { GripVerticalIcon, XIcon } from "lucide-react";

import { Badge } from "../../components/ui/badge.tsx";
import { Button } from "../../components/ui/button.tsx";
import { Combobox, ComboboxCollection, ComboboxEmpty, ComboboxGroup, ComboboxGroupLabel, ComboboxItem, ComboboxList, ComboboxPopup, ComboboxSearchInput, ComboboxTrigger } from "../../components/ui/combobox.tsx";
import { Input } from "../../components/ui/input.tsx";
import { Radio, RadioGroup } from "../../components/ui/radio-group.tsx";
import { Select, SelectButton, SelectItem, SelectPopup, SelectTrigger, SelectValue } from "../../components/ui/select.tsx";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../../components/ui/table.tsx";
import { ToggleGroup, ToggleGroupItem } from "../../components/ui/toggle-group.tsx";
import type { CloudSelection } from "../../contracts/account.ts";
import type { PreferenceChoice } from "../../contracts/conversation.ts";
import { useClient } from "../client.ts";
import { SettingsRow, SettingsSection } from "../settings/settingsLayout.tsx";
import { AccountError } from "./api.ts";
import { ControlError, ToggleControl } from "./controls.tsx";
import { useAccountApi, useAccountBootstrap } from "./context.ts";
import { newKey } from "./idempotency.ts";
import { useMutation, useResource } from "./useResource.ts";

const EFFORTS = ["low", "medium", "high", "xhigh"];
const LEVEL_ORDER = ["normal", "complex", "very_complex"];
const STAGE_ORDER = ["plan", "code", "rework", "merge", "validator", "routine", "security_audit"];
type SelectionStage = NonNullable<CloudSelection["stages"]>[string];
type Option = { value: string; label: string };
const isolateControl = { onFocus: (event: React.FocusEvent) => event.stopPropagation() };
const radioKeys: NonNullable<React.ComponentProps<typeof RadioGroup>["onKeyDown"]> = (event) => {
  if (!(event.target as HTMLElement).closest("[role=radio]")) event.preventBaseUIHandler?.();
};
const title = (value: string) => value.replaceAll("_", " ").replace(/^./, (letter) => letter.toUpperCase());
function ordered<T>(values: Readonly<Record<string, T>>, order: readonly string[]): [string, T][] {
  return Object.entries(values).sort(([a], [b]) => {
    const rank = (name: string) => order.includes(name) ? order.indexOf(name) : order.length;
    return rank(a) - rank(b) || a.localeCompare(b);
  });
}

function Choice({ label, value, options, disabled, onChange }: {
  label: string; value: string; options: readonly Option[]; disabled: boolean; onChange: (value: string) => void;
}) {
  const choices = options.some((option) => option.value === value) ? options : [...options, { value, label: value || "Inherited" }];
  return <Select value={value} items={choices} disabled={disabled} onValueChange={(next) => { if (next !== null) onChange(next); }}>
    <SelectTrigger size="sm" aria-label={label}><SelectValue /></SelectTrigger>
    <SelectPopup>{choices.map((option) => <SelectItem key={option.value} value={option.value}>{option.label}</SelectItem>)}</SelectPopup>
  </Select>;
}

function EffortPicker({ label, value, stage = false, disabled, onChange }: {
  label: string; value: string; stage?: boolean; disabled: boolean; onChange: (value: string) => void;
}) {
  const selected = value || (stage ? "level" : "");
  const options = stage ? [...EFFORTS, "level"] : EFFORTS;
  const values = options.includes(selected) ? options : [...options, selected];
  return <ToggleGroup aria-label={label} value={[selected]} disabled={disabled} onValueChange={(next) => { if (next.length > 0) onChange(next[0] === "level" ? "" : next[0]!); }}>
    {values.map((effort) => <ToggleGroupItem key={effort} value={effort} aria-label={`${label}: ${effort || "level"}`}>{effort || "level"}</ToggleGroupItem>)}
  </ToggleGroup>;
}

function ModelPicker({ label, value, models, disabled, onChange }: {
  label: string; value: string; models: readonly PreferenceChoice[]; disabled: boolean; onChange: (value: string) => void;
}) {
  const [other, setOther] = React.useState(false);
  const [custom, setCustom] = React.useState(value);
  const anchor = React.useRef<HTMLDivElement>(null);
  useProjectSettingsDirty(other && custom !== value);
  const choices = models.filter((model) => model.id !== "auto");
  if (value && !choices.some((model) => model.id === value)) choices.push({ id: value, label: value, default: false, provider: "Saved model" });
  const groups = [...new Set(choices.map((model) => model.provider || "Other"))].map((provider) => ({
    provider, items: choices.filter((model) => (model.provider || "Other") === provider),
  }));
  groups.push({ provider: "Custom", items: [{ id: "", label: "Other model ID…", default: false }] });
  const selected = choices.find((model) => model.id === value) ?? null;
  return <div ref={anchor} className="w-full min-w-0 sm:w-72">
    <Combobox items={groups} value={selected} disabled={disabled} filter={(model, query) => model.id === "" || `${model.label} ${model.id}`.toLowerCase().includes(query.toLowerCase())} itemToStringLabel={(model: PreferenceChoice) => `${model.label} ${model.id}`} isItemEqualToValue={(a, b) => a.id === b.id}
      onValueChange={(model) => {
        if (!model) return;
        if (model.id === "") { setCustom(value); setOther(true); }
        else { onChange(model.id); setOther(false); }
      }}>
      <ComboboxTrigger render={<SelectButton size="sm" aria-label={label} />}>
        {selected ? <span className="inline-flex max-w-full items-baseline gap-2"><span className="truncate">{selected.label}</span>{selected.label !== selected.id ? <span className="truncate font-mono text-xs text-muted-foreground">{selected.id}</span> : null}</span> : "Choose a model"}
      </ComboboxTrigger>
      <ComboboxPopup anchor={anchor} className="w-80 max-w-[calc(100vw-2rem)]">
        <ComboboxSearchInput aria-label={`Search ${label.toLowerCase()}`} placeholder="Search models…" />
        <ComboboxEmpty>No models found.</ComboboxEmpty>
        <ComboboxList>{(group: typeof groups[number]) => <ComboboxGroup key={group.provider} items={group.items}>
          <ComboboxGroupLabel>{group.provider}</ComboboxGroupLabel>
          <ComboboxCollection>{(model: PreferenceChoice) => <ComboboxItem key={model.id} value={model}>
            <span className="flex flex-col"><span>{model.label}</span>{model.id !== "" ? <span className="font-mono text-xs text-muted-foreground">{model.id}</span> : null}</span>
          </ComboboxItem>}</ComboboxCollection>
        </ComboboxGroup>}</ComboboxList>
      </ComboboxPopup>
    </Combobox>
    {other ? <form className="mt-2 flex items-center gap-2" onSubmit={(event) => { event.preventDefault(); if (custom.trim()) { onChange(custom.trim()); setOther(false); } }}>
      <Input autoFocus size="sm" aria-label={`${label}: other model ID`} value={custom} disabled={disabled} onChange={(event) => setCustom(event.target.value)} />
      <Button type="submit" size="xs" disabled={disabled || !custom.trim()}>Use</Button>
      <Button type="button" size="xs" variant="ghost" disabled={disabled} onClick={() => setOther(false)}>Cancel</Button>
    </form> : null}
  </div>;
}

function modelChoice(model: string, selection: CloudSelection): string {
  if (model && model === selection.normal_model) return "normal";
  if (model && model === selection.complex_model) return "complex";
  return model;
}
function stageChoice(stage: SelectionStage, selection: CloudSelection): string {
  if (stage.model) return `model:${modelChoice(stage.model, selection)}`;
  if (stage.level) return `level:${stage.level}`;
  return stage.issue_complexity ? "issue" : "default";
}
function changeStageChoice(stage: SelectionStage, value: string): SelectionStage {
  const { model: _model, level: _level, ...rest } = stage;
  if (value.startsWith("model:")) return { ...rest, model: value.slice(6), issue_complexity: false };
  if (value.startsWith("level:")) return { ...rest, level: value.slice(6), issue_complexity: false };
  return { ...rest, issue_complexity: value === "issue" };
}

function FallbackChip({ name, index, disabled, onRemove }: { name: string; index: number; disabled: boolean; onRemove: () => void }) {
  const { attributes, listeners, setNodeRef, setActivatorNodeRef, transform, transition } = useSortable({ id: `${index}:${name}`, disabled });
  return <li ref={setNodeRef} style={{ transform: CSS.Transform.toString(transform), transition }} className="inline-flex items-center gap-1">
    <Button ref={setActivatorNodeRef} size="xs" variant="outline" disabled={disabled} className="touch-none" {...attributes} {...listeners} aria-label={`Reorder ${title(name)} fallback`}>
      <GripVerticalIcon aria-hidden />{index + 1} · {title(name)}
    </Button>
    <Button size="icon-micro" variant="ghost-muted" disabled={disabled} aria-label={`Remove ${title(name)} fallback`} onClick={onRemove}><XIcon aria-hidden /></Button>
  </li>;
}

export function ModelSelectionSettings({ projectId, canManage }: {
  readonly projectId?: string;
  readonly canManage: boolean;
}): React.ReactElement {
  const api = useAccountApi();
  const client = useClient();
  const account = useAccountBootstrap();
  const models = client.bootstrap?.preferences?.models ?? [];
  const labels = [...new Set((account?.projects ?? []).filter((project) => projectId === undefined || project.id === projectId).flatMap((project) => project.labels ?? []))].sort();
  const resource = useResource(() => api.modelSelection(projectId), [api, projectId]);
  const [draft, setDraft] = React.useState<CloudSelection | null>(null);
  const [override, setOverride] = React.useState(false);
  const [saved, setSaved] = React.useState<string | null>(null);
  const sensors = useSensors(useSensor(PointerSensor, { activationConstraint: { distance: 5 } }), useSensor(KeyboardSensor, { coordinateGetter: sortableKeyboardCoordinates }));
  React.useEffect(() => {
    if (resource.value === undefined) return;
    setDraft(resource.value.effective);
    setOverride(resource.value.selection !== null);
  }, [resource.value]);
  const save = useMutation(async () => {
    if (resource.value === undefined || draft === null) return;
    try {
      const next = await api.saveModelSelection({ projectId, revision: resource.value.revision, selection: projectId !== undefined && !override ? null : draft, key: newKey() });
      resource.set(next);
      setSaved(next.revision);
    } catch (cause) {
      if (cause instanceof AccountError && cause.isConflict) await resource.refresh();
      throw cause;
    }
  });
  const update = (next: CloudSelection) => { setDraft(next); setSaved(null); save.clearError(); };
  const disabled = !canManage || save.pending || (projectId !== undefined && !override);
  const dirty = resource.value !== undefined && ((projectId !== undefined && override !== (resource.value.selection !== null)) || ((projectId === undefined || override) && JSON.stringify(draft) !== JSON.stringify(resource.value.effective)));
  useProjectSettingsDirty(draft !== null && dirty);
  if (draft === null) return <SettingsSection title="Model selection" tabIndex={canManage ? undefined : 0}><p role="status" className="px-3 text-sm text-muted-foreground">{resource.error?.message ?? "Loading model selection."}</p></SettingsSection>;
  const levels = ordered(draft.levels ?? {}, LEVEL_ORDER);
  const defaultLevel = draft.default_level ?? "normal";
  const modelOptions = [{ value: "normal", label: "Normal model" }, { value: "complex", label: "Complex model" }];
  const stages = ordered(draft.stages ?? {}, STAGE_ORDER);
  const fallback = draft.fallback_order ?? [];
  const fallbackDisabled = disabled || (draft.unavailable ?? "fallback") !== "fallback";
  const changeLabels = (ruleIndex: number, include: readonly string[]) => update({ ...draft, rules: draft.rules?.map((rule, index) => index === ruleIndex ? { ...rule, selector: { ...rule.selector, labels: { ...rule.selector?.labels, include } } } : rule) });
  const addLabel = (level: string, label: string) => {
    const index = draft.rules?.findIndex((rule) => rule.level === level && !rule.disabled) ?? -1;
    if (index >= 0) changeLabels(index, [...(draft.rules?.[index]?.selector?.labels?.include ?? []), label]);
    else {
      let name = level;
      while (draft.rules?.some((rule) => rule.name === name)) name += "_labels";
      update({ ...draft, rules: [...(draft.rules ?? []), { name, level, selector: { labels: { include: [label] } } }] });
    }
  };
  return <div className="min-w-0 space-y-6">
    <SettingsSection title="Model selection" tabIndex={canManage ? undefined : 0}>
      {projectId !== undefined ? <SettingsRow title="Project override" description="Use project settings instead of the organization default. Turn off to inherit the latest organization settings."
        control={<ToggleControl label="Override organization model selection" checked={override} disabled={!canManage || save.pending} onCheckedChange={(checked) => { setOverride(checked); setSaved(null); save.clearError(); }} />} /> : null}
      <SettingsRow title="Automatic selection" description="Pick a model per issue from its complexity. Per-issue effort still stops at each stage’s ceiling."
        control={<ToggleControl label="Automatic model selection" checked={draft.enabled ?? false} disabled={disabled} onCheckedChange={(enabled) => update({ ...draft, enabled })} />} />
    </SettingsSection>
    <SettingsSection title="Models">
      {(["normal_model", "complex_model"] as const).map((field) => <SettingsRow key={field} title={title(field)} description={field === "normal_model" ? "Everyday implementation work." : "Planning, validation and work labelled very complex."}
        control={<ModelPicker label={title(field)} value={draft[field] ?? ""} models={models} disabled={disabled} onChange={(value) => update({ ...draft, [field]: value })} />} />)}
    </SettingsSection>
    <SettingsSection title="Complexity levels">
      <RadioGroup onKeyDown={radioKeys} aria-label="Default complexity" value={defaultLevel} disabled={disabled} onValueChange={(default_level) => { if (typeof default_level === "string") update({ ...draft, default_level }); }}>
        <Table aria-label="Complexity levels" className="min-w-3xl" tabIndex={disabled ? 0 : undefined}>
          <TableHeader><TableRow>{["Level", "Model", "Effort ceiling", "Applied by label", "Default"].map((heading) => <TableHead key={heading}>{heading}</TableHead>)}</TableRow></TableHeader>
          <TableBody>{levels.map(([name, level]) => {
            const rules = (draft.rules ?? []).map((rule, index) => ({ rule, index })).filter(({ rule }) => rule.level === name);
            const applied = rules.flatMap(({ rule }) => rule.selector?.labels?.include ?? []);
            return <TableRow key={name}>
              <TableCell className="font-medium whitespace-nowrap">{title(name)}</TableCell>
              <TableCell {...isolateControl}><Choice label={`${title(name)} complexity model`} value={modelChoice(level.model ?? "", draft)} options={modelOptions} disabled={disabled} onChange={(model) => update({ ...draft, levels: { ...draft.levels, [name]: { ...level, model } } })} /></TableCell>
              <TableCell {...isolateControl}><EffortPicker label={`${title(name)} complexity effort ceiling`} value={level.effort ?? "high"} disabled={disabled} onChange={(effort) => update({ ...draft, levels: { ...draft.levels, [name]: { ...level, effort } } })} /></TableCell>
              <TableCell {...isolateControl}><div className="flex flex-wrap items-center gap-1">
                {rules.flatMap(({ rule, index }) => (rule.selector?.labels?.include ?? []).map((label, labelIndex) => <Badge key={`${index}:${labelIndex}`} variant="secondary">
                  {label}{rule.disabled ? " (disabled)" : ""}<Button size="icon-micro" variant="ghost-muted" disabled={disabled} aria-label={`Remove ${label} from ${title(name)}`} onClick={() => changeLabels(index, rule.selector!.labels!.include!.filter((_, i) => i !== labelIndex))}><XIcon aria-hidden /></Button>
                </Badge>))}
                <Choice label={`Add label to ${title(name)}`} value="" options={[{ value: "", label: "Add label" }, ...labels.filter((label) => !applied.includes(label)).map((label) => ({ value: label, label }))]} disabled={disabled || labels.every((label) => applied.includes(label))} onChange={(label) => { if (label) addLabel(name, label); }} />
              </div></TableCell>
              <TableCell><Radio value={name} aria-label={`${title(name)} is the default complexity`} /></TableCell>
            </TableRow>;
          })}</TableBody>
        </Table>
      </RadioGroup>
    </SettingsSection>
    <SettingsSection title="Stages">
      <Table aria-label="Stages" className="min-w-2xl" tabIndex={disabled ? 0 : undefined}>
        <TableHeader><TableRow><TableHead>Stage</TableHead><TableHead>Model</TableHead><TableHead>Effort ceiling</TableHead></TableRow></TableHeader>
        <TableBody>{stages.map(([name, stage]) => <TableRow key={name}>
          <TableCell className="font-medium whitespace-nowrap">{title(name)}</TableCell>
          <TableCell><Choice label={`${title(name)} stage model`} value={stageChoice(stage, draft)} options={[
            { value: "issue", label: "Follow issue level" }, { value: "default", label: `Default level (${title(defaultLevel)})` },
            ...modelOptions.map((option) => ({ ...option, value: `model:${option.value}` })),
            ...(stage.model && !["normal", "complex"].includes(modelChoice(stage.model, draft)) ? [{ value: `model:${stage.model}`, label: stage.model }] : []),
            ...(stage.level ? [{ value: `level:${stage.level}`, label: `${title(stage.level)} level` }] : []),
          ]} disabled={disabled} onChange={(value) => update({ ...draft, stages: { ...draft.stages, [name]: changeStageChoice(stage, value) } })} /></TableCell>
          <TableCell><EffortPicker label={`${title(name)} stage effort ceiling`} value={stage.effort ?? ""} stage disabled={disabled} onChange={(effort) => update({ ...draft, stages: { ...draft.stages, [name]: { ...stage, effort } } })} /></TableCell>
        </TableRow>)}</TableBody>
      </Table>
    </SettingsSection>
    <SettingsSection title="When a model is unavailable">
      <RadioGroup onKeyDown={radioKeys} aria-label="Unavailable model" value={draft.unavailable ?? "fallback"} disabled={disabled} onValueChange={(unavailable) => { if (typeof unavailable === "string") update({ ...draft, unavailable }); }}>
        <div className="space-y-3 p-3 sm:p-4">
          <label className="flex items-center gap-2 text-sm font-medium"><Radio value="fallback" />Fall back to another level</label>
          <div {...isolateControl} className="space-y-3">
          <p className="text-xs text-muted-foreground">Try these levels in order. Drag to reorder, or press Space on a chip and use the arrow keys.</p>
          <DndContext sensors={sensors} collisionDetection={closestCenter} onDragEnd={({ active, over }) => {
            if (fallbackDisabled || over === null || active.id === over.id) return;
            const ids = fallback.map((name, index) => `${index}:${name}`);
            update({ ...draft, fallback_order: arrayMove([...fallback], ids.indexOf(String(active.id)), ids.indexOf(String(over.id))) });
          }}>
            <SortableContext items={fallback.map((name, index) => `${index}:${name}`)} strategy={rectSortingStrategy}>
              <ol aria-label="Fallback order" className="flex flex-wrap items-center gap-2">
                {fallback.map((name, index) => <FallbackChip key={`${index}:${name}`} name={name} index={index} disabled={fallbackDisabled} onRemove={() => update({ ...draft, fallback_order: fallback.filter((_, i) => i !== index) })} />)}
              </ol>
            </SortableContext>
          </DndContext>
          <div className="w-fit"><Choice label="Add fallback level" value="" options={[{ value: "", label: "Add level" }, ...levels.filter(([name]) => !fallback.includes(name)).map(([name]) => ({ value: name, label: title(name) }))]} disabled={fallbackDisabled || levels.every(([name]) => fallback.includes(name))} onChange={(name) => { if (name) update({ ...draft, fallback_order: [...fallback, name] }); }} /></div>
          </div>
        </div>
        <label className="flex items-center gap-2 border-t border-border/50 p-3 text-sm font-medium sm:p-4"><Radio value="fail" />Stop with an error</label>
      </RadioGroup>
    </SettingsSection>
    <ControlError message={save.error?.isConflict ? "These settings changed. Review the latest values and save again." : save.error?.message ?? null} />
    {saved === resource.value?.revision ? <p role="status" className="text-sm text-muted-foreground">Model selection saved.</p> : null}
    {dirty && canManage ? <div role="region" aria-label="Unsaved model selection" className="sticky bottom-3 z-10 flex flex-wrap items-center justify-between gap-2 rounded-xl border border-border bg-card p-3 shadow-lg">
      <p role="status" className="text-sm">You have unsaved changes</p>
      <div className="flex items-center gap-2">
        <Button size="sm" variant="ghost" disabled={save.pending} onClick={() => { if (resource.value) { setDraft(resource.value.effective); setOverride(resource.value.selection !== null); setSaved(null); save.clearError(); } }}>Discard</Button>
        <Button size="sm" disabled={save.pending} onClick={() => void save.call()}>{save.pending ? "Saving…" : "Save changes"}</Button>
      </div>
    </div> : null}
  </div>;
}
