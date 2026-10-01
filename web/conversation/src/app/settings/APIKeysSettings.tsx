import React from "react";

import type { OperatorAPIKey } from "../../contracts/account.ts";
import { Button } from "../../components/ui/button.tsx";
import { useAccountApi, useAccountBootstrap } from "../account/context.ts";
import { SettingsRow } from "./settingsLayout.tsx";

export function APIKeysSettings({ onAvailability }: { readonly onAvailability: (available: boolean) => void }): React.ReactElement {
  const account = useAccountBootstrap();
  const api = useAccountApi();
  const [keys, setKeys] = React.useState<readonly OperatorAPIKey[]>([]);
  const [loaded, setLoaded] = React.useState(false);
  const [error, setError] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const [secret, setSecret] = React.useState("");
  const [secretCopied, setSecretCopied] = React.useState(false);
  React.useEffect(() => {
    let current = true;
    setLoaded(false);
    setSecret("");
    setSecretCopied(false);
    setError("");
    setKeys([]);
    onAvailability(false);
    if (account === null) return;
    api.apiKeys().then((result) => {
      if (!current) return;
      setKeys(result.keys);
      setLoaded(true);
      onAvailability(true);
    }).catch(() => {
      if (current) setError("Direct API-key setup is unavailable on this deployment or for this session. Sign in as an organization member and check with your Cloud operator.");
    });
    return () => { current = false; };
  }, [api, account, onAvailability]);

  async function create(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const values = new FormData(event.currentTarget);
    setBusy(true);
    setError("");
    setSecret("");
    setSecretCopied(false);
    try {
      const result = await api.createAPIKey({ name: String(values.get("name")), scope: String(values.get("scope")), expires_days: Number(values.get("expires_days")), project_ids: values.getAll("project").map(String) });
      setSecret(result.token);
      setKeys((await api.apiKeys()).keys);
    } catch {
      setError("Could not create the key. Check the name, expiry, current role and selected project access, then try again.");
    } finally {
      setBusy(false);
    }
  }

  async function revoke(id: string) {
    setBusy(true);
    setError("");
    try {
      await api.revokeAPIKey(id);
      setSecret("");
      setKeys((await api.apiKeys()).keys);
    } catch {
      setError("Could not revoke the key. Reload and check your current access.");
    } finally {
      setBusy(false);
    }
  }

  async function copyKey() {
    try {
      await navigator.clipboard.writeText(secret);
      setSecretCopied(true);
    } catch {
      setError("Could not copy the key. Check clipboard permissions before leaving this page.");
    }
  }

  return <SettingsRow title="Shared API keys" description="Keys belong to you and this organization. Select only the projects your agent needs. Scope can narrow your current permissions; it cannot expand them.">
    {error && <p role="alert" className="pb-3 text-sm text-muted-foreground">{error}</p>}
    {!loaded && !error && <p role="status" className="pb-3 text-sm text-muted-foreground">Checking direct API-key availability…</p>}
    {loaded && account && <div className="space-y-4 pb-3 text-sm">
      <form onSubmit={create} className="space-y-3">
        <label className="block">Key name<input name="name" required maxLength={200} className="mt-1 block w-full rounded-md border border-border bg-background p-2" /></label>
        <div className="flex flex-wrap gap-4">
          <label>Scope<select name="scope" className="ms-2 rounded-md border border-border bg-background p-2"><option value="read">Read</option>{account.actor.role !== "viewer" && <option value="write">Write</option>}{account.actor.can_manage && <option value="admin">Admin</option>}</select></label>
          <label>Expiry<select name="expires_days" defaultValue="30" className="ms-2 rounded-md border border-border bg-background p-2"><option value="7">7 days</option><option value="30">30 days</option><option value="90">90 days</option></select></label>
        </div>
        <fieldset className="space-y-2"><legend className="mb-2 font-medium">Projects</legend>{account.projects.map((project) => <label key={project.id} className="flex items-center gap-2"><input type="checkbox" name="project" value={project.id} />{project.name} <span className="text-xs text-muted-foreground">{project.id}</span></label>)}</fieldset>
        {account.projects.length === 0 && <p>Ask an administrator for project access before creating a key.</p>}
        <Button type="submit" disabled={busy || account.projects.length === 0}>Create API key</Button>
      </form>
      {secret && <div className="space-y-2 rounded-md border border-border p-3">
        <p role="status">Save this key privately now. It is shown only once. Copy it directly into your private secret store; never into an AI conversation, screenshot, URL, log or committed file.</p>
        <label htmlFor="new-api-key">New API key</label>
        <input id="new-api-key" type="password" value={secret} readOnly autoComplete="off" className="block w-full rounded-md border border-border bg-background p-2 font-mono" />
        <Button type="button" variant="outline" onClick={() => void copyKey()}>{secretCopied ? "Key copied" : "Copy key privately"}</Button>
        <Button type="button" variant="outline" onClick={() => setSecret("")}>Dismiss key</Button>
      </div>}
      <h3 className="font-medium">Your API keys</h3>
      {keys.length === 0 && <p className="text-muted-foreground">You have no API keys in this organization.</p>}
      {keys.map((key) => <div key={key.id} className="space-y-2 border-b border-border pb-3">
        <p className="font-medium">{key.name}</p>
        <p className="text-xs text-muted-foreground">{key.scope} · expires {key.expires_at} · fingerprint {key.fingerprint}</p>
        <p className="break-words text-xs text-muted-foreground">Projects: {key.project_ids.join(", ")}</p>
        {key.revoked ? <p>Revoked</p> : <Button type="button" variant="outline" disabled={busy} onClick={() => void revoke(key.id)}>Revoke {key.name}</Button>}
      </div>)}
    </div>}
  </SettingsRow>;
}
