import React from "react";

import { Button } from "../../components/ui/button.tsx";
import { Alert, AlertTitle, AlertDescription } from "../../components/ui/alert.tsx";
import { Dialog, DialogTrigger, DialogPopup, DialogHeader, DialogTitle, DialogDescription, DialogPanel } from "../../components/ui/dialog.tsx";
import { Input } from "../../components/ui/input.tsx";
import { SettingsRow, SettingsWarning } from "../settings/settingsLayout.tsx";
import { AccountError } from "./api.ts";
import { useAccountApi, useAccountBootstrap } from "./context.ts";
import { useResource } from "./useResource.ts";

export function SpritePoolCard({ projectId, canManage, compact = false, attentionOnly = false, onAttentionChange }: {
  readonly projectId: string;
  readonly canManage: boolean;
  readonly compact?: boolean;
  readonly attentionOnly?: boolean;
  readonly onAttentionChange?: (projectId: string, needsAttention: boolean) => void;
}): React.ReactElement {
  const api = useAccountApi();
  const account = useAccountBootstrap();
  const token = useResource(() => api.spritesSecret(""), [api]);
  const pool = useResource(() => api.spritePool(), [api]);
  const [floor, setFloor] = React.useState(0);
  const [ceiling, setCeiling] = React.useState(0);
  const [idle, setIdle] = React.useState(300);
  const [bootstrap, setBootstrap] = React.useState("");
  const [pending, setPending] = React.useState(false);
  const [error, setError] = React.useState<AccountError | null>(null);
  const failure = error ?? pool.error;
  const forbidden = failure?.status === 403;
  const readOnly = !canManage || forbidden;
  const unavailable = failure !== null && !forbidden;

  React.useEffect(() => {
    if (pool.value === undefined) return;
    setFloor(pool.value.min_runners);
    setCeiling(pool.value.max_runners);
    setIdle(pool.value.idle_seconds);
    setBootstrap(pool.value.bootstrap);
  }, [pool.value, readOnly]);

  async function reload(): Promise<void> {
    setError((current) => current?.status === 403 ? current : null);
    await Promise.all([pool.refresh(), token.refresh()]);
  }

  async function save(): Promise<void> {
    if (pending || readOnly || pool.loading || pool.value === undefined) return;
    setPending(true);
    setError(null);
    try {
      pool.set(await api.setSpritePool({ min_runners: floor, max_runners: ceiling, idle_seconds: idle, bootstrap, revision: pool.value.revision }));
    } catch (cause) {
      setError(cause instanceof AccountError ? cause : new AccountError({ status: 0, code: "network", message: "Could not save the pool." }));
    } finally {
      setPending(false);
    }
  }

  const title = "Organization Sprite pool";
  const turnedOn = pool.value !== undefined && (pool.value.max_runners > 0 || pool.value.members.some((member) => member.state !== "deleted"));
  const notSetUp = pool.value !== undefined && !turnedOn;
  const needsAttention = turnedOn && !token.loading && (token.error !== null || token.value?.present === false);
  const integrationsUrl = `${account?.base_path ?? ""}/settings/integrations#sprites`;
  const project = account?.projects.find((candidate) => candidate.id === projectId);
  const context = `${account?.organization.name ?? "This organization"} shares this pool across all projects.${projectId ? ` Viewing from ${project?.name ?? "the selected project"}; these settings apply to the whole organization.` : ""}`;
  const tokenMessage = token.error !== null ? "Could not check the organization’s Sprites token." : "No organization Sprites token is set, so the Hub cannot start Sprites.";
  const tokenAction = <a className="underline underline-offset-4" href={integrationsUrl}>{token.error !== null ? "Review the token" : "Set a Sprites token"}</a>;
  React.useEffect(() => {
    if (compact) onAttentionChange?.(projectId, needsAttention);
  }, [compact, projectId, needsAttention, onAttentionChange]);
  const status = unavailable ? "Could not load the pool." : pool.value === undefined ? (forbidden ? "Owner or admin access required." : "Loading…") : notSetUp ? "Not set up." : `${pool.value.members.filter((member) => member.state !== "deleted").length} members`;
  const content = <div className="w-full space-y-4 py-4">
      <p className="text-sm text-muted-foreground">{context}</p>
      {unavailable ? <Alert variant="error">
        <AlertTitle>{error === null ? "Could not load the pool." : "Could not save the pool."}</AlertTitle>
        <AlertDescription>{failure?.isConflict ? "The pool settings changed. Reload the saved settings before trying again." : "Try loading the organization’s pool settings again."}
          <div><Button type="button" size="sm" variant="outline" disabled={pending || pool.loading} onClick={() => void reload()}>{pool.loading ? "Retrying…" : "Retry"}</Button></div>
        </AlertDescription>
      </Alert> : <>
      {readOnly ? <Alert><AlertTitle>Owner or admin access required to change the pool.</AlertTitle><AlertDescription>Ask an organization owner or admin to update these settings.{account ? <a className="underline underline-offset-4" href={`${account.base_path}/settings/organization/members`}>View members</a> : null}</AlertDescription></Alert> : null}
      {needsAttention ? <SettingsWarning>{tokenMessage}{" "}{tokenAction}</SettingsWarning> : null}
      {pool.loading && pool.value === undefined ? <p className="text-sm text-muted-foreground" role="status">Loading pool settings…</p> : null}
      {pool.value !== undefined ? <form className="space-y-3" onSubmit={(event) => { event.preventDefault(); void save(); }}>
        <div className="grid gap-3 sm:grid-cols-3">
          <label className="space-y-1 text-sm">Minimum runners<Input type="number" min={0} required value={floor} disabled={pending || pool.loading || readOnly} onChange={(event) => setFloor(Number(event.target.value))} /></label>
          <label className="space-y-1 text-sm">Maximum runners<Input type="number" min={floor} required value={ceiling} disabled={pending || pool.loading || readOnly} onChange={(event) => setCeiling(Number(event.target.value))} /></label>
          <label className="space-y-1 text-sm">Idle seconds<Input type="number" min={30} max={86400} required value={idle} disabled={pending || pool.loading || readOnly} onChange={(event) => setIdle(Number(event.target.value))} /></label>
        </div>
        <label className="block space-y-1 text-sm">Extra bootstrap (optional)
          <textarea className="min-h-32 w-full rounded-md border border-border bg-background p-2 font-mono text-xs" value={bootstrap} maxLength={65536} disabled={pending || pool.loading || readOnly} onChange={(event) => setBootstrap(event.target.value)} />
        </label>
        <p className="text-xs text-muted-foreground">Optional shell steps run before runner enrollment. Repository setup comes from hooks.runner_setup after checkout. Use your secret delivery service; do not paste credentials here. DETENT_PROJECT_ID and DETENT_HUB_URL are available. Runner policy approval is still required.</p>
        {readOnly ? null : <Button type="submit" size="sm" disabled={pending || pool.loading}>{pending ? "Saving…" : "Save pool"}</Button>}
      </form> : null}
      <Button type="button" size="sm" variant="outline" disabled={pending || pool.loading} onClick={() => void reload()}>{forbidden && pool.value === undefined ? "Retry" : "Reload pool"}</Button>
      {pool.value?.members.map((member) => <div key={member.name} className="space-y-1 rounded-md border border-border p-3 text-sm">
        <p className="break-all font-medium">{member.name} · {member.state}</p>
        {member.runner_id === "" ? <p className="text-xs text-muted-foreground">Runner has not enrolled.</p> : <p className="text-xs text-muted-foreground">Enrolled; work readiness follows runner health, provider setup and project policy.</p>}
        <details><summary className="cursor-pointer text-xs">Last bootstrap log</summary><pre className="mt-2 whitespace-pre-wrap break-words text-xs">{member.bootstrap_log || "No bootstrap progress recorded."}</pre></details>
      </div>)}
      </>}
    </div>;
  if (!compact) return <SettingsRow title={title} description="The Hub creates fresh runners as queued work needs them and deletes idle members above the minimum. Set the maximum to zero to disable provisioning." status={status}>{content}</SettingsRow>;
  if (attentionOnly && !needsAttention) return <></>;
  return <div className="px-4" data-testid="sprite-pool-row">
    <Dialog>
      <SettingsRow title={title}
        description={unavailable || pool.value === undefined ? status : notSetUp ? "Not set up. Sprites start runners for every project when work is queued." : `Floor ${pool.value.min_runners} · Ceiling ${pool.value.max_runners} · Idle ${pool.value.idle_seconds}s · ${status}`}
        control={<DialogTrigger render={<Button size="xs" variant="outline">{notSetUp && !readOnly ? "Set up Sprites" : readOnly ? "View pool" : "Configure pool"}</Button>} />}
      >
        {turnedOn && token.loading ? <p className="pb-3 text-xs text-muted-foreground">Checking Sprites token…</p> : needsAttention ? <p className="pb-3 text-xs text-warning-foreground">
          {tokenMessage}{" "}{tokenAction}
        </p> : null}
      </SettingsRow>
      <DialogPopup className="sm:max-w-xl">
        <DialogHeader><DialogTitle>{title}</DialogTitle><DialogDescription>The Hub creates runners for queued work and deletes idle members above the floor. Set the ceiling to zero to disable provisioning.</DialogDescription></DialogHeader>
        <DialogPanel>{content}</DialogPanel>
      </DialogPopup>
    </Dialog>
  </div>;
}
