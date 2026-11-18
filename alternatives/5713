import { useProjectSettingsDirty } from "../settings/projectSettingsDirty.ts";
import React from "react";
import { LockKeyholeIcon } from "lucide-react";
import { Tooltip, TooltipPopup, TooltipTrigger } from "../../components/ui/tooltip.tsx";

import { Button } from "../../components/ui/button.tsx";
import { Input } from "../../components/ui/input.tsx";
import { SettingsRow } from "../settings/settingsLayout.tsx";
import { IntegrationStatus, IntegrationTitle } from "../settings/integrationStatus.tsx";
import { useAccountApi } from "./context.ts";
import { ControlError, StatusDot } from "./controls.tsx";
import { useResource } from "./useResource.ts";

export function SpritesCard({ projectId, canManage }: {
  readonly projectId: string;
  readonly canManage: boolean;
}): React.ReactElement {
  return projectId ? <ProjectSpritesCard projectId={projectId} canManage={canManage} /> : <OrganizationSpritesRow canManage={canManage} />;
}

function OrganizationSpritesRow({ canManage }: { readonly canManage: boolean }): React.ReactElement {
  const api = useAccountApi();
  const status = useResource(() => api.spritesSecret(""), [api]);
  const [editing, setEditing] = React.useState(false);
  const [token, setToken] = React.useState("");
  const [pending, setPending] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);

  const change = async (remove: boolean) => {
    const value = token;
    setToken("");
    setPending(true);
    setError(null);
    try {
      status.set(remove ? await api.removeSpritesSecret("") : await api.setSpritesSecret("", value));
      setEditing(false);
    } catch {
      setError(remove ? "Could not remove the token. Try again." : "Could not save the token. Check the organization token and try again, or ask the Hub operator to check its secret key configuration.");
    } finally {
      setPending(false);
    }
  };

  return <SettingsRow
    id="sprites"
    title={<IntegrationTitle title="Sprites organization token" help="Projects inherit this token unless they set an override. Use a dedicated Fly organization and set a spend alert." secret="token" />}
    status={status.value === undefined ? (status.error === null ? "Loading…" : "Could not load Sprites settings.") : <IntegrationStatus tone={status.value.present ? "connected" : "empty"}>
      {status.value.present ? <>Connected to {status.value.organization_slug} · <a className="text-info-foreground underline" href="https://sprites.dev/account" target="_blank" rel="noreferrer">Manage in Sprites</a></> : "Not set up"}
    </IntegrationStatus>}
    control={canManage ? <div className="flex w-full min-w-0 flex-col items-start gap-2 sm:items-end">
      {editing ? <form className="flex w-full min-w-0 flex-col gap-2 sm:w-96" onSubmit={(event) => { event.preventDefault(); if (token.trim() && !pending) void change(false); }}>
        <Input aria-label="Sprites organization token" type="password" size="sm" autoComplete="off" spellCheck={false} value={token} maxLength={8192} disabled={pending} onChange={(event) => setToken(event.target.value)} placeholder="Organization token" />
        <div className="flex justify-end gap-2">
          <Button size="sm" type="button" variant="ghost" disabled={pending} onClick={() => { setToken(""); setError(null); setEditing(false); }}>Cancel</Button>
          <Button size="sm" type="submit" disabled={pending || !token.trim()}>{pending ? "Saving…" : "Save"}</Button>
        </div>
      </form> : <div className="flex flex-wrap gap-2">
        <Button size="sm" type="button" variant="outline" disabled={pending || status.value === undefined} onClick={() => { setToken(""); setError(null); setEditing(true); }}>{status.value?.present ? "Replace token" : "Set token"}</Button>
        {status.value?.present ? <Button size="sm" variant="ghost" type="button" disabled={pending} onClick={() => void change(true)}>Remove</Button> : null}
      </div>}
      {error ? <ControlError message={error} /> : null}
    </div> : <span className="text-sm text-muted-foreground">Owner or admin access required to change the token.</span>}
  />;
}

function ProjectSpritesCard({ projectId, canManage }: {
  readonly projectId: string;
  readonly canManage: boolean;
}): React.ReactElement {
  const api = useAccountApi();
  const status = useResource(() => api.spritesSecret(projectId), [api, projectId]);
  const [editing, setEditing] = React.useState(false);
  const [token, setToken] = React.useState("");
  const [pending, setPending] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);

  useProjectSettingsDirty(token.trim() !== "");
  const change = async (remove: boolean) => {
    // The input is cleared as soon as the request starts, including on failure.
    const value = token;
    setToken("");
    setPending(true);
    setError(null);
    try {
      status.set(remove ? await api.removeSpritesSecret(projectId) : await api.setSpritesSecret(projectId, value));
      setEditing(false);
    } catch {
      // A network or provider error must never echo a submitted credential.
      setError(remove ? "Could not remove the token. Try again." : "Could not save the token. Check the organization token and try again, or ask the Hub operator to check its secret key configuration.");
    } finally {
      setPending(false);
    }
  };

  return (
    <SettingsRow
      id="sprites"
      title={<span className="inline-flex items-center gap-1.5">Sprites project override<Tooltip>
        <TooltipTrigger render={<Button size="icon-micro" variant="ghost-muted" aria-label="Token security" />}><LockKeyholeIcon aria-hidden className="size-3" /></TooltipTrigger>
        <TooltipPopup>Encrypted · write-only</TooltipPopup>
      </Tooltip></span>}
      status={status.value === undefined ? (status.error === null ? "Loading…" : "Could not load Sprites settings.") : <span className="inline-flex flex-wrap items-center gap-1.5">
        <StatusDot tone={status.value.present ? "ok" : "idle"} />{status.value.present ? <>Connected to {status.value.organization_slug} · <a className="text-primary underline underline-offset-2" href="https://sprites.dev/account" target="_blank" rel="noreferrer">Manage in Sprites</a></> : "Using organization token"}
      </span>}
      control={canManage && !editing ? <div className="flex w-full gap-2 sm:w-auto">
        <Button size="sm" variant="outline" className="flex-1 sm:flex-none" disabled={pending || status.value === undefined} onClick={() => { setEditing(true); setError(null); }}>{status.value?.present ? "Replace token" : "Set override"}</Button>
        {status.value?.present ? <Button size="sm" variant="ghost" disabled={pending} onClick={() => void change(true)}>Remove</Button> : null}
      </div> : null}
    >
      {canManage && editing ? <form className="flex w-full flex-col gap-3 pb-3" onSubmit={(event) => { event.preventDefault(); if (token.trim() !== "" && !pending) void change(false); }}>
            <Input
              aria-label="Sprites organization token"
              type="password"
              autoComplete="off"
              spellCheck={false}
              value={token}
              maxLength={8192}
              disabled={pending || status.value === undefined}
              onChange={(event) => setToken(event.target.value)}
              placeholder={status.value?.present ? "Replacement token" : "Organization token"}
            />
            <div className="flex flex-col gap-2 sm:flex-row sm:justify-end">
              <Button size="sm" variant="ghost" type="button" disabled={pending} onClick={() => { setEditing(false); setToken(""); setError(null); }}>Cancel</Button>
              <Button size="sm" type="submit" disabled={pending || token.trim() === "" || status.value === undefined}>{pending ? "Saving…" : "Save"}</Button>
            </div>
          </form> : null}
      <ControlError message={error} />
    </SettingsRow>
  );
}
