import React from "react";

import { Button } from "../../components/ui/button.tsx";
import { Input } from "../../components/ui/input.tsx";
import type { CloudSelection } from "../../contracts/account.ts";
import { SettingsRow, SettingsSection } from "../settings/settingsLayout.tsx";
import { AccountError } from "./api.ts";
import { ControlError, NativeSelect, ToggleControl } from "./controls.tsx";
import { useAccountApi } from "./context.ts";
import { newKey } from "./idempotency.ts";
import { useMutation, useResource } from "./useResource.ts";

const EFFORTS = ["low", "medium", "high", "xhigh"].map((value) => ({ value, label: value }));
const words = (value: string) => value.split(",").map((word) => word.trim()).filter(Boolean);
const LEVEL_ORDER = ["normal", "complex", "very_complex"];
const STAGE_ORDER = ["plan", "code", "rework", "merge", "validator", "routine", "security_audit"];
function ordered<T>(values: Readonly<Record<string, T>>, order: readonly string[]): [string, T][] {
  return Object.entries(values).sort(([a], [b]) => {
    const rank = (name: string) => order.includes(name) ? order.indexOf(name) : order.length;
    return rank(a) - rank(b) || a.localeCompare(b);
  });
}
const title = (value: string) => value.replaceAll("_", " ").replace(/^./, (letter) => letter.toUpperCase());

export function ModelSelectionSettings({ projectId, canManage }: {
  readonly projectId?: string;
  readonly canManage: boolean;
}): React.ReactElement {
  const api = useAccountApi();
  const resource = useResource(() => api.modelSelection(projectId), [api, projectId]);
  const [draft, setDraft] = React.useState<CloudSelection | null>(null);
  const [override, setOverride] = React.useState(false);
  const [saved, setSaved] = React.useState<string | null>(null);
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
  const update = (next: CloudSelection) => { setDraft(next); setSaved(null); };
  const disabled = !canManage || save.pending || (projectId !== undefined && !override);
  if (draft === null) return <SettingsSection title="Model selection" tabIndex={canManage ? undefined : 0}><p role="status" className="px-3 text-sm text-muted-foreground">{resource.error?.message ?? "Loading model selection."}</p></SettingsSection>;
  return (
    <SettingsSection title="Model selection" tabIndex={canManage ? undefined : 0}>
      {projectId === undefined ? <SettingsRow title="Organization default" description="Projects inherit these models and effort ceilings unless they have a Cloud override." /> : (
        <SettingsRow title="Project override" description="Use project settings instead of the organization default. Turn off to inherit the latest organization settings."
          control={<ToggleControl label="Override organization model selection" checked={override} disabled={!canManage || save.pending} onCheckedChange={(checked) => { setOverride(checked); setSaved(null); }} />} />
      )}
      <SettingsRow title="Automatic selection" description="Per-issue effort still clamps to the selected stage’s ceiling."
        control={<ToggleControl label="Automatic model selection" checked={draft.enabled ?? false} disabled={disabled} onCheckedChange={(enabled) => update({ ...draft, enabled })} />} />
      {(["normal_model", "complex_model"] as const).map((field) => <SettingsRow key={field} title={field === "normal_model" ? "Normal model" : "Complex model"}
        control={<Input aria-label={title(field)} value={draft[field] ?? ""} disabled={disabled} onChange={(event) => update({ ...draft, [field]: event.target.value })} />} />)}
      {ordered(draft.levels ?? {}, LEVEL_ORDER).map(([name, level]) => <SettingsRow key={name} title={`${title(name)} level`}
        control={<div className="flex flex-wrap items-center justify-end gap-2">
          <Input className="w-44" aria-label={`${title(name)} complexity model`} value={level.model ?? ""} disabled={disabled} onChange={(event) => update({ ...draft, levels: { ...draft.levels, [name]: { ...level, model: event.target.value } } })} />
          <NativeSelect className="w-28 min-w-0" aria-label={`${title(name)} complexity effort ceiling`} value={level.effort ?? "high"} options={EFFORTS} disabled={disabled} onValueChange={(effort) => update({ ...draft, levels: { ...draft.levels, [name]: { ...level, effort } } })} />
        </div>} />)}
      {ordered(draft.stages ?? {}, STAGE_ORDER).map(([name, stage]) => <SettingsRow key={name} title={`${title(name)} stage`}
        control={<div className="flex flex-wrap items-center justify-end gap-2">
          <Input className="w-44" aria-label={`${title(name)} stage model`} placeholder="Use complexity model" value={stage.model ?? ""} disabled={disabled} onChange={(event) => update({ ...draft, stages: { ...draft.stages, [name]: { ...stage, model: event.target.value } } })} />
          <NativeSelect className="w-52" aria-label={`${title(name)} stage effort ceiling`} value={stage.effort ?? ""} options={[{ value: "", label: "Use complexity ceiling" }, ...EFFORTS]} disabled={disabled} onValueChange={(effort) => update({ ...draft, stages: { ...draft.stages, [name]: { ...stage, effort } } })} />
          <label className="flex items-center gap-2 text-xs text-muted-foreground">Issue complexity<ToggleControl label={`${title(name)} uses issue complexity`} checked={stage.issue_complexity ?? false} disabled={disabled} onCheckedChange={(issue_complexity) => update({ ...draft, stages: { ...draft.stages, [name]: { ...stage, issue_complexity } } })} /></label>
        </div>} />)}
      {(draft.rules ?? []).map((rule, index) => <SettingsRow key={rule.name} title={`${title(rule.name)} label rule`}
        control={<Input aria-label={`${title(rule.name)} labels`} value={rule.selector?.labels?.include?.join(", ") ?? ""} disabled={disabled} onChange={(event) => update({ ...draft, rules: draft.rules?.map((current, i) => i === index ? { ...current, selector: { ...current.selector, labels: { ...current.selector?.labels, include: words(event.target.value) } } } : current) })} />} />)}
      <SettingsRow title="Default complexity" control={<NativeSelect aria-label="Default complexity" value={draft.default_level ?? "normal"} options={Object.keys(draft.levels ?? {}).map((value) => ({ value, label: title(value) }))} disabled={disabled} onValueChange={(default_level) => update({ ...draft, default_level })} />} />
      <SettingsRow title="Unavailable model" control={<NativeSelect aria-label="Unavailable model" value={draft.unavailable ?? "fallback"} options={[{ value: "fallback", label: "Use fallback" }, { value: "fail", label: "Stop with an error" }]} disabled={disabled} onValueChange={(unavailable) => update({ ...draft, unavailable })} />} />
      <SettingsRow title="Fallback order" description="Complexity levels, separated by commas." control={<Input aria-label="Fallback order" value={draft.fallback_order?.join(", ") ?? ""} disabled={disabled} onChange={(event) => update({ ...draft, fallback_order: words(event.target.value) })} />} />
      <SettingsRow title="Save model selection" status={<><ControlError message={save.error?.isConflict ? "These settings changed. Review the latest values and save again." : save.error?.message ?? null} />{saved === resource.value?.revision ? <span role="status">Model selection saved.</span> : null}</>}
        control={<Button size="sm" disabled={!canManage || save.pending} onClick={() => void save.call()}>{save.pending ? "Saving…" : "Save model selection"}</Button>} />
    </SettingsSection>
  );
}
