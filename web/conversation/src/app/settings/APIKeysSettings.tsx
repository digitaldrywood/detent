import React from "react";
import { ChevronDownIcon, ChevronRightIcon } from "lucide-react";

import type {
  OperatorAPIKey,
  OperatorProjectAccess,
  CreatedOperatorAPIKey,
} from "../../contracts/account.ts";
import { Input } from "../../components/ui/input.tsx";
import { Label } from "../../components/ui/label.tsx";
import { Checkbox } from "../../components/ui/checkbox.tsx";
import { Badge } from "../../components/ui/badge.tsx";
import { Button } from "../../components/ui/button.tsx";
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
import { ControlError, NativeSelect } from "../account/controls.tsx";
import { useAccountApi, useAccountBootstrap } from "../account/context.ts";
import {
  SettingsRow,
  SettingsSection,
  SettingsWarning,
  useRelativeTimeTick,
} from "./settingsLayout.tsx";

export type APIKeyAccess = Pick<
  CreatedOperatorAPIKey,
  "project_access" | "project_ids"
>;

const permissions = [
  {
    value: "read",
    label: "Read",
    description: "Read permitted projects and work items.",
  },
  {
    value: "write",
    label: "Write",
    description: "Read and make changes within your current permissions.",
  },
  {
    value: "admin",
    label: "Admin",
    description: "Read, write and administer within your current authority.",
  },
] as const;

function permissionLabel(scope: string): string {
  return (
    permissions.find((permission) => permission.value === scope)?.label ?? scope
  );
}

function keyDate(value: string, year = true): string {
  const date = new Date(value);
  return Number.isNaN(date.getTime())
    ? "Unknown date"
    : date.toLocaleDateString(undefined, {
        month: "short",
        day: "numeric",
        ...(year ? { year: "numeric" } : {}),
      });
}

export function APIKeysSettings({
  onAvailability,
  onKeyCreated,
  setupActions,
}: {
  readonly onAvailability: (available: boolean) => void;
  readonly onKeyCreated: (key: APIKeyAccess) => void;
  readonly setupActions: (key: APIKeyAccess) => React.ReactNode;
}): React.ReactElement {
  const account = useAccountBootstrap();
  const api = useAccountApi();
  const now = useRelativeTimeTick(60_000);
  const [projectAccess, setProjectAccess] =
    React.useState<OperatorProjectAccess>("all");
  const [selectedProjects, setSelectedProjects] = React.useState<
    readonly string[]
  >([]);
  const [name, setName] = React.useState("");
  const [scope, setScope] = React.useState("read");
  const [days, setDays] = React.useState("30");
  const [keys, setKeys] = React.useState<readonly OperatorAPIKey[]>([]);
  const [loaded, setLoaded] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);
  const [dialogError, setDialogError] = React.useState<string | null>(null);
  const [busy, setBusy] = React.useState(false);
  const [open, setOpen] = React.useState(false);
  const [created, setCreated] = React.useState<CreatedOperatorAPIKey | null>(
    null,
  );
  const [secretCopied, setSecretCopied] = React.useState(false);
  const [revokeKey, setRevokeKey] = React.useState<OperatorAPIKey | null>(null);
  const [historyOpen, setHistoryOpen] = React.useState(false);
  const currentAccount = React.useRef(account);
  currentAccount.current = account;

  React.useEffect(() => {
    let current = true;
    setLoaded(false);
    setCreated(null);
    setOpen(false);
    setRevokeKey(null);
    setHistoryOpen(false);
    setError(null);
    setKeys([]);
    onAvailability(false);
    if (account === null) return;
    api
      .apiKeys()
      .then((result) => {
        if (!current) return;
        setKeys(result.keys);
        setLoaded(true);
        onAvailability(true);
      })
      .catch(() => {
        if (current)
          setError(
            "Direct API-key setup is unavailable on this deployment or for this session. Sign in as an organization member and check with your Cloud operator.",
          );
      });
    return () => {
      current = false;
    };
  }, [api, account, onAvailability]);

  function openCreate() {
    setName("");
    setScope("read");
    setDays("30");
    setProjectAccess("all");
    setSelectedProjects([]);
    setCreated(null);
    setSecretCopied(false);
    setDialogError(null);
    setOpen(true);
  }

  function closeCreate(nextOpen: boolean) {
    if (busy && created === null) return;
    setOpen(nextOpen);
    if (!nextOpen) {
      setCreated(null);
      setSecretCopied(false);
      setDialogError(null);
    }
  }

  async function create(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setBusy(true);
    setDialogError(null);
    try {
      const result = await api.createAPIKey({
        name: name.trim(),
        scope,
        expires_days: Number(days),
        project_access: projectAccess,
        project_ids: projectAccess === "selected" ? selectedProjects : [],
      });
      if (currentAccount.current !== account) return;
      setCreated(result);
      onKeyCreated({
        project_access: result.project_access,
        project_ids: result.project_ids,
      });
      try {
        const listed = await api.apiKeys();
        if (currentAccount.current === account) setKeys(listed.keys);
      } catch {
        if (currentAccount.current === account)
          setError(
            "The key was created, but the list could not refresh. Reload after saving your key.",
          );
      }
    } catch {
      if (currentAccount.current === account)
        setDialogError(
          "Could not create the key. Check the name, expiry, current role and selected project access, then try again.",
        );
    } finally {
      setBusy(false);
    }
  }

  async function revoke() {
    if (revokeKey === null) return;
    setBusy(true);
    setDialogError(null);
    try {
      await api.revokeAPIKey(revokeKey.id);
      if (currentAccount.current !== account) return;
      setKeys((current) =>
        current.map((key) =>
          key.id === revokeKey.id
            ? { ...key, revoked: true, revoked_at: new Date().toISOString() }
            : key,
        ),
      );
      setRevokeKey(null);
      try {
        const listed = await api.apiKeys();
        if (currentAccount.current === account) setKeys(listed.keys);
      } catch {
        if (currentAccount.current === account)
          setError(
            "The key was revoked, but the list could not refresh. Reload to see the latest history.",
          );
      }
    } catch {
      if (currentAccount.current === account)
        setDialogError(
          "Could not revoke the key. Check your current access and try again.",
        );
    } finally {
      setBusy(false);
    }
  }

  async function copyKey() {
    if (created === null) return;
    try {
      await navigator.clipboard.writeText(created.token);
      setSecretCopied(true);
      setDialogError(null);
    } catch {
      setDialogError(
        "Could not copy the key. Check clipboard permissions before closing this dialog.",
      );
    }
  }

  function projectBadges(key: APIKeyAccess) {
    return (
      <span className="flex flex-wrap gap-1.5">
        {key.project_access === "all" ? (
          <Badge variant="outline">All projects</Badge>
        ) : (
          key.project_ids.map((id) => (
            <Badge
              key={id}
              variant="outline"
              className="max-w-full whitespace-normal break-all"
            >
              {account?.projects.find((project) => project.id === id)?.name ??
                id}
            </Badge>
          ))
        )}
      </span>
    );
  }

  const inactive = (key: OperatorAPIKey) =>
    key.revoked || new Date(key.expires_at).getTime() <= now;
  const activeKeys = keys.filter((key) => !inactive(key));
  const history = keys.filter(inactive);
  const allowedPermissions = permissions.filter(
    (permission) =>
      permission.value === "read" ||
      (permission.value === "write" && account?.actor.role !== "viewer") ||
      (permission.value === "admin" &&
        (account?.actor.role === "owner" || account?.actor.role === "admin")),
  );

  return (
    <>
      <SettingsSection
        id="settings-api-keys"
        title="API keys"
        headerAction={
          loaded && (
            <Button size="xs" disabled={busy} onClick={openCreate}>
              Create key
            </Button>
          )
        }
      >
        {error && (
          <div className="px-3 py-3 sm:px-4">
            <ControlError message={error} />
          </div>
        )}
        {!loaded && !error && (
          <SettingsRow
            title="Checking direct API-key availability…"
            role="status"
          />
        )}
        {loaded && (
          <>
            {activeKeys.length === 0 && (
              <SettingsRow
                title="No active API keys"
                description="Create a key to connect your agent."
              />
            )}
            {activeKeys.map((key) => (
              <SettingsRow
                key={key.id}
                title={
                  <span className="flex flex-wrap items-center gap-2 break-all">
                    {key.name}
                    <Badge variant="info">{permissionLabel(key.scope)}</Badge>
                  </span>
                }
                description={`Expires ${keyDate(key.expires_at)}${key.created_at ? ` · created ${keyDate(key.created_at, false)}` : ""} · ${key.fingerprint.slice(0, 12)}`}
                status={projectBadges(key)}
                control={
                  <Button
                    size="xs"
                    variant="destructive-outline"
                    disabled={busy}
                    aria-label={`Revoke ${key.name}`}
                    onClick={() => {
                      setDialogError(null);
                      setRevokeKey(key);
                    }}
                  >
                    Revoke
                  </Button>
                }
              />
            ))}
            {history.length > 0 && (
              <>
                <div className="px-3 py-2 sm:px-4">
                  <Button
                    size="sm"
                    variant="ghost"
                    aria-expanded={historyOpen}
                    aria-controls="api-key-history"
                    onClick={() => setHistoryOpen(!historyOpen)}
                  >
                    {historyOpen ? (
                      <ChevronDownIcon aria-hidden="true" />
                    ) : (
                      <ChevronRightIcon aria-hidden="true" />
                    )}
                    Key history · {history.length} revoked or expired
                  </Button>
                </div>
                {historyOpen && (
                  <div id="api-key-history">
                    {history.map((key) => (
                      <SettingsRow
                        key={key.id}
                        className="opacity-64"
                        title={
                          <span className="flex flex-wrap items-center gap-2 break-all">
                            {key.name}
                            <Badge variant={key.revoked ? "error" : "outline"}>
                              {key.revoked ? "Revoked" : "Expired"}
                            </Badge>
                          </span>
                        }
                        description={`${key.revoked ? `Revoked${key.revoked_at ? ` ${keyDate(key.revoked_at)}` : ""}` : `Expired ${keyDate(key.expires_at)}`} · ${permissionLabel(key.scope)} · ${key.fingerprint.slice(0, 12)}`}
                        status={projectBadges(key)}
                      />
                    ))}
                  </div>
                )}
              </>
            )}
          </>
        )}
      </SettingsSection>
      <p className="px-3 text-xs leading-relaxed text-muted-foreground sm:px-4">
        A key narrows your access, never expands it. Removing membership or
        project access applies to every key immediately.
      </p>
      <Dialog open={open} onOpenChange={closeCreate}>
        <DialogPopup>
          <DialogHeader>
            <DialogTitle>
              {created ? "Key created" : "Create API key"}
            </DialogTitle>
            <DialogDescription>
              {created
                ? `${name.trim()} · ${permissionLabel(scope)} · expires in ${days} days`
                : "Use one shared key for Direct API and MCP."}
            </DialogDescription>
          </DialogHeader>
          {created ? (
            <>
              <DialogPanel className="space-y-5">
                {projectBadges(created)}
                <div className="space-y-3">
                  <h3 className="text-sm font-medium">
                    1. Save your key privately
                  </h3>
                  <SettingsWarning>
                    This key will not be shown again. Save it as{" "}
                    <code>DETENT_API_KEY</code> in your private environment or
                    secret store before closing. Never paste it into an agent
                    conversation.
                  </SettingsWarning>
                  <Label htmlFor="new-api-key">New API key</Label>
                  <div className="flex flex-col gap-2 sm:flex-row sm:items-center">
                    <Input
                      id="new-api-key"
                      type="password"
                      value={created.token}
                      readOnly
                      autoComplete="off"
                      className="min-w-0 flex-1 font-mono"
                    />
                    <Button onClick={() => void copyKey()}>
                      {secretCopied ? "Key copied" : "Copy key privately"}
                    </Button>
                  </div>
                  <span className="sr-only" role="status">
                    {secretCopied ? "Key copied privately." : ""}
                  </span>
                </div>
                <div className="space-y-3">
                  <h3 className="text-sm font-medium">
                    2. Hand a setup prompt to your agent
                  </h3>
                  <p className="text-xs text-muted-foreground">
                    Both prompts reference DETENT_API_KEY and contain no secret.
                  </p>
                  {setupActions(created)}
                </div>
                <ControlError message={dialogError} />
              </DialogPanel>
              <DialogFooter>
                <DialogClose render={<Button>Done</Button>} />
              </DialogFooter>
            </>
          ) : (
            <form className="contents" onSubmit={(event) => void create(event)}>
              <DialogPanel className="space-y-5">
                <div className="space-y-2">
                  <Label htmlFor="api-key-name">Name</Label>
                  <Input
                    id="api-key-name"
                    name="name"
                    value={name}
                    onChange={(event) => setName(event.currentTarget.value)}
                    required
                    disabled={busy}
                    maxLength={200}
                    aria-describedby="api-key-name-hint"
                  />
                  <p
                    id="api-key-name-hint"
                    className="text-xs text-muted-foreground"
                  >
                    Name it after the agent or machine that will use it.
                  </p>
                </div>
                <fieldset className="space-y-2">
                  <legend className="mb-2 text-sm font-medium">
                    Permissions
                  </legend>
                  <SettingsSection title="Permission levels" hideTitle>
                    {allowedPermissions.map((permission) => (
                      <Label
                        key={permission.value}
                        className={`flex items-center gap-3 px-3 py-2.5 first:rounded-t-xl last:rounded-b-xl has-focus-visible:outline-2 has-focus-visible:outline-ring ${scope === permission.value ? "bg-primary/8" : ""}`}
                      >
                        <Input
                          nativeInput
                          unstyled
                          type="radio"
                          disabled={busy}
                          name="scope"
                          value={permission.value}
                          checked={scope === permission.value}
                          onChange={() => setScope(permission.value)}
                          className="w-5 shrink-0"
                          aria-label={permission.label}
                          aria-describedby={`api-key-${permission.value}-hint`}
                        />
                        <span>
                          <span className="block">{permission.label}</span>
                          <span
                            id={`api-key-${permission.value}-hint`}
                            className="block text-xs font-normal text-muted-foreground"
                          >
                            {permission.description}
                          </span>
                        </span>
                      </Label>
                    ))}
                  </SettingsSection>
                </fieldset>
                <div className="space-y-2">
                  <Label htmlFor="api-key-expiry">Expires</Label>
                  <NativeSelect
                    id="api-key-expiry"
                    disabled={busy}
                    name="expires_days"
                    value={days}
                    onValueChange={setDays}
                    options={[
                      { value: "7", label: "7 days" },
                      { value: "30", label: "30 days" },
                      { value: "90", label: "90 days" },
                    ]}
                  />
                </div>
                <fieldset className="space-y-2">
                  <legend className="mb-2 text-sm font-medium">
                    Project access
                  </legend>
                  <SettingsSection title="Project access options" hideTitle>
                    <Label
                      className={`flex items-center gap-3 rounded-t-xl px-3 py-2.5 has-focus-visible:outline-2 has-focus-visible:outline-ring ${projectAccess === "all" ? "bg-primary/8" : ""}`}
                    >
                      <Input
                        nativeInput
                        unstyled
                        type="radio"
                        disabled={busy}
                        name="project_access"
                        value="all"
                        checked={projectAccess === "all"}
                        onChange={() => setProjectAccess("all")}
                        className="w-5 shrink-0"
                        aria-label="All projects"
                        aria-describedby="api-key-all-hint"
                      />
                      <span>
                        <span className="block">All projects</span>
                        <span
                          id="api-key-all-hint"
                          className="block text-xs font-normal text-muted-foreground"
                        >
                          Includes current and future projects you have
                          permission to access.
                        </span>
                      </span>
                    </Label>
                    <Label
                      className={`flex items-center gap-3 rounded-b-xl px-3 py-2.5 has-focus-visible:outline-2 has-focus-visible:outline-ring ${projectAccess === "selected" ? "bg-primary/8" : ""}`}
                    >
                      <Input
                        nativeInput
                        unstyled
                        type="radio"
                        disabled={busy}
                        name="project_access"
                        value="selected"
                        checked={projectAccess === "selected"}
                        onChange={() => setProjectAccess("selected")}
                        className="w-5 shrink-0"
                        aria-label="Selected projects"
                        aria-describedby="api-key-selected-hint"
                      />
                      <span>
                        <span className="block">Selected projects</span>
                        <span
                          id="api-key-selected-hint"
                          className="block text-xs font-normal text-muted-foreground"
                        >
                          Restrict the key to specific projects. New projects
                          are not added.
                        </span>
                      </span>
                    </Label>
                  </SettingsSection>
                  {projectAccess === "selected" && (
                    <div className="space-y-2 ps-8">
                      {account?.projects.map((project) => (
                        <Label
                          key={project.id}
                          className="flex min-h-8 items-center gap-2"
                        >
                          <Checkbox
                            disabled={busy}
                            checked={selectedProjects.includes(project.id)}
                            onCheckedChange={(checked) =>
                              setSelectedProjects((current) =>
                                checked
                                  ? [...current, project.id]
                                  : current.filter((id) => id !== project.id),
                              )
                            }
                          />
                          <span className="min-w-0 break-words">
                            {project.name}
                          </span>
                        </Label>
                      ))}
                      {account?.projects.length === 0 && (
                        <p className="text-xs text-muted-foreground">
                          Ask an administrator for project access to create a
                          selected-project key.
                        </p>
                      )}
                    </div>
                  )}
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
                  disabled={
                    busy ||
                    !name.trim() ||
                    (projectAccess === "selected" &&
                      selectedProjects.length === 0)
                  }
                >
                  {busy ? "Creating…" : "Create key"}
                </Button>
              </DialogFooter>
            </form>
          )}
        </DialogPopup>
      </Dialog>
      <Dialog
        open={revokeKey !== null}
        onOpenChange={(nextOpen) => {
          if (!nextOpen && !busy) {
            setRevokeKey(null);
            setDialogError(null);
          }
        }}
      >
        <DialogPopup>
          <DialogHeader>
            <DialogTitle>Revoke {revokeKey?.name ?? "this key"}?</DialogTitle>
            <DialogDescription>
              API and MCP access using this key stop immediately. This cannot be
              undone or restored.
            </DialogDescription>
          </DialogHeader>
          <DialogPanel>
            <ControlError message={dialogError} />
          </DialogPanel>
          <DialogFooter>
            <DialogClose
              render={
                <Button variant="outline" disabled={busy}>
                  Keep key
                </Button>
              }
            />
            <Button
              variant="destructive"
              disabled={busy}
              onClick={() => void revoke()}
            >
              {busy ? "Revoking…" : "Revoke key"}
            </Button>
          </DialogFooter>
        </DialogPopup>
      </Dialog>
    </>
  );
}
