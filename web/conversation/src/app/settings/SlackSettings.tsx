import React from "react";

import { Button } from "../../components/ui/button.tsx";
import { Input } from "../../components/ui/input.tsx";
import { useAccountApi, useAccountBootstrap } from "../account/context.ts";
import { ControlError } from "../account/controls.tsx";
import { useResource } from "../account/useResource.ts";
import { SettingsRow, SettingsSection } from "./settingsLayout.tsx";

export function SlackSettings(): React.ReactElement | null {
  const bootstrap = useAccountBootstrap();
  if (!bootstrap?.actor.can_manage) return null;
  return <SlackSettingsForm />;
}

function SlackSettingsForm(): React.ReactElement {
  const api = useAccountApi();
  const status = useResource(() => api.slackIntegration(), [api]);
  const [webhook, setWebhook] = React.useState<string | null>(null);
  const [channel, setChannel] = React.useState("");
  const [pending, setPending] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);
  const [notice, setNotice] = React.useState<string | null>(null);
  React.useEffect(() => {
    if (status.value) setChannel(status.value.channel_name);
  }, [status.value]);

  const change = async (action: "save" | "disable" | "test") => {
    const value = webhook;
    setWebhook(null);
    setPending(true);
    setError(null);
    setNotice(null);
    try {
      const result = action === "test"
        ? await api.testSlackIntegration()
        : await api.saveSlackIntegration({ channel_name: action === "disable" ? "" : channel, ...(action === "disable" ? { webhook: "" } : value !== null || !status.value?.webhook ? { webhook: value ?? "" } : {}) });
      status.set(result);
      setNotice(action === "test" ? result.last_delivery_failed ? "Test message failed. Check the webhook and try again." : "Test message sent." : action === "disable" || !result.webhook ? "Slack notifications disabled." : "Slack notifications saved.");
    } catch {
      setError(action === "test" ? "Could not send the test message. Check the webhook and try again." : "Could not save Slack settings. Check the webhook URL and channel name, or ask the Hub operator to check secret storage.");
    } finally {
      setPending(false);
    }
  };

  const value = status.value;
  return <SettingsSection title="Organization notifications" id="slack-notifications">
    <SettingsRow
      title={<label htmlFor="slack-webhook">Slack incoming webhook</label>}
      description={<span className="text-muted-foreground">Post attention findings when they open and resolve. Leave empty to disable. The webhook is stored as an organization secret.</span>}
      status={<div className="space-y-1">
        {status.loading ? <span>Loading Slack settings…</span> : status.error ? <span role="alert">Could not load Slack settings.</span> : <>
          <span>{value?.webhook ? `Saved webhook: ${value.webhook}` : "Slack notifications are off."}</span>
          {value?.last_success_at ? <p>Last successful delivery: <time dateTime={value.last_success_at}>{new Date(value.last_success_at).toLocaleString()}</time></p> : null}
          {value?.last_delivery_failed && value.last_failure_at ? <p className="text-warning-foreground">Last delivery failed: <time dateTime={value.last_failure_at}>{new Date(value.last_failure_at).toLocaleString()}</time>{value.last_status_code ? ` (HTTP ${value.last_status_code})` : " (unavailable)"}</p> : null}
        </>}
      </div>}
      control={<form className="flex w-full min-w-0 flex-col gap-2 sm:w-64" onSubmit={(event) => { event.preventDefault(); void change("save"); }}>
        <Input id="slack-webhook" type="password" size="sm" autoComplete="off" spellCheck={false} placeholder={value?.webhook ? "Replace saved webhook" : "https://hooks.slack.com/services/…"} value={webhook ?? value?.webhook ?? ""} onChange={(event) => setWebhook(event.target.value)} aria-describedby="slack-webhook-help" disabled={pending || status.loading || Boolean(status.error)} />
        <span id="slack-webhook-help" className="text-xs text-muted-foreground">The full URL is never shown after saving.</span>
        <label htmlFor="slack-channel" className="text-xs text-muted-foreground">Channel name (optional, for display)</label>
        <Input id="slack-channel" size="sm" placeholder="#operations" maxLength={81} value={channel} onChange={(event) => setChannel(event.target.value)} disabled={pending || status.loading || Boolean(status.error)} />
        <div className="flex flex-wrap gap-2">
          <Button size="sm" type="submit" disabled={pending || status.loading || Boolean(status.error)}>{pending ? "Working…" : "Save"}</Button>
          <Button size="sm" type="button" variant="outline" disabled={pending || !value?.webhook} onClick={() => void change("test")}>Send test message</Button>
          {value?.webhook ? <Button size="sm" type="button" variant="ghost" disabled={pending} onClick={() => void change("disable")}>Disable</Button> : null}
        </div>
        {error ? <ControlError message={error} /> : null}
        {notice ? <p role="status" className="text-xs text-muted-foreground">{notice}</p> : null}
      </form>}
    />
  </SettingsSection>;
}
