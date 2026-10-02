import React from "react";
import { Button } from "../../components/ui/button.tsx";
import { Input } from "../../components/ui/input.tsx";
import { Checkbox } from "../../components/ui/checkbox.tsx";
import type { IntakeCommand, IssueIntake } from "../../contracts/githubIntake.ts";
import { ControlError, NativeSelect } from "./controls.tsx";
import { useAccountApi } from "./context.ts";
import { setupKey, retireKey } from "./idempotency.ts";
import { useMutation, useResource } from "./useResource.ts";

export function GitHubIssueIntake({ projectId, repository, runners }: {
  readonly projectId: string;
  readonly repository: string;
  readonly runners: readonly { readonly id: string; readonly name: string }[];
}): React.ReactElement {
  const api = useAccountApi();
  const resource = useResource<IssueIntake>(() => api.issueIntake(projectId), [api, projectId]);
  const [runner, setRunner] = React.useState("");
  const [labels, setLabels] = React.useState("");
  const [closed, setClosed] = React.useState(false);
  const [selection, setSelection] = React.useState<readonly number[]>([]);
  const [numbers, setNumbers] = React.useState("");
  const [destination, setDestination] = React.useState("");
  const [dispatch, setDispatch] = React.useState(false);
  const batch = resource.value?.batch;
  const lanes = resource.value?.lanes ?? [];
  const laneName = destination || lanes.find(lane => !lane.dispatchable && !lane.terminal)?.name || "";
  const lane = lanes.find(lane => lane.name === laneName);
  const command = useMutation(async (input: Omit<IntakeCommand, "revision">) => {
    const body = { ...input, revision: batch?.revision ?? 0 };
    const key = await setupKey(`/projects/${projectId}/onboarding/issue-intake`, body);
    const result = await api.commandIssueIntake(projectId, body, key.key);
    retireKey(key.storageKey);
    resource.set(result);
    if (input.action === "discover") { setSelection([]); setNumbers(""); }
    return result;
  });
  const busy = command.pending;
  const working = batch?.status === "discovering" || batch?.status === "importing";
  const completed = batch?.items.filter(item => item.status === "completed").length ?? 0;
  const skipped = batch?.items.filter(item => item.status === "skipped").length ?? 0;
  const failed = batch?.items.filter(item => item.status === "failed").length ?? 0;
  return <section aria-label="Import GitHub issues" className="mb-3 flex min-w-0 flex-col gap-3 rounded-xl border border-border/60 bg-muted p-4">
    <h3 className="text-sm font-medium">Import existing GitHub issues</h3>
    <p className="text-sm text-muted-foreground">Read selected issues from {repository} using your runner. Discussion and workflow continue in Hub after import. GitHub stays a historical link; this does not close issues or keep syncing.</p>
    <label className="text-sm">Runner
      <NativeSelect aria-label="Import runner" value={runner || batch?.runner_id || runners[0]?.id || ""} onValueChange={setRunner} options={runners.map(item => ({ value: item.id, label: item.name }))} />
    </label>
    <label className="text-sm">GitHub labels (comma separated)
      <Input aria-label="GitHub label filters" value={labels} onChange={event => setLabels(event.currentTarget.value)} disabled={working || busy} />
    </label>
    <label className="flex items-center gap-2 text-sm"><Checkbox aria-label="Include closed history" checked={closed} onCheckedChange={value => setClosed(value === true)} disabled={working || busy} />Include closed issues and history</label>
    <div className="flex flex-wrap gap-2">
      <Button variant="outline" disabled={working || busy || runners.length === 0} onClick={() => void command.call({ action: "discover", runner_id: runner || batch?.runner_id || runners[0]?.id, labels: labels.split(",").map(label => label.trim()).filter(Boolean), include_closed: closed })}>Preview issues</Button>
      <Button variant="outline" disabled={resource.loading || busy} onClick={() => void resource.refresh()}>Refresh progress</Button>
    </div>
    {working && runner !== "" && runner !== batch?.runner_id ? <Button variant="outline" disabled={busy} onClick={() => void command.call({ action: "retry", runner_id: runner })}>Resume on selected runner</Button> : null}
    {working ? <p role="status" className="text-sm">Waiting for runner intake. Refresh progress to see the next result.</p> : null}
    {batch?.error ? <p role="alert" className="text-sm text-destructive">{batch.error}{batch.retry_at ? ` · Retry after ${batch.retry_at}` : ""}</p> : null}
    {batch?.status === "preview" ? <>
      <p className="text-sm">{batch.page.total} matching issues · {batch.page.issues.length} previewed · {selection.length} selected. {batch.discovery.include_closed ? "Open and closed history" : "Open issues only"}{batch.discovery.labels?.length ? ` · Labels: ${batch.discovery.labels.join(", ")}` : ""}.</p>
      <div className="max-h-72 overflow-y-auto rounded-lg border border-border bg-background">
        {batch.page.issues.map(issue => <div key={issue.id} className="border-b border-border/60 p-3 last:border-b-0">
          <label className="flex items-start gap-2 text-sm"><Checkbox aria-label={`Select issue ${issue.number}`} checked={selection.includes(issue.number)} onCheckedChange={checked => setSelection(current => checked === true ? [...current, issue.number] : current.filter(number => number !== issue.number))} disabled={busy} /><span className="min-w-0 break-words">#{issue.number} {issue.title} · {issue.closed ? "Closed" : "Open"}</span></label>
          <details className="mt-1 text-xs text-muted-foreground"><summary>Source preview</summary><p className="mt-2 whitespace-pre-wrap break-words">{issue.body || "No source description"}</p><a className="underline" href={issue.url} target="_blank" rel="noreferrer">View original issue</a></details>
        </div>)}
      </div>
      <div className="flex flex-wrap gap-2">
        <Button variant="outline" disabled={busy} onClick={() => setSelection(batch.page.issues.map(issue => issue.number))}>Select previewed</Button>
        <Button variant="outline" disabled={busy || batch.page.next_cursor !== "" || batch.page.issues.length !== batch.page.total} onClick={() => setSelection(batch.page.issues.map(issue => issue.number))}>Select all matching</Button>
        <Button variant="outline" disabled={busy || batch.page.next_cursor === "" || batch.page.issues.length >= 1000} onClick={() => void command.call({ action: "more" })}>Load next 100</Button>
      </div>
      {batch.page.next_cursor ? <p className="text-xs text-muted-foreground">Load remaining pages to select all matching. Each intake previews up to 1,000 issues; use labels to narrow larger repositories.</p> : null}
      <label className="text-sm">Select by issue number
        <Input aria-label="Select by issue number" placeholder="12, 34, 56" value={numbers} onChange={event => { const value = event.currentTarget.value; setNumbers(value); const wanted = value.split(/[\s,]+/).map(Number); setSelection(batch.page.issues.filter(issue => wanted.includes(issue.number)).map(issue => issue.number)); }} disabled={busy} />
      </label>
      <label className="text-sm">Destination lane
        <NativeSelect aria-label="Import destination lane" value={laneName} onValueChange={value => { setDestination(value); setDispatch(false); }} disabled={busy} options={[{ value: "", label: "Select a lane" }, ...lanes.map(item => ({ value: item.name, label: `${item.name}${item.dispatchable ? " (dispatchable)" : ""}` }))]} />
      </label>
      {lane?.dispatchable ? <label className="flex items-center gap-2 text-sm"><Checkbox aria-label="Authorize dispatch of selected issues" checked={dispatch} onCheckedChange={value => setDispatch(value === true)} disabled={busy} />I authorize execution of these selected issues in {laneName}.</label> : null}
      <p className="text-sm">Import {selection.length} selected issues into {laneName || "a selected lane"}. Already linked issues will be skipped. {lane?.dispatchable ? "These issues can start execution." : "Move imported issues to a dispatchable lane when you want execution."}</p>
      <Button disabled={busy || selection.length === 0 || !lane || (lane.dispatchable && !dispatch)} onClick={() => void command.call({ action: "apply", numbers: selection, destination: laneName, allow_dispatch: dispatch })}>Import selected issues</Button>
    </> : null}
    {batch && batch.items.length > 0 ? <>
      <p role="status" className="text-sm">{completed} completed · {skipped} skipped · {failed} incomplete · {batch.items.filter(item => item.status === "pending").length} pending · Destination: {batch.destination}</p>
      {batch.items.filter(item => item.error).map(item => <p key={item.number} className="text-sm text-destructive">#{item.number}: {item.error}{item.retry_at ? ` · Retry after ${item.retry_at}` : ""}</p>)}
    </> : null}
    {(failed > 0 || batch?.status === "discovery_failed") && !working ? <Button variant="outline" disabled={busy} onClick={() => void command.call({ action: "retry", runner_id: runner || batch?.runner_id })}>Retry incomplete intake</Button> : null}
    <ControlError message={command.error?.message ?? resource.error?.message ?? null} />
  </section>;
}
