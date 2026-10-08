import React from "react";
import { Button } from "../../../components/ui/button.tsx";
import { Select, SelectTrigger, SelectValue, SelectPopup, SelectItem } from "../../../components/ui/select.tsx";
import type { SourceRecovery } from "../../../contracts/work.ts";
import { newWorkKey, WorkApiError, type WorkHttp } from "../lib/workHttp.ts";

export function SourceRecoveryControls(props: {
  readonly http: WorkHttp;
  readonly projectId: string;
  readonly itemId: string;
  readonly revision: string;
  readonly canManage: boolean;
  readonly onTransferred: () => void;
}): React.ReactElement | null {
  const [source, setSource] = React.useState<SourceRecovery | null>(null);
  const [destination, setDestination] = React.useState("");
  const [error, setError] = React.useState<string | null>(null);
  const [saving, setSaving] = React.useState(false);
  const command = React.useRef<{ identity: string; key: string } | null>(null);
  React.useEffect(() => {
    let active = true;
    setSource(null);
    void props.http.getSourceRecovery(props.projectId, props.itemId).then((view) => {
      if (active) { setSource(view); setDestination(view.destination_runner_id); setError(null); }
    }).catch((failure: unknown) => { if (active) setError(failure instanceof Error ? failure.message : "Recovery information is unavailable"); });
    return () => { active = false; };
  }, [props.http, props.projectId, props.itemId, props.revision]);

  async function transfer(selected: string): Promise<void> {
    if (source === null || saving) return;
    const identity = `${source.revision}:${source.version_id}:${selected}`;
    if (command.current?.identity !== identity) command.current = { identity, key: newWorkKey("source-transfer") };
    setSaving(true); setError(null);
    try {
      const updated = await props.http.transferSource({ projectId: props.projectId, itemId: props.itemId, key: command.current.key, expectedRevision: source.revision, versionId: source.version_id, destinationRunnerId: selected });
      setSource(updated); setDestination(updated.destination_runner_id); command.current = null; props.onTransferred();
    } catch (failure) {
      if (failure instanceof WorkApiError && failure.conflict) {
        try { setSource(await props.http.getSourceRecovery(props.projectId, props.itemId)); setDestination(""); command.current = null; }
        catch { setSource(null); }
        setError("Source changed. Review the checkpoint and choose a destination again.");
      } else setError(failure instanceof Error ? failure.message : "Source transfer was refused");
    }
    finally { setSaving(false); }
  }

  if (source === null) return error === null ? null : <p role="alert" className="text-sm text-destructive">{error}</p>;
  if (source.attempt_id === "" && source.version_id === "") return null;
  return (
    <section aria-label="Source recovery" className="flex flex-col gap-3 rounded-[var(--radius)] border border-border bg-card p-4 text-sm">
      <h2 className="font-medium">Source recovery</h2>
      <p className="break-all text-muted-foreground">Source runner: {source.source_runner_name || source.source_runner_id || source.source_machine_id || "Unknown"}</p>
      <p>Checkpoint: {source.available ? "Verified retained source" : "Source capture required"}{source.head_sha !== "" ? ` · ${source.head_sha.slice(0, 12)}` : ""}</p>
      <p>Execution: {source.quiesced ? "Stopped and lease released" : "Active or not confirmed stopped"}</p>
      <p className="text-muted-foreground">{source.reason}</p>
      {props.canManage && source.available && source.quiesced ? (
        <>
          <Select value={destination || null} onValueChange={(value) => setDestination(value ?? "")} disabled={saving}>
            <SelectTrigger aria-label="Destination runner"><SelectValue placeholder="Choose a destination runner" /></SelectTrigger>
            <SelectPopup>{source.destinations.map((runner) => <SelectItem key={runner.runner_id} value={runner.runner_id}>{runner.name || runner.runner_id}</SelectItem>)}</SelectPopup>
          </Select>
          <p className="text-muted-foreground">The destination restores and verifies this source before continuing. Transfer preserves workflow, human and delivery holds.</p>
          <div className="flex flex-wrap gap-2">
            <Button size="sm" disabled={saving || destination === "" || destination === source.destination_runner_id} onClick={() => void transfer(destination)}>{saving ? "Transferring…" : "Transfer recovery"}</Button>
            {source.destination_runner_id !== "" ? <Button size="sm" variant="outline" disabled={saving} onClick={() => void transfer("")}>Use automatic routing</Button> : null}
          </div>
        </>
      ) : null}
      {error !== null ? <p role="alert" className="text-destructive">{error}</p> : null}
    </section>
  );
}
