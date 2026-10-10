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
import { useAccountApi, useAccountBootstrap } from "../account/context.ts";
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
          {apiKey.access_context === "global" ? "All my orgs" : "Selected orgs"}
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
  return `Use my personal Detent key from Account settings → API keys. A key never grants more than its owner has. Store it privately as DETENT_API_KEY; never paste it into chat, logs or source files. ${direct ? `Use Authorization: Bearer with that private environment variable at ${origin}/api/v2. Read GET /api/v2/organizations, then GET /api/v2/organizations/ORG_ID/projects to choose a granted organization and project before using documented routes.` : `Configure one Streamable HTTP MCP server at ${endpoint}, with bearer_token_env_var = "DETENT_API_KEY". Initialize MCP, retain Mcp-Session-Id and protocol version, send notifications/initialized and discover every tools/list page. Use organization_list and list_projects to choose granted organization_id and project_id selectors for calls.`} Check the returned organization and project before reporting success. Current membership, project grants, org policy, permission, expiry and revocation apply on every call. Your client's permission settings control sensitive-operation confirmation. This prompt contains no secret.`;
}

export function PersonalAPIKeysSettings() {
  return (
    <SettingsPageContainer>
      <SettingsSection title="Account settings" variant="plain">
        <SettingsRow
          title="Your personal API keys"
          description="One key can reach the organizations and projects you choose. A key never grants more than its owner has."
        />
      </SettingsSection>
      <AccessKeyManager />
    </SettingsPageContainer>
  );
}

export function AccessKeyManager({
  service = false,
}: {
  readonly service?: boolean;
}) {
  const api = useAccountApi();
  const account = useAccountBootstrap();
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
  const [allOrgs, setAllOrgs] = React.useState(true);
  const [selections, setSelections] = React.useState<
    readonly KeyOrganization[]
  >([]);
  const [history, setHistory] = React.useState(false);
  const currentApi = React.useRef(api);
  currentApi.current = api;
  const list = React.useCallback(
    () => (service ? api.serviceKeys() : api.personalKeys()),
    [api, service],
  );
  React.useEffect(() => {
    let current = true;
    setLoaded(false);
    setKeys([]);
    setContext(null);
    setError(null);
    setOpen(false);
    setCreated(null);
    setAction(null);
    Promise.all([list(), api.keyContext()])
      .then(([listed, choices]) => {
        if (!current) return;
        setKeys(listed.keys);
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
      (org) => !service || org.organization_id === account?.organization.id,
    ) ?? [];
  const inactive = (key: AccessKey) =>
    Boolean(key.revoked_at) ||
    Boolean(key.expires_at && new Date(key.expires_at).getTime() <= now);
  const visible = keys.filter((key) =>
    history ? inactive(key) : !inactive(key),
  );
  function openCreate() {
    setName("");
    setPermission("read");
    setDays("30");
    setAllOrgs(!service);
    setSelections(
      service && account
        ? [
            {
              organization_id: account.organization.id,
              project_access: "all",
              project_ids: [],
            },
          ]
        : [],
    );
    setCreated(null);
    setCopied(false);
    setDialogError(null);
    setOpen(true);
  }
  async function refresh() {
    try {
      const listed = await list();
      if (currentApi.current === api) setKeys(listed.keys);
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
      access_context: allOrgs
        ? "global"
        : selections.length === 1 && selections[0]?.project_access === "project"
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
      else if (service) await api.revokeServiceKey(action.key.id);
      else await api.revokePersonalKey(action.key.id);
      if (currentApi.current !== api) return;
      setAction(null);
      if (result) {
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
  function selectOrg(id: string, checked: boolean) {
    setSelections((current) =>
      checked
        ? [
            ...current,
            { organization_id: id, project_access: "all", project_ids: [] },
          ]
        : current.filter((org) => org.organization_id !== id),
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
    (!allOrgs && selections.length === 0) ||
    selections.some(
      (org) =>
        org.project_access !== "all" &&
        (org.project_ids.length === 0 ||
          (org.project_access === "project" && org.project_ids.length !== 1)),
    );
  return (
    <>
      <SettingsSection
        title={service ? "Org service keys" : "API keys"}
        headerAction={
          loaded && (
            <Button size="xs" onClick={openCreate} disabled={busy}>
              Create {service ? "service " : ""}key
            </Button>
          )
        }
      >
        {error && (
          <div className="p-3">
            <ControlError message={error} />
          </div>
        )}
        {!loaded && !error && (
          <SettingsRow title="Loading API keys…" role="status" />
        )}
        {loaded && visible.length === 0 && (
          <SettingsRow
            title={
              history
                ? "No inactive keys"
                : service
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
        {visible.map((key) => (
          <SettingsRow
            key={key.id}
            title={
              <span className="flex flex-wrap items-center gap-2 break-words">
                {key.name}
                <Badge variant="outline">{key.permission}</Badge>
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
                  {!service && (
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
                onClick={() => setHistory(!history)}
              >
                {history ? "Show active keys" : "Show revoked and expired keys"}
              </Button>
            }
          />
        )}
      </SettingsSection>
      {!service && context && (
        <SettingsSection title="Connect across organizations">
          <SettingsRow
            title="One connection for your personal keys"
            description="A key never grants more than its owner has."
          />
          <ConnectionValue label="MCP endpoint" value={context.mcp_endpoint} />
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
        </SettingsSection>
      )}
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
                {!service && context && (
                  <div className="flex flex-wrap gap-2">
                    <SetupPromptControl
                      label="MCP setup prompt"
                      copyLabel="Copy MCP prompt"
                      value={keySetupPrompt(context.mcp_endpoint)}
                    />
                    <SetupPromptControl
                      label="Direct API setup prompt"
                      copyLabel="Copy Direct API prompt"
                      value={keySetupPrompt(context.mcp_endpoint, true)}
                    />
                  </div>
                )}
                <ControlError message={dialogError} />
              </DialogPanel>
              <DialogFooter>
                <DialogClose render={<Button>Done</Button>} />
              </DialogFooter>
            </>
          ) : (
            <form className="contents" onSubmit={(event) => void create(event)}>
              <DialogPanel className="space-y-4">
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
                {!service && (
                  <div className="space-y-2">
                    <Label htmlFor="personal-key-orgs">
                      Organization scope
                    </Label>
                    <NativeSelect
                      id="personal-key-orgs"
                      value={allOrgs ? "all" : "selected"}
                      disabled={busy}
                      onValueChange={(value) => setAllOrgs(value === "all")}
                      options={[
                        {
                          value: "all",
                          label: "All my orgs, including future orgs",
                        },
                        { value: "selected", label: "Selected orgs" },
                      ]}
                    />
                  </div>
                )}
                <fieldset className="space-y-4">
                  <legend className="mb-2 text-sm font-medium">
                    Project scope per org
                  </legend>
                  {orgs.map((org) => {
                    const selection = selections.find(
                      (item) => item.organization_id === org.organization_id,
                    );
                    const selected = service || allOrgs || Boolean(selection);
                    return (
                      <div key={org.organization_id} className="space-y-2">
                        <Label className="flex min-h-8 items-center gap-2">
                          {!service && !allOrgs && (
                            <Checkbox
                              checked={Boolean(selection)}
                              disabled={busy}
                              onCheckedChange={(checked) =>
                                selectOrg(org.organization_id, Boolean(checked))
                              }
                            />
                          )}
                          <span className="break-words">{org.name}</span>
                        </Label>
                        {selected && (
                          <>
                            <Label
                              htmlFor={`scope-${org.organization_id}`}
                              className="sr-only"
                            >
                              {org.name} project scope
                            </Label>
                            <NativeSelect
                              id={`scope-${org.organization_id}`}
                              disabled={busy}
                              value={selection?.project_access ?? "all"}
                              onValueChange={(value) =>
                                changeOrg(
                                  org.organization_id,
                                  value as KeyOrganization["project_access"],
                                )
                              }
                              options={[
                                {
                                  value: "all",
                                  label:
                                    "All permitted projects, including future projects",
                                },
                                {
                                  value: "selected",
                                  label: "Selected projects",
                                },
                                { value: "project", label: "One project" },
                              ]}
                            />
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
                                    changeOrg(org.organization_id, "project", [
                                      value,
                                    ])
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
                  {busy ? "Creating…" : "Create key"}
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
