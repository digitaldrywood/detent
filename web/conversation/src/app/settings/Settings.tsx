import {
  BlocksIcon,
  CreditCardIcon,
  InfoIcon,
  KeyboardIcon,
  PanelsTopLeftIcon,
  ReceiptTextIcon,
  Settings2Icon,
} from "lucide-react";
import React from "react";

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
import { Input } from "../../components/ui/input.tsx";
import { Kbd } from "../../components/ui/kbd.tsx";
import { Label } from "../../components/ui/label.tsx";
import {
  Select,
  SelectItem,
  SelectPopup,
  SelectTrigger,
  SelectValue,
} from "../../components/ui/select.tsx";
import { Switch } from "../../components/ui/switch.tsx";
import {
  WorkspaceBreadcrumb,
  WorkspaceBreadcrumbItem,
  WorkspaceBreadcrumbSeparator,
} from "../../components/WorkspaceBreadcrumb.tsx";
import { WorkspacePageHeader } from "../../components/WorkspacePageHeader.tsx";
import type { BillingReport, PlanReport, ProjectsResponse } from "../../contracts/account.ts";
import { ControlError } from "../account/controls.tsx";
import { useAccountApi, useAccountBootstrap } from "../account/context.ts";
import { newKey } from "../account/idempotency.ts";
import { OrganizationRoute } from "../account/Organization.tsx";
import { ProjectSettingsRoute } from "../account/ProjectSettings.tsx";
import { useMutation, useResource } from "../account/useResource.ts";
import { RunnersSettings } from "../fleet/RunnersSection.tsx";
import { NEW_CHAT_KEYSHORTCUTS, SEARCH_KEYSHORTCUTS } from "../lib/shortcuts.ts";
import { keybindingCatalogue } from "../adapters/keybindings.ts";
import {
  DEFAULT_SECTION,
  isSettingsSectionId,
  SETTINGS_SECTION_LABELS,
  settingsNavItems,
  type SettingsSectionId,
} from "./sections.tsx";
import { SettingsPageContainer, SettingsRow, SettingsSection } from "./settingsLayout.tsx";
import { signInPath } from "../../runtime/basePath.ts";

/** The allowance label the plan report's keys become. */
export function allowanceLabel(name: string): string {
  return name.replace(/^api_/, "API ").replaceAll("_", " ");
}

/** An allowance row's three facts: consumed, allowed, and whether it is over. */
export interface AllowanceRow {
  readonly name: string;
  readonly label: string;
  readonly used: number;
  readonly limit: number;
  readonly overLimit: boolean;
}

export function allowanceRows(plan: PlanReport): readonly AllowanceRow[] {
  const names = new Set([...Object.keys(plan.allowances), ...Object.keys(plan.usage)]);
  return [...names].toSorted().map((name) => {
    const used = plan.usage[name] ?? 0;
    const limit = plan.allowances[name] ?? 0;
    return { name, label: allowanceLabel(name), used, limit, overLimit: used > limit };
  });
}

export function AllowanceList({ rows }: { readonly rows: readonly AllowanceRow[] }): React.ReactElement {
  return (
    <ul className="grid grid-cols-1 gap-x-8 gap-y-2 py-3 sm:grid-cols-2">
      {rows.map((row) => (
        <li key={row.name} className="flex items-baseline justify-between gap-3 text-sm">
          <span className="capitalize text-muted-foreground">{row.label}</span>
          <span
            className={
              row.overLimit
                ? "font-medium tabular-nums text-warning-foreground"
                : "tabular-nums text-foreground"
            }
          >
            {row.used.toLocaleString()} / {row.limit.toLocaleString()}
            {row.overLimit ? <span className="ps-1.5 text-xs">over limit</span> : null}
          </span>
        </li>
      ))}
    </ul>
  );
}

export function NewProjectDialog({
  open,
  onOpenChange,
  onCreate,
  pending,
  error,
}: {
  readonly open: boolean;
  readonly onOpenChange: (open: boolean) => void;
  readonly onCreate: (input: { name: string; grantAccess: boolean }) => void;
  readonly pending: boolean;
  readonly error: string | null;
}): React.ReactElement {
  const [name, setName] = React.useState("");
  const [grantAccess, setGrantAccess] = React.useState(true);
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogPopup>
        <DialogHeader>
          <DialogTitle>New project</DialogTitle>
          <DialogDescription>
            A project owns its own board, policy, runners and repository binding.
          </DialogDescription>
        </DialogHeader>
        <DialogPanel>
          <form
            id="new-project-form"
            className="flex flex-col gap-4"
            onSubmit={(event) => {
              event.preventDefault();
              const value = name.trim();
              if (value.length > 0) onCreate({ name: value, grantAccess });
            }}
          >
            <div className="flex flex-col gap-1.5">
              <Label htmlFor="new-project-name">Name</Label>
              <Input
                id="new-project-name"
                value={name}
                placeholder="parable"
                onChange={(event) => setName(event.currentTarget.value)}
              />
            </div>
            <label className="flex items-center gap-2.5 text-sm">
              <Switch
                aria-label="Grant yourself write access"
                checked={grantAccess}
                onCheckedChange={setGrantAccess}
              />
              Grant yourself write access
            </label>
            <ControlError message={error} />
          </form>
        </DialogPanel>
        <DialogFooter>
          <DialogClose render={<Button variant="outline">Cancel</Button>} />
          <Button
            type="submit"
            form="new-project-form"
            disabled={pending || name.trim().length === 0}
          >
            {pending ? "Creating…" : "Create project"}
          </Button>
        </DialogFooter>
      </DialogPopup>
    </Dialog>
  );
}

// --- Sections ---------------------------------------------------------------

/** General: what this session is, and which organization it is acting on. */
export function GeneralSettings(): React.ReactElement {
  const bootstrap = useAccountBootstrap();
  const api = useAccountApi();
  const signOut = useMutation(async () => {
    await api.logout();
    globalThis.location?.assign(signInPath());
    return null;
  });

  return (
    <SettingsPageContainer>
      <SettingsSection
        id="settings-general"
        title="General"
        icon={<Settings2Icon className="size-3.5" />}
      >
        <SettingsRow
          id="settings-general-organization"
          title="Organization"
          description="The organization every project, runner and conversation on this session belongs to."
          control={
            <span className="text-sm text-foreground">
              {bootstrap?.organization.name ?? "Not signed in"}
            </span>
          }
        />
        <SettingsRow
          id="settings-general-account"
          title="Your account"
          description={bootstrap?.actor.email ?? "This hub does not report an actor."}
          status={bootstrap === null ? undefined : `Role: ${bootstrap.actor.role}`}
          control={
            <Button
              size="sm"
              variant="outline"
              disabled={signOut.pending}
              onClick={() => void signOut.call()}
            >
              {signOut.pending ? "Signing out…" : "Sign out"}
            </Button>
          }
        />
        {bootstrap?.plan == null ? null : (
          <SettingsRow
            id="settings-general-plan"
            title="Plan"
            description={bootstrap.plan.name}
            status={`Usage window ends ${bootstrap.plan.window_ends_at}`}
          />
        )}
      </SettingsSection>
    </SettingsPageContainer>
  );
}

export function ProjectsSettings({
  onNavigate,
}: {
  readonly onNavigate?: (to: string) => void;
}): React.ReactElement {
  const api = useAccountApi();
  const bootstrap = useAccountBootstrap();
  const canManage = bootstrap?.actor.can_manage ?? false;
  const projects = useResource<ProjectsResponse>(() => api.projects(), [api]);
  const [creating, setCreating] = React.useState(false);
  const createProject = useMutation(async (input: { name: string; grantAccess: boolean }) => {
    const created = await api.createProject({ ...input, key: newKey() });
    await projects.refresh();
    setCreating(false);
    return created;
  });

  return (
    <SettingsPageContainer>
      <SettingsSection
        id="settings-projects"
        title="Projects"
        icon={<PanelsTopLeftIcon className="size-3.5" />}
        headerAction={
          canManage ? (
            <Button size="xs" variant="outline" onClick={() => setCreating(true)}>
              New project
            </Button>
          ) : null
        }
      >
        {projects.loading && projects.value === undefined ? (
          <SettingsRow title="Loading the projects" />
        ) : projects.error !== null ? (
          <SettingsRow
            title="Projects are not available"
            description={
              projects.error.isAccessError
                ? "You do not have access to this organization's projects."
                : projects.error.message
            }
            control={
              <Button size="sm" variant="outline" onClick={() => void projects.refresh()}>
                Try again
              </Button>
            }
          />
        ) : (projects.value ?? []).length === 0 ? (
          <SettingsRow
            title="No projects yet"
            description="A project is where issues, runners and a repository binding live."
          />
        ) : (
          (projects.value ?? []).map((project) => (
            <SettingsRow
              key={project.id}
              title={project.name}
              description={`${project.profile} · ${project.states.length} workflow states`}
              status={
                project.onboarding.ready
                  ? "Set up"
                  : `${project.onboarding.steps.filter((step) => step.state !== "ready").length} setup steps left`
              }
              control={
                <div className="flex gap-2">
                  <Button
                    size="sm"
                    variant="outline"
                    onClick={() => onNavigate?.(`/settings/integrations?project=${project.id}`)}
                  >
                    Settings
                  </Button>
                  {project.onboarding.ready ? null : (
                    <Button size="sm" onClick={() => onNavigate?.(`/projects/${project.id}/setup`)}>
                      Finish setup
                    </Button>
                  )}
                </div>
              }
            />
          ))
        )}
      </SettingsSection>

      <NewProjectDialog
        open={creating}
        onOpenChange={setCreating}
        onCreate={(input) => void createProject.call(input)}
        pending={createProject.pending}
        error={createProject.error?.message ?? null}
      />
    </SettingsPageContainer>
  );
}

export function IntegrationsSettings({
  project,
  onNavigate,
}: {
  readonly project: string | null;
  readonly onNavigate?: (to: string) => void;
}): React.ReactElement {
  const bootstrap = useAccountBootstrap();
  const projects = bootstrap?.projects ?? [];
  const selected = project ?? projects[0]?.id ?? "";

  if (projects.length === 0) {
    return (
      <SettingsPageContainer>
        <SettingsSection
          id="settings-integrations"
          title="Integrations"
          icon={<BlocksIcon className="size-3.5" />}
        >
          <SettingsRow
            title="No projects yet"
            description="An integration binds one project to one repository, so there has to be a project first."
          />
        </SettingsSection>
      </SettingsPageContainer>
    );
  }

  const picker = (
    <SettingsSection
      id="settings-integrations"
      title="Integrations"
      icon={<BlocksIcon className="size-3.5" />}
    >
      <SettingsRow
        title="Project"
        description="Integration, intake, projection and policy are configured per project."
        control={
          <Select
            value={selected}
            onValueChange={(value) =>
              onNavigate?.(`/settings/integrations?project=${String(value)}`)
            }
          >
            <SelectTrigger aria-label="Project" size="sm" className="min-w-44">
              <SelectValue>
                {projects.find((candidate) => candidate.id === selected)?.name ?? selected}
              </SelectValue>
            </SelectTrigger>
            <SelectPopup>
              {projects.map((candidate) => (
                <SelectItem key={candidate.id} value={candidate.id}>
                  {candidate.name}
                </SelectItem>
              ))}
            </SelectPopup>
          </Select>
        }
      />
    </SettingsSection>
  );

  // One scroll container, not two: the picker goes inside the project screen's
  // own `SettingsPageContainer` through its `header` slot.
  return <ProjectSettingsRoute projectId={selected} onNavigate={onNavigate} header={picker} />;
}

export function PlanSettings(): React.ReactElement {
  const api = useAccountApi();
  const plan = useResource<PlanReport>(() => api.plan(), [api]);

  return (
    <SettingsPageContainer>
      <SettingsSection
        id="settings-plan"
        title="Plan"
        icon={<ReceiptTextIcon className="size-3.5" />}
      >
        {plan.loading && plan.value === undefined ? (
          <SettingsRow title="Loading the plan" />
        ) : plan.error !== null ? (
          <SettingsRow
            title="Plan is not available"
            description={
              plan.error.isAccessError
                ? "Plan and usage need owner or admin access."
                : plan.error.message
            }
          />
        ) : plan.value === undefined ? null : (
          <>
            <SettingsRow
              title={`${plan.value.effective_base.id} · version ${plan.value.effective_base.version}`}
              description={
                plan.value.source === "subscription" ? "Subscription-derived plan" : "Base plan"
              }
              status={`Usage window ends ${plan.value.window_ends_at}`}
            />
            <SettingsRow title="Allowances">
              <AllowanceList rows={allowanceRows(plan.value)} />
            </SettingsRow>
          </>
        )}
      </SettingsSection>
    </SettingsPageContainer>
  );
}

export function BillingSettings(): React.ReactElement {
  const api = useAccountApi();
  const billing = useResource<BillingReport>(() => api.billing(), [api]);
  const checkout = useMutation(async (price: string) => {
    const result = await api.checkout({ price, key: newKey() });
    globalThis.location?.assign(result.url);
    return result;
  });
  const portal = useMutation(async () => {
    const result = await api.portal({ key: newKey() });
    globalThis.location?.assign(result.url);
    return result;
  });

  return (
    <SettingsPageContainer>
      <SettingsSection
        id="settings-billing"
        title="Billing"
        icon={<CreditCardIcon className="size-3.5" />}
      >
        {billing.loading && billing.value === undefined ? (
          <SettingsRow title="Loading billing" />
        ) : billing.error !== null ? (
          <SettingsRow
            title="Billing is not available"
            description={
              billing.error.isAccessError
                ? "Billing needs owner access, and is closed during a support session."
                : billing.error.message
            }
          />
        ) : billing.value === undefined ? null : (
          <>
            <SettingsRow
              title={`Subscription ${billing.value.state.status}`}
              description={`Plan ${billing.value.state.plan.id} · paid through ${billing.value.state.paid_through}`}
              status={`Reconciled ${billing.value.reconciled_at || "never"}`}
              control={
                <Button
                  size="sm"
                  variant="outline"
                  disabled={portal.pending}
                  onClick={() => void portal.call()}
                >
                  {portal.pending ? "Opening…" : "Billing portal"}
                </Button>
              }
            />
            {billing.value.prices.map((price) => (
              <SettingsRow
                key={price.id}
                title={price.label}
                description={price.id}
                control={
                  <Button
                    size="sm"
                    disabled={checkout.pending || billing.value?.can_checkout === false}
                    onClick={() => void checkout.call(price.id)}
                  >
                    {checkout.pending ? "Opening…" : "Subscribe"}
                  </Button>
                }
              />
            ))}
            <SettingsRow title="">
              <div className="pb-3">
                <ControlError message={checkout.error?.message ?? portal.error?.message ?? null} />
              </div>
            </SettingsRow>
          </>
        )}
      </SettingsSection>
    </SettingsPageContainer>
  );
}

const SHELL_SHORTCUTS: readonly { readonly action: string; readonly keys: readonly string[] }[] = [
  { action: "New conversation", keys: NEW_CHAT_KEYSHORTCUTS.split(" ") },
  { action: "Focus search", keys: SEARCH_KEYSHORTCUTS.split(" ") },
];

export function KeybindingsSettings(): React.ReactElement {
  const rows = React.useMemo(() => keybindingCatalogue(), []);
  return (
    <SettingsPageContainer>
      <SettingsSection
        id="settings-keybindings"
        title="Keybindings"
        icon={<KeyboardIcon className="size-3.5" />}
      >
        {SHELL_SHORTCUTS.map((binding) => (
          <SettingsRow
            key={binding.action}
            title={binding.action}
            control={
              <span className="flex items-center gap-1.5">
                {binding.keys.map((keys) => (
                  <Kbd key={keys}>{keys.replaceAll("+", " ")}</Kbd>
                ))}
              </span>
            }
          />
        ))}
        {rows.map((row) => (
          <SettingsRow
            key={row.command}
            title={row.label}
            data-testid={row.chord === null ? "keybinding-unbound" : "keybinding-bound"}
            data-command={row.command}
            {...(row.reason === null ? {} : { description: row.reason })}
            control={
              row.chord === null ? (
                <span className="text-muted-foreground text-xs" aria-disabled="true">
                  Unbound
                </span>
              ) : (
                <Kbd>{row.chord}</Kbd>
              )
            }
          />
        ))}
        <SettingsRow
          title="Rebinding"
          description="Keybindings are fixed in this release. Custom keybindings are not available yet."
        />
      </SettingsSection>
    </SettingsPageContainer>
  );
}

export function AboutSettings(): React.ReactElement {
  const bootstrap = useAccountBootstrap();
  return (
    <SettingsPageContainer>
      <SettingsSection id="settings-about" title="About" icon={<InfoIcon className="size-3.5" />}>
        <SettingsRow
          title="Detent"
          description={bootstrap?.version ?? "This hub does not report a version."}
          status={bootstrap?.api_base}
        />
        <SettingsRow
          title="Interface"
          description="Portions derived from T3 Code. Copyright (c) 2026 T3 Tools Inc. MIT License."
        />
      </SettingsSection>
    </SettingsPageContainer>
  );
}

// --- The page ---------------------------------------------------------------

export function SettingsRoute({
  section = DEFAULT_SECTION,
  project = null,
  onNavigate,
}: {
  /** The path segment after `/settings`. */
  readonly section?: SettingsSectionId;
  /** `?project=`, for the Integrations section. */
  readonly project?: string | null;
  /** The router's navigation. Absent in a test that renders the page alone. */
  readonly onNavigate?: (to: string) => void;
}): React.ReactElement {
  const bootstrap = useAccountBootstrap();
  const canManage = bootstrap?.actor.can_manage ?? false;
  const supporting = bootstrap?.support != null;

  const items = React.useMemo(
    () => settingsNavItems({ canManage, supporting }),
    [canManage, supporting],
  );
  // A section the actor cannot read is not on the list, so asking for it by
  // path lands on the default rather than on an empty column.
  const activeId = items.some((item) => item.id === section && item.disabled !== true)
    ? section
    : DEFAULT_SECTION;

  return (
    <div className="flex min-h-0 min-w-0 flex-1 flex-col bg-background text-foreground">
      <WorkspacePageHeader className="h-auto">
        <WorkspaceBreadcrumb ariaLabel="Settings breadcrumb" className="min-w-0 py-2">
          <WorkspaceBreadcrumbItem>
            <h1>Settings</h1>
          </WorkspaceBreadcrumbItem>
          <WorkspaceBreadcrumbSeparator />
          <WorkspaceBreadcrumbItem current className="min-w-10">
            <span className="min-w-0 truncate">{SETTINGS_SECTION_LABELS[activeId]}</span>
          </WorkspaceBreadcrumbItem>
        </WorkspaceBreadcrumb>
      </WorkspacePageHeader>

      <div className="flex min-h-0 flex-1 flex-col">
        <SettingsBody section={activeId} project={project} onNavigate={onNavigate} />
      </div>
    </div>
  );
}

function SettingsBody({
  section,
  project,
  onNavigate,
}: {
  readonly section: SettingsSectionId;
  readonly project: string | null;
  readonly onNavigate?: (to: string) => void;
}): React.ReactElement {
  switch (section) {
    case "organization":
      return <OrganizationRoute />;
    case "projects":
      return <ProjectsSettings onNavigate={onNavigate} />;
    case "runners":
      return <RunnersSettings />;
    case "integrations":
      return <IntegrationsSettings project={project} onNavigate={onNavigate} />;
    case "plan":
      return <PlanSettings />;
    case "billing":
      return <BillingSettings />;
    case "keybindings":
      return <KeybindingsSettings />;
    case "about":
      return <AboutSettings />;
    case "general":
    default:
      return <GeneralSettings />;
  }
}

export { isSettingsSectionId, SETTINGS_SECTION_LABELS, settingsNavItems };
export type { SettingsSectionId };
