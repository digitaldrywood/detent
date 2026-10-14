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
  SettingsWarning,
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

export function keySetupPrompt(endpoint: string, direct = false): string {
  const origin = endpoint.replace(/\/mcp$/, "");
  return `Use a Detent key from Settings → API keys & MCP. A key never grants more than its owner has. Store it privately as DETENT_API_KEY; never paste it into chat, logs or source files. ${direct ? `Use Authorization: Bearer with that private environment variable at ${origin}/api/v2. Read GET /api/v2/organizations, then GET /api/v2/organizations/ORG_ID/projects to choose a granted organization and project before using documented routes.` : `Configure one Streamable HTTP MCP server at ${endpoint}, with bearer_token_env_var = "DETENT_API_KEY". Initialize MCP, retain Mcp-Session-Id and protocol version, send notifications/initialized and discover every tools/list page. Use organization_list and list_projects to choose granted organization_id and project_id selectors for calls.`} Check the returned organization and project before reporting success. Current membership, project grants, org policy, permission, expiry and revocation apply on every call. Your client's permission settings control sensitive-operation confirmation. This prompt contains no secret.`;
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
  const [copied, setCopied] = React.useState(false);
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

  function openCreate() {
    setName("");
    setPermission("read");
    setDays("30");
    setService(false);
    setSelections([]);
    setReach("global");
    setCreated(null);
    setCopied(false);
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
        setCopied(false);
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
          <SettingsRow
            title="API and MCP access"
            description={`Connect Claude, Codex, scripts and CI to ${account?.organization.name ?? "your organization"}. A key never grants more than its owner has.`}
          />
        </SettingsSection>
        <SettingsSection title="Which key do I need?" variant="plain">
          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
            <SettingsSection title="Personal key">
              <SettingsRow
                title="Acts as you"
                description="For your own agents (Claude, Codex, CLI). Edits are recorded as you, so saving an issue confirms its contract. Stops working if you leave the org."
              />
            </SettingsSection>
            <SettingsSection title="Service key">
              <SettingsRow
                title="Acts as the organization"
                description="For shared automation (CI, bots, a team server). Edits are recorded as an integration and never count as a person. Owners/admins only. Keeps working when its creator leaves."
              />
            </SettingsSection>
          </div>
        </SettingsSection>
        {error && <ControlError message={error} />}
        {renderKeys("personal", keys)}
        {canManage && renderKeys("service", serviceKeys)}
        <SettingsSection title="Connect">
          <SettingsRow
            title="One connection for every key"
            description="The same endpoint for every key. The key decides who you are and what you reach."
          />
          {context && (
            <>
              <ConnectionValue
                label="MCP endpoint"
                value={context.mcp_endpoint}
              />
              <SettingsRow
                title="MCP setup prompt"
                control={
                  <SetupPromptControl
                    label="MCP setup prompt"
                    value={keySetupPrompt(context.mcp_endpoint)}
                    preview
                  />
                }
              />
              <SettingsRow
                title="Direct API setup prompt"
                control={
                  <SetupPromptControl
                    label="Direct API setup prompt"
                    value={keySetupPrompt(context.mcp_endpoint, true)}
                    preview
                  />
                }
              />
            </>
          )}
        </SettingsSection>
        {canManage && <OrganizationKeyControls />}
      </SettingsPageContainer>
      <Dialog
        open={open}
        onOpenChange={(value) => {
          if (busy) return;
          setOpen(value);
          if (!value) {
            setCreated(null);
            setDialogError(null);
            setCopied(false);
          }
        }}
      >
        <DialogPopup>
          <DialogHeader>
            <DialogTitle>
              {created
                ? "Save your key"
                : service
                  ? "Create service key"
                  : "Create personal key"}
            </DialogTitle>
            <DialogDescription>
              {created
                ? "Save this secret now. It will not be shown again."
                : "A key never grants more than its owner has."}
            </DialogDescription>
          </DialogHeader>
          {created ? (
            <>
              <DialogPanel className="space-y-4">
                <SettingsWarning>
                  Save as DETENT_API_KEY in your private environment or secret
                  store. Never paste the key into an agent conversation.
                </SettingsWarning>
                <Label htmlFor="new-api-key">New API key</Label>
                <Input
                  id="new-api-key"
                  type="password"
                  value={created.token}
                  readOnly
                  autoComplete="off"
                />
                <Button
                  onClick={() => {
                    void navigator.clipboard
                      .writeText(created.token)
                      .then(() => setCopied(true))
                      .catch(() =>
                        setDialogError(
                          "Could not copy. Select and save the key privately before closing.",
                        ),
                      );
                  }}
                >
                  {copied ? "Key copied" : "Copy key privately"}
                </Button>
                <ControlError message={dialogError} />
              </DialogPanel>
              <DialogFooter>
                <DialogClose render={<Button>Done</Button>} />
              </DialogFooter>
            </>
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
