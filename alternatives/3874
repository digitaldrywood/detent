import React from "react";

import { Avatar } from "../../components/ui/avatar.tsx";
import { Badge } from "../../components/ui/badge.tsx";
import { Button } from "../../components/ui/button.tsx";
import { Label } from "../../components/ui/label.tsx";
import {
  Select,
  SelectItem,
  SelectPopup,
  SelectTrigger,
  SelectValue,
} from "../../components/ui/select.tsx";
import {
  Sheet,
  SheetClose,
  SheetDescription,
  SheetFooter,
  SheetHeader,
  SheetPanel,
  SheetPopup,
  SheetTitle,
} from "../../components/ui/sheet.tsx";
import { Switch } from "../../components/ui/switch.tsx";
import { Toggle, ToggleGroup } from "../../components/ui/toggle-group.tsx";
import type {
  Member,
  MemberRole,
  ProjectGrant,
} from "../../contracts/account.ts";
import { ControlError } from "./controls.tsx";

export type AccessProject = {
  readonly id: string;
  readonly name: string;
  readonly can_write: boolean;
  readonly can_manage_runners: boolean;
};

export function memberIdentity(member: Member): string {
  return member.email.trim() || member.name?.trim() || member.user_id;
}

export function MemberIdentity({
  member,
  isActor = false,
}: {
  readonly member: Member;
  readonly isActor?: boolean;
}): React.ReactElement {
  return (
    <div className="flex items-center gap-3">
      <Avatar size="md" name={member.name?.trim() || memberIdentity(member)} />
      <div className="min-w-0">
        <div className="flex items-center gap-2 font-medium">
          <span>{memberIdentity(member)}</span>
          {isActor ? <Badge variant="outline">You</Badge> : null}
        </div>
        {member.email.trim() && member.name?.trim() ? (
          <p className="text-xs text-muted-foreground">{member.name}</p>
        ) : null}
        {member.never_signed_in ? (
          <Badge variant="outline">Hasn't signed in</Badge>
        ) : null}
        {member.status !== "active" ? (
          <Badge variant="outline">Disabled</Badge>
        ) : null}
      </div>
    </div>
  );
}

export function RolePicker({
  roles,
  value,
  actorRole,
  label,
  disabled = false,
  onChange,
}: {
  readonly roles: readonly MemberRole[];
  readonly value: string;
  readonly actorRole: string;
  readonly label: string;
  readonly disabled?: boolean;
  readonly onChange: (role: string) => void;
}): React.ReactElement {
  return (
    <Select
      items={roles.map((role) => ({ value: role.slug, label: role.name }))}
      value={value}
      disabled={disabled || roles.length === 0}
      onValueChange={(role) => {
        if (role !== null) onChange(role);
      }}
    >
      <SelectTrigger aria-label={label}>
        <SelectValue>
          {roles.find((role) => role.slug === value)?.name ?? value}
        </SelectValue>
      </SelectTrigger>
      <SelectPopup>
        {roles.map((role) => (
          <SelectItem
            key={role.slug}
            value={role.slug}
            disabled={role.slug === "owner" && actorRole !== "owner"}
          >
            {role.name}
          </SelectItem>
        ))}
      </SelectPopup>
    </Select>
  );
}

function levelFor(grant: ProjectGrant | undefined): string {
  return grant === undefined ? "none" : grant.write ? "write" : "read";
}

export function accessSummary(
  projects: readonly AccessProject[],
  grants: readonly ProjectGrant[],
): string {
  const granted = projects.filter((project) =>
    grants.some((grant) => grant.project_id === project.id),
  );
  if (granted.length === 0) return "No projects · Grant access";
  const levels = new Set(
    granted.map((project) =>
      levelFor(grants.find((grant) => grant.project_id === project.id)),
    ),
  );
  return `${granted.length === projects.length ? "All " : ""}${granted.length}${granted.length === projects.length ? "" : ` project${granted.length === 1 ? "" : "s"}`} · ${levels.size > 1 ? "Mixed access" : levels.has("write") ? "Write" : "Read"}`;
}

export function ProjectMarks({
  projects,
  grants,
}: {
  readonly projects: readonly AccessProject[];
  readonly grants: readonly ProjectGrant[];
}): React.ReactElement {
  return (
    <span aria-hidden="true" className="flex items-center gap-1">
      {projects
        .filter((project) =>
          grants.some((grant) => grant.project_id === project.id),
        )
        .slice(0, 3)
        .map((project) => (
          <Badge key={project.id} variant="secondary" title={project.name}>
            {project.name.slice(0, 1).toUpperCase()}
          </Badge>
        ))}
    </span>
  );
}

export function ProjectAccessMatrix({
  projects,
  grants,
  identity,
  disabled = false,
  onChange,
}: {
  readonly projects: readonly AccessProject[];
  readonly grants: readonly ProjectGrant[];
  readonly identity: string;
  readonly disabled?: boolean;
  readonly onChange: (grants: ProjectGrant[]) => void;
}): React.ReactElement {
  const currentGrants = projects.map((project) =>
    grants.find((grant) => grant.project_id === project.id),
  );
  const levels = new Set(currentGrants.map(levelFor));
  const allLevel = levels.size === 1 ? [...levels][0]! : "mixed";
  const allRunners =
    currentGrants.length > 0 && currentGrants.every((grant) => grant?.runner);
  const replace = (
    project: AccessProject,
    level: string,
    runner?: boolean,
  ): ProjectGrant[] => {
    const previous = grants.find((grant) => grant.project_id === project.id);
    const next = grants.filter((grant) => grant.project_id !== project.id);
    if (level !== "none")
      next.push({
        project_id: project.id,
        write: level === "write",
        runner: runner ?? previous?.runner ?? false,
      });
    return next;
  };
  const row = (project: AccessProject | null) => {
    const name = project?.name ?? "All projects";
    const grant =
      project === null
        ? undefined
        : grants.find((grant) => grant.project_id === project.id);
    const level = project === null ? allLevel : levelFor(grant);
    const canWrite =
      project?.can_write ??
      (projects.length > 0 && projects.every((project) => project.can_write));
    const canRun =
      project?.can_manage_runners ??
      (projects.length > 0 &&
        projects.every((project) => project.can_manage_runners));
    const runnerDisabled =
      disabled ||
      !canRun ||
      (project === null
        ? currentGrants.some((grant) => grant === undefined)
        : grant === undefined);
    return (
      <div
        key={project?.id ?? "all"}
        className="flex flex-wrap items-center justify-between gap-3 px-3 py-3"
      >
        <span className="flex min-w-0 items-center gap-2 text-sm font-medium">
          {project === null ? null : (
            <Badge variant="secondary" aria-hidden="true">
              {name.slice(0, 1).toUpperCase()}
            </Badge>
          )}
          {name}
        </span>
        <div className="flex flex-wrap items-center gap-3">
          <ToggleGroup
            aria-label={`Access to ${name} for ${identity}`}
            value={level === "mixed" ? [] : [level]}
            disabled={disabled || projects.length === 0}
            onValueChange={(values) => {
              const next = values[0];
              if (next === undefined) return;
              if (project !== null) onChange(replace(project, next));
              else {
                const other = grants.filter(
                  (grant) =>
                    !projects.some(
                      (project) => project.id === grant.project_id,
                    ),
                );
                onChange(
                  next === "none"
                    ? other
                    : [
                        ...other,
                        ...projects.map((project) => ({
                          project_id: project.id,
                          write: next === "write",
                          runner:
                            grants.find(
                              (grant) => grant.project_id === project.id,
                            )?.runner ?? false,
                        })),
                      ],
                );
              }
            }}
          >
            <Toggle value="none">None</Toggle>
            <Toggle value="read">Read</Toggle>
            <Toggle value="write" disabled={!canWrite}>
              Write
            </Toggle>
          </ToggleGroup>
          <label className="flex items-center gap-2 text-xs text-muted-foreground">
            <Switch
              size="sm"
              aria-label={`Runner management on ${name} for ${identity}`}
              checked={project === null ? allRunners : (grant?.runner ?? false)}
              disabled={runnerDisabled}
              onCheckedChange={(runner) => {
                if (project !== null) onChange(replace(project, level, runner));
                else
                  onChange(
                    grants.map((grant) =>
                      projects.some(
                        (project) => project.id === grant.project_id,
                      )
                        ? { ...grant, runner }
                        : grant,
                    ),
                  );
              }}
            />
            Runners
          </label>
        </div>
      </div>
    );
  };
  return (
    <div className="flex flex-col gap-2">
      <div className="divide-y overflow-hidden rounded-lg border">
        {row(null)}
        {projects.map(row)}
      </div>
      {projects.length === 0 ? (
        <p className="text-sm text-muted-foreground">
          No projects in this organization.
        </p>
      ) : null}
      <p className="text-xs text-muted-foreground">
        Runners lets them enroll and manage runners for that project. It needs
        Read or Write.
      </p>
    </div>
  );
}

export function MemberAccessSheet({
  member,
  projects,
  roles,
  actorRole,
  pending,
  error,
  onSave,
  onRemove,
  onClose,
}: {
  readonly member: Member;
  readonly projects: readonly AccessProject[];
  readonly roles: readonly MemberRole[];
  readonly actorRole: string;
  readonly pending: boolean;
  readonly error: string | null;
  readonly onSave: (
    member: Member,
    role: string,
    grants: readonly ProjectGrant[],
  ) => Promise<boolean>;
  readonly onRemove: (member: Member) => void;
  readonly onClose: () => void;
}): React.ReactElement {
  const [role, setRole] = React.useState(member.role);
  const [grants, setGrants] = React.useState<readonly ProjectGrant[]>(
    member.grants,
  );
  React.useEffect(() => {
    setRole(member.role);
    setGrants(member.grants);
  }, [member]);
  return (
    <Sheet
      open
      onOpenChange={(open) => {
        if (!open && !pending) onClose();
      }}
    >
      <SheetPopup showCloseButton={!pending}>
        <SheetHeader>
          <SheetTitle>Project access</SheetTitle>
          <SheetDescription>
            Choose the projects this member can reach.
          </SheetDescription>
          <MemberIdentity member={member} />
        </SheetHeader>
        <SheetPanel className="flex flex-col gap-6 space-y-0">
          <div className="flex flex-col gap-2">
            <Label>Role</Label>
            {member.role === "owner" ? (
              <span className="text-sm">
                {roles.find((role) => role.slug === member.role)?.name ??
                  member.role}
              </span>
            ) : (
              <RolePicker
                roles={roles}
                value={role}
                actorRole={actorRole}
                label={`Role for ${memberIdentity(member)}`}
                disabled={pending}
                onChange={setRole}
              />
            )}
          </div>
          <div className="flex flex-col gap-2">
            <h3 className="text-sm font-medium">Project access</h3>
            <ProjectAccessMatrix
              projects={projects}
              grants={grants}
              identity={memberIdentity(member)}
              disabled={pending}
              onChange={setGrants}
            />
          </div>
          <ControlError message={error} />
        </SheetPanel>
        <SheetFooter className="flex-wrap">
          <Button
            variant="ghost-destructive"
            disabled={pending}
            onClick={() => onRemove(member)}
          >
            Remove from organization
          </Button>
          <SheetClose
            render={
              <Button variant="outline" disabled={pending}>
                Cancel
              </Button>
            }
          />
          <Button
            disabled={pending}
            aria-busy={pending}
            onClick={async () => {
              if (await onSave(member, role, grants)) onClose();
            }}
          >
            Save access
          </Button>
        </SheetFooter>
      </SheetPopup>
    </Sheet>
  );
}
