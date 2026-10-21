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
import { ChevronDown, ChevronRight, MoreHorizontal } from "lucide-react";
import {
  Tabs,
  TabsList,
  TabsTab,
  TabsPanel,
} from "../../components/ui/tabs.tsx";
import {
  Table,
  TableHeader,
  TableBody,
  TableRow,
  TableHead,
  TableCell,
} from "../../components/ui/table.tsx";
import {
  Menu,
  MenuTrigger,
  MenuPopup,
  MenuItem,
} from "../../components/ui/menu.tsx";
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

function keyAccessSummary(
  apiKey: AccessKey,
  context?: KeyContext | null,
): string {
  if (apiKey.kind === "personal" && apiKey.access_context === "global") {
    return `All my orgs · ${apiKey.effective_reach?.length ?? 0}`;
  }
  const organizations = apiKey.organizations ?? [];
  const org = organizations[0];
  if (organizations.length === 1 && org) {
    if (org.project_access === "all") {
      return apiKey.kind === "service" ? "All org projects" : "This org";
    }
    if (org.project_ids.length === 1) {
      const projectId = org.project_ids[0];
      const reach =
        apiKey.effective_reach?.find(
          (item) => item.organization_id === org.organization_id,
        ) ??
        context?.organizations.find(
          (item) => item.organization_id === org.organization_id,
        );
      return `One project: ${reach?.projects.find((project) => project.id === projectId)?.name ?? projectId}`;
    }
    return `${org.project_ids.length} selected projects`;
  }
  return `${organizations.length} selected orgs`;
}

export function KeyScope({
  apiKey,
  context,
}: {
  readonly apiKey: AccessKey;
  readonly context?: KeyContext | null;
}) {
  const scope =
    apiKey.access_context === "global" && apiKey.kind === "personal"
      ? "All my orgs"
      : keyAccessSummary(apiKey, context);
  return (
    <div className="space-y-2 text-xs text-muted-foreground">
      <p>Effective reach today · {scope}</p>
      {apiKey.effective_reach?.length === 0 && (
        <p>No organization access today.</p>
      )}
      {apiKey.effective_reach && apiKey.effective_reach.length > 0 && (
        <div className="divide-y divide-border rounded-lg border border-border">
          {apiKey.effective_reach.map((org) => (
            <div
              key={org.organization_id}
              className="grid grid-cols-[minmax(0,1fr)_minmax(4rem,auto)_minmax(0,2fr)] gap-4 px-3 py-2"
            >
              <span className="break-words text-foreground">{org.name}</span>
              <span>{org.role}</span>
              <span className="break-words">
                {org.status === "pending"
                  ? "Awaiting approval"
                  : org.status === "blocked"
                    ? "Blocked"
                    : org.status === "inactive"
                      ? "Inactive"
                      : org.projects.length
                        ? org.projects
                            .map(
                              (project) =>
                                `${project.name}${project.can_write ? " (write)" : " (read)"}`,
                            )
                            .join(", ")
                        : "No project access"}
              </span>
            </div>
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
  const [showHistory, setShowHistory] = React.useState(false);
  const [keyTab, setKeyTab] = React.useState("personal");
  const [expandedKeys, setExpandedKeys] = React.useState<ReadonlySet<string>>(
    new Set(),
  );
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
    setKeyTab("personal");
    setExpandedKeys(new Set());
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
    const visible = listed.filter((key) => showHistory || !inactive(key));
    return (
      <TabsPanel value={kind} className="space-y-2 pt-3">
        <p className="text-xs text-muted-foreground">
          {kind === "personal"
            ? "Personal keys act as you: edits are recorded as you, and the key stops working if you leave an organization."
            : `Service keys act as ${account?.organization.name ?? "the organization"} for CI, bots and team servers. Owners and admins manage them; they keep working when their creator leaves.`}
        </p>
        <div className="overflow-hidden rounded-xl border border-border">
          <Table
            aria-label={kind === "personal" ? "Personal keys" : "Service keys"}
          >
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>Permission</TableHead>
                <TableHead>Access</TableHead>
                <TableHead>Last used</TableHead>
                <TableHead>Expires</TableHead>
                <TableHead>
                  <span className="sr-only">Actions</span>
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {!loaded && !error && (
                <TableRow>
                  <TableCell colSpan={6}>
                    <span role="status">Loading API keys…</span>
                  </TableCell>
                </TableRow>
              )}
              {loaded && visible.length === 0 && (
                <TableRow>
                  <TableCell
                    colSpan={6}
                    className="whitespace-normal text-muted-foreground"
                  >
                    No active {kind} keys. Create a key for your agent or
                    integration.
                  </TableCell>
                </TableRow>
              )}
              {visible.map((key) => {
                const isInactive = inactive(key);
                const expanded = expandedKeys.has(key.id);
                const reachId = `key-reach-${kind}-${key.id}`;
                return (
                  <React.Fragment key={key.id}>
                    <TableRow
                      className={
                        isInactive ? "text-muted-foreground" : undefined
                      }
                    >
                      <TableCell>
                        <div className="flex items-center gap-1">
                          <Button
                            size="icon-xs"
                            variant="ghost-muted"
                            aria-label={`${expanded ? "Collapse" : "Expand"} ${key.name}`}
                            aria-expanded={expanded}
                            aria-controls={reachId}
                            onClick={() =>
                              setExpandedKeys((current) => {
                                const next = new Set(current);
                                if (expanded) next.delete(key.id);
                                else next.add(key.id);
                                return next;
                              })
                            }
                          >
                            {expanded ? (
                              <ChevronDown aria-hidden="true" />
                            ) : (
                              <ChevronRight aria-hidden="true" />
                            )}
                          </Button>
                          <span className="font-medium">{key.name}</span>
                        </div>
                      </TableCell>
                      <TableCell>{key.permission}</TableCell>
                      <TableCell>{keyAccessSummary(key, context)}</TableCell>
                      <TableCell>
                        <span
                          className="inline-flex items-center gap-1.5 text-muted-foreground"
                          title={
                            key.last_used_at
                              ? new Date(key.last_used_at).toLocaleString()
                              : undefined
                          }
                        >
                          <span
                            aria-hidden="true"
                            className={`size-1.5 shrink-0 rounded-full ${!key.last_used_at ? "bg-muted-foreground/40" : now - new Date(key.last_used_at).getTime() < 86_400_000 ? "bg-success-foreground" : "bg-muted-foreground"}`}
                          />
                          {key.last_used_at
                            ? formatRelativeTimeLabel(key.last_used_at)
                            : "Never"}
                        </span>
                      </TableCell>
                      <TableCell>
                        {key.revoked_at
                          ? `Revoked ${new Date(key.revoked_at).toLocaleDateString()}`
                          : key.expires_at
                            ? `${isInactive ? "Expired " : ""}${new Date(key.expires_at).toLocaleDateString()}`
                            : "Never"}
                      </TableCell>
                      <TableCell>
                        {!isInactive && (
                          <div className="flex items-center justify-end gap-1">
                            <Button
                              size="xs"
                              variant="outline"
                              disabled={!context || busy}
                              aria-label={`Connect ${key.name}`}
                              onClick={() => setConnectId(key.id)}
                            >
                              Connect
                            </Button>
                            <Menu>
                              <MenuTrigger
                                render={
                                  <Button
                                    size="icon-xs"
                                    variant="ghost-muted"
                                  />
                                }
                                disabled={busy}
                                aria-label={`More actions for ${key.name}`}
                              >
                                <MoreHorizontal aria-hidden="true" />
                              </MenuTrigger>
                              <MenuPopup align="end">
                                {kind === "personal" && (
                                  <MenuItem
                                    onClick={() => {
                                      setDialogError(null);
                                      setAction({ key, rotate: true });
                                    }}
                                  >
                                    Rotate token
                                  </MenuItem>
                                )}
                                <MenuItem
                                  variant="destructive"
                                  onClick={() => {
                                    setDialogError(null);
                                    setAction({ key, rotate: false });
                                  }}
                                >
                                  Revoke
                                </MenuItem>
                              </MenuPopup>
                            </Menu>
                          </div>
                        )}
                      </TableCell>
                    </TableRow>
                    {expanded && (
                      <TableRow>
                        <TableCell colSpan={6} className="whitespace-normal">
                          <div id={reachId} className="px-6 pb-2">
                            <KeyScope apiKey={key} context={context} />
                          </div>
                        </TableCell>
                      </TableRow>
                    )}
                  </React.Fragment>
                );
              })}
            </TableBody>
          </Table>
        </div>
      </TabsPanel>
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
        <Tabs
          value={keyTab}
          onValueChange={(value) => setKeyTab(String(value))}
        >
          <div className="flex flex-wrap items-center justify-between gap-x-4 gap-y-2">
            <TabsList aria-label="Key type">
              <TabsTab value="personal">
                Personal {keys.filter((key) => !inactive(key)).length}
              </TabsTab>
              {canManage && (
                <TabsTab value="service">
                  Service {serviceKeys.filter((key) => !inactive(key)).length}
                </TabsTab>
              )}
            </TabsList>
            <Label className="flex items-center gap-2 text-xs text-muted-foreground">
              <Checkbox
                checked={showHistory}
                onCheckedChange={setShowHistory}
              />
              Show revoked and expired
            </Label>
          </div>
          {renderKeys("personal", keys)}
          {canManage && renderKeys("service", serviceKeys)}
        </Tabs>
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
