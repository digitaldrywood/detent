import { CircleDashedIcon, BookmarkIcon, LayoutListIcon } from "lucide-react";
import { ViewActions } from "../work/SavedViewActions.tsx";
import { useSavedViews, savedViewPath } from "../work/lib/savedViews.tsx";
import {
  ActivityIcon,
  CircleAlertIcon,
  TriangleAlertIcon,
  UsersIcon,
  BookOpenIcon,
  ChartNoAxesColumnIcon,
  ChevronDownIcon,
  KanbanIcon,
  SquarePenIcon,
  StethoscopeIcon,
} from "lucide-react";
import React from "react";

import { ProjectSetupContext } from "../account/projectSetup.tsx";
import { firstUnreadyStep, orderedSteps } from "../account/setupSteps.ts";
import { Badge } from "../../components/ui/badge.tsx";
import { relativeTimeLabel } from "../lib/sidebarLogic.ts";
import { cn } from "../../lib/utils.ts";
import { SidebarMenu, SidebarMenuButton, SidebarMenuItem, useSidebar } from "../../components/ui/sidebar.tsx";
import { Tooltip, TooltipPopup, TooltipTrigger } from "../../components/ui/tooltip.tsx";
import { useUiStateStore } from "../../uiStateStore.ts";
import { sidebarProjectScopeKey } from "./shell.ts";
import { useSidebarData } from "./sidebarData.tsx";

export function SidebarLandmark({
  children,
}: {
  children: React.ReactNode;
}): React.ReactElement {
  return (
    <aside
      id="dc-sidebar"
      className="dc-side flex h-full min-h-0 w-full flex-col"
      aria-label="Conversations"
    >
      {children}
    </aside>
  );
}

/**
 * The Browse group, from artifact screen 1's sidebar. `to: null` is a
 * destination this client does not serve yet: it renders disabled with one
 * tooltip saying so rather than as a link into a 404, because the rule is
 * that a control with nothing behind it must not look like a control that
 * works.
 */
interface BrowseRow {
  readonly id: string;
  readonly label: string;
  readonly icon: React.ReactNode;
  readonly to: string | null;
}

const BROWSE_ROWS: readonly BrowseRow[] = [
  { id: "activity", label: "Activity", icon: <ActivityIcon />, to: "/activity" },
  { id: "diagnostics", label: "Diagnostics", icon: <StethoscopeIcon />, to: "/diagnostics" },
  { id: "reports", label: "Reports", icon: <ChartNoAxesColumnIcon />, to: "/reports" },
  { id: "library", label: "Library", icon: <BookOpenIcon />, to: null },
];

function NavRow({
  icon,
  label,
  active,
  onClick,
  onIntent,
  disabled = false,
  trailing,
  testId,
  action,
}: {
  icon: React.ReactNode;
  label: string;
  active: boolean;
  onClick?: (() => void) | undefined;
  onIntent?: (() => void) | undefined;
  disabled?: boolean;
  trailing?: React.ReactNode;
  testId?: string;
  action?: React.ReactNode;
}): React.ReactElement {
  const button = (
    <SidebarMenuButton
      type="button"
      data-testid={testId}
      isActive={active}
      aria-current={active ? "page" : undefined}
      aria-disabled={disabled ? true : undefined}
      disabled={disabled}
      onClick={disabled ? undefined : onClick}
      onPointerEnter={disabled ? undefined : onIntent}
      onFocus={disabled ? undefined : onIntent}
      className={cn("w-full min-w-0 focus-visible:ring-inset", action && "pr-8")}
    >
      {icon}
      <span className="min-w-0 flex-1 truncate text-left">{label}</span>
      {trailing}
    </SidebarMenuButton>
  );
  return (
    <SidebarMenuItem>
      {disabled ? (
        <Tooltip>
          {/* The trigger wraps rather than renders the button: a disabled
              button emits no pointer events, so the tooltip would never open
              on the one row that most needs to explain itself. */}
          <TooltipTrigger
            render={
              <span
                className="block w-full"
                tabIndex={0}
                role="note"
                aria-label={`${label}: coming soon`}
              />
            }
          >
            {button}
          </TooltipTrigger>
          <TooltipPopup side="right">Coming soon</TooltipPopup>
        </Tooltip>
      ) : (
        button
      )}
      {action}
    </SidebarMenuItem>
  );
}

function FindingStateIcon({ instance }: { instance: boolean }): React.ReactElement {
  return (
    <span className="size-4 shrink-0">
      {instance ? (
        <CircleAlertIcon aria-label="Error" className="size-4 text-destructive-foreground" />
      ) : (
        <TriangleAlertIcon aria-label="Warning" className="size-4 text-warning-foreground" />
      )}
    </span>
  );
}

function SectionHeader({
  label,
  expanded,
  onToggle,
}: {
  label: string;
  expanded: boolean;
  onToggle: () => void;
}): React.ReactElement {
  return (
    <h2 className="mx-0.5 h-8" data-testid="sidebar-section-header">
      <button
        type="button"
        onClick={onToggle}
        aria-expanded={expanded}
        data-testid={`sidebar-${label.toLowerCase().replace(/\s+/g, "-")}-shelf-toggle`}
        className={cn(
          "flex h-full w-full cursor-pointer items-center gap-2 px-2 text-left text-xs font-medium",
          "text-sidebar-muted-foreground/60",
        )}
      >
        <span className="shrink-0">{label}</span>
        <span aria-hidden className="h-px min-w-2 flex-1 bg-sidebar-border/60" />
        <ChevronDownIcon
          aria-hidden
          className={cn("size-3 shrink-0 transition-transform", expanded && "rotate-180")}
        />
      </button>
    </h2>
  );
}

export function SidebarDestinations(): React.ReactElement | null {
  const data = useSidebarData();
  const setup = React.useContext(ProjectSetupContext);
  const saved = useSavedViews();
  const { isMobile, setOpenMobile } = useSidebar();
  const [browseExpanded, setBrowseExpanded] = React.useState(true);
  const navigation = data?.navigation;
  const findings = data?.findings ?? [];
  const focus = useUiStateStore((state) => state.sidebarProjectScopeKey);
  const workingKeys = useUiStateStore((state) => state.sidebarProjectWorkingSetKeys);
  const visibleProjects = data?.projects.filter((project) => focus !== null ? sidebarProjectScopeKey(project) === focus : workingKeys.length === 0 || workingKeys.includes(sidebarProjectScopeKey(project)));
  const humanActions = (data?.humanActions ?? []).filter((action) => action.projectId === undefined || (focus === null && workingKeys.length === 0) || visibleProjects?.some((project) => project.id === action.projectId));
  const [needsExpanded, setNeedsExpanded] = React.useState(true);
  const [favoritesExpanded, setFavoritesExpanded] = React.useState(true);
  const activeProjectId = data?.activeProjectId ?? null;
  const setupSteps = orderedSteps(setup?.onboarding.value, setup?.integration.value);
  const setupPath = `/projects/${activeProjectId}/setup`;
  const needsSetup = setup?.projectId === activeProjectId && setup?.onboarding.value?.ready === false;

  const navigate = React.useCallback(
    (to: string) => {
      if (isMobile) setOpenMobile(false);
      navigation?.onNavigate(to);
    },
    [isMobile, navigation, setOpenMobile],
  );

  if (navigation === undefined) return null;

  return (
    <>
      {/* The artifact's primary destination. It sits directly above the
          thread list because Work is the other half of the same question. */}
      <SidebarMenu className="gap-px pb-1">
        {needsSetup ? <NavRow
          icon={<CircleDashedIcon />}
          label="Setup"
          active={navigation.activePath === setupPath}
          onClick={() => navigate(setupPath)}
          trailing={<span className="text-xs text-muted-foreground">{firstUnreadyStep(setupSteps) + 1} of {setupSteps.length}</span>}
        /> : null}
        <NavRow
          testId="nav-work"
          icon={<KanbanIcon />}
          label="Work"
          active={navigation.activePath.startsWith("/work") && !navigation.activePath.startsWith("/work/views")}
          onClick={() =>
            navigate(activeProjectId === null ? "/work" : `/work/p/${activeProjectId}`)
          }
          onIntent={() => navigation.onPrefetch?.(activeProjectId === null ? "/work" : `/work/p/${activeProjectId}`)}
        />
        <NavRow
          testId="nav-chat"
          icon={<SquarePenIcon />}
          label="Chat"
          active={navigation.activePath.startsWith("/chat")}
          onClick={() => navigate("/chat")}
        />
      </SidebarMenu>

      {saved ? (
        <section aria-label="Favorites" className="flex flex-col pb-1">
          <SectionHeader
            label="Favorites"
            expanded={favoritesExpanded}
            onToggle={() => setFavoritesExpanded((value) => !value)}
          />
          {favoritesExpanded ? (
            <SidebarMenu className="gap-px">
              {saved.views.filter((view) => view.favorite).map((view) => (
                <NavRow
                  key={view.id}
                  testId={`favorite-${view.id}`}
                  icon={view.state.view === "board" ? <KanbanIcon /> : <LayoutListIcon />}
                  label={view.name}
                  active={navigation.activePath === savedViewPath(view)}
                  onClick={() => navigate(savedViewPath(view))}
                  trailing={view.scope === "organization" && navigation.activePath !== savedViewPath(view) ? <UsersIcon aria-label="Shared with organization" /> : undefined}
                  action={
                    <ViewActions
                      view={view}
                      sidebar
                      active={navigation.activePath === savedViewPath(view)}
                      onDeleted={() => {
                        if (navigation.activePath === savedViewPath(view)) navigate("/work");
                      }}
                    />
                  }
                />
              ))}
              <NavRow
                testId="nav-views"
                icon={<BookmarkIcon />}
                label="Views"
                active={navigation.activePath === "/work/views"}
                onClick={() => navigate("/work/views")}
              />
            </SidebarMenu>
          ) : null}
        </section>
      ) : null}

      {humanActions.length > 0 ? (
        <section aria-label="Needs you" data-testid="sidebar-needs-you" className="flex flex-col pb-1">
          <SectionHeader
            label="Needs you"
            expanded={needsExpanded}
            onToggle={() => setNeedsExpanded((value) => !value)}
          />
          {needsExpanded ? (
            <SidebarMenu className="gap-px">
              {humanActions.map((finding) => (
                <NavRow
                  key={finding.id}
                  testId={`attention-issue-${finding.id}`}
                  icon={<FindingStateIcon instance={false} />}
                  label={finding.label}
                  active={navigation.activePath === finding.to}
                  onClick={() => navigate(finding.to)}
                  trailing={
                    <time
                      dateTime={finding.opened_at}
                      title={finding.opened_at}
                      className="shrink-0 text-xs tabular-nums text-sidebar-muted-foreground"
                    >
                      {relativeTimeLabel(finding.opened_at, { now: Date.now() })}
                    </time>
                  }
                />
              ))}
            </SidebarMenu>
          ) : null}
        </section>
      ) : null}

      {/* Browse: the artifact's cross-cutting destinations. */}
      <section aria-label="Browse" className="flex flex-col pb-1">
        <SectionHeader
          label="Browse"
          expanded={browseExpanded}
          onToggle={() => setBrowseExpanded((value) => !value)}
        />
        {browseExpanded ? (
          <SidebarMenu className="gap-px">
            {BROWSE_ROWS.map((row) => (
              <NavRow
                key={row.id}
                testId={`nav-${row.id}`}
                icon={row.icon}
                label={row.label}
                disabled={row.to === null}
                trailing={row.id === "diagnostics" && findings.length > 0 ? (
                  <Badge variant="warning" size="sm" data-testid="diagnostics-findings-count" className="shrink-0 tabular-nums">
                    {findings.length}
                  </Badge>
                ) : undefined}
                active={row.to !== null && (navigation.activePath === row.to || navigation.activePath.startsWith(`${row.to}/`))}
                onClick={row.to === null ? undefined : () => navigate(row.to as string)}
                onIntent={row.to === null ? undefined : () => navigation.onPrefetch?.(row.to as string)}
              />
            ))}
          </SidebarMenu>
        ) : null}
      </section>
    </>
  );
}
