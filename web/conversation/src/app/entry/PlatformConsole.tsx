import React from "react";
import { Link, Outlet, useRouterState } from "@tanstack/react-router";
import { Activity, Building2, ChevronDown, ListChecks, ScrollText, Users, UserSearch } from "lucide-react";

import { Menu, MenuItem, MenuPopup, MenuTrigger } from "../../components/ui/menu.tsx";
import {
  Sidebar,
  SidebarContent,
  SidebarFooter,
  SidebarGroup,
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
import { DetentCloudLogo } from "../account/Login.tsx";
import { usePlatformResource } from "./usePlatformResource.ts";
import { usePageTitle } from "../pageTitle.ts";
import {
  type PlatformAllowlist,
  type PlatformHealth,
} from "./api.ts";
import { PlatformTenants } from "./PlatformTenants.tsx";
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
    <section aria-label={title} className="rounded-2xl border border-border bg-card p-5">
      <h2 className="text-base font-semibold">{title}</h2>
      {description === undefined ? null : (
        <p className="mt-1 text-[13px] text-muted-foreground">{description}</p>
      )}
      <div className="mt-4">{children}</div>
    </section>
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
  { id: "audit", title: "Audit", icon: ScrollText },
  { id: "health", title: "Health", icon: Activity },
  { id: "allowlist", title: "Allowlist", icon: ListChecks },
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

function PlatformNavigation({ role }: { readonly role: string }): React.ReactElement {
  const pathname = useRouterState({ select: (state) => state.location.pathname });
  const { isMobile, setOpenMobile } = useSidebar();
  return (
    <Sidebar className={isMobile ? "w-[calc(100vw-var(--spacing)*3)]" : "absolute inset-y-0 h-full"}>
      <SidebarContent>
        <SidebarGroup>
          <nav aria-label="Platform navigation">
            <SidebarMenu>
              {PLATFORM_SECTIONS.filter((section) => role !== "" && (section.id !== "staff" || role === "admin")).map((section) => {
                const href = `/platform/${section.id}`;
                const active = pathname === href;
                return (
                  <SidebarMenuItem key={section.id}>
                    <SidebarMenuButton
                      isActive={active}
                      render={<Link to={href as never} aria-current={active ? "page" : undefined} />}
                      onClick={() => setOpenMobile(false)}
                    >
                      <section.icon />
                      <span>{section.title}</span>
                    </SidebarMenuButton>
                  </SidebarMenuItem>
                );
              })}
            </SidebarMenu>
          </nav>
        </SidebarGroup>
      </SidebarContent>
      {role === "" ? null : (
        <>
          <SidebarSeparator />
          <SidebarFooter>
            <span className="px-2 text-xs text-muted-foreground">{PLATFORM_ROLES[role] ?? role}</span>
          </SidebarFooter>
        </>
      )}
    </Sidebar>
  );
}

export function PlatformConsole(): React.ReactElement {
  const api = useEntryApi();
  const session = usePlatformResource(() => api.session(), [api]);
  const organizations = usePlatformResource(() => api.organizations(), [api]);
  const value = session.value;
  const role = value?.platform_role ?? "";
  const error = session.error ?? organizations.error;
  return (
    <PlatformRoleContext.Provider value={role}>
      <PlatformCSRFContext.Provider value={value?.csrf ?? ""}>
      <SidebarProvider open className="h-full min-h-0 flex-1 flex-col">
        <header className="flex shrink-0 flex-wrap items-center gap-3 border-b border-border/60 px-4 py-3 sm:px-8">
          <SidebarTrigger className="md:hidden" />
          <DetentCloudLogo />
          <Badge variant="info">Platform</Badge>
          <span className="flex-1" />
          {(organizations.value?.organizations.length ?? 0) === 0 ? null : (
            <Menu>
              <MenuTrigger render={<Button size="sm" variant="outline" />}>
                Open organization <ChevronDown />
              </MenuTrigger>
              <MenuPopup align="end">
                {organizations.value?.organizations.map((organization) => (
                  <MenuItem key={organization.id} render={<a href={organization.url} />}>
                    {organization.name}
                  </MenuItem>
                ))}
              </MenuPopup>
            </Menu>
          )}
          {value === undefined ? null : (
            <>
              <span className="max-w-full truncate text-sm text-muted-foreground">{value.email}</span>
              <SignOut csrf={value.csrf} />
            </>
          )}
        </header>
        <div className="relative flex min-h-0 flex-1">
          <PlatformNavigation role={role} />
          <main className="min-w-0 flex-1 overflow-y-auto">
            <div className="mx-auto flex w-full max-w-6xl flex-col gap-5 px-4 py-6 sm:px-8">
              <Problem message={error?.message ?? null} />
              {value === undefined ? (
                error === null ? <p className="text-sm text-muted-foreground">Loading platform console…</p> : null
              ) : role === "" ? (
                <Problem message="The platform console is limited to Detent staff." />
              ) : <Outlet />}
            </div>
          </main>
        </div>
      </SidebarProvider>
    </PlatformCSRFContext.Provider>
    </PlatformRoleContext.Provider>
  );
}

export function PlatformPageTitle({ section }: { readonly section: PlatformSection }): React.ReactElement {
  const title = PLATFORM_SECTIONS.find((item) => item.id === section)!.title;
  usePageTitle(title, "Platform");
  return <h1 className="text-2xl font-semibold tracking-[-0.02em]">{title}</h1>;
}

export function PlatformTenantsPage(): React.ReactElement {
  const role = React.useContext(PlatformRoleContext);
  return <><PlatformPageTitle section="tenants" /><PlatformTenants role={role} /></>;
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
