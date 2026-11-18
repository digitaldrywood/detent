import React from "react";
import {
  Link,
  Outlet,
  useNavigate,
  useRouterState,
} from "@tanstack/react-router";
import { FolderIcon, ListIcon, PlugIcon } from "lucide-react";
import { AppSidebarLayout } from "../../components/AppSidebarLayout.tsx";
import { SidebarChromeHeader } from "../../components/sidebar/SidebarChrome.tsx";
import { SidebarWorkspacePicker } from "../../components/sidebar/SidebarWorkspacePicker.tsx";
import {
  SidebarContent,
  SidebarFooter,
  SidebarMenu,
  SidebarMenuButton,
  SidebarMenuItem,
  SidebarInset,
  useSidebar,
} from "../../components/ui/sidebar.tsx";
import { NativeSelect } from "../account/controls.tsx";
import { useAccountApi, useAccountBootstrap } from "../account/context.ts";
import { useResource } from "../account/useResource.ts";
import { withoutBasePath, routerBasePath } from "../../runtime/basePath.ts";
import { readConnections, landingPath } from "./connections.ts";
import { ClientContext } from "../client.ts";

const ProjectScope = React.createContext({
  project: "",
  setProject: (_value: string) => {},
});
export function useReporterProject() {
  return React.useContext(ProjectScope);
}

export function RoleSidebar({ children }: { children: React.ReactNode }) {
  const client = React.useContext(ClientContext);
  return client?.account?.actor.role === "reporter" ? (
    <ReporterSidebar />
  ) : (
    <>{children}</>
  );
}

export function reporterPathAllowed(path: string): boolean {
  return (
    path === "/reporter" ||
    path === "/my-issues" ||
    path === "/connect-ai" ||
    /^\/work\/i\/[^/]+$/.test(path) ||
    /^\/[^/]+\/issues\/\d+$/.test(path)
  );
}

export function ReporterSidebar() {
  const account = useAccountBootstrap();
  const scope = useReporterProject();
  const { setOpenMobile } = useSidebar();
  const path = useRouterState({
    select: (state) => withoutBasePath(state.location.pathname, routerBasePath()),
  });
  return (
    <>
      <SidebarChromeHeader isElectron={false} />
      <SidebarContent className="p-2">
        <div className="flex items-center gap-2 px-2 py-3">
          <FolderIcon
            aria-hidden
            className="size-4 shrink-0 text-muted-foreground"
          />
          <NativeSelect
            aria-label="Project"
            value={scope.project}
            onValueChange={scope.setProject}
            options={[
              { value: "", label: "All projects" },
              ...(account?.projects.map((project) => ({
                value: project.id,
                label: project.name,
              })) ?? []),
            ]}
          />
        </div>
        <SidebarMenu>
          {(
            [
              { to: "/my-issues", label: "My issues", Icon: ListIcon },
              { to: "/connect-ai", label: "Connect AI", Icon: PlugIcon },
            ] as const
          ).map(({ to, label, Icon }) => (
            <SidebarMenuItem key={to}>
              <SidebarMenuButton
                isActive={
                  to === path || (to === "/my-issues" && path !== "/connect-ai")
                }
                render={<Link to={to} />}
                onClick={() => setOpenMobile(false)}
              >
                <Icon aria-hidden />
                <span>{label}</span>
              </SidebarMenuButton>
            </SidebarMenuItem>
          ))}
        </SidebarMenu>
      </SidebarContent>
      <SidebarFooter>
        <SidebarMenu>
          <SidebarWorkspacePicker />
        </SidebarMenu>
      </SidebarFooter>
    </>
  );
}

export function ReporterShell() {
  const [project, setProject] = React.useState("");
  return (
    <ProjectScope.Provider value={{ project, setProject }}>
      <AppSidebarLayout>
        <SidebarInset className="min-h-0 overflow-hidden">
          <ReporterOutlet />
        </SidebarInset>
      </AppSidebarLayout>
    </ProjectScope.Provider>
  );
}

export function ReporterOutlet() {
  const path = useRouterState({
    select: (state) => withoutBasePath(state.location.pathname, routerBasePath()),
  });
  const navigate = useNavigate();
  const allowed = reporterPathAllowed(path);
  React.useEffect(() => {
    if (!allowed) void navigate({ to: "/my-issues", replace: true });
  }, [allowed, navigate]);
  return allowed ? <Outlet /> : null;
}

export function ReporterLanding() {
  const api = useAccountApi();
  const account = useAccountBootstrap();
  const navigate = useNavigate();
  const result = useResource(
    () => readConnections(api, account!.organization.id),
    [api, account],
  );
  React.useEffect(() => {
    if (result.value)
      void navigate({
        to: landingPath(account?.actor.role ?? "", result.value),
        replace: true,
      });
  }, [result.value, navigate, account]);
  return (
    <div className="p-6">
      {result.error ? (
        <p role="alert">{result.error.message}</p>
      ) : (
        <p role="status">Loading workspace…</p>
      )}
    </div>
  );
}
