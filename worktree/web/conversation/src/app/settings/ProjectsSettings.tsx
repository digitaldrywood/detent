import React from "react";
import {
  DndContext,
  PointerSensor,
  closestCenter,
  useSensor,
  useSensors,
  type DragEndEvent,
} from "@dnd-kit/core";
import {
  SortableContext,
  arrayMove,
  useSortable,
  verticalListSortingStrategy,
} from "@dnd-kit/sortable";
import { CSS } from "@dnd-kit/utilities";
import {
  CircleAlertIcon,
  FolderCodeIcon,
  PlusIcon,
  GripVerticalIcon,
  InfoIcon,
  PanelsTopLeftIcon,
  SettingsIcon,
} from "lucide-react";

import {
  Alert,
  AlertDescription,
  AlertTitle,
  AlertAction,
} from "../../components/ui/alert.tsx";
import {
  Empty,
  EmptyHeader,
  EmptyMedia,
  EmptyContent,
} from "../../components/ui/empty.tsx";
import { Badge } from "../../components/ui/badge.tsx";
import { Button } from "../../components/ui/button.tsx";
import { Skeleton } from "../../components/ui/skeleton.tsx";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "../../components/ui/table.tsx";
import { toastManager } from "../../components/ui/toast.tsx";
import {
  Tooltip,
  TooltipPopup,
  TooltipTrigger,
} from "../../components/ui/tooltip.tsx";
import type {
  OrganizationProjectRank,
  ProjectSummary,
} from "../../contracts/account.ts";
import { cn } from "../../lib/utils.ts";
import { AccountError } from "../account/api.ts";
import { useAccountApi, useAccountBootstrap } from "../account/context.ts";
import { useResource } from "../account/useResource.ts";
import { ProjectGlyph } from "../components/ProjectGlyph.tsx";
import { useNewProject } from "../projects/NewProject.tsx";
import { setupStepsLeftLabel } from "../work/components/FirstRun.tsx";
import { projectSettingsPath } from "./projectSettings.ts";
import { SettingsPageContainer, SettingsSection } from "./settingsLayout.tsx";

const dispatchHelp =
  "Work is dispatched in this order. Issue priority comes first, then project order, then oldest issue. Running work keeps its slots.";

export function ProjectsSettings({
  onNavigate,
}: {
  readonly onNavigate?: (to: string) => void;
}): React.ReactElement {
  const api = useAccountApi();
  const bootstrap = useAccountBootstrap();
  const canManage =
    bootstrap?.actor.role === "owner" || bootstrap?.actor.role === "admin";
  const read = React.useCallback(async () => {
    const [projects, rank] = await Promise.all([
      api.projects(),
      api.projectRank(),
    ]);
    return { projects, rank };
  }, [api]);
  const resource = useResource(read, [read]);
  const { openNewProject } = useNewProject();
  const [draft, setDraft] = React.useState<readonly string[] | null>(null);
  const [lifted, setLifted] = React.useState<string | null>(null);
  const [saving, setSaving] = React.useState(false);
  const [error, setError] = React.useState("");
  const [announcement, setAnnouncement] = React.useState("");
  const savingRef = React.useRef(false);
  const sensors = useSensors(
    useSensor(PointerSensor, { activationConstraint: { distance: 5 } }),
  );
  const data = resource.value;
  const order = draft ?? data?.rank.project_ids ?? [];
  const projectsById = new Map(
    data?.projects.map((project) => [project.id, project]),
  );
  const projects = order.flatMap((id) => {
    const project = projectsById.get(id);
    return project ? [project] : [];
  });

  function announce(id: string, ids: readonly string[], prefix = ""): void {
    setAnnouncement(
      `${prefix}${projectsById.get(id)?.name ?? "Project"}, position ${ids.indexOf(id) + 1} of ${ids.length}`,
    );
  }

  async function save(ids: readonly string[]): Promise<void> {
    if (!data || savingRef.current) return;
    setLifted(null);
    if (ids.every((id, index) => data.rank.project_ids[index] === id)) {
      setDraft(null);
      return;
    }
    savingRef.current = true;
    setSaving(true);
    setDraft(ids);
    setError("");
    try {
      const rank: OrganizationProjectRank = await api.updateProjectRank({
        expectedRevision: data.rank.revision,
        projectIds: ids,
      });
      resource.set({ ...data, rank });
    } catch (cause) {
      if (cause instanceof AccountError && cause.isConflict) {
        try {
          resource.set(await read());
          toastManager.add({
            type: "info",
            title: "Project order changed elsewhere. Showing the latest order.",
          });
        } catch {
          setError("Project order could not be reloaded. Try again.");
        }
      } else {
        setError("Project order could not be saved. Try again.");
      }
    } finally {
      setDraft(null);
      setSaving(false);
      savingRef.current = false;
    }
  }

  function keyDown(event: React.KeyboardEvent, id: string): void {
    if (savingRef.current || projects.length < 2) return;
    if (event.code === "Space" || event.key === " ") {
      event.preventDefault();
      if (event.repeat) return;
      if (lifted === id) {
        announce(id, order, "Dropped. ");
        void save(order);
      } else if (lifted === null) {
        setLifted(id);
        setDraft(order);
        announce(id, order, "Lifted. ");
      }
    } else if (lifted === id && event.key === "Escape") {
      event.preventDefault();
      setLifted(null);
      setDraft(null);
      announce(id, data?.rank.project_ids ?? [], "Cancelled. ");
    } else if (
      lifted === id &&
      (event.key === "ArrowUp" || event.key === "ArrowDown")
    ) {
      event.preventDefault();
      const index = order.indexOf(id);
      const target = index + (event.key === "ArrowUp" ? -1 : 1);
      if (target < 0 || target >= order.length) return;
      const next = arrayMove([...order], index, target);
      setDraft(next);
      announce(id, next);
    }
  }

  function dragEnd({ active, over }: DragEndEvent): void {
    if (!over) return;
    const next = arrayMove(
      [...order],
      order.indexOf(String(active.id)),
      order.indexOf(String(over.id)),
    );
    announce(String(active.id), next, "Dropped. ");
    void save(next);
  }

  const title = (
    <>
      <span>Projects</span>
      <Tooltip>
        <TooltipTrigger
          render={
            <Button
              size="icon-micro"
              variant="ghost-muted"
              aria-label="Dispatch order information"
            >
              <InfoIcon className="size-3.5" />
            </Button>
          }
        />
        <TooltipPopup side="bottom" className="max-w-80">
          {dispatchHelp}
        </TooltipPopup>
      </Tooltip>
    </>
  );
  return (
    <SettingsPageContainer width="wide">
      <SettingsSection
        id="settings-projects"
        title={title}
        icon={<PanelsTopLeftIcon className="size-3.5" />}
        variant={
          resource.error || (data && projects.length === 0)
            ? "plain"
            : "grouped"
        }
        headerAction={
          canManage && !resource.error && (!data || projects.length > 0) ? (
            <Button size="xs" variant="outline" onClick={openNewProject}>
              New project
            </Button>
          ) : null
        }
      >
        {resource.error ? (
          <Alert variant="error">
            <CircleAlertIcon />
            <AlertTitle>Projects are not available</AlertTitle>
            <AlertDescription>
              {resource.error.isAccessError
                ? "You do not have access to this organization's projects."
                : "Could not load projects. Try again."}
            </AlertDescription>
            <AlertAction>
              <Button
                size="sm"
                variant="outline"
                onClick={() => void resource.refresh()}
              >
                Try again
              </Button>
            </AlertAction>
          </Alert>
        ) : !data ? (
          <div role="status" aria-label="Loading projects">
            <Table aria-hidden="true">
              <ProjectTableHeader canReorder={false} />
              <TableBody>
                {[0, 1, 2].map((row) => (
                  <TableRow key={row}>
                    <TableCell>
                      <span className="flex items-center gap-3 py-2">
                        <Skeleton className="h-3 w-3" />
                        <Skeleton className="h-3 w-32" />
                      </span>
                    </TableCell>
                    <TableCell>
                      <Skeleton className="h-3 w-14" />
                    </TableCell>
                    <TableCell>
                      <Skeleton className="h-3 w-16" />
                    </TableCell>
                    <TableCell>
                      <Skeleton className="h-3 w-16" />
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </div>
        ) : projects.length === 0 ? (
          <Empty size="entry" panel="dashed">
            <EmptyHeader>
              <EmptyMedia variant="icon">
                <FolderCodeIcon />
              </EmptyMedia>
              <h3 className="text-sm font-medium">No projects yet</h3>
            </EmptyHeader>
            {canManage ? (
              <EmptyContent>
                <Button size="sm" onClick={openNewProject}>
                  <PlusIcon />
                  New project
                </Button>
              </EmptyContent>
            ) : null}
          </Empty>
        ) : (
          <DndContext
            sensors={sensors}
            collisionDetection={closestCenter}
            onDragStart={({ active }) =>
              announce(String(active.id), order, "Lifted. ")
            }
            onDragOver={({ active, over }) => {
              if (over)
                announce(
                  String(active.id),
                  arrayMove(
                    [...order],
                    order.indexOf(String(active.id)),
                    order.indexOf(String(over.id)),
                  ),
                );
            }}
            onDragCancel={({ active }) =>
              announce(String(active.id), order, "Cancelled. ")
            }
            onDragEnd={dragEnd}
          >
            <SortableContext
              items={projects.map((project) => project.id)}
              strategy={verticalListSortingStrategy}
            >
              <Table
                role="table"
                aria-label="Projects in dispatch order"
                aria-busy={saving}
                className="max-sm:block"
              >
                <ProjectTableHeader
                  canReorder={canManage && projects.length > 1}
                />
                <TableBody role="rowgroup" className="max-sm:block">
                  {projects.map((project) => (
                    <ProjectRow
                      key={project.id}
                      project={project}
                      canManage={canManage && projects.length > 1}
                      disabled={
                        saving || projects.length < 2 || lifted !== null
                      }
                      lifted={lifted === project.id}
                      onKeyDown={keyDown}
                      onNavigate={onNavigate}
                    />
                  ))}
                </TableBody>
              </Table>
            </SortableContext>
          </DndContext>
        )}
      </SettingsSection>
      {error ? (
        <Alert variant="error">
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      ) : null}
      <div
        className="sr-only"
        role="status"
        aria-live="polite"
        aria-atomic="true"
      >
        {announcement}
      </div>
    </SettingsPageContainer>
  );
}

function ProjectRow({
  project,
  canManage,
  disabled,
  lifted,
  onKeyDown,
  onNavigate,
}: {
  readonly project: ProjectSummary;
  readonly canManage: boolean;
  readonly disabled: boolean;
  readonly lifted: boolean;
  readonly onKeyDown: (event: React.KeyboardEvent, id: string) => void;
  readonly onNavigate: ((to: string) => void) | undefined;
}): React.ReactElement {
  const {
    attributes,
    listeners,
    setNodeRef,
    setActivatorNodeRef,
    transform,
    transition,
    isDragging,
  } = useSortable({ id: project.id, disabled: !canManage || disabled });
  const status = (
    <span
      className={cn(
        "inline-flex items-center gap-1.5",
        project.onboarding.ready
          ? "text-success-foreground"
          : "text-muted-foreground",
      )}
    >
      <span
        aria-hidden="true"
        className={cn(
          "size-1.5 shrink-0 rounded-full",
          project.onboarding.ready ? "bg-success" : "bg-muted-foreground",
        )}
      />
      {project.onboarding.ready
        ? "Set up"
        : setupStepsLeftLabel(
            project.onboarding.steps.filter((step) => step.state !== "ready")
              .length,
          )}
    </span>
  );
  const tracker = (
    <Badge variant="outline">
      {project.profile === "native"
        ? "Native"
        : project.profile === "github" ||
            project.profile === "github_compatible"
          ? "GitHub"
          : project.profile}
    </Badge>
  );
  return (
    <TableRow
      ref={setNodeRef}
      role="row"
      style={{ transform: CSS.Transform.toString(transform), transition }}
      data-state={isDragging || lifted ? "selected" : undefined}
      className={cn(
        "max-sm:grid max-sm:grid-cols-[minmax(0,1fr)_44px] max-sm:items-center max-sm:px-2 max-sm:py-3",
        canManage && "max-sm:grid-cols-[36px_minmax(0,1fr)_44px]",
        (isDragging || lifted) && "relative z-10",
      )}
    >
      {canManage ? (
        <TableCell role="cell" className="w-9 max-sm:row-span-2">
          <Button
            ref={setActivatorNodeRef}
            {...attributes}
            {...listeners}
            onKeyDown={(event) => onKeyDown(event, project.id)}
            size="icon-sm"
            variant="ghost-muted"
            aria-label={`Reorder ${project.name}`}
            aria-pressed={lifted || isDragging}
            aria-describedby={`project-reorder-${project.id}`}
            disabled={disabled && !lifted}
            style={{ touchAction: "none" }}
          >
            <GripVerticalIcon className="size-3.5" />
          </Button>
          <span id={`project-reorder-${project.id}`} className="sr-only">
            Press Space to lift, use arrow keys to move, Space to drop, or
            Escape to cancel.
          </span>
        </TableCell>
      ) : null}
      <TableCell
        role="cell"
        className="max-w-0 py-3 max-sm:max-w-none max-sm:py-1"
      >
        <span className="flex min-w-0 items-center gap-2">
          <ProjectGlyph projectId={project.id} projectName={project.name} />
          <span className="truncate text-sm font-medium">{project.name}</span>
        </span>
        <span className="mt-1 flex flex-wrap items-center gap-2 sm:hidden">
          {tracker}
          {status}
        </span>
      </TableCell>
      <TableCell role="cell" className="hidden sm:table-cell">
        {tracker}
      </TableCell>
      <TableCell role="cell" className="hidden sm:table-cell">
        {status}
      </TableCell>
      <TableCell
        role="cell"
        className="text-right max-sm:col-start-[-2] max-sm:row-start-1 max-sm:self-start"
      >
        <span className="flex items-center justify-end gap-2">
          <Button
            size="xs"
            variant="outline"
            render={
              <a
                href={projectSettingsPath(project)}
                onClick={(event) => {
                  if (
                    onNavigate &&
                    !event.metaKey &&
                    !event.ctrlKey &&
                    !event.shiftKey &&
                    !event.altKey &&
                    event.button === 0
                  ) {
                    event.preventDefault();
                    onNavigate(projectSettingsPath(project));
                  }
                }}
              />
            }
            aria-label={`Settings for ${project.name}`}
          >
            <span className="hidden sm:inline">Settings</span>
            <SettingsIcon className="size-4 sm:hidden" />
          </Button>

          {!project.onboarding.ready ? (
            <span className="hidden sm:inline-flex">
              <Button
                size="xs"
                onClick={() => onNavigate?.(`/projects/${project.id}/setup`)}
              >
                Finish setup
              </Button>
            </span>
          ) : null}
        </span>
      </TableCell>
      {!project.onboarding.ready ? (
        <TableCell
          className={cn("sm:hidden", canManage ? "col-start-2" : "col-start-1")}
        >
          <Button
            size="xs"
            onClick={() => onNavigate?.(`/projects/${project.id}/setup`)}
          >
            Finish setup
          </Button>
        </TableCell>
      ) : null}
    </TableRow>
  );
}

function ProjectTableHeader({
  canReorder,
}: {
  readonly canReorder: boolean;
}): React.ReactElement {
  return (
    <TableHeader role="rowgroup" className="max-sm:sr-only">
      <TableRow role="row">
        {canReorder ? (
          <TableHead role="columnheader" className="w-10">
            <span className="sr-only">Reorder</span>
          </TableHead>
        ) : null}
        <TableHead role="columnheader" className="w-2/5">
          Project
        </TableHead>
        <TableHead role="columnheader" className="w-1/6">
          Tracker
        </TableHead>
        <TableHead role="columnheader">Setup</TableHead>
        <TableHead role="columnheader" className="w-24">
          <span className="sr-only">Settings</span>
        </TableHead>
      </TableRow>
    </TableHeader>
  );
}
