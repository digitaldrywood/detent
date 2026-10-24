import React from "react";
import { Link } from "@tanstack/react-router";
import { CopyIcon, PlusIcon } from "lucide-react";
import { Button } from "../../components/ui/button.tsx";
import { Input } from "../../components/ui/input.tsx";
import { Label } from "../../components/ui/label.tsx";
import {
  Tabs,
  TabsList,
  TabsTab,
  TabsPanel,
} from "../../components/ui/tabs.tsx";
import { useCopyToClipboard } from "../../hooks/useCopyToClipboard.ts";
import { useAccountApi, useAccountBootstrap } from "../account/context.ts";
import { useMutation, useResource } from "../account/useResource.ts";
import type { CreatedAccessKey } from "../../contracts/accessKeys.ts";
import { formatRelativeTimeLabel } from "../../timestampFormat.ts";
import { ReporterPage } from "./ReporterPage.tsx";
import {
  MCP_URL,
  readConnections,
  reporterInstructions,
} from "./connections.ts";
import { useRelativeTimeTick } from "../settings/settingsLayout.tsx";

function Copy({
  value,
  children,
  primary = false,
}: {
  value: string;
  children: React.ReactNode;
  primary?: boolean;
}) {
  const [failed, setFailed] = React.useState(false);
  const copy = useCopyToClipboard({
    target: "Connection instructions",
    onError: () => setFailed(true),
    onCopy: () => setFailed(false),
  });
  return (
    <>
      <Button
        variant={primary ? "default" : "outline"}
        size="sm"
        onClick={() => copy.copyToClipboard(value)}
      >
        <CopyIcon aria-hidden />
        {copy.isCopied ? "Copied" : children}
      </Button>
      {failed ? (
        <p role="alert" className="text-sm text-destructive-foreground">
          Could not copy. Select the text and copy it manually.
        </p>
      ) : null}
    </>
  );
}

function KeyInstructions({
  token,
  onDone,
}: {
  token: string;
  onDone?: () => void;
}) {
  const instructions = reporterInstructions(token);
  return (
    <div className="space-y-4">
      <pre className="overflow-x-auto whitespace-pre-wrap break-words rounded-lg border border-border bg-muted p-4 font-mono text-sm">
        {instructions}
      </pre>
      <div className="flex flex-wrap items-center gap-2">
        <Copy value={instructions} primary>
          Copy instructions
        </Copy>
        {onDone ? (
          <Button size="sm" variant="outline" onClick={onDone}>
            Done
          </Button>
        ) : null}
        <p className="text-sm text-warning-foreground">
          This key is shown once. Anyone who has it can file issues as you.
        </p>
      </div>
    </div>
  );
}

function AssistantSteps({
  assistant,
  organization,
}: {
  assistant: "ChatGPT" | "Claude";
  organization: string;
}) {
  const steps = [
    assistant === "ChatGPT" ? (
      <>
        Open ChatGPT settings and find Connectors (some plans call it Apps).
        Turn on developer mode if ChatGPT asks, then choose Create.
      </>
    ) : (
      <>
        In the Claude desktop app or claude.ai, open Settings, then Connectors,
        and choose Add custom connector.
      </>
    ),
    <>
      Name it <strong>Detent</strong> and paste this address:
      <div className="mt-2 flex flex-wrap items-center gap-2">
        <Input
          aria-label="Server URL"
          value={MCP_URL}
          readOnly
          className="min-w-0 flex-1 font-mono"
        />
        <Copy value={MCP_URL}>Copy</Copy>
      </div>
    </>,
    assistant === "ChatGPT" ? (
      <>
        Choose OAuth, save, and sign in to Detent when ChatGPT asks. Approve
        access to {organization}.
      </>
    ) : (
      <>
        Click Connect and sign in to Detent. Approve access to {organization}.
      </>
    ),
    <>
      Try it. Ask {assistant}:
      <blockquote className="mt-2 rounded-lg bg-muted p-3">
        “Use Detent to file a bug: the Export button on the Reports page does
        nothing when I click it.”
      </blockquote>
    </>,
  ];
  return (
    <>
      <ol className="divide-y divide-border rounded-xl border border-border bg-card">
        {steps.map((step, index) => (
          <li key={index} className="flex gap-4 p-4 sm:p-5">
            <span className="flex size-6 shrink-0 items-center justify-center rounded-full bg-muted text-sm font-semibold text-muted-foreground">
              {index + 1}
            </span>
            <div className="min-w-0 flex-1 text-sm leading-relaxed">{step}</div>
          </li>
        ))}
      </ol>
      <p className="mt-5 text-sm text-muted-foreground">
        No key needed. You sign in to Detent once, and {assistant} can file
        issues, add comments, and check status. It only sees issues you filed.
      </p>
    </>
  );
}

export function ConnectAI() {
  const account = useAccountBootstrap()!;
  const api = useAccountApi();
  const connections = useResource(
    () => readConnections(api, account.organization.id),
    [api, account.organization.id],
  );
  const [connecting, setConnecting] = React.useState(false);
  const [creating, setCreating] = React.useState(false);
  const [name, setName] = React.useState("");
  const [created, setCreated] = React.useState<CreatedAccessKey | null>(null);
  useRelativeTimeTick(60_000);
  const hadConnections = React.useRef<boolean | null>(null);
  if (connections.value && hadConnections.current === null)
    hadConnections.current = !!(
      connections.value.keys.length || connections.value.grants.length
    );
  const returning = hadConnections.current === true;
  const create = useMutation(async (keyName: string) => {
    const key = await api.createPersonalKey({
      name: keyName,
      permission: "write",
      access_context: "selected",
      organizations: [
        {
          organization_id: account.organization.id,
          project_access: "all",
          project_ids: [],
        },
      ],
      expires_days: 90,
      never_expires: false,
    });
    setCreated(key);
    await connections.refresh();
  });
  const revoke = useMutation(async (id: string, oauth: boolean) => {
    if (oauth) await api.revokeOAuthGrant(id);
    else await api.revokePersonalKey(id);
    await connections.refresh();
  });
  const firstVisit = !returning || connecting;
  const keyForm = (
    <div className="space-y-4 rounded-xl border border-border bg-card p-4 sm:p-5">
      {returning ? (
        <div className="space-y-2">
          <Label htmlFor="reporter-key-name">Key name</Label>
          <Input
            id="reporter-key-name"
            value={name}
            readOnly={created !== null}
            onChange={(event) => setName(event.currentTarget.value)}
            placeholder="Codex on work laptop"
          />
        </div>
      ) : (
        <p className="text-sm leading-relaxed">
          Paste these instructions into Claude Code, Codex, or any agent that
          can change its own settings. They contain your personal key, and the
          agent sets itself up from them.
        </p>
      )}
      {created ? (
        <KeyInstructions
          token={created.token}
          onDone={
            returning
              ? () => {
                  setCreated(null);
                  setCreating(false);
                  setConnecting(false);
                  setName("");
                }
              : undefined
          }
        />
      ) : (
        <Button
          size="sm"
          disabled={create.pending || (returning && !name.trim())}
          onClick={() => void create.call(name.trim() || "My agent")}
        >
          {create.pending
            ? "Creating key…"
            : returning
              ? "Create key"
              : "Create my key"}
        </Button>
      )}
    </div>
  );
  return (
    <ReporterPage title="Connect AI" parent={account.organization.name}>
      {connections.loading && !connections.value ? (
        <p role="status">Loading connections…</p>
      ) : connections.error ? (
        <p role="alert">{connections.error.message}</p>
      ) : (
        <>
          {firstVisit ? (
            <>
              <div className="space-y-2">
                <p className="text-sm text-muted-foreground">
                  Welcome to {account.organization.name}. You're a reporter
                  here: you send in problems and requests, and the team works on
                  them.
                </p>
                <h1 className="text-2xl font-semibold tracking-tight">
                  Connect your AI assistant
                </h1>
                <p className="text-sm leading-relaxed">
                  Tell your AI assistant about a bug or something you want
                  changed, and it files the issue in Detent for you. Later, ask
                  it where your issues stand. Pick the assistant you use.
                </p>
              </div>
              <Tabs defaultValue="chatgpt" variant="segmented">
                <TabsList>
                  <TabsTab value="chatgpt">ChatGPT</TabsTab>
                  <TabsTab value="claude">Claude</TabsTab>
                  <TabsTab value="agents">
                    Claude Code, Codex, other agents
                  </TabsTab>
                </TabsList>
                <TabsPanel value="chatgpt">
                  <AssistantSteps
                    assistant="ChatGPT"
                    organization={account.organization.name}
                  />
                </TabsPanel>
                <TabsPanel value="claude">
                  <AssistantSteps
                    assistant="Claude"
                    organization={account.organization.name}
                  />
                </TabsPanel>
                <TabsPanel value="agents">{keyForm}</TabsPanel>
              </Tabs>
              <div className="flex flex-wrap items-center justify-between gap-3 text-sm">
                <p className="text-muted-foreground">
                  Prefer the web? You can also file issues from My issues.
                </p>
                <Button variant="link" render={<Link to="/my-issues" />}>
                  Go to My issues →
                </Button>
              </div>
            </>
          ) : (
            <>
              <div>
                <h1 className="text-2xl font-semibold tracking-tight">
                  Connect AI
                </h1>
                <p className="mt-1 text-sm text-muted-foreground">
                  The assistants and keys that can file issues as you.
                </p>
              </div>
              <section className="space-y-3">
                <div className="flex items-center justify-between gap-2">
                  <h2 className="text-sm font-semibold">
                    Connected assistants
                  </h2>
                  <Button
                    size="sm"
                    variant="outline"
                    onClick={() => setConnecting(true)}
                  >
                    Connect another
                  </Button>
                </div>
                <div className="divide-y divide-border rounded-xl border border-border bg-card">
                  {connections.value?.grants.map((grant) => (
                    <div
                      key={grant.id}
                      className="flex items-center justify-between gap-3 p-4"
                    >
                      <div className="min-w-0">
                        <p className="break-words text-sm">
                          {grant.client_name}
                        </p>
                        <p className="text-xs text-muted-foreground">
                          Connected{" "}
                          {new Date(grant.connected_at).toLocaleDateString()} ·{" "}
                          {grant.last_used_at
                            ? `last used ${formatRelativeTimeLabel(grant.last_used_at)}`
                            : "never used"}
                        </p>
                      </div>
                      <Button
                        variant="ghost-destructive"
                        size="sm"
                        disabled={revoke.pending}
                        onClick={() => void revoke.call(grant.id, true)}
                      >
                        Disconnect
                      </Button>
                    </div>
                  ))}
                  {connections.value?.grants.length === 0 ? (
                    <p className="p-4 text-sm text-muted-foreground">
                      No connected assistants.
                    </p>
                  ) : null}
                </div>
              </section>
              <section className="space-y-3">
                <div className="flex items-center justify-between gap-2">
                  <h2 className="text-sm font-semibold">Personal keys</h2>
                  {!creating ? (
                    <Button
                      size="sm"
                      variant="outline"
                      onClick={() => {
                        setCreating(true);
                        setCreated(null);
                        setName("");
                      }}
                    >
                      <PlusIcon aria-hidden />
                      Create key
                    </Button>
                  ) : null}
                </div>
                <p className="text-sm text-muted-foreground">
                  For Claude Code, Codex, and other agents that use a key
                  instead of signing in.
                </p>
                {creating ? keyForm : null}
                <div className="divide-y divide-border rounded-xl border border-border bg-card">
                  {connections.value?.keys
                    .filter((key) => key.id !== created?.id)
                    .map((key) => (
                      <div
                        key={key.id}
                        className="flex items-center justify-between gap-3 p-4"
                      >
                        <div className="min-w-0">
                          <p className="break-words text-sm">{key.name}</p>
                          <p className="text-xs text-muted-foreground">
                            Created{" "}
                            {new Date(key.created_at).toLocaleDateString()} ·{" "}
                            {key.last_used_at
                              ? `last used ${formatRelativeTimeLabel(key.last_used_at)}`
                              : "never used"}{" "}
                            ·{" "}
                            {key.expires_at
                              ? `expires ${new Date(key.expires_at).toLocaleDateString()}`
                              : "never expires"}
                          </p>
                        </div>
                        <Button
                          size="sm"
                          variant="ghost-destructive"
                          disabled={revoke.pending}
                          onClick={() => void revoke.call(key.id, false)}
                        >
                          Revoke
                        </Button>
                      </div>
                    ))}
                  {connections.value?.keys.length === 0 ? (
                    <p className="p-4 text-sm text-muted-foreground">
                      No personal keys.
                    </p>
                  ) : null}
                </div>
              </section>
            </>
          )}
          {create.error || revoke.error ? (
            <p role="alert" className="text-sm text-destructive-foreground">
              {create.error?.message ?? revoke.error?.message}
            </p>
          ) : null}
        </>
      )}
    </ReporterPage>
  );
}
