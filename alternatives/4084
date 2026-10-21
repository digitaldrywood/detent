import {
  AlertDialog,
  AlertDialogClose,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogPopup,
  AlertDialogTitle,
} from "../../components/ui/alert-dialog.tsx";
import React from "react";
import type {
  AccessKey,
  CreatedAccessKey,
  CreateAccessKey,
  KeyContext,
  KeyOrganization,
} from "../../contracts/accessKeys.ts";
import { Toggle, ToggleGroup } from "../../components/ui/toggle-group.tsx";
import { Button } from "../../components/ui/button.tsx";
import { Badge } from "../../components/ui/badge.tsx";
import { Radio, RadioGroup } from "../../components/ui/radio-group.tsx";
import { OrganizationKeyControls } from "./OrganizationAPIKeysSettings.tsx";
import { Checkbox } from "../../components/ui/checkbox.tsx";
import { Input } from "../../components/ui/input.tsx";
import { Label } from "../../components/ui/label.tsx";
import {
  Dialog,
  DialogClose,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogPanel,
  DialogPopup,
  DialogTitle,
} from "../../components/ui/dialog.tsx";
import {
  useAccountApi,
  useAccountBootstrap,
  useCanManage,
} from "../account/context.ts";
import { ControlError, NativeSelect } from "../account/controls.tsx";
import { ConnectionValue, SetupPromptControl } from "./keySetup.tsx";
import {
  SettingsPageContainer,
  SettingsRow,
  SettingsSection,
  useRelativeTimeTick,
} from "./settingsLayout.tsx";
import { formatRelativeTimeLabel } from "../../timestampFormat.ts";

export function lastKeyUse(value: string | null | undefined): React.ReactNode {
  return value ? (
    <span title={new Date(value).toLocaleString()}>
      Last used {formatRelativeTimeLabel(value)}
    </span>
  ) : (
    "Never used"
  );
}

export function KeyScope({
  apiKey,
  context,
}: {
  readonly apiKey: AccessKey;
  readonly context?: KeyContext | null;
}) {
  return (
    <div className="space-y-2">
      <div className="flex flex-wrap gap-1.5">
        <Badge variant="outline">
          {apiKey.kind === "service"
            ? "Organization projects"
            : apiKey.access_context === "global"
              ? "All my orgs"
              : "Selected orgs"}
        </Badge>
        {(apiKey.organizations ?? []).map((org) => (
          <Badge key={org.organization_id} variant="outline">
            {context?.organizations.find(
              (item) => item.organization_id === org.organization_id,
            )?.name ?? org.organization_id}
            :{" "}
            {org.project_access === "all"
              ? "All projects"
              : `${org.project_ids.length} selected ${org.project_ids.length === 1 ? "project" : "projects"}`}
          </Badge>
        ))}
      </div>
      {apiKey.effective_reach && (
        <div className="space-y-1 text-xs text-muted-foreground">
          <p>Effective reach today</p>
          {apiKey.effective_reach.length === 0 && (
            <p>No organization access today.</p>
          )}
          {apiKey.effective_reach.map((org) => (
            <p key={org.organization_id} className="break-words">
              {org.name} ·{" "}
              {org.status === "pending"
                ? "Awaiting approval"
                : org.status === "blocked"
                  ? "Blocked"
                  : org.status === "inactive"
                    ? "Inactive"
                    : `${org.role} · ${org.projects.length ? org.projects.map((project) => `${project.name}${project.can_write ? " (write)" : " (read)"}`).join(", ") : "No project access"}`}
            </p>
          ))}
        </div>
      )}
    </div>
  );
}

export function keySetupPrompt(
  endpoint: string,
  direct = false,
  kind?: AccessKey["kind"],
): string {
  const origin = endpoint.replace(/\/mcp$/, "");
  return `Use a Detent key from Settings → API keys & MCP. A key never grants more than its owner has. Store it privately as DETENT_API_KEY; never paste it into chat, logs or source files. ${direct ? `Use Authorization: Bearer with that private environment variable at ${origin}/api/v2. Read GET /api/v2/organizations, then GET /api/v2/organizations/ORG_ID/projects to choose a granted organization and project before using documented routes.` : `Configure one Streamable HTTP MCP server at ${endpoint}, with bearer_token_env_var = "DETENT_API_KEY". Initialize MCP, retain Mcp-Session-Id and protocol version, send notifications/initialized and discover every tools/list page. Call connection_info and confirm key.kind is ${kind ? `"${kind}"` : '"personal" or "service"'}${kind ? ", matching the key just created" : " and explain who it acts as"} before reporting success. Use organization_list and list_projects to choose granted organization_id and project_id selectors for calls.`} Check the returned organization and project before reporting success. Current membership, project grants, org policy, permission, expiry and revocation apply on every call. Your client's permission settings control sensitive-operation confirmation. This prompt contains no secret.`;
}

function useKeyOwnerName(apiKey: AccessKey) {
  const api = useAccountApi();
  const { kind, owner, owner_email: ownerEmail } = apiKey;
  const [ownerName, setOwnerName] = React.useState(ownerEmail || owner);
  React.useEffect(() => {
    if (kind !== "personal") return;
    let current = true;
    void api
      .members()
      .then(({ members }) => {
        const member = members.find(
          (item) => item.user_id === owner || item.email === ownerEmail,
        );
        if (current && member?.name) setOwnerName(member.name);
      })
      .catch(() => {});
    return () => {
      current = false;
    };
  }, [api, kind, owner, ownerEmail]);
  return ownerName;
}

function useKeyLastCall(apiKey: AccessKey, preserveFirst = false) {
  const api = useAccountApi();
  const { id, kind } = apiKey;
  const [lastCall, setLastCall] = React.useState(apiKey.last_used_at ?? null);
  useRelativeTimeTick(60_000);

  React.useEffect(() => {
    let current = true;
    let timer: ReturnType<typeof setTimeout>;
    async function poll() {
      try {
        const result = await (kind === "personal"
          ? api.personalKeys()
          : api.serviceKeys());
        if (!current) return;
        const used = result.keys.find((key) => key.id === id)?.last_used_at;
        if (used) {
          setLastCall((previous) => (preserveFirst ? previous ?? used : used));
        }
      } catch {
        if (!current) return;
      }
      if (current) timer = setTimeout(() => void poll(), 5_000);
    }
    void poll();
    return () => {
      current = false;
      clearTimeout(timer);
    };
  }, [api, id, kind, preserveFirst]);

  return lastCall;
}

function SetupTransport({
  value,
  onChange,
}: {
  readonly value: string;
  readonly onChange: (value: string) => void;
}) {
  return (
    <ToggleGroup
      aria-label="Setup connection"
      value={[value]}
      onValueChange={(values) => {
        if (values[0]) onChange(values[0]);
      }}
    >
      <Toggle value="mcp">MCP</Toggle>
      <Toggle value="rest">REST API</Toggle>
    </ToggleGroup>
  );
}

function ConnectKeySteps({
  apiKey,
  endpoint,
  onRotate,
}: {
  readonly apiKey: AccessKey;
  readonly endpoint: string;
  readonly onRotate: () => void;
}) {
  const account = useAccountBootstrap();
  const ownerName = useKeyOwnerName(apiKey);
  const lastCall = useKeyLastCall(apiKey);
  const [transport, setTransport] = React.useState("mcp");
  const promptLabel = `${transport === "mcp" ? "MCP" : "REST"} setup prompt`;
  const prompt = keySetupPrompt(endpoint, transport === "rest", apiKey.kind);
  return (
    <>
      <DialogPanel>
        <ol className="space-y-5">
          <li className="flex gap-3">
            <span className="font-semibold text-muted-foreground">1</span>
            <div className="min-w-0 flex-1 space-y-2">
              <h3 className="text-sm font-medium">Use the key you saved</h3>
              <p className="text-sm text-muted-foreground">
                The token was shown once, when the key was created. Your agent
                reads it from DETENT_API_KEY. Lost it?{" "}
                {apiKey.kind === "personal" ? (
                  <Button size="sm" variant="link" onClick={onRotate}>
                    Rotate this key
                  </Button>
                ) : (
                  "Create a replacement key."
                )}
              </p>
            </div>
          </li>
          <li className="flex gap-3">
            <span className="font-semibold text-muted-foreground">2</span>
            <div className="min-w-0 flex-1 space-y-2">
              <h3 className="text-sm font-medium">
                Give your agent the setup prompt
              </h3>
              <SetupTransport value={transport} onChange={setTransport} />
              <pre
                aria-label={`${promptLabel} preview`}
                tabIndex={0}
                className="max-h-48 overflow-y-auto rounded-lg border border-border bg-background p-3 font-mono text-xs leading-relaxed whitespace-pre-wrap [overflow-wrap:anywhere]"
              >
                {prompt}
              </pre>
              <SetupPromptControl
                key={transport}
                label={promptLabel}
                value={prompt}
              />
            </div>
          </li>
          <li className="flex gap-3">
            <span
              className={
                lastCall
                  ? "font-semibold text-success-foreground"
                  : "font-semibold text-muted-foreground"
              }
            >
              3
            </span>
            <div className="min-w-0 flex-1 space-y-2">
              <h3 className="text-sm font-medium">Check it connected</h3>
              <p
                role="status"
                className="flex items-center gap-2 text-sm break-words text-muted-foreground"
              >
                {lastCall ? (
                  <>
                    <span
                      aria-hidden="true"
                      className="size-1.5 shrink-0 rounded-full bg-success-foreground"
                    />
                    <span>
                      Last call {formatRelativeTimeLabel(lastCall)} ·
                      acting as{" "}
                      {apiKey.kind === "personal"
                        ? ownerName
                        : `${account?.organization.name} (service)`}
                    </span>
                  </>
                ) : (
                  "Waiting for the first call…"
                )}
              </p>
            </div>
          </li>
        </ol>
      </DialogPanel>
      <DialogFooter>
        <DialogClose render={<Button>Done</Button>} />
      </DialogFooter>
    </>
  );
}

function CreatedKeySteps({
  created,
  endpoint,
}: {
  readonly created: CreatedAccessKey;
  readonly endpoint: string;
}) {
  const account = useAccountBootstrap();
  const { kind } = created;
  const [revealed, setRevealed] = React.useState(false);
  const [keyCopied, setKeyCopied] = React.useState(false);
  const [copied, setCopied] = React.useState<string | null>(null);
  const [error, setError] = React.useState<string | null>(null);
  const [transport, setTransport] = React.useState("mcp");
  const [fullPrompt, setFullPrompt] = React.useState(false);
  const firstUse = useKeyLastCall(created, true);
  const ownerName = useKeyOwnerName(created);
  const prompt = keySetupPrompt(endpoint, transport === "rest", kind);
  const promptLabel = `${transport === "mcp" ? "MCP" : "REST API"} setup prompt`;

  async function copy(value: string, target: string) {
    try {
      await navigator.clipboard.writeText(value);
      setError(null);
      setCopied(target);
      if (target === "key" || target === "export") setKeyCopied(true);
    } catch {
      setError(
        "Could not copy. Select and save the key privately before closing.",
      );
    }
  }

  return (
    <>
      <DialogPanel className="space-y-5">
        <ol className="space-y-5">
          <li className="flex gap-3">
            <span
              aria-label={keyCopied ? "Step 1 complete" : "Step 1"}
              className={
                keyCopied
                  ? "font-semibold text-success-foreground"
                  : "font-semibold text-muted-foreground"
              }
            >
              1
            </span>
            <div className="min-w-0 flex-1 space-y-2">
              <h3 className="text-sm font-medium">
                Save the key on your machine
              </h3>
              <p className="text-sm text-muted-foreground">
                Store it as DETENT_API_KEY in your shell profile or secret
                store. Never paste it into a chat with your agent.
              </p>
              <Label htmlFor="new-api-key" className="sr-only">
                New API key
              </Label>
              <div className="flex items-center gap-2">
                <Input
                  id="new-api-key"
                  type={revealed ? "text" : "password"}
                  value={created.token}
                  readOnly
                  autoComplete="off"
                  className="min-w-0 font-mono"
                />
                <Button
                  variant="outline"
                  aria-controls="new-api-key"
                  aria-pressed={revealed}
                  onClick={() => setRevealed(!revealed)}
                >
                  {revealed ? "Hide" : "Reveal"}
                </Button>
              </div>
              <div className="flex flex-wrap items-center gap-2">
                <Button
                  aria-label="Copy key"
                  onClick={() => void copy(created.token, "key")}
                >
                  {copied === "key" ? "Copied" : "Copy key"}
                </Button>
                <Button
                  size="sm-multiline"
                  variant="link"
                  aria-label="Copy as export DETENT_API_KEY=… line"
                  onClick={() =>
                    void copy(
                      `export DETENT_API_KEY='${created.token.replace(/'/g, "'\\''")}'`,
                      "export",
                    )
                  }
                >
                  {copied === "export"
                    ? "Copied"
                    : "Copy as export DETENT_API_KEY=… line"}
                </Button>
              </div>
            </div>
          </li>
          <li className="flex gap-3">
            <span className="font-semibold text-muted-foreground">2</span>
            <div className="min-w-0 flex-1 space-y-2">
              <h3 className="text-sm font-medium">
                Give your agent the setup prompt
              </h3>
              <p className="text-sm text-muted-foreground">
                Paste it into Claude, Codex or any agent. It reads the key from
                DETENT_API_KEY, so the prompt holds no secret.
              </p>
              <ToggleGroup
                aria-label="Setup connection"
                value={[transport]}
                onValueChange={(values) => {
                  if (values[0]) {
                    setTransport(values[0]);
                    setCopied(null);
                  }
                }}
              >
                <Toggle value="mcp">MCP</Toggle>
                <Toggle value="rest">REST API</Toggle>
              </ToggleGroup>
              <pre
                aria-label={`${promptLabel} preview`}
                tabIndex={0}
                className="max-h-48 overflow-y-auto font-mono text-xs leading-relaxed whitespace-pre-wrap [overflow-wrap:anywhere]"
              >
                {fullPrompt
                  ? prompt
                  : transport === "mcp"
                    ? `Connect to ${endpoint} using DETENT_API_KEY. Call connection_info and confirm key.kind is "${kind}".`
                    : `Use Authorization: Bearer with DETENT_API_KEY at ${endpoint.replace(/\/mcp$/, "")}/api/v2.`}
              </pre>
              <div className="flex flex-wrap items-center gap-2">
                <Button
                  size="sm-multiline"
                  variant="outline"
                  aria-label={`Copy ${promptLabel}`}
                  onClick={() => void copy(prompt, transport)}
                >
                  {copied === transport ? "Copied" : `Copy ${promptLabel}`}
                </Button>
                <Button
                  size="sm"
                  variant="link"
                  aria-expanded={fullPrompt}
                  onClick={() => setFullPrompt(!fullPrompt)}
                >
                  {fullPrompt ? "Show less" : "Show full prompt"}
                </Button>
              </div>
            </div>
          </li>
          <li className="flex gap-3">
            <span
              className={
                firstUse
                  ? "font-semibold text-success-foreground"
                  : "font-semibold text-muted-foreground"
              }
            >
              3
            </span>
            <div className="min-w-0 flex-1 space-y-2">
              <h3 className="text-sm font-medium">
                {kind === "personal"
                  ? "Check it connected as you"
                  : "Check it connected"}
              </h3>
              <p
                role="status"
                className="text-sm break-words text-muted-foreground"
              >
                {firstUse ? (
                  <>
                    First call {formatRelativeTimeLabel(firstUse)} · acting as{" "}
                    {kind === "personal"
                      ? ownerName
                      : `${account?.organization.name} (service)`}
                  </>
                ) : (
                  "Waiting for the first call…"
                )}
              </p>
            </div>
          </li>
        </ol>
        <span role="status" className="sr-only">
          {copied ? "Copied" : ""}
        </span>
        <ControlError message={error} />
      </DialogPanel>
      <DialogFooter>
        <p className="min-w-0 flex-1 text-sm text-muted-foreground">
          {keyCopied
            ? "You can close this. The key stays in your list."
            : "Copy the key before closing. It cannot be shown again."}
        </p>
        <DialogClose
          render={
            <Button variant={keyCopied ? "default" : "secondary"}>Done</Button>
          }
        />
      </DialogFooter>
    </>
  );
}

export function APIKeysMCPSettings() {
  const api = useAccountApi();
  const account = useAccountBootstrap();
  const canManage = useCanManage();
  const [service, setService] = React.useState(false);
  const [serviceKeys, setServiceKeys] = React.useState<readonly AccessKey[]>(
    [],
  );
  const now = useRelativeTimeTick(60_000);
  const [keys, setKeys] = React.useState<readonly AccessKey[]>([]);
  const [context, setContext] = React.useState<KeyContext | null>(null);
  const [loaded, setLoaded] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);
  const [dialogError, setDialogError] = React.useState<string | null>(null);
  const [open, setOpen] = React.useState(false);
  const [created, setCreated] = React.useState<CreatedAccessKey | null>(null);
  const [action, setAction] = React.useState<{
    key: AccessKey;
    rotate: boolean;
  } | null>(null);
  const [busy, setBusy] = React.useState(false);
  const [name, setName] = React.useState("");
  const [permission, setPermission] =
    React.useState<CreateAccessKey["permission"]>("read");
  const [days, setDays] = React.useState("30");
  const [reach, setReach] = React.useState("global");
  const [selections, setSelections] = React.useState<
    readonly KeyOrganization[]
  >([]);
  const [personalHistory, setPersonalHistory] = React.useState(false);
  const [serviceHistory, setServiceHistory] = React.useState(false);
  const [transport, setTransport] = React.useState("mcp");
  const [connectId, setConnectId] = React.useState<string | null>(null);
  const currentApi = React.useRef(api);
  currentApi.current = api;
  const list = React.useCallback(async () => {
    const [personal, services] = await Promise.all([
      api.personalKeys(),
      canManage ? api.serviceKeys() : Promise.resolve({ keys: [] }),
    ]);
    return { personal: personal.keys, services: services.keys };
  }, [api, canManage]);
  React.useEffect(() => {
    let current = true;
    setLoaded(false);
    setKeys([]);
    setServiceKeys([]);
    setContext(null);
    setError(null);
    setOpen(false);
    setCreated(null);
    setAction(null);
    setConnectId(null);
    Promise.all([list(), api.keyContext()])
      .then(([listed, choices]) => {
        if (!current) return;
        setKeys(listed.personal);
        setServiceKeys(listed.services);
        setContext(choices);
        setLoaded(true);
      })
      .catch(() => {
        if (current) setError("Could not load API keys. Reload to try again.");
      });
    return () => {
      current = false;
    };
  }, [api, list]);
  const orgs =
    context?.organizations.filter(
      (org) => org.organization_id === account?.organization.id,
    ) ?? [];
  const inactive = (key: AccessKey) =>
    Boolean(key.revoked_at) ||
    Boolean(key.expires_at && new Date(key.expires_at).getTime() <= now);

  const connectKey = [...keys, ...(canManage ? serviceKeys : [])].find(
    (key) => key.id === connectId && !inactive(key),
  );
  const promptLabel = `${transport === "mcp" ? "MCP" : "REST"} setup prompt`;

  function openCreate() {
    setName("");
    setPermission("read");
    setDays("30");
    setService(false);
    setSelections([]);
    setReach("global");
    setCreated(null);
    setDialogError(null);
    setOpen(true);
  }
  async function refresh() {
    try {
      const listed = await list();
      if (currentApi.current === api) {
        setKeys(listed.personal);
        setServiceKeys(listed.services);
      }
    } catch {
      if (currentApi.current === api)
        setError(
          "The change succeeded, but the key list could not refresh. Reload after saving your key.",
        );
    }
  }
  async function create(event: React.FormEvent) {
    event.preventDefault();
    setBusy(true);
    setDialogError(null);
    const input: CreateAccessKey = {
      name: name.trim(),
      permission,
      expires_days: Number(days),
      never_expires: days === "0",
      access_context:
        !service && reach === "global"
          ? "global"
          : selections.length === 1 &&
              selections[0]?.project_access === "project"
            ? "project"
            : "selected",
      organizations: selections,
    };
    try {
      const result = await (service
        ? api.createServiceKey(input)
        : api.createPersonalKey(input));
      if (currentApi.current !== api) return;
      setCreated(result);
      await refresh();
    } catch {
      if (currentApi.current === api)
        setDialogError(
          "Could not create the key. Check your scope and current access, then try again.",
        );
    } finally {
      setBusy(false);
    }
  }
  async function applyAction() {
    if (!action) return;
    setBusy(true);
    setDialogError(null);
    try {
      let result: CreatedAccessKey | null = null;
      if (action.rotate) result = await api.rotatePersonalKey(action.key.id);
      else if (action.key.kind === "service")
        await api.revokeServiceKey(action.key.id);
      else await api.revokePersonalKey(action.key.id);
      if (currentApi.current !== api) return;
      setAction(null);
      if (result) {
        setService(false);
        setCreated(result);
        setOpen(true);
      }
      await refresh();
    } catch {
      if (currentApi.current === api)
        setDialogError(
          "Could not change this key. Check your current access and try again.",
        );
    } finally {
      setBusy(false);
    }
  }
  function chooseKind(value: string) {
    const nextService = value === "service";
    if (nextService && !canManage) return;
    setService(nextService);
    chooseReach(nextService ? "all" : "global");
  }
  function chooseReach(value: string) {
    setReach(value);
    setSelections(
      account && value !== "global"
        ? [
            {
              organization_id: account.organization.id,
              project_access:
                value === "project"
                  ? "project"
                  : value === "selected"
                    ? "selected"
                    : "all",
              project_ids: [],
            },
          ]
        : [],
    );
  }
  function changeOrg(
    id: string,
    access: KeyOrganization["project_access"],
    projects: readonly string[] = [],
  ) {
    setSelections((current) => [
      ...current.filter((org) => org.organization_id !== id),
      { organization_id: id, project_access: access, project_ids: projects },
    ]);
  }
  const invalid =
    (reach !== "global" && selections.length === 0) ||
    selections.some(
      (org) =>
        org.project_access !== "all" &&
        (org.project_ids.length === 0 ||
          (org.project_access === "project" && org.project_ids.length !== 1)),
    );
  function renderKeys(
    kind: "personal" | "service",
    listed: readonly AccessKey[],
  ) {
    const history = kind === "personal" ? personalHistory : serviceHistory;
    const setShowHistory =
      kind === "personal" ? setPersonalHistory : setServiceHistory;
    return (
      <SettingsSection
        title={
          kind === "personal"
            ? "Your personal keys"
            : "Organization service keys"
        }
      >
        {!loaded && !error && (
          <SettingsRow title="Loading API keys…" role="status" />
        )}
        {loaded &&
          listed.filter((key) => (history ? inactive(key) : !inactive(key)))
            .length === 0 && (
            <SettingsRow
              title={
                history
                  ? "No inactive keys"
                  : kind === "service"
                    ? "No active service keys"
                    : "No active personal keys"
              }
              description={
                history
                  ? "Revoked and expired keys appear here."
                  : "Create a key for your agent or integration."
              }
            />
          )}
        {listed
          .filter((key) => (history ? inactive(key) : !inactive(key)))
          .map((key) => (
            <SettingsRow
              key={key.id}
              title={
                <span className="flex flex-wrap items-center gap-2 break-words">
                  {key.name}
                  <Badge variant="outline">{key.permission}</Badge>
                  <Badge variant="outline">
                    {key.kind === "personal" ? "Personal" : "Service"}
                  </Badge>
                  {inactive(key) && (
                    <Badge variant="outline">
                      {key.revoked_at ? "Revoked" : "Expired"}
                    </Badge>
                  )}
                </span>
              }
              description={
                <span>
                  {lastKeyUse(key.last_used_at)} ·{" "}
                  {key.expires_at
                    ? `Expires ${new Date(key.expires_at).toLocaleDateString()}`
                    : "Never expires"}
                </span>
              }
              status={<KeyScope apiKey={key} context={context} />}
              control={
                !inactive(key) && (
                  <div className="flex flex-wrap gap-2">
                    <Button
                      size="xs"
                      variant="outline"
                      disabled={!context || busy}
                      aria-label={`Connect ${key.name}`}
                      onClick={() => setConnectId(key.id)}
                    >
                      Connect
                    </Button>
                    {key.kind === "personal" && (
                      <Button
                        size="xs"
                        variant="outline"
                        disabled={busy}
                        aria-label={`Rotate ${key.name}`}
                        onClick={() => {
                          setDialogError(null);
                          setAction({ key, rotate: true });
                        }}
                      >
                        Rotate
                      </Button>
                    )}
                    <Button
                      size="xs"
                      variant="destructive-outline"
                      disabled={busy}
                      aria-label={`Revoke ${key.name}`}
                      onClick={() => {
                        setDialogError(null);
                        setAction({ key, rotate: false });
                      }}
                    >
                      Revoke
                    </Button>
                  </div>
                )
              }
            />
          ))}
        {loaded && (
          <SettingsRow
            title="Key history"
            control={
              <Button
                size="xs"
                variant="ghost"
                onClick={() => setShowHistory(!history)}
              >
                {history ? "Show active keys" : "Show revoked and expired keys"}
              </Button>
            }
          />
        )}
      </SettingsSection>
    );
  }
  return (
    <>
      <SettingsPageContainer>
        <SettingsSection
          title="API keys & MCP"
          variant="plain"
          headerAction={
            <Button size="xs" onClick={openCreate} disabled={!loaded || busy}>
              Create key
            </Button>
          }
        >
          <p className="px-3 text-sm text-muted-foreground sm:px-4">
            Connect Claude, Codex, scripts and CI to{" "}
            {account?.organization.name ?? "your organization"}. A key never grants
            more than its owner has.
          </p>
        </SettingsSection>
        <SettingsSection
          title="Connect an agent"
          headerAction={
            <SetupTransport value={transport} onChange={setTransport} />
          }
        >
          <div className="space-y-3 p-3 sm:p-4">
            <p className="text-sm text-muted-foreground">
              One endpoint for every key. The prompt reads DETENT_API_KEY and
              holds no secret.
            </p>
            {context && (
              <>
                <div className="flex flex-wrap items-center gap-2">
                  <ConnectionValue
                    label="MCP endpoint"
                    value={context.mcp_endpoint}
                    inline
                  />
                  <SetupPromptControl
                    key={transport}
                    label={promptLabel}
                    value={keySetupPrompt(context.mcp_endpoint, transport === "rest")}
                    inlinePreview
                  />
                </div>
                <p className="text-xs break-words text-muted-foreground">
                  Claude Code:{" "}
                  <code className="font-mono [overflow-wrap:anywhere]">{`claude mcp add --transport http detent ${context.mcp_endpoint} --header 'Authorization: Bearer \${DETENT_API_KEY}'`}</code>
                </p>
              </>
            )}
          </div>
        </SettingsSection>
        {error && <ControlError message={error} />}
        {renderKeys("personal", keys)}
        {canManage && renderKeys("service", serviceKeys)}
        {canManage && <OrganizationKeyControls />}
      </SettingsPageContainer>
      <Dialog
        open={Boolean(connectKey)}
        onOpenChange={(value) => {
          if (!value) setConnectId(null);
        }}
      >
        <DialogPopup>
          {connectKey && context && (
            <>
              <DialogHeader>
                <DialogTitle>Connect {connectKey.name}</DialogTitle>
                <div>
                  <Badge variant="outline">
                    {connectKey.kind === "personal"
                      ? "Personal · acts as you"
                      : "Service · acts as the organization"}
                  </Badge>
                </div>
                <DialogDescription className="sr-only">
                  Three steps to connect your agent using the key you saved.
                </DialogDescription>
              </DialogHeader>
              <ConnectKeySteps
                key={connectKey.id}
                apiKey={connectKey}
                endpoint={context.mcp_endpoint}
                onRotate={() => {
                  setConnectId(null);
                  setDialogError(null);
                  setAction({ key: connectKey, rotate: true });
                }}
              />
            </>
          )}
        </DialogPopup>
      </Dialog>
      <Dialog
        open={open}
        onOpenChange={(value) => {
          if (busy) return;
          setOpen(value);
          if (!value) {
            setCreated(null);
            setDialogError(null);
          }
        }}
      >
        <DialogPopup>
          <DialogHeader>
            <DialogTitle>
              {created
                ? `${created.name} is ready`
                : service
                  ? "Create service key"
                  : "Create personal key"}
            </DialogTitle>
            {created && (
              <div>
                <Badge variant="outline">
                  {created.kind === "personal"
                    ? "Personal · acts as you"
                    : "Service · acts as the organization"}
                </Badge>
              </div>
            )}
            <DialogDescription>
              {created
                ? "Three steps to connect your agent. This is the only time the key is shown."
                : "A key never grants more than its owner has."}
            </DialogDescription>
          </DialogHeader>
          {created ? (
            open && (
              <CreatedKeySteps
                created={created}
                endpoint={context?.mcp_endpoint ?? ""}
              />
            )
          ) : (
            <form className="contents" onSubmit={(event) => void create(event)}>
              <DialogPanel className="space-y-4">
                <fieldset className="space-y-2">
                  <legend className="mb-2 text-sm font-medium">
                    Who does this key act as?
                  </legend>
                  <RadioGroup
                    value={service ? "service" : "personal"}
                    onValueChange={chooseKind}
                    disabled={busy}
                    aria-label="Who does this key act as?"
                  >
                    <Label className="flex min-h-8 items-center gap-2">
                      <Radio value="personal" />
                      You
                    </Label>
                    <Label className="flex min-h-8 items-center gap-2">
                      <Radio
                        value="service"
                        disabled={!canManage}
                        aria-describedby={
                          !canManage ? "service-key-reason" : undefined
                        }
                      />
                      The organization
                    </Label>
                  </RadioGroup>
                  {!canManage && (
                    <p
                      id="service-key-reason"
                      className="text-xs text-muted-foreground"
                    >
                      Owners and admins create service keys.
                    </p>
                  )}
                </fieldset>
                <div className="space-y-2">
                  <Label htmlFor="personal-key-name">Name</Label>
                  <Input
                    id="personal-key-name"
                    required
                    maxLength={128}
                    value={name}
                    disabled={busy}
                    onChange={(event) => setName(event.target.value)}
                  />
                </div>
                <div className="space-y-2">
                  <Label htmlFor="personal-key-permission">Permission</Label>
                  <NativeSelect
                    id="personal-key-permission"
                    disabled={busy}
                    value={permission}
                    onValueChange={(value) =>
                      setPermission(value as CreateAccessKey["permission"])
                    }
                    options={[
                      { value: "read", label: "Read" },
                      { value: "write", label: "Write" },
                      { value: "admin", label: "Admin" },
                    ]}
                  />
                </div>
                <div className="space-y-2">
                  <Label htmlFor="key-reach">Reach</Label>
                  <NativeSelect
                    id="key-reach"
                    value={reach}
                    disabled={busy}
                    onValueChange={(value) => chooseReach(value)}
                    options={
                      service
                        ? [
                            {
                              value: "all",
                              label: "All this organization's projects",
                            },
                            { value: "selected", label: "Selected projects" },
                            { value: "project", label: "One project" },
                          ]
                        : [
                            { value: "global", label: "All my organizations" },
                            { value: "all", label: "This organization only" },
                            { value: "project", label: "One project" },
                          ]
                    }
                  />
                </div>
                {(reach === "project" || reach === "selected") && (
                  <fieldset className="space-y-4">
                    <legend className="mb-2 text-sm font-medium">
                      Project
                    </legend>
                    {orgs.map((org) => {
                      const selection = selections.find(
                        (item) => item.organization_id === org.organization_id,
                      );
                      const selected = Boolean(selection);
                      return (
                        <div key={org.organization_id} className="space-y-2">
                          <Label className="flex min-h-8 items-center gap-2">
                            <span className="break-words">{org.name}</span>
                          </Label>
                          {selected && (
                            <>
                              {selection?.project_access === "project" ? (
                                <>
                                  <Label
                                    htmlFor={`project-${org.organization_id}`}
                                  >
                                    Project in {org.name}
                                  </Label>
                                  <NativeSelect
                                    id={`project-${org.organization_id}`}
                                    disabled={busy}
                                    value={selection.project_ids[0] ?? ""}
                                    onValueChange={(value) =>
                                      changeOrg(
                                        org.organization_id,
                                        "project",
                                        [value],
                                      )
                                    }
                                    options={[
                                      { value: "", label: "Choose a project" },
                                      ...org.projects.map((project) => ({
                                        value: project.id,
                                        label: project.name,
                                      })),
                                    ]}
                                  />
                                </>
                              ) : (
                                selection?.project_access === "selected" && (
                                  <div className="space-y-2">
                                    {org.projects.map((project) => (
                                      <Label
                                        key={project.id}
                                        className="flex min-h-8 items-center gap-2"
                                      >
                                        <Checkbox
                                          checked={selection.project_ids.includes(
                                            project.id,
                                          )}
                                          disabled={busy}
                                          onCheckedChange={(checked) =>
                                            changeOrg(
                                              org.organization_id,
                                              "selected",
                                              checked
                                                ? [
                                                    ...selection.project_ids,
                                                    project.id,
                                                  ]
                                                : selection.project_ids.filter(
                                                    (id) => id !== project.id,
                                                  ),
                                            )
                                          }
                                        />
                                        <span className="break-words">
                                          {project.name}
                                        </span>
                                      </Label>
                                    ))}
                                    {org.projects.length === 0 && (
                                      <p className="text-xs text-muted-foreground">
                                        No permitted projects in this org.
                                      </p>
                                    )}
                                  </div>
                                )
                              )}
                            </>
                          )}
                        </div>
                      );
                    })}
                  </fieldset>
                )}
                <div className="space-y-2">
                  <Label htmlFor="personal-key-expiry">Expires</Label>
                  <NativeSelect
                    id="personal-key-expiry"
                    disabled={busy}
                    value={days}
                    onValueChange={setDays}
                    options={[
                      { value: "7", label: "7 days" },
                      { value: "30", label: "30 days" },
                      { value: "90", label: "90 days" },
                      { value: "0", label: "Never" },
                    ]}
                  />
                </div>
                <ControlError message={dialogError} />
              </DialogPanel>
              <DialogFooter>
                <DialogClose
                  render={
                    <Button variant="outline" disabled={busy}>
                      Cancel
                    </Button>
                  }
                />
                <Button
                  type="submit"
                  disabled={busy || !name.trim() || invalid}
                >
                  {busy
                    ? "Creating…"
                    : service
                      ? "Create service key"
                      : "Create personal key"}
                </Button>
              </DialogFooter>
            </form>
          )}
        </DialogPopup>
      </Dialog>
      <AlertDialog
        open={action !== null}
        onOpenChange={(value) => {
          if (!value && !busy) {
            setAction(null);
            setDialogError(null);
          }
        }}
      >
        <AlertDialogPopup>
          <AlertDialogHeader>
            <AlertDialogTitle>
              {action?.rotate ? "Rotate" : "Revoke"} {action?.key.name}?
            </AlertDialogTitle>
            <AlertDialogDescription>
              {action?.rotate
                ? "The old token stops working immediately. Save the replacement privately and update your clients."
                : "API and MCP access using this key stops immediately."}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <div className="px-6 pb-4">
            <ControlError message={dialogError} />
          </div>
          <AlertDialogFooter>
            <AlertDialogClose
              render={
                <Button variant="outline" disabled={busy}>
                  Keep key
                </Button>
              }
            />
            <Button
              variant="destructive"
              disabled={busy}
              onClick={() => void applyAction()}
            >
              {busy ? "Saving…" : action?.rotate ? "Rotate key" : "Revoke key"}
            </Button>
          </AlertDialogFooter>
        </AlertDialogPopup>
      </AlertDialog>
    </>
  );
}
