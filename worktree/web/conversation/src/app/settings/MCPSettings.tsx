import React from "react";
import { ConnectionValue, SetupPromptControl } from "./keySetup.tsx";
import { OrganizationAPIKeysSettings } from "./OrganizationAPIKeysSettings.tsx";
import { CheckIcon, CopyIcon, PlugIcon } from "lucide-react";

import type { AccountBootstrap } from "../../contracts/account.ts";
import { Button } from "../../components/ui/button.tsx";
import { useCopyToClipboard } from "../../hooks/useCopyToClipboard.ts";
import { basePath } from "../../runtime/basePath.ts";
import { useAccountBootstrap } from "../account/context.ts";
import { APIKeysSettings, type APIKeyAccess } from "./APIKeysSettings.tsx";
import {
  SettingsPageContainer,
  SettingsRow,
  SettingsSection,
} from "./settingsLayout.tsx";

export function organizationMCPEndpoint(account: AccountBootstrap | null): string | null {
  if (account === null) return null;
  const publicURL = account.organization.public_url ?? account.organizations.find(
    (organization) => organization.id === account.organization.id,
  )?.public_url;
  if (!publicURL) return null;
  try {
    const url = new URL(publicURL);
    if (url.protocol !== "https:" || url.username || url.password || url.search || url.hash) return null;
    const mount = basePath();
    const publicPath = url.pathname.replace(/\/+$/, "");
    if (mount && publicPath && publicPath !== mount) return null;
    const endpointPath = mount || publicPath;
    if (endpointPath.startsWith("/organizations/") && endpointPath !== `/organizations/${encodeURIComponent(account.organization.id)}`) return null;
    url.pathname = `${endpointPath}/mcp`;
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
          {isCopied ? "Copied" : `Copy ${label}`}
        </Button>
      </div>
      <details><summary className="cursor-pointer text-xs text-muted-foreground">Show {label}</summary><pre tabIndex={0} aria-label={label} className="max-w-full rounded-md border border-border bg-muted/40 p-3 font-mono text-xs leading-relaxed whitespace-pre-wrap [overflow-wrap:anywhere]">
        <code>{value}</code>
      </pre></details>
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

function keySetupPrompt(endpoint: string, access: string): string {
  const settings = endpoint.replace(/\/mcp$/, "/settings/mcp");
  return `Use the shared API key from Detent Settings → API & MCP (${settings}). Use the permission level configured for your key on that same page. Start with Read for read-only work. Default project access is All projects, including future projects; choose Selected projects to restrict it. The same key works for direct HTTP API and MCP. Write/Admin keys still require my current role and project grants; expiry, revocation and membership removal apply to both.
Project access: ${access}. Access always stays within this organization and my current project grants. Newly authorized projects work with an all-project key without replacing it or reconnecting MCP.
Ask me to store the secret directly in your private secret store or private environment as DETENT_API_KEY. Never ask me to paste it into this conversation. Never expose it in URLs, screenshots, logs, command traces, diagnostics or committed files. All examples below reference a private environment variable, not an embedded secret.
401: check key configuration, expiry and revocation. 403 or hidden 404: check current membership, role, key scope and project grants. Revoke the key on this page when finished; deleting client configuration alone does not revoke it.`;
}

export function apiSetupPrompt(endpoint: string, organization: string, project: string, access = "As listed for your key: All projects, including future projects, or Selected projects"): string {
  const base = endpoint.replace(/\/mcp$/, `/api/v2/organizations/${encodeURIComponent(organization)}`);
  return `Connect directly to the Detent HTTP API without an MCP client.
Organization: ${organization}
API base URL: ${base}
${keySetupPrompt(endpoint, access)}

Use Authorization: Bearer with the private DETENT_API_KEY environment variable; no browser cookies, session or CSRF state. Select a granted project ID from Settings → API & MCP; ${project} is the initial project context. With shell tracing disabled, verify an authorized read:
DETENT_API_BASE='${base}'
DETENT_PROJECT_ID='${project}'
curl --fail --silent --show-error --header "Authorization: Bearer $DETENT_API_KEY" "$DETENT_API_BASE/projects/$DETENT_PROJECT_ID"
curl --fail --silent --show-error --header "Authorization: Bearer $DETENT_API_KEY" "$DETENT_API_BASE/projects/$DETENT_PROJECT_ID/work-items?limit=20"
Check returned project/organization IDs before reporting success; report no secret values. These are implemented GET routes. Use the native endpoint tables and request schemas at https://github.com/digitaldrywood/detent/blob/develop/docs/hub-api.md (Native collaboration and Changes); hosted API keys do not grant worker, runner or instance-admin endpoints. Do not invent /api/v1/state, an OpenAPI route, or unrestricted organization enumeration on Cloud.

Before mutations, consult that operation's documented payload, revisions, idempotency and approval requirements. Ordinary writes require Write scope and current write grants. Authorized operations execute directly after current scope, project and grant checks. Confirmation is controlled by your client's local permission settings. Use narrower API keys to limit authority. Workflow requests retain the orchestrator lane owner.`;
}

export function mcpSetupPrompt(endpoint: string, organization: string, project: string, access = "As listed for your key: All projects, including future projects, or Selected projects"): string {
  return `Configure your supported MCP client for this exact Detent Cloud organization endpoint:
${endpoint}
Organization: ${organization}
${keySetupPrompt(endpoint, access)}

Use Streamable HTTP with application/json JSON POSTs, Accept: application/json, text/event-stream and Authorization: Bearer from the private DETENT_API_KEY environment variable. No browser cookies, session or CSRF token is needed. GET/SSE and MCP OAuth are unsupported. For example, in Codex client configuration:
[mcp_servers.detent]
url = "${endpoint}"
bearer_token_env_var = "DETENT_API_KEY"
For another client, use its documented private header/secret configuration. If it cannot send bearer headers with MCP JSON POSTs or requires SSE/OAuth, explicitly report that client as unsupported and stop.

Initialize MCP, retain the returned Mcp-Session-Id and negotiated protocol version, send notifications/initialized, then discover every tools/list page. Call the discovered work_list tool with {"project_id":"${project}","limit":20}, using a granted project selected on the same page. Verify the result belongs to this organization/project and report success without the key. Authorized calls execute directly. Configure sensitive-operation confirmation in your client; connecting never expands authority.`;
}

export function MCPSettings(): React.ReactElement {
  return basePath().startsWith("/organizations/") ? <OrganizationAPIKeysSettings /> : <LegacyMCPSettings />;
}

function LegacyMCPSettings(): React.ReactElement {
  const account = useAccountBootstrap();
  const endpoint = organizationMCPEndpoint(account);
  const [available, setAvailable] = React.useState(false);
  const [createdKey, setCreatedKey] = React.useState<APIKeyAccess | null>(null);
  React.useEffect(() => setCreatedKey(null), [account]);
  function prompts(key: APIKeyAccess | null) {
    const access =
      key?.project_access === "selected"
        ? `Selected projects: ${key.project_ids.map((id) => account?.projects.find((project) => project.id === id)?.name ?? id).join(", ")}`
        : key
          ? "All projects, including future projects"
          : "As listed for your key: All projects, including future projects, or Selected projects";
    const project =
      (key?.project_access === "selected"
        ? key.project_ids[0]
        : account?.projects[0]?.id) ?? "YOUR_GRANTED_PROJECT_ID";
    return {
      direct: apiSetupPrompt(
        endpoint!,
        account!.organization.id,
        project,
        access,
      ),
      mcp: mcpSetupPrompt(endpoint!, account!.organization.id, project, access),
    };
  }
  const setup = endpoint && account ? prompts(createdKey) : null;
  return (
    <SettingsPageContainer>
      <SettingsSection
        id="settings-mcp"
        title="Connect your agent"
        icon={<PlugIcon className="size-3.5" />}
      >
        <div className="px-3 py-3 text-[13px] leading-relaxed text-muted-foreground sm:px-4">
          One organization connection, one shared key, two setup prompts. Every
          call checks your current membership, project permissions, key
          permissions, expiry and revocation.
        </div>
        {endpoint && account ? (
          <>
            <ConnectionValue label="MCP endpoint" value={endpoint} />
            <ConnectionValue
              label="API base URL"
              value={endpoint.replace(
                /\/mcp$/,
                `/api/v2/organizations/${encodeURIComponent(account.organization.id)}`,
              )}
            />
          </>
        ) : (
          <SettingsRow
            title="Organization connection unavailable"
            description="The organization HTTPS URL is unavailable. Ask your Cloud operator to confirm its public URL."
          />
        )}
        {setup && available && (
          <>
            <SettingsRow
              title="Direct API setup prompt"
              description="Connect using HTTP requests and your private DETENT_API_KEY."
              control={
                <SetupPromptControl
                  label="Direct API setup prompt"
                  value={setup.direct}
                  preview
                />
              }
            />
            <SettingsRow
              title="MCP setup prompt"
              description="Connect a supported MCP client using the same private key."
              control={
                <SetupPromptControl
                  label="MCP setup prompt"
                  value={setup.mcp}
                  preview
                />
              }
            />
          </>
        )}
      </SettingsSection>
      <APIKeysSettings
        onAvailability={setAvailable}
        onKeyCreated={setCreatedKey}
        setupActions={(key) => {
          if (!endpoint || !account)
            return (
              <p className="text-xs text-muted-foreground">
                Return to API & MCP settings when your organization connection
                is available.
              </p>
            );
          const setup = prompts(key);
          return (
            <div className="flex flex-wrap gap-2">
              <SetupPromptControl
                label="Direct API setup prompt"
                copyLabel="Copy Direct API prompt"
                value={setup.direct}
              />
              <SetupPromptControl
                label="MCP setup prompt"
                copyLabel="Copy MCP prompt"
                value={setup.mcp}
              />
            </div>
          );
        }}
      />

      <SettingsSection id="settings-mcp-permissions" title="Permissions and tools">
        <SettingsRow title="Access follows your current permissions" description="Viewers can read permitted projects. Writes require a role and project grant that allow the operation; administration requires owner or admin authority. Billing requires an owner without a support session. Support access keeps its existing restrictions." />
        <SettingsRow title="Client confirmation" description="Authorized calls execute directly. Your client’s permission settings control confirmation for sensitive operations. Use narrower API key scopes and project access to limit what it can do." />
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
        <SettingsRow title="Invalid or revoked credential" description="Check the shared API key, expiry and revocation on this page. Worker and runner credentials cannot grant operator MCP access. A self-hosted daemon can report unauthorized, token_expired or token_revoked." />
        <SettingsRow title="Insufficient permissions" description="A 403 access_denied or tool access-denied result requires checking the current role, scope, project grants and resource ownership. Reconnecting does not grant access. Ask an organization administrator for the permissions the operation needs." />
        <SettingsRow title="Transport or unavailable tools" description="A GET returns 405: server-sent events are not supported. Send application/json POST requests through an HTTP MCP client. If a tool is unavailable, refresh discovery and check deployment services and capability coverage. Discovery alone never authorizes a call." />
      </SettingsSection>
    </SettingsPageContainer>
  );
}
