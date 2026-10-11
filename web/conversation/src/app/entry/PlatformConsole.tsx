import React from "react";
import { Link, Outlet, useRouterState } from "@tanstack/react-router";
import { Activity, ArrowUpRight, Building2, ListChecks, ScrollText, Search, Users, UserSearch } from "lucide-react";

import { WorkspacePageHeader } from "../../components/WorkspacePageHeader.tsx";
import { WorkspacePageContainer } from "../../components/WorkspacePageContainer.tsx";
import { DetentWordmark } from "../../components/DetentWordmark.tsx";
import { Empty, EmptyHeader, EmptyTitle, EmptyDescription } from "../../components/ui/empty.tsx";
import { Skeleton } from "../../components/ui/skeleton.tsx";
import { SettingsSection } from "../settings/settingsLayout.tsx";
import { PlatformSearch } from "./PlatformSearch.tsx";
import { PlatformActionsContext } from "./PlatformPageActions.tsx";
import {
  Sidebar,
  SidebarContent,
  SidebarFooter,
  SidebarGroup,
  SidebarGroupLabel,
  SidebarHeader,
  SidebarMenu,
  SidebarMenuButton,
  SidebarMenuItem,
  SidebarProvider,
  SidebarSeparator,
  SidebarTrigger,
  useSidebar,
} from "../../components/ui/sidebar.tsx";

import { Badge } from "../../components/ui/badge.tsx";
import { Button } from "../../components/ui/button.tsx";
import { usePlatformResource } from "./usePlatformResource.ts";
import { usePageTitle } from "../pageTitle.ts";
import {
  type EntryOrganizations,
  type PlatformAllowlist,
  type PlatformHealth,
} from "./api.ts";
import { PlatformTenants, PlatformTenant } from "./PlatformTenants.tsx";
import { PlatformStaff } from "./PlatformStaff.tsx";
import { Problem, SignOut, useEntryApi } from "./EntryScreens.tsx";

function Panel({
  title,
  description,
  children,
}: {
  readonly title: string;
  readonly description?: string;
  readonly children: React.ReactNode;
}): React.ReactElement {
  return (
    <SettingsSection title={title} aria-label={title}>
      <div className="p-4">
        {description === undefined ? null : <p className="mb-4 text-sm text-muted-foreground">{description}</p>}
        {children}
      </div>
    </SettingsSection>
  );
}

export function formatBytes(value: number): string {
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let amount = value;
  let unit = 0;
  while (amount >= 1024 && unit < units.length - 1) {
    amount /= 1024;
    unit += 1;
  }
  return `${unit === 0 ? amount : amount.toFixed(1)} ${units[unit]}`;
}

export function PlatformAllowlistPanel({ value }: { readonly value: PlatformAllowlist }): React.ReactElement {
  const source = value.source.file === "" ? "the shared entry configuration" : value.source.file;
  return (
    <Panel
      title="Signup allowlist"
      description={`Read-only. Configured in ${source} under ${value.source.keys.join(" and ")}.`}
    >
      {!value.self_service ? (
        <p className="text-sm text-muted-foreground">Self-service organization creation is not configured.</p>
      ) : value.open === true ? (
        <p className="text-sm">Open: any verified account may create an organization.</p>
      ) : (
        <dl className="grid gap-3 text-sm sm:grid-cols-2">
          <div>
            <dt className="font-medium">Allowed emails</dt>
            <dd className="text-muted-foreground">
              {value.allowed_emails.length === 0 ? "None" : value.allowed_emails.join(", ")}
            </dd>
          </div>
          <div>
            <dt className="font-medium">Allowed domains</dt>
            <dd className="text-muted-foreground">
              {value.allowed_domains.length === 0 ? "None" : value.allowed_domains.join(", ")}
            </dd>
          </div>
        </dl>
      )}
    </Panel>
  );
}

function Reading({ label, value }: { readonly label: string; readonly value: string }): React.ReactElement {
  return (
    <div>
      <dt className="text-muted-foreground">{label}</dt>
      <dd className="font-medium">{value}</dd>
    </div>
  );
}

export function PlatformHealthPanel({ value }: { readonly value: PlatformHealth }): React.ReactElement {
  const admission = value.admission;
  return (
    <Panel title="Service health">
      <dl className="grid gap-3 text-sm sm:grid-cols-2">
        <Reading label="Registry" value={value.registry.ok ? "OK" : "Unavailable"} />
        {value.tenants === undefined ? null : (
          <Reading label="Tenant Hubs running" value={`${value.tenants.running} of ${value.tenants.expected}`} />
        )}
        {admission === undefined ? null : (
          <>
            <Reading label="Tenant slots" value={`${admission.tenants} of ${admission.max_tenants}`} />
            <Reading label="Provisioning now" value={`${admission.allocating} of ${admission.max_concurrent}`} />
            <Reading
              label="Free disk"
              value={
                admission.disk_measured
                  ? `${formatBytes(admission.free_disk_bytes)} (floor ${formatBytes(admission.min_free_disk_bytes)})`
                  : "Not measurable"
              }
            />
            <Reading
              label="Available memory"
              value={
                admission.memory_measured
                  ? `${formatBytes(admission.available_memory_bytes)} (floor ${formatBytes(admission.min_available_memory_bytes)})`
                  : "Not measurable"
              }
            />
          </>
        )}
      </dl>
      {admission === undefined ? (
        <p className="mt-3 text-[13px] text-muted-foreground">Self-service allocation is not configured, so there are no admission limits.</p>
      ) : null}
    </Panel>
  );
}

const PLATFORM_SECTIONS = [
  { id: "tenants", title: "Tenants", icon: Building2 },
  { id: "users", title: "Users", icon: UserSearch },
  { id: "staff", title: "Staff", icon: Users },
  { id: "audit", title: "Audit log", icon: ScrollText },
  { id: "health", title: "Health", icon: Activity },
  { id: "allowlist", title: "Sign-up allowlist", icon: ListChecks },
] as const;

type PlatformSection = typeof PLATFORM_SECTIONS[number]["id"];

const PLATFORM_ROLES: Record<string, string> = {
  admin: "Admin",
  support: "Support",
  billing: "Billing",
  viewer: "Viewer",
};

const PlatformRoleContext = React.createContext("");
const PlatformCSRFContext = React.createContext("");

function HealthBadge({ value }: { readonly value: PlatformHealth }): React.ReactElement {
  const admission = value.admission;
  const down = !value.registry.ok;
  const degraded = (value.tenants !== undefined && value.tenants.running < value.tenants.expected) ||
    (admission !== undefined && ((admission.disk_measured && admission.free_disk_bytes < admission.min_free_disk_bytes) ||
      (admission.memory_measured && admission.available_memory_bytes < admission.min_available_memory_bytes)));
  return <Badge variant={down ? "error" : degraded ? "warning" : "success"}>{down ? "Down" : degraded ? "Degraded" : "OK"}</Badge>;
}

function PlatformNavigation({ role, email, csrf, organizations, count, health, onSearch }: {
  readonly role: string;
  readonly email: string;
  readonly csrf: string;
  readonly organizations: EntryOrganizations["organizations"];
  readonly count?: number;
  readonly health?: PlatformHealth | null;
  readonly onSearch: () => void;
}): React.ReactElement {
  const pathname = useRouterState({ select: (state) => state.location.pathname });
  const { isMobile, setOpenMobile } = useSidebar();
  const group = (title: string, ids: readonly PlatformSection[]) => (
    <SidebarGroup>
      <SidebarGroupLabel><span>{title}</span><span className="ml-2 h-px flex-1 bg-sidebar-border" /></SidebarGroupLabel>
      <SidebarMenu>
        {ids.map((id) => PLATFORM_SECTIONS.find((section) => section.id === id)!).filter((section) => section.id !== "staff" || role === "admin").map((section) => {
          const href = `/platform/${section.id}`;
          const active = pathname === href || pathname.startsWith(`${href}/`);
          return (
            <SidebarMenuItem key={section.id}>
              <SidebarMenuButton isActive={active} render={<Link to={href as never} aria-label={section.title} aria-current={active ? "page" : undefined} />} onClick={() => setOpenMobile(false)}>
                <section.icon /><span className="flex-1">{section.title}</span>
                {section.id === "tenants" && count !== undefined ? <Badge variant="outline">{count}</Badge> : null}
                {section.id === "health" && health ? <HealthBadge value={health} /> : null}
              </SidebarMenuButton>
            </SidebarMenuItem>
          );
        })}
      </SidebarMenu>
    </SidebarGroup>
  );
  return (
    <Sidebar className={isMobile ? "w-[calc(100vw-var(--spacing)*3)]" : "absolute inset-y-0 h-full"}>
      <SidebarHeader>
        <div className="flex h-[var(--workspace-topbar-height)] items-center gap-2 px-2">
          <DetentWordmark aria-hidden="true" className="h-4 w-auto shrink-0" />
          <span className="text-sm font-semibold">Detent Cloud</span><Badge variant="info">Platform</Badge>
        </div>
        {role === "" ? null : <Button variant="outline" size="sm" onClick={() => { setOpenMobile(false); onSearch(); }}>
          <Search /><span className="flex-1 text-left">Find tenant or user</span><kbd className="text-xs text-muted-foreground">⌘K</kbd>
        </Button>}
      </SidebarHeader>
      <SidebarContent>
        {role === "" ? null : <nav aria-label="Platform navigation">
          {group("Customers", ["tenants", "users", "audit"])}
          {group("System", ["health", "allowlist", "staff"])}
        </nav>}
        <SidebarGroup>
          <SidebarGroupLabel><span>Your organizations</span><span className="ml-2 h-px flex-1 bg-sidebar-border" /></SidebarGroupLabel>
          <nav aria-label="Your organizations">
            <SidebarMenu>
              {organizations.map((organization) => <SidebarMenuItem key={organization.id}>
                <SidebarMenuButton render={<a href={organization.url} />}>
                  <Building2 /><span className="flex-1">{organization.name}</span><ArrowUpRight />
                </SidebarMenuButton>
              </SidebarMenuItem>)}
            </SidebarMenu>
          </nav>
        </SidebarGroup>
      </SidebarContent>
      {email === "" ? null : <><SidebarSeparator /><SidebarFooter role="contentinfo" aria-label="Platform account">
        <span className="truncate px-2 text-sm" title={email}>{email}</span>
        <div className="flex items-center justify-between pl-2">
          <span className="text-xs text-muted-foreground">{PLATFORM_ROLES[role] ?? role}</span><SignOut csrf={csrf} />
        </div>
      </SidebarFooter></>}
    </Sidebar>
  );
}

export function PlatformConsole(): React.ReactElement {
  const api = useEntryApi();
  const session = usePlatformResource(() => api.session(), [api]);
  const organizations = usePlatformResource(() => api.organizations(), [api]);
  const value = session.value;
  const role = value?.platform_role ?? "";
  const tenants = usePlatformResource(() => role === "" ? Promise.resolve(null) : api.platformOrganizations(), [api, role]);
  const health = usePlatformResource(() => role === "" ? Promise.resolve(null) : api.platformHealth(), [api, role]);
  const pathname = useRouterState({ select: (state) => state.location.pathname });
  const section = PLATFORM_SECTIONS.find((section) => (pathname === `/platform/${section.id}` || pathname.startsWith(`/platform/${section.id}/`)));
  usePageTitle(section?.title ?? "Platform", "Platform");
  const [searchOpen, setSearchOpen] = React.useState(false);
  const [actions, setActions] = React.useState<HTMLDivElement | null>(null);
  const error = session.error ?? organizations.error;
  const denied = role === "" || (section?.id === "staff" && role !== "admin");
  return (
    <PlatformRoleContext.Provider value={role}>
      <PlatformCSRFContext.Provider value={value?.csrf ?? ""}>
        <PlatformActionsContext.Provider value={actions}>
          <SidebarProvider open className="relative h-full min-h-0 flex-1">
            <PlatformNavigation role={role} email={value?.email ?? ""} csrf={value?.csrf ?? ""} organizations={organizations.value?.organizations ?? []}
              count={tenants.value?.organizations.length} health={health.value} onSearch={() => setSearchOpen(true)} />
            <main className="flex min-h-0 min-w-0 flex-1 flex-col">
              <WorkspacePageHeader className="border-b border-border">
                <SidebarTrigger className="md:hidden" />
                <h1 className="min-w-0 truncate text-sm font-medium">{section?.title ?? "Platform"}</h1>
                <div ref={setActions} className="ml-auto flex shrink-0 items-center gap-2" />
              </WorkspacePageHeader>
              <div className="min-h-0 flex-1 overflow-y-auto">
                <WorkspacePageContainer width="wide">
                  <Problem message={error?.message ?? null} />
                  {value === undefined ? (error === null ? <Skeleton className="h-48" aria-label="Loading platform console" /> : null) : denied ? (
                    <Empty><EmptyHeader><EmptyTitle>You don't have access to this page</EmptyTitle>
                      {role === "" ? <EmptyDescription>The platform console is limited to Detent staff.</EmptyDescription> : null}
                    </EmptyHeader></Empty>
                  ) : <Outlet />}
                </WorkspacePageContainer>
              </div>
            </main>
            {role === "" ? null : <PlatformSearch open={searchOpen} onOpenChange={setSearchOpen} tenants={tenants.value?.organizations ?? []} tenantError={tenants.error} />}
          </SidebarProvider>
        </PlatformActionsContext.Provider>
      </PlatformCSRFContext.Provider>
    </PlatformRoleContext.Provider>
  );
}

export function PlatformPageTitle({ section }: { readonly section: PlatformSection }): React.ReactElement {
  const title = PLATFORM_SECTIONS.find((item) => item.id === section)!.title;
  usePageTitle(title, "Platform");
  return <></>;
}

export function PlatformTenantsPage(): React.ReactElement {
  return <><PlatformPageTitle section="tenants" /><PlatformTenants /></>;
}

export function PlatformTenantPage(): React.ReactElement {
  const role = React.useContext(PlatformRoleContext);
  return <><PlatformPageTitle section="tenants" /><PlatformTenant role={role} /></>;
}

export function PlatformHealthPage(): React.ReactElement {
  const api = useEntryApi();
  const health = usePlatformResource(() => api.platformHealth(), [api]);
  return (
    <>
      <PlatformPageTitle section="health" />
      <Problem message={health.error?.message ?? null} />
      {health.value === undefined ? (
        health.loading ? <p className="text-sm text-muted-foreground">Loading health…</p> : null
      ) : <PlatformHealthPanel value={health.value} />}
    </>
  );
}

export function PlatformAllowlistPage(): React.ReactElement {
  const api = useEntryApi();
  const allowlist = usePlatformResource(() => api.platformAllowlist(), [api]);
  return (
    <>
      <PlatformPageTitle section="allowlist" />
      <Problem message={allowlist.error?.message ?? null} />
      {allowlist.value === undefined ? (
        allowlist.loading ? <p className="text-sm text-muted-foreground">Loading allowlist…</p> : null
      ) : <PlatformAllowlistPanel value={allowlist.value} />}
    </>
  );
}

export function PlatformSectionPage({ section }: { readonly section: "staff" | "audit" }): React.ReactElement {
  const role = React.useContext(PlatformRoleContext);
  const csrf = React.useContext(PlatformCSRFContext);
  usePageTitle(section === "staff" ? "Staff" : "Audit", "Platform");
  if (section === "staff" && role === "admin") return <PlatformStaff csrf={csrf} />;
  return (
    <>
      <PlatformPageTitle section={section} />
      {section === "staff" ? <Problem message="You do not have permission to do that." /> : null}
    </>
  );
}
