import React from "react";
import { Link, Outlet, useParams, useRouterState } from "@tanstack/react-router";
import { Activity, ArrowUpRight, Building2, ListChecks, ScrollText, Search, Users, UserSearch } from "lucide-react";

import { WorkspacePageHeader } from "../../components/WorkspacePageHeader.tsx";
import { WorkspacePageContainer } from "../../components/WorkspacePageContainer.tsx";
import { DetentWordmark } from "../../components/DetentWordmark.tsx";
import { Empty, EmptyHeader, EmptyTitle, EmptyDescription } from "../../components/ui/empty.tsx";
import { Skeleton } from "../../components/ui/skeleton.tsx";
import { SettingsRow, SettingsSection } from "../settings/settingsLayout.tsx";
import { Input } from "../../components/ui/input.tsx";
import { Label } from "../../components/ui/label.tsx";
import { Table, TableBody, TableCell, TableRow } from "../../components/ui/table.tsx";
import { PlatformSearch } from "./PlatformSearch.tsx";
import { PlatformActionsContext, PlatformHeadingContext } from "./PlatformPageActions.tsx";
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
import { PlatformTenant } from "./PlatformTenant.tsx";
import { PlatformTenants } from "./PlatformTenants.tsx";
import { PlatformHealthContext, PlatformHealthContent, platformHealthState } from "./PlatformHealth.tsx";
import { PlatformStaff } from "./PlatformStaff.tsx";
import { Problem, SignOut, useEntryApi } from "./EntryScreens.tsx";

export { formatBytes, PlatformHealthPanel } from "./PlatformHealth.tsx";

export function PlatformAllowlistPanel({ value }: { readonly value: PlatformAllowlist }): React.ReactElement {
  const [query, setQuery] = React.useState("");
  const term = query.trim().toLowerCase();
  const open = value.self_service && (value.open ?? (value.allowed_domains.length === 0 && value.allowed_emails.length === 0));
  const description = !value.self_service ? "Self-service organization creation is not configured." :
    open ? "Any verified account may create an organization." : "Only the domains and emails below.";
  const lists = [
    { title: "Domains", entries: value.allowed_domains, empty: "No domains configured.", noMatch: "No domains match this filter." },
    { title: "Individual emails", entries: value.allowed_emails, empty: "No individual emails configured.", noMatch: "No individual emails match this filter." },
  ];
  return (
    <section aria-label="Sign-up allowlist" className="grid min-w-0 gap-6">
      <SettingsSection title="Access" aria-label="Access">
        <SettingsRow title="Who can sign up" description={`${description} Changes are made in the Cloud configuration.`}
          control={<Badge variant="outline">{!value.self_service ? "Disabled" : open ? "Open" : "Restricted"}</Badge>} />
      </SettingsSection>
      {value.self_service && !open ? <>
        <div className="grid w-full max-w-sm gap-2">
          <Label htmlFor="allowlist-filter">Filter domains and emails</Label>
          <Input id="allowlist-filter" type="search" size="compact" placeholder="Filter domains and emails"
            value={query} onChange={(event) => setQuery(event.target.value)} />
        </div>
        {lists.map(({ title, entries, empty, noMatch }) => {
          const filtered = entries.filter((entry) => entry.toLowerCase().includes(term));
          return <SettingsSection key={title} title={title} aria-label={title}
            headerAction={<Badge variant="outline">{filtered.length}</Badge>}>
            {filtered.length === 0 ? <p className="p-4 text-sm text-muted-foreground">{entries.length === 0 ? empty : noMatch}</p> :
              <Table aria-label={title}>
                <TableBody>{filtered.map((entry) => <TableRow key={entry}>
                  <TableCell className="whitespace-normal break-all px-4 py-3 font-medium">{entry}</TableCell>
                </TableRow>)}</TableBody>
              </Table>}
          </SettingsSection>;
        })}
      </> : null}
    </section>
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

export const PlatformRoleContext = React.createContext("");
const PlatformCSRFContext = React.createContext("");

function HealthBadge({ value }: { readonly value: PlatformHealth }): React.ReactElement {
  const state = platformHealthState(value);
  return <Badge variant={state}>{state === "error" ? "Down" : state === "warning" ? "Degraded" : "OK"}</Badge>;
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
  const health = usePlatformResource(() => role === "" ? Promise.resolve(null) : api.platformHealth().then((value) => ({ ...value, updatedAt: Date.now() })), [api, role]);
  const pathname = useRouterState({ select: (state) => state.location.pathname });
  const section = PLATFORM_SECTIONS.find((section) => pathname === `/platform/${section.id}` || pathname.startsWith(`/platform/${section.id}/`));
  const { email } = useParams({ strict: false }) as { email?: string };
  const query = useRouterState({ select: (state) => (state.location.search as { q?: string }).q });
  usePageTitle(section?.title ?? "Platform", "Platform");
  const [searchOpen, setSearchOpen] = React.useState(false);
  const [heading, setHeading] = React.useState<React.ReactNode>(null);
  const [actions, setActions] = React.useState<HTMLDivElement | null>(null);
  const error = session.error ?? organizations.error;
  const denied = role === "" || (section?.id === "staff" && role !== "admin");
  return (
    <PlatformRoleContext.Provider value={role}>
      <PlatformCSRFContext.Provider value={value?.csrf ?? ""}>
        <PlatformHealthContext.Provider value={health}>
          <PlatformHeadingContext.Provider value={setHeading}><PlatformActionsContext.Provider value={actions}>
            <SidebarProvider open className="relative h-full min-h-0 flex-1">
              <PlatformNavigation role={role} email={value?.email ?? ""} csrf={value?.csrf ?? ""} organizations={organizations.value?.organizations ?? []}
                count={tenants.value?.organizations.length} health={health.value} onSearch={() => setSearchOpen(true)} />
              <main className="flex min-h-0 min-w-0 flex-1 flex-col">
                <WorkspacePageHeader className="border-b border-border">
                  <SidebarTrigger className="md:hidden" />
                  <div className="min-w-0 flex-1">{heading ?? <h1 className="truncate text-sm font-medium">{email === undefined ? section?.title ?? "Platform" : <><Link to={"/platform/users" as never} search={{ q: query } as never} className="text-muted-foreground hover:underline">Users</Link><span className="mx-2 text-muted-foreground">/</span>{email}</>}</h1>}</div>
                  <div ref={setActions} className="ml-auto flex shrink-0 flex-wrap items-center justify-end gap-2" />
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
          </PlatformActionsContext.Provider></PlatformHeadingContext.Provider>
        </PlatformHealthContext.Provider>
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
  return <PlatformTenant role={role} />;
}

export function PlatformHealthPage(): React.ReactElement {
  return <><PlatformPageTitle section="health" /><PlatformHealthContent /></>;
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
