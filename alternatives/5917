import React from "react";

import { Button } from "../../components/ui/button.tsx";
import { Input } from "../../components/ui/input.tsx";
import { useAccountApi, useAccountBootstrap } from "../account/context.ts";
import { ControlError } from "../account/controls.tsx";
import { useResource } from "../account/useResource.ts";
import { formatRelativeTimeLabel } from "../../timestampFormat.ts";
import { IntegrationStatus, IntegrationTitle } from "./integrationStatus.tsx";
import { SettingsRow, SettingsSection, useRelativeTimeTick } from "./settingsLayout.tsx";

export function SlackSettings(): React.ReactElement | null {
  const bootstrap = useAccountBootstrap();
  if (!bootstrap?.actor.can_manage) return null;
  return <SlackSettingsForm />;
}

function SlackSettingsForm(): React.ReactElement {
  const api = useAccountApi();
  const status = useResource(() => api.slackIntegration(), [api]);
  const [webhook, setWebhook] = React.useState("");
  const [editing, setEditing] = React.useState(false);
  const [channel, setChannel] = React.useState("");
  const [pending, setPending] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);
  const [notice, setNotice] = React.useState<string | null>(null);
  useRelativeTimeTick(60_000);

  const edit = () => {
    setWebhook("");
    setChannel(status.value?.channel_name ?? "");
    setError(null);
    setNotice(null);
    setEditing(true);
  };

  const change = async (action: "save" | "disable" | "test") => {
    const value = webhook;
    setWebhook("");
    setPending(true);
    setError(null);
    setNotice(null);
    try {
      const result = action === "test"
        ? await api.testSlackIntegration()
        : await api.saveSlackIntegration({ channel_name: action === "disable" ? "" : channel, webhook: action === "disable" ? "" : value });
      status.set(result);
      setEditing(false);
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
      title={<IntegrationTitle title="Slack incoming webhook" help="Post attention findings when they open and resolve." secret="webhook" />}
      status={status.loading ? <span>Loading Slack settings…</span> : status.error ? <span role="alert">Could not load Slack settings.</span> : <>
          <IntegrationStatus tone={!value?.webhook ? "empty" : value.last_delivery_failed ? "failed" : "connected"}>
            {!value?.webhook ? "Not set up" : value.last_delivery_failed ? `Last delivery failed${value.last_status_code ? ` · HTTP ${value.last_status_code}` : ""}` : <>
              {value.channel_name || "Slack"}{value.last_success_at ? <> · delivered <time dateTime={value.last_success_at}>{formatRelativeTimeLabel(value.last_success_at)}</time></> : null}
            </>}
          </IntegrationStatus>
        </>}
      control={<div className="flex w-full min-w-0 flex-col items-start gap-2 sm:items-end">
        {editing ? <form className="flex w-full min-w-0 flex-col gap-2 sm:w-96" onSubmit={(event) => { event.preventDefault(); if (webhook.trim() && !pending) void change("save"); }}>
          <div className="grid gap-2 sm:grid-cols-[2fr_1fr]">
            <Input id="slack-webhook" aria-label="Slack webhook URL" type="password" size="sm" autoComplete="off" spellCheck={false} placeholder="https://hooks.slack.com/services/…" value={webhook} onChange={(event) => setWebhook(event.target.value)} disabled={pending} />
            <Input id="slack-channel" aria-label="Channel name (optional, for display)" size="sm" placeholder="#operations" maxLength={81} value={channel} onChange={(event) => setChannel(event.target.value)} disabled={pending} />
          </div>
          <div className="flex justify-end gap-2">
            <Button size="sm" type="button" variant="ghost" disabled={pending} onClick={() => { setWebhook(""); setChannel(""); setError(null); setEditing(false); }}>Cancel</Button>
            <Button size="sm" type="submit" disabled={pending || !webhook.trim()}>{pending ? "Working…" : "Save"}</Button>
          </div>
        </form> : <div className="flex flex-wrap gap-2">
          <Button size="sm" type="button" variant="outline" disabled={pending || status.loading || Boolean(status.error)} onClick={edit}>{value?.webhook ? "Replace webhook" : "Set webhook"}</Button>
          {value?.webhook ? <>
            <Button size="sm" type="button" variant="outline" disabled={pending} onClick={() => void change("test")}>Send test message</Button>
            <Button size="sm" type="button" variant="ghost" disabled={pending} onClick={() => void change("disable")}>Disable</Button>
          </> : null}
        </div>}
        {error ? <ControlError message={error} /> : null}
        {notice ? <p role="status" className="text-xs text-muted-foreground">{notice}</p> : null}
      </div>}
    />
  </SettingsSection>;
}
