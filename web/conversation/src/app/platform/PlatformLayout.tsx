import { Link, Outlet, useLocation } from "@tanstack/react-router";
import { LogOutIcon, ShieldAlertIcon } from "lucide-react";
import React from "react";

import { DetentWordmark } from "../../components/DetentWordmark.tsx";
import { Badge } from "../../components/ui/badge.tsx";
import { Button } from "../../components/ui/button.tsx";
import { Empty, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from "../../components/ui/empty.tsx";
import {
  Sidebar,
  SidebarContent,
  SidebarFooter,
  SidebarGroup,
  SidebarGroupLabel,
  SidebarHeader,
  SidebarInset,
  SidebarMenu,
  SidebarMenuButton,
  SidebarMenuItem,
  SidebarProvider,
  SidebarRail,
  SidebarTrigger,
  useSidebar,
} from "../../components/ui/sidebar.tsx";
import { useResource } from "../account/useResource.ts";
import type { PlatformOrganizations } from "../entry/api.ts";
import { SIGN_IN_PLATFORM } from "../entry/api.ts";
import { EntryApiContext } from "../entry/EntryScreens.tsx";
import { accessState, PlatformAccessProvider, type PlatformAccess } from "./access.tsx";
import { makePlatformApi, type PlatformApi } from "./api.ts";
import { activePlatformPage, PLATFORM_NAV, PLATFORM_NAV_GROUPS } from "./nav.ts";
import { FailedState, LoadingRows } from "./ui.tsx";

const PlatformApiContext = React.createContext<PlatformApi>(makePlatformApi());

export const PlatformApiProvider = PlatformApiContext.Provider;

export function usePlatformApi(): PlatformApi {
  return React.useContext(PlatformApiContext);
}

function PlatformNav(): React.ReactElement {
  const pathname = useLocation({ select: (location) => location.pathname });
  const active = activePlatformPage(pathname);
  const { isMobile, setOpenMobile } = useSidebar();
  return (
    <SidebarContent className="overflow-x-hidden">
      {PLATFORM_NAV_GROUPS.map((group) => (
        <SidebarGroup key={group.id} className="gap-1 px-[var(--sidebar-content-inset)] py-1.5">
          <SidebarGroupLabel className="text-sidebar-muted-foreground/70">{group.label}</SidebarGroupLabel>
          <SidebarMenu className="ps-px">
            {PLATFORM_NAV.filter((item) => item.group === group.id).map((item) => {
              const Icon = item.icon;
              const isActive = item.id === active;
              return (
                <SidebarMenuItem key={item.id}>
                  <SidebarMenuButton
                    isActive={isActive}
                    aria-current={isActive ? "page" : undefined}
                    render={<Link to={item.to as never} />}
                    onClick={() => {
                      if (isMobile) setOpenMobile(false);
                    }}
                  >
                    <Icon />
                    <span>{item.label}</span>
                  </SidebarMenuButton>
                </SidebarMenuItem>
              );
            })}
          </SidebarMenu>
        </SidebarGroup>
      ))}
    </SidebarContent>
  );
}

function PlatformBrand(): React.ReactElement {
  return (
    <SidebarHeader className="h-[var(--workspace-topbar-height)] shrink-0 flex-row items-center justify-between px-3 py-0">
      <Link
        to="/platform"
        aria-label="Go to the platform overview"
        className="flex h-8 min-w-0 items-center gap-1.5 rounded-md text-foreground outline-hidden ring-ring focus-visible:ring-2"
      >
        <DetentWordmark aria-hidden="true" className="h-4 w-auto shrink-0" />
        <span className="truncate text-base font-semibold tracking-tight">Detent</span>
        <Badge variant="info" size="sm">
          Platform
        </Badge>
      </Link>
      <SidebarTrigger aria-label="Collapse sidebar" className="ml-auto" />
    </SidebarHeader>
  );
}

function PlatformAccount({ access }: { readonly access: PlatformAccess }): React.ReactElement {
  const roles = [access.canSupport ? "support" : null, access.canGrant ? "entitlements" : null].filter(
    (role): role is string => role !== null,
  );
  return (
    <SidebarFooter className="gap-1 p-[var(--sidebar-content-inset)]">
      <div className="min-w-0 px-2 py-1">
        <div className="truncate text-sm font-medium text-sidebar-foreground">{access.email}</div>
        <div className="truncate text-xs text-sidebar-muted-foreground">
          Staff{roles.length === 0 ? "" : ` with ${roles.join(" and ")}`}
        </div>
      </div>
      <form method="post" action="/logout">
        <input type="hidden" name="csrf" value={access.csrf} />
        <SidebarMenu>
          <SidebarMenuItem>
            <SidebarMenuButton type="submit">
              <LogOutIcon />
              <span>Sign out</span>
            </SidebarMenuButton>
          </SidebarMenuItem>
        </SidebarMenu>
      </form>
    </SidebarFooter>
  );
}

export function PlatformRestricted(): React.ReactElement {
  return (
    <Empty className="h-full">
      <EmptyHeader>
        <EmptyMedia variant="icon">
          <ShieldAlertIcon />
        </EmptyMedia>
        <EmptyTitle>Detent staff only</EmptyTitle>
        <EmptyDescription>
          This account is not on the platform staff list. Sign in with a staff account, or go to your organizations.
        </EmptyDescription>
      </EmptyHeader>
      <div className="flex gap-2">
        <Button variant="outline" render={<a href="/organizations" />}>
          Your organizations
        </Button>
        <Button render={<a href={SIGN_IN_PLATFORM} />}>Sign in again</Button>
      </div>
    </Empty>
  );
}

export function PlatformLayout({ children }: { readonly children?: React.ReactNode }): React.ReactElement {
  const api = usePlatformApi();
  const listing = useResource<PlatformOrganizations>(() => api.live.organizations(), [api]);
  const state = accessState(listing.value, listing.error);
  React.useEffect(() => {
    if (state.kind === "signed-out") globalThis.location?.assign(SIGN_IN_PLATFORM);
  }, [state.kind]);

  if (state.kind === "forbidden") return <PlatformRestricted />;
  if (state.kind !== "ready") {
    return (
      <div className="mx-auto w-full max-w-3xl p-6">
        {state.kind === "failed" && listing.error !== null ? (
          <FailedState error={listing.error} onRetry={() => void listing.refresh()} />
        ) : (
          <LoadingRows label="Loading the platform console" rows={3} />
        )}
      </div>
    );
  }
  return (
    <EntryApiContext.Provider value={api.entry}>
      <PlatformAccessProvider value={state.access}>
        <SidebarProvider className="h-dvh! min-h-0!" defaultOpen>
          <Sidebar
            side="left"
            collapsible="offcanvas"
            aria-label="Platform navigation"
            className="border-r border-sidebar-border bg-sidebar text-sidebar-foreground"
          >
            <PlatformBrand />
            <PlatformNav />
            <PlatformAccount access={state.access} />
            <SidebarRail />
          </Sidebar>
          <SidebarInset className="dc-main min-h-0 overflow-hidden">{children ?? <Outlet />}</SidebarInset>
        </SidebarProvider>
      </PlatformAccessProvider>
    </EntryApiContext.Provider>
  );
}
