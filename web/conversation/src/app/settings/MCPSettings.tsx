import React from "react";
import { CheckIcon, CopyIcon, PlugIcon } from "lucide-react";

import type { AccountBootstrap } from "../../contracts/account.ts";
import { Button } from "../../components/ui/button.tsx";
import { useCopyToClipboard } from "../../hooks/useCopyToClipboard.ts";
import { useAccountBootstrap } from "../account/context.ts";
import { SettingsPageContainer, SettingsRow, SettingsSection } from "./settingsLayout.tsx";

export function organizationMCPEndpoint(account: AccountBootstrap | null): string | null {
  if (account === null) return null;
  const publicURL = account.organization.public_url ?? account.organizations.find(
    (organization) => organization.id === account.organization.id,
  )?.public_url;
  if (!publicURL) return null;
  try {
    const url = new URL(publicURL);
    if (url.protocol !== "https:" || url.username || url.password || url.search || url.hash) return null;
    url.pathname = `${url.pathname.replace(/\/+$/, "")}/mcp`;
    return url.href;
  } catch {
    return null;
  }
}

function CopyExample({ label, value }: { readonly label: string; readonly value: string }) {
  const [failed, setFailed] = React.useState(false);
  const { copyToClipboard, isCopied } = useCopyToClipboard({
    target: label,
    onCopy: () => setFailed(false),
    onError: () => setFailed(true),
  });
  return (
    <div className="min-w-0 space-y-2 pb-3">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <span className="text-xs text-muted-foreground">{label}</span>
        <Button type="button" size="sm" variant="outline" aria-label={`Copy ${label}`} onClick={() => copyToClipboard(value)}>
          {isCopied ? <CheckIcon aria-hidden="true" /> : <CopyIcon aria-hidden="true" />}
          {isCopied ? "Copied" : "Copy"}
        </Button>
      </div>
      <pre tabIndex={0} aria-label={label} className="max-w-full rounded-md border border-border bg-muted/40 p-3 font-mono text-xs leading-relaxed whitespace-pre-wrap [overflow-wrap:anywhere]">
        <code>{value}</code>
      </pre>
      <span role="status" aria-label={`${label} copy status`} className={failed ? "text-xs text-muted-foreground" : "sr-only"}>
        {failed ? "Could not copy. Select the text and copy it manually." : isCopied ? `${label} copied.` : ""}
      </span>
    </div>
  );
}

const STDIO_CONFIG = JSON.stringify({
  mcpServers: {
    detent: {
      command: "detent",
      args: ["--host", "127.0.0.1", "--port", "YOUR_DAEMON_PORT", "mcp"],
      env: { DETENT_API_TOKEN: "YOUR_DETENT_API_TOKEN" },
    },
  },
}, null, 2);

const CURSOR_CONFIG = JSON.stringify({
  mcpServers: {
    detent: {
      url: "https://YOUR_DETENT_DAEMON/mcp",
      headers: { Authorization: "Bearer YOUR_DETENT_API_TOKEN" },
    },
  },
}, null, 2);

export function MCPSettings(): React.ReactElement {
  const account = useAccountBootstrap();
  const endpoint = organizationMCPEndpoint(account);
  return (
    <SettingsPageContainer>
      <SettingsSection id="settings-mcp" title="MCP" icon={<PlugIcon className="size-3.5" />}>
        <SettingsRow
          title="Connect an AI client"
          description="Model Context Protocol (MCP) lets an AI client read work and request actions in Detent. Every call checks the authenticated identity's current organization role and project permissions. Connecting does not unlock every tool."
        />
        <SettingsRow title="Organization endpoint" description={account?.organization.name ?? "Loading organization…"}>
          {endpoint ? <CopyExample key={endpoint} label="Organization MCP endpoint" value={endpoint} /> : (
            <p className="pb-3 text-sm text-muted-foreground">The organization HTTPS URL is unavailable. Ask your Cloud operator to confirm its public URL.</p>
          )}
        </SettingsRow>
        <SettingsRow title="Cloud connection requirements">
          <ol className="list-decimal space-y-2 ps-5 pb-3 text-[13px] leading-relaxed text-muted-foreground">
            <li>Confirm the endpoint belongs to the organization you want to use.</li>
            <li>Cloud MCP currently requires a signed-in hosted browser session and CSRF protection. API tokens, worker tokens and runner credentials cannot authenticate an operator MCP client in Cloud.</li>
            <li>External client installation with a bearer token is not available for Cloud yet. Detent does not offer MCP OAuth authorization. Do not copy browser cookies or session credentials into a client.</li>
          </ol>
        </SettingsRow>
      </SettingsSection>

      <SettingsSection id="settings-mcp-permissions" title="Permissions and tools">
        <SettingsRow title="Access follows your current permissions" description="Viewers can read permitted projects. Writes require a role and project grant that allow the operation; administration requires owner or admin authority. Billing requires an owner without a support session. Support access keeps its existing restrictions." />
        <SettingsRow title="Confirm material actions" description="Material or destructive actions return a preview and approval URL. Open it in an authenticated Detent browser, review the exact action, then confirm or reject it. Connection approval never expands your permissions." />
        <SettingsRow title="Tool availability" description="Clients discover only tools available to their identity and deployment. Follow all tools/list pages. A tool can be absent because of permissions, a missing service, or capability work still in progress.">
          <p className="pb-3 text-[13px] text-muted-foreground">
            <a className="underline underline-offset-4" href="https://github.com/digitaldrywood/detent/issues/3259" target="_blank" rel="noreferrer">MCP capability coverage (#3259)</a>
          </p>
        </SettingsRow>
      </SettingsSection>

      <SettingsSection id="settings-mcp-local" title="Local and self-hosted clients" variant="plain">
        <details className="rounded-xl border border-border/60 px-3 py-3 sm:px-4">
          <summary className="cursor-pointer text-sm font-medium focus-visible:outline-2 focus-visible:outline-ring">Connect to your own Detent daemon</summary>
          <div className="mt-3 min-w-0 space-y-3 text-[13px] leading-relaxed text-muted-foreground">
            <p>These examples connect to a separately running self-hosted daemon, not the Cloud organization above. Install the Detent binary for stdio. The existing <code>detent mcp</code> command uses the daemon's authenticated HTTP API; it does not start a daemon.</p>
            <p>Use existing API key management on that daemon: <code>detent key add --name mcp-client --scope read --expires-in 30d</code>. Start with read scope. For intended writes, select write scope and appropriate project restrictions with <code>--project</code>; reserve admin scope for administration. Store the returned token privately. Replace the placeholders only in private client configuration, never in a committed file or shared screenshot.</p>
            <CopyExample label="Local stdio configuration" value={STDIO_CONFIG} />
            <p>Replace <code>YOUR_DAEMON_PORT</code> with the running daemon's port. To revoke access, use <code>detent key list</code> and <code>detent key revoke KEY_ID</code> against that daemon's configuration. Removing a client entry does not revoke its token.</p>
            <p>For remote self-hosted access, use your daemon's trusted HTTPS URL and a bearer token. Choose Streamable HTTP with JSON POSTs. SSE, WebSockets and MCP OAuth are not supported. Local HTTP is appropriate only for isolated loopback access.</p>
            <CopyExample label="Cursor self-hosted HTTP configuration" value={CURSOR_CONFIG} />
            <p>Put the entry in Cursor's MCP configuration. For Claude Code, use the command below and check the connection with <code>/mcp</code>. Replace the daemon URL and token placeholders.</p>
            <CopyExample label="Claude Code self-hosted HTTP command" value={'claude mcp add --transport http detent https://YOUR_DETENT_DAEMON/mcp --header "Authorization: Bearer YOUR_DETENT_API_TOKEN"'} />
            <p className="flex flex-wrap gap-x-4 gap-y-2">
              <a className="underline underline-offset-4" href="https://cursor.com/docs/mcp" target="_blank" rel="noreferrer">Cursor installation instructions</a>
              <a className="underline underline-offset-4" href="https://code.claude.com/docs/en/mcp" target="_blank" rel="noreferrer">Claude Code installation instructions</a>
            </p>
          </div>
        </details>
      </SettingsSection>

      <SettingsSection id="settings-mcp-troubleshooting" title="Troubleshooting">
        <SettingsRow title="Incorrect URL or 404" description="Use the complete organization endpoint, including /organizations/… on shared Cloud. Do not use a project URL or /sse. An unavailable organization or hidden resource can also return 404." />
        <SettingsRow title="Invalid or revoked credential" description="Cloud reports access_denied when MCP authentication fails. Sign in again; a machine bearer token cannot grant operator access. A self-hosted daemon can report unauthorized, token_expired or token_revoked. Check the token and replace it through existing key management." />
        <SettingsRow title="Insufficient permissions" description="A 403 access_denied or tool access-denied result requires checking the current role, scope, project grants and resource ownership. Reconnecting does not grant access. Ask an organization administrator for the permissions the operation needs." />
        <SettingsRow title="Transport or unavailable tools" description="A GET returns 405: server-sent events are not supported. Send application/json POST requests through an HTTP MCP client. If a tool is unavailable, refresh discovery and check deployment services and capability coverage. Discovery alone never authorizes a call." />
      </SettingsSection>
    </SettingsPageContainer>
  );
}
