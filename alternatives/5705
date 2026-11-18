import React from "react";

import { Alert, AlertDescription, AlertTitle } from "../../components/ui/alert.tsx";
import { Button } from "../../components/ui/button.tsx";
import { CopyableCommand } from "../fleet/EnrollRunner.tsx";
import { SettingsRow } from "../settings/settingsLayout.tsx";
import { AccountError } from "./api.ts";
import { useAccountApi, useAccountBootstrap } from "./context.ts";
import { SpritePoolCard } from "./SpritePoolCard.tsx";
import { useResource } from "./useResource.ts";

const memberStatuses: Record<string, string> = {
  provisioning: "Provisioning",
  awaiting_login: "Waiting for login",
  awaiting_setup: "Waiting for project setup",
  ready: "Ready",
  sleeping: "Sleeping · ready to wake",
  unreachable: "Unreachable · recovery needed",
  draining: "Draining · finishing durable work",
  deleting: "Deleting",
  deleted: "Deleted",
};

export function ProjectSpriteControls({ projectId, canManage }: {
  readonly projectId: string;
  readonly canManage: boolean;
}): React.ReactElement {
  const api = useAccountApi();
  const account = useAccountBootstrap();
  const snapshot = useResource(async () => {
    const [allocation, pool] = await Promise.all([
      api.projectSpriteAllocation(projectId), api.spritePool(projectId),
    ]);
    return { allocation, pool };
  }, [api, projectId]);
  const token = useResource(() => api.spritesSecret(""), [api]);
  const [pending, setPending] = React.useState(false);
  const [error, setError] = React.useState<AccountError | null>(null);
  const inFlight = React.useRef(false);
  const command = React.useRef<Parameters<typeof api.setProjectSpriteAllocation>[0] | null>(null);
  const failure = error ?? snapshot.error;
  const readOnly = !canManage || failure?.status === 403;
  const disabled = pending || readOnly || snapshot.loading || failure !== null;
  const current = snapshot.value;
  const allocation = current?.allocation;
  const members = current?.pool.members.filter((member) => member.allocation_project_id === projectId) ?? [];
  const active = members.filter((member) => member.state !== "deleted");
  const ready = active.filter((member) => member.status === "ready" || member.status === "sleeping").length;
  const draining = active.filter((member) => member.status === "draining" || member.state === "deleting").length;
  const prepared = current !== undefined && current.pool.revision > 0 && current.pool.max_runners > 0 && !token.loading && token.error === null && token.value?.present && !current.pool.isolation_tier_unavailable_reason;
  const refresh = snapshot.refresh;
  const refreshToken = token.refresh;

  React.useEffect(() => {
    const timer = globalThis.setInterval(() => {
      if (!inFlight.current && document.visibilityState !== "hidden") {
        void refresh();
        void refreshToken();
      }
    }, 5000);
    return () => globalThis.clearInterval(timer);
  }, [refresh, refreshToken]);

  async function reload(): Promise<void> {
    if (inFlight.current) return;
    setError(null);
    command.current = null;
    await Promise.all([refresh(), refreshToken()]);
  }

  async function save(desiredCount: number, retry = false): Promise<void> {
    if (inFlight.current || readOnly || allocation === undefined || current === undefined || snapshot.loading || snapshot.error !== null) return;
    if (retry && command.current === null) return;
    if (!retry) command.current = { projectId, desiredCount, revision: allocation.revision, key: crypto.randomUUID() };
    const input = command.current;
    if (input === null) return;
    inFlight.current = true;
    setPending(true);
    setError(null);
    try {
      const saved = await api.setProjectSpriteAllocation(input);
      snapshot.set({ ...current, allocation: saved });
      command.current = null;
      await refresh();
    } catch (cause) {
      setError(cause instanceof AccountError ? cause : new AccountError({ status: 0, code: "network", message: "Could not update the project Sprite count." }));
    } finally {
      inFlight.current = false;
      setPending(false);
    }
  }

  return <SettingsRow title="Project Sprites" description="Set the total Sprites reserved for this project. Other projects and organization-only runners keep their own capacity.">
    <div className="space-y-4 py-4">
      {readOnly ? <p className="text-sm text-muted-foreground">An owner or admin with write access to this project can change its Sprite count.</p> : null}
      {failure ? <Alert variant="error">
        <AlertTitle>{error ? "Could not update project Sprites" : "Could not load project Sprites"}</AlertTitle>
        <AlertDescription>
          {failure.isConflict ? "The project count changed. Reload before choosing a new total." : failure.message}
          <div className="flex flex-wrap gap-2">
            {error && (error.status === 0 || error.status >= 500) && command.current ? <Button size="sm" variant="outline" disabled={pending || readOnly} onClick={() => void save(command.current!.desiredCount, true)}>Retry request</Button> : null}
            {current === undefined ? <Button size="sm" variant="outline" disabled={pending || snapshot.loading} onClick={() => void reload()}>Reload Sprites</Button> : null}
          </div>
        </AlertDescription>
      </Alert> : null}
      {current === undefined ? <p role="status" className="text-sm text-muted-foreground">{snapshot.loading ? "Loading project Sprites…" : "Project Sprite settings are unavailable."}</p> : <>
        {!prepared ? <div className="space-y-3 text-sm text-muted-foreground">
          <p>Prepare Sprites once: configure the organization pool and its maximum, then set the organization’s Sprites token. The Hub must support hosted Sprite provisioning and encrypted secrets. Sandbox requires a supported runner isolation probe.</p>
          <p>Each new Sprite needs its own provider and GitHub sign-in in its console. Subscription logins cannot be copied. Detent handles creation, fresh runner enrollment and repository setup; no other terminal setup is needed.</p>
          <p>Provider billing stays as configured. Runner policy approval and any repository setup requirements still apply.</p>
          {current.pool.isolation_tier_unavailable_reason ? <p>{current.pool.isolation_tier_unavailable_reason}</p> : null}
          {token.error ? <p>Could not check the organization’s Sprites token. Decrease or turn Off project Sprites, or reload to check again before provisioning.</p> : token.loading ? <p>Checking Sprites token…</p> : null}
          {token.error || !token.value?.present ? <a className="underline underline-offset-4" href={`${account?.base_path ?? ""}/settings/integrations#sprites`}>{token.error ? "Review the organization Sprites token" : "Set the organization Sprites token"}</a> : null}
          <details>
            <summary className="cursor-pointer">Prepare organization pool</summary>
            <SpritePoolCard projectId={projectId} canManage={canManage} />
          </details>
          <p>After preparation, reload Sprites to enable the project controls.</p>
        </div> : null}
        <p role="status" aria-live="polite" className="text-sm">
          Desired {current.allocation.desired_count} · Actual {current.allocation.allocated_count} · Ready {ready}
          {pending ? " · Saving…" : ""}
          {draining > 0 ? ` · Draining ${draining}` : ""}
        </p>
        <div role="group" aria-label="Project Sprite count" className="flex flex-wrap gap-2">
          <Button size="sm" variant="outline" aria-label="Decrease project Sprites" disabled={disabled || current.allocation.desired_count === 0} onClick={() => void save(current.allocation.desired_count - 1)}>−</Button>
          <Button size="sm" variant="outline" aria-label="Increase project Sprites" disabled={disabled || !prepared} onClick={() => void save(current.allocation.desired_count + 1)}>+</Button>
          <Button size="sm" variant="outline" disabled={disabled || current.allocation.desired_count === 0} onClick={() => void save(0)}>Off</Button>
          <Button size="sm" variant="outline" disabled={pending || snapshot.loading} onClick={() => void reload()}>Reload Sprites</Button>
        </div>
        {current.allocation.shortfall > 0 ? <p className="text-sm text-muted-foreground">Shortfall {current.allocation.shortfall}: waiting for creation within the organization ceiling of {current.pool.max_runners}, provider capacity and budget.</p> : null}
        {current.allocation.provision_failures > 0 ? <Alert variant="warning">
          <AlertTitle>Some Sprites could not be provisioned</AlertTitle>
          <AlertDescription>{current.allocation.provision_failures} failed attempts. Review member progress and the organization’s token, capacity and setup before retrying.
            <div><Button size="sm" variant="outline" disabled={disabled || !prepared} onClick={() => void save(current.allocation.desired_count)}>Retry provisioning</Button></div>
          </AlertDescription>
        </Alert> : null}
        {current.allocation.managed && current.allocation.desired_count === 0 ? <p className="text-sm text-muted-foreground">Off: managed members finish active work and publish durable recovery before deletion. Draining members can still incur compute and storage charges. Retained checkpoints or other provider storage may continue to cost money after deletion; review them in Sprites.</p> : null}
        {active.some((member) => member.status === "provisioning") ? <p className="text-sm text-muted-foreground">Creating and enrolling fresh runners…</p> : null}
        {members.map((member) => <div key={member.name} className="space-y-2 rounded-md border border-border p-3 text-sm">
          <p className="break-all font-medium">{member.name} · {memberStatuses[member.status ?? member.state] ?? "Waiting for runner status"}</p>
          {member.waiting_for?.length ? <div className="space-y-2">
            <p>In {member.name}’s Sprite console, run these login commands. This member stays not ready until both provider and GitHub checks pass.</p>
            {member.waiting_for.map((step) => <div key={`${step.check}:${step.command}`}>
              <p className="text-xs text-muted-foreground">{step.check === "github" ? "GitHub" : "Provider"}</p>
              <CopyableCommand value={step.command} label={`${step.check} login for ${member.name}`} />
            </div>)}
          </div> : null}
          {member.status === "unreachable" || member.status === "awaiting_setup" ? <p className="text-xs text-muted-foreground">Review this runner in Providers &amp; runners and its bootstrap progress. Restore its connection or project setup, then reload Sprites.</p> : null}
          <details><summary className="cursor-pointer text-xs">Bootstrap progress</summary><pre className="mt-2 whitespace-pre-wrap break-words text-xs">{member.bootstrap_log || "No bootstrap progress recorded."}</pre></details>
        </div>)}
      </>}
    </div>
  </SettingsRow>;
}
