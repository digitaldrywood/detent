import React from "react";

import { Button } from "../../components/ui/button.tsx";
import { Input } from "../../components/ui/input.tsx";
import { SettingsRow } from "../settings/settingsLayout.tsx";
import { useAccountApi } from "./context.ts";
import { ControlError } from "./controls.tsx";
import { useResource } from "./useResource.ts";

export function SpritePoolCard({ projectId, projectName, canManage }: {
  readonly projectId: string;
  readonly projectName?: string;
  readonly canManage: boolean;
}): React.ReactElement {
  const api = useAccountApi();
  const pool = useResource(() => api.spritePool(projectId), [api, projectId]);
  const [floor, setFloor] = React.useState(0);
  const [ceiling, setCeiling] = React.useState(0);
  const [idle, setIdle] = React.useState(300);
  const [bootstrap, setBootstrap] = React.useState("");
  const [pending, setPending] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);

  React.useEffect(() => {
    if (pool.value === undefined) return;
    setFloor(pool.value.min_runners);
    setCeiling(pool.value.max_runners);
    setIdle(pool.value.idle_seconds);
    setBootstrap(pool.value.bootstrap);
  }, [pool.value?.revision]);

  async function save(): Promise<void> {
    if (pending || pool.value === undefined) return;
    setPending(true);
    setError(null);
    try {
      pool.set(await api.setSpritePool(projectId, { min_runners: floor, max_runners: ceiling, idle_seconds: idle, bootstrap, revision: pool.value.revision }));
    } catch {
      setError("Could not save the pool. Reload its settings and try again.");
    } finally {
      setPending(false);
    }
  }

  return <SettingsRow
    title={projectName === undefined ? "Sprite pool" : `Sprite pool · ${projectName}`}
    description="The Hub creates fresh runners as queued work needs them and deletes idle members above the minimum. Set the maximum to zero to disable provisioning."
    status={pool.value === undefined ? (pool.error === null ? "Loading…" : "Could not load the pool.") : `${pool.value.members.filter((member) => member.state !== "deleted").length} members`}
  ><div className="w-full space-y-4 py-4">
      {canManage && pool.value !== undefined ? <form className="space-y-3" onSubmit={(event) => { event.preventDefault(); void save(); }}>
        <div className="grid gap-3 sm:grid-cols-3">
          <label className="space-y-1 text-sm">Minimum runners<Input type="number" min={0} max={100} required value={floor} disabled={pending} onChange={(event) => setFloor(Number(event.target.value))} /></label>
          <label className="space-y-1 text-sm">Maximum runners<Input type="number" min={floor} max={100} required value={ceiling} disabled={pending} onChange={(event) => setCeiling(Number(event.target.value))} /></label>
          <label className="space-y-1 text-sm">Idle seconds<Input type="number" min={30} max={86400} required value={idle} disabled={pending} onChange={(event) => setIdle(Number(event.target.value))} /></label>
        </div>
        <label className="block space-y-1 text-sm">Customer bootstrap
          <textarea className="min-h-32 w-full rounded-md border border-border bg-background p-2 font-mono text-xs" value={bootstrap} maxLength={65536} required={ceiling > 0} disabled={pending} onChange={(event) => setBootstrap(event.target.value)} />
        </label>
        <p className="text-xs text-muted-foreground">These shell steps run before runner enrollment. Configure legitimate provider authentication, Git access and the project checkout. Use your secret delivery service; do not paste credentials here. DETENT_PROJECT_ID and DETENT_HUB_URL are available. Runner policy approval is still required.</p>
        <Button type="submit" size="sm" disabled={pending}>{pending ? "Saving…" : "Save pool"}</Button>
      </form> : null}
      {error === null ? null : <ControlError message={error} />}
      <Button type="button" size="sm" variant="outline" disabled={pending} onClick={() => void pool.refresh()}>Reload pool</Button>
      {pool.value?.members.map((member) => <div key={member.name} className="space-y-1 rounded-md border border-border p-3 text-sm">
        <p className="break-all font-medium">{member.name} · {member.state}</p>
        {member.runner_id === "" ? <p className="text-xs text-muted-foreground">Runner has not enrolled.</p> : <p className="text-xs text-muted-foreground">Enrolled; work readiness follows runner health, provider setup and project policy.</p>}
        <details><summary className="cursor-pointer text-xs">Last bootstrap log</summary><pre className="mt-2 whitespace-pre-wrap break-words text-xs">{member.bootstrap_log || "No bootstrap progress recorded."}</pre></details>
      </div>)}
    </div>
  </SettingsRow>;
}
