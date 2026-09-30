import {
  ActivityIcon,
  BookOpenIcon,
  ChartNoAxesColumnIcon,
  ChevronDownIcon,
  KanbanIcon,
  SquarePenIcon,
  StethoscopeIcon,
} from "lucide-react";
import React from "react";

import { cn } from "../../lib/utils.ts";
import { SidebarMenu, SidebarMenuButton, SidebarMenuItem, useSidebar } from "../../components/ui/sidebar.tsx";
import { Tooltip, TooltipPopup, TooltipTrigger } from "../../components/ui/tooltip.tsx";
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
 * tooltip saying so rather than as a link into a 404, because A.12's rule is
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
  { id: "activity", label: "Activity", icon: <ActivityIcon />, to: null },
  { id: "diagnostics", label: "Diagnostics", icon: <StethoscopeIcon />, to: null },
  { id: "reports", label: "Reports", icon: <ChartNoAxesColumnIcon />, to: null },
  { id: "library", label: "Library", icon: <BookOpenIcon />, to: null },
];

function NavRow({
  icon,
  label,
  active,
  onClick,
  disabled = false,
  trailing,
  testId,
}: {
  icon: React.ReactNode;
  label: string;
  active: boolean;
  onClick?: (() => void) | undefined;
  disabled?: boolean;
  trailing?: React.ReactNode;
  testId?: string;
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
      className="w-full min-w-0 focus-visible:ring-offset-2 focus-visible:ring-offset-sidebar"
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
    </SidebarMenuItem>
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
  const { isMobile, setOpenMobile } = useSidebar();
  const [browseExpanded, setBrowseExpanded] = React.useState(true);
  const navigation = data?.navigation;
  const activeProjectId = data?.activeProjectId ?? null;

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
      {/* The artifact's primary destination (A.1). It sits directly above the
          thread list because Work is the other half of the same question. */}
      <SidebarMenu className="gap-px pb-1">
        <NavRow
          testId="nav-work"
          icon={<KanbanIcon />}
          label="Work"
          active={navigation.activePath.startsWith("/work")}
          onClick={() =>
            navigate(activeProjectId === null ? "/work" : `/work/p/${activeProjectId}`)
          }
        />
        <NavRow
          testId="nav-chat"
          icon={<SquarePenIcon />}
          label="Chat"
          active={navigation.activePath.startsWith("/chat")}
          onClick={() => navigate("/chat")}
        />
      </SidebarMenu>

      {/* Browse: the artifact's cross-cutting destinations (A.1, A.6). */}
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
                active={row.to !== null && navigation.activePath === row.to}
                onClick={row.to === null ? undefined : () => navigate(row.to as string)}
              />
            ))}
          </SidebarMenu>
        ) : null}
      </section>
    </>
  );
}
