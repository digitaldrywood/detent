import React from "react";

import { Button } from "../../components/ui/button.tsx";
import { Input } from "../../components/ui/input.tsx";
import { SettingsRow } from "../settings/settingsLayout.tsx";
import { useAccountApi } from "./context.ts";
import { ControlError } from "./controls.tsx";
import { useResource } from "./useResource.ts";

export function SpritesCard({ projectId, canManage }: {
  readonly projectId: string;
  readonly canManage: boolean;
}): React.ReactElement {
  const api = useAccountApi();
  const status = useResource(() => api.spritesSecret(projectId), [api, projectId]);
  const [token, setToken] = React.useState("");
  const [pending, setPending] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);

  const change = async (remove: boolean) => {
    // The input is cleared as soon as the request starts, including on failure.
    const value = token;
    setToken("");
    setPending(true);
    setError(null);
    try {
      status.set(remove ? await api.removeSpritesSecret(projectId) : await api.setSpritesSecret(projectId, value));
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
      title="Sprites"
      description={
        <>
          A Sprites organization token lets Detent create and drive sprites only in that Fly organization.
          Use a dedicated Fly organization with nothing else in it, and set a spend alert.
          {" "}<a className="underline" href="https://sprites.dev/account" target="_blank" rel="noreferrer">Manage or revoke tokens in your Sprites account</a>.
          {" "}The token is encrypted and write-only. Removing it here removes Detent's copy; revoke it in Sprites to invalidate it.
        </>
      }
      status={status.value === undefined ? (status.error === null ? "Loading…" : "Could not load Sprites settings.") : status.value.present ? `Connected to ${status.value.organization_slug}` : "No token set"}
      control={
        canManage ? (
          <form className="flex w-full flex-col gap-2 sm:max-w-sm" onSubmit={(event) => { event.preventDefault(); if (token.trim() !== "" && !pending) void change(false); }}>
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
            <div className="flex gap-2">
              <Button size="sm" type="submit" disabled={pending || token.trim() === "" || status.value === undefined}>
                {pending ? "Saving…" : status.value?.present ? "Replace token" : "Set token"}
              </Button>
              {status.value?.present ? <Button size="sm" variant="outline" type="button" disabled={pending} onClick={() => void change(true)}>Remove</Button> : null}
            </div>
            {error === null ? null : <ControlError message={error} />}
          </form>
        ) : <span className="text-sm text-muted-foreground">Owner or admin access required to change the token.</span>
      }
    />
  );
}
