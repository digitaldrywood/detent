import React from "react";

import { Button } from "../../components/ui/button.tsx";
import { useAccountApi } from "../account/context.ts";
import { useResource } from "../account/useResource.ts";
import { AccountError } from "../account/api.ts";
import { SettingsRow } from "./settingsLayout.tsx";

export function ProjectRankSettings(): React.ReactElement {
  const api = useAccountApi();
  const rank = useResource(() => api.projectRank(), [api]);
  const projects = useResource(() => api.projects(), [api]);
  const [draft, setDraft] = React.useState<readonly string[]>([]);
  const [saving, setSaving] = React.useState(false);
  const [error, setError] = React.useState("");
  React.useEffect(() => { if (rank.value) setDraft(rank.value.project_ids); }, [rank.value]);
  const names = new Map(projects.value?.map((project) => [project.id, project.name]));
  function move(index: number, offset: number): void {
    setDraft((current) => {
      const next = [...current];
      const target = index + offset;
      if (target < 0 || target >= next.length) return current;
      [next[index], next[target]] = [next[target]!, next[index]!];
      return next;
    });
  }
  async function save(): Promise<void> {
    if (!rank.value || saving) return;
    setSaving(true);
    setError("");
    try {
      await api.updateProjectRank({ expectedRevision: rank.value.revision, projectIds: draft });
      await rank.refresh();
    } catch (cause) {
      if (cause instanceof AccountError && cause.isConflict) {
        await rank.refresh();
        setError("Project rank changed. Review the reloaded order and save again.");
      } else setError(cause instanceof Error ? cause.message : "Project rank could not be saved.");
    } finally { setSaving(false); }
  }
  return <SettingsRow id="settings-project-rank" title="Project rank" description="Issue priority comes first, then project rank, then oldest issue. Running work keeps its slots." control={<div className="space-y-3">
    {rank.value ? <>
      <ol aria-label="Project rank" className="space-y-2">
        {draft.map((id, index) => <li key={id} className="flex items-center gap-2">
          <span className="min-w-0 flex-1 break-words">{index + 1}. {names.get(id) ?? id}</span>
          <Button type="button" size="sm" variant="outline" aria-label={`Move ${names.get(id) ?? id} up`} disabled={saving || index === 0} onClick={() => move(index, -1)}>↑</Button>
          <Button type="button" size="sm" variant="outline" aria-label={`Move ${names.get(id) ?? id} down`} disabled={saving || index === draft.length - 1} onClick={() => move(index, 1)}>↓</Button>
        </li>)}
      </ol>
      <Button size="sm" disabled={saving} onClick={() => void save()}>{saving ? "Saving…" : "Save project rank"}</Button>
    </> : rank.error ? <p role="alert">{rank.error.message}</p> : <p>Loading project rank…</p>}
    {error ? <p role="alert">{error}</p> : null}
  </div>} />;
}
