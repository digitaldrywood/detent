import React from "react";
import { Link } from "@tanstack/react-router";
import {
  MoreHorizontalIcon,
  PencilIcon,
  PlusIcon,
  CheckIcon,
  ChevronRightIcon,
} from "lucide-react";
import type { NativeIssue } from "../../contracts/work.ts";
import { Button } from "../../components/ui/button.tsx";
import { Badge } from "../../components/ui/badge.tsx";
import { Input } from "../../components/ui/input.tsx";
import {
  Menu,
  MenuTrigger,
  MenuPopup,
  MenuItem,
} from "../../components/ui/menu.tsx";
import {
  Collapsible,
  CollapsibleTrigger,
  CollapsiblePanel,
} from "../../components/ui/collapsible.tsx";
import { useAccountBootstrap } from "../account/context.ts";
import { useMutation, useResource } from "../account/useResource.ts";
import { useWorkHttp } from "../work/lib/useWork.ts";
import { newWorkKey, type WorkHttp } from "../work/lib/workHttp.ts";
import { NewIssueDialog } from "../work/NewIssue.tsx";
import { formatRelativeTimeLabel } from "../../timestampFormat.ts";
import { ReporterPage } from "./ReporterPage.tsx";
import { useReporterProject } from "./ReporterShell.tsx";
import { useRelativeTimeTick } from "../settings/settingsLayout.tsx";

export const REPORTER_GROUPS = [
  {
    status: "submitted",
    title: "Submitted",
    description: "Waiting for the team to review. You can still edit these.",
  },
  {
    status: "planned",
    title: "Planned",
    description: "Accepted and waiting its turn.",
  },
  {
    status: "in_progress",
    title: "In progress",
    description: "Being worked on now.",
  },
  {
    status: "done",
    title: "Done",
    description: "Fixed. Check it works for you.",
  },
  { status: "cancelled", title: "Cancelled", description: "" },
] as const;

export async function readMyIssues(
  http: WorkHttp,
  projects: readonly { id: string }[],
) {
  const pages = await Promise.all(
    projects.map(async (project) => {
      const items: NativeIssue[] = [];
      let cursor: string | undefined;
      do {
        const page = await http.listWorkItems({
          projectId: project.id,
          createdBy: "me",
          limit: 200,
          cursor,
        });
        items.push(...page.items);
        cursor = page.next_cursor;
      } while (cursor);
      return items;
    }),
  );
  return pages.flat().sort((a, b) => b.created_at.localeCompare(a.created_at));
}

export function ReporterIssueRow({
  issue,
  project,
  pending,
  onCancel,
  onVerify,
}: {
  issue: NativeIssue;
  project: string;
  pending: boolean;
  onCancel: () => void;
  onVerify: () => void;
}) {
  return (
    <div className="flex flex-col gap-2 p-3 sm:flex-row sm:items-center sm:gap-3 sm:p-4">
      <Link
        to="/work/i/$workItemId"
        params={{ workItemId: issue.work_item_id }}
        className="min-w-0 flex-1 break-words text-base font-medium hover:underline sm:text-sm"
      >
        {issue.title}
      </Link>
      <div className="flex flex-wrap items-center gap-x-3 gap-y-2 text-sm text-muted-foreground sm:text-xs">
        <span className="inline-flex items-center gap-1.5">
          <span
            aria-hidden
            className="size-1.5 shrink-0 rounded-full bg-primary"
          />
          {project}
        </span>
        <span>#{issue.number}</span>
        <time dateTime={issue.created_at}>
          {formatRelativeTimeLabel(issue.created_at)}
        </time>
      </div>
      {issue.reporter_status === "submitted" ? (
        <div className="hidden gap-1 sm:flex">
          <Button
            variant="ghost"
            size="icon-sm"
            aria-label={`Edit ${issue.title}`}
            render={
              <Link
                to="/work/i/$workItemId"
                params={{ workItemId: issue.work_item_id }}
              />
            }
          >
            <PencilIcon aria-hidden />
          </Button>
          <Menu>
            <MenuTrigger
              render={
                <Button
                  variant="ghost"
                  size="icon-sm"
                  aria-label={`More for ${issue.title}`}
                />
              }
            >
              <MoreHorizontalIcon aria-hidden />
            </MenuTrigger>
            <MenuPopup>
              <MenuItem disabled={pending} onClick={onCancel}>
                Cancel
              </MenuItem>
            </MenuPopup>
          </Menu>
        </div>
      ) : null}
      {issue.reporter_status === "done" ? (
        issue.verification?.state === "verified" ? (
          <Badge variant="success" className="w-fit">
            You confirmed
          </Badge>
        ) : (
          <Button
            className="w-fit"
            size="sm"
            variant="outline"
            disabled={pending}
            onClick={onVerify}
          >
            <CheckIcon aria-hidden />
            Confirm it's fixed
          </Button>
        )
      ) : null}
    </div>
  );
}

export function ReporterIssueGroups({
  items,
  projectName,
  pending,
  onCancel,
  onVerify,
}: {
  items: readonly NativeIssue[];
  projectName: (id: string) => string;
  pending: boolean;
  onCancel: (issue: NativeIssue) => void;
  onVerify: (issue: NativeIssue) => void;
}) {
  return (
    <>
      {REPORTER_GROUPS.map((group) => {
        const issues = items.filter(
          (issue) => issue.reporter_status === group.status,
        );
        const rows = (
          <div className="divide-y divide-border rounded-xl border border-border bg-card">
            {issues.map((issue) => (
              <ReporterIssueRow
                key={issue.work_item_id}
                issue={issue}
                project={projectName(issue.project_id)}
                pending={pending}
                onCancel={() => onCancel(issue)}
                onVerify={() => onVerify(issue)}
              />
            ))}
            {!issues.length ? (
              <p className="p-4 text-sm text-muted-foreground">
                No {group.title.toLowerCase()} issues.
              </p>
            ) : null}
          </div>
        );
        if (group.status === "cancelled")
          return (
            <Collapsible key={group.status}>
              <CollapsibleTrigger className="group inline-flex items-center gap-2 text-sm text-muted-foreground">
                <ChevronRightIcon
                  aria-hidden
                  className="size-3 group-data-[panel-open]:rotate-90"
                />
                Cancelled · {issues.length}
              </CollapsibleTrigger>
              <CollapsiblePanel>{rows}</CollapsiblePanel>
            </Collapsible>
          );
        return (
          <section
            key={group.status}
            aria-label={group.title}
            className="space-y-2"
          >
            <div className="flex flex-wrap items-center justify-between gap-2">
              <h2 className="text-base font-semibold sm:text-sm">
                {group.title}{" "}
                <span className="ml-1 text-sm font-normal text-muted-foreground sm:text-xs">
                  {issues.length}
                </span>
              </h2>
              <p className="hidden text-xs text-muted-foreground sm:block">
                {group.description}
              </p>
            </div>
            {rows}
          </section>
        );
      })}
    </>
  );
}

export function MyIssues() {
  const account = useAccountBootstrap()!;
  const http = useWorkHttp();
  const scope = useReporterProject();
  const [query, setQuery] = React.useState("");
  const [newIssue, setNewIssue] = React.useState(false);
  useRelativeTimeTick(60_000);
  const projects = account.projects.filter(
    (project) => !scope.project || project.id === scope.project,
  );
  const projectKey = projects.map((project) => project.id).join(",");
  const issues = useResource(
    () => readMyIssues(http, projects),
    [http, projectKey],
  );
  const mutation = useMutation(async (issue: NativeIssue, verify: boolean) => {
    const input = {
      projectId: issue.project_id,
      itemId: issue.work_item_id,
      expectedRevision: issue.revision,
      key: newWorkKey(verify ? "verify" : "cancel"),
    };
    if (verify) await http.verifyFix({ ...input, result: "verified" });
    else await http.transition({ ...input, state: "Cancelled" });
    await issues.refresh();
  });
  const filtered = (issues.value ?? []).filter((issue) =>
    `${issue.title} #${issue.number} ${projects.find((project) => project.id === issue.project_id)?.name ?? ""}`
      .toLocaleLowerCase()
      .includes(query.trim().toLocaleLowerCase()),
  );
  return (
    <ReporterPage
      title="My issues"
      parent={
        projects.find((project) => project.id === scope.project)?.name ??
        "All projects"
      }
      wide
      actions={
        <Button size="sm" onClick={() => setNewIssue(true)}>
          <PlusIcon aria-hidden />
          <span className="hidden sm:inline">New issue</span>
          <span className="sr-only sm:hidden">New issue</span>
        </Button>
      }
    >
      <div className="hidden flex-wrap items-center gap-3 sm:flex">
        <Input
          aria-label="Search my issues"
          placeholder="Search my issues"
          type="search"
          value={query}
          onChange={(event) => setQuery(event.currentTarget.value)}
          className="min-w-0 flex-1"
        />
        <p className="text-xs text-muted-foreground">
          Only issues you filed are shown.
        </p>
      </div>
      {issues.loading && !issues.value ? (
        <p role="status">Loading your issues…</p>
      ) : issues.error ? (
        <p role="alert">{issues.error.message}</p>
      ) : (
        <ReporterIssueGroups
          items={filtered}
          projectName={(id) =>
            account.projects.find((project) => project.id === id)?.name ?? id
          }
          pending={mutation.pending}
          onCancel={(issue) => void mutation.call(issue, false)}
          onVerify={(issue) => void mutation.call(issue, true)}
        />
      )}
      {mutation.error ? <p role="alert">{mutation.error.message}</p> : null}
      <NewIssueDialog
        open={newIssue}
        onOpenChange={setNewIssue}
        projects={account.projects}
        projectId={scope.project || null}
        state="Triage"
        onCreated={() => {
          setNewIssue(false);
          void issues.refresh();
        }}
      />
    </ReporterPage>
  );
}
