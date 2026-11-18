import React from "react";
import { ChevronDown, ChevronRight } from "lucide-react";
import type { AccessKey, KeyContext } from "../../contracts/accessKeys.ts";
import { Button } from "../../components/ui/button.tsx";
import {
  Table,
  TableHeader,
  TableBody,
  TableRow,
  TableHead,
  TableCell,
} from "../../components/ui/table.tsx";
import { formatRelativeTimeLabel } from "../../timestampFormat.ts";
import { useRelativeTimeTick } from "./settingsLayout.tsx";

function keyAccessSummary(
  apiKey: AccessKey,
  context?: KeyContext | null,
): string {
  if (apiKey.kind === "personal" && apiKey.access_context === "global") {
    return `All my orgs · ${apiKey.effective_reach?.length ?? 0}`;
  }
  const organizations = apiKey.organizations ?? [];
  const org = organizations[0];
  if (organizations.length === 1 && org) {
    if (org.project_access === "all") {
      return apiKey.kind === "service" ? "All org projects" : "This org";
    }
    if (org.project_ids.length === 1) {
      const projectId = org.project_ids[0];
      const reach =
        apiKey.effective_reach?.find(
          (item) => item.organization_id === org.organization_id,
        ) ??
        context?.organizations.find(
          (item) => item.organization_id === org.organization_id,
        );
      return `One project: ${reach?.projects.find((project) => project.id === projectId)?.name ?? projectId}`;
    }
    return `${org.project_ids.length} selected projects`;
  }
  return `${organizations.length} selected orgs`;
}

export function KeyScope({
  apiKey,
  context,
}: {
  readonly apiKey: AccessKey;
  readonly context?: KeyContext | null;
}) {
  const scope =
    apiKey.access_context === "global" && apiKey.kind === "personal"
      ? "All my orgs"
      : keyAccessSummary(apiKey, context);
  return (
    <div className="space-y-2 text-xs text-muted-foreground">
      <p>Effective reach today · {scope}</p>
      {apiKey.effective_reach?.length === 0 && (
        <p>No organization access today.</p>
      )}
      {apiKey.effective_reach && apiKey.effective_reach.length > 0 && (
        <div className="divide-y divide-border rounded-lg border border-border">
          {apiKey.effective_reach.map((org) => (
            <div
              key={org.organization_id}
              className="grid grid-cols-[minmax(0,1fr)_minmax(4rem,auto)_minmax(0,2fr)] gap-4 px-3 py-2"
            >
              <span className="break-words text-foreground">{org.name}</span>
              <span>{org.role}</span>
              <span className="break-words">
                {org.status === "pending"
                  ? "Awaiting approval"
                  : org.status === "blocked"
                    ? "Blocked"
                    : org.status === "inactive"
                      ? "Inactive"
                      : org.projects.length
                        ? org.projects
                            .map(
                              (project) =>
                                `${project.name}${project.can_write ? " (write)" : " (read)"}`,
                            )
                            .join(", ")
                        : "No project access"}
              </span>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

export function keyIsInactive(key: AccessKey, now: number) {
  return (
    Boolean(key.revoked_at) ||
    Boolean(key.expires_at && new Date(key.expires_at).getTime() <= now)
  );
}

function memberKeyAccessSummary(
  key: AccessKey,
  organizationId: string,
  context?: KeyContext | null,
) {
  const reach = key.effective_reach?.find(
    (org) => org.organization_id === organizationId,
  );
  if (reach?.status === "pending") return "awaiting approval";
  if (reach?.status === "blocked") return "blocked";
  if (reach?.status === "inactive") return "inactive";
  if (!reach?.projects.length) return "No project access";
  const projects = context?.organizations.find(
    (org) => org.organization_id === organizationId,
  )?.projects;
  if (
    projects?.length === reach.projects.length &&
    projects.every((project) =>
      reach.projects.some((item) => item.id === project.id),
    )
  )
    return "all projects";
  return reach.projects.length === 1
    ? reach.projects[0]?.name
    : `${reach.projects.length} projects`;
}

export function APIKeyTable({
  label,
  keys,
  context,
  loaded,
  error,
  emptyMessage,
  organizationId,
  renderActions,
}: {
  readonly label: string;
  readonly keys: readonly AccessKey[];
  readonly context?: KeyContext | null;
  readonly loaded: boolean;
  readonly error: boolean;
  readonly emptyMessage: string;
  readonly organizationId?: string;
  readonly renderActions: (
    key: AccessKey,
    inactive: boolean,
  ) => React.ReactNode;
}) {
  const now = useRelativeTimeTick(60_000);
  const [expandedKeys, setExpandedKeys] = React.useState<ReadonlySet<string>>(
    new Set(),
  );
  React.useEffect(() => setExpandedKeys(new Set()), [organizationId, label]);
  return (
    <div className="overflow-hidden rounded-xl border border-border">
      <Table aria-label={label}>
        <TableHeader>
          <TableRow>
            <TableHead>Name</TableHead>
            <TableHead>{organizationId ? "Owner" : "Permission"}</TableHead>
            <TableHead>Access</TableHead>
            <TableHead>Last used</TableHead>
            <TableHead>Expires</TableHead>
            <TableHead>
              <span className="sr-only">Actions</span>
            </TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {!loaded && !error && (
            <TableRow>
              <TableCell colSpan={6}>
                <span role="status">Loading API keys…</span>
              </TableCell>
            </TableRow>
          )}
          {loaded && keys.length === 0 && (
            <TableRow>
              <TableCell
                colSpan={6}
                className="whitespace-normal text-muted-foreground"
              >
                {emptyMessage}
              </TableCell>
            </TableRow>
          )}
          {keys.map((key) => {
            const inactive = keyIsInactive(key, now);
            const expanded = expandedKeys.has(key.id);
            const reachId = `key-reach-${organizationId ?? key.kind}-${key.id}`;
            const reach = key.effective_reach?.find(
              (org) => org.organization_id === organizationId,
            );
            const lastUsed = organizationId
              ? reach?.last_used_at
              : key.last_used_at;
            return (
              <React.Fragment key={key.id}>
                <TableRow
                  className={inactive ? "text-muted-foreground" : undefined}
                >
                  <TableCell>
                    <div className="flex items-center gap-1">
                      <Button
                        size="icon-xs"
                        variant="ghost-muted"
                        aria-label={`${expanded ? "Collapse" : "Expand"} ${key.name}`}
                        aria-expanded={expanded}
                        aria-controls={reachId}
                        onClick={() =>
                          setExpandedKeys((current) => {
                            const next = new Set(current);
                            if (expanded) next.delete(key.id);
                            else next.add(key.id);
                            return next;
                          })
                        }
                      >
                        {expanded ? (
                          <ChevronDown aria-hidden="true" />
                        ) : (
                          <ChevronRight aria-hidden="true" />
                        )}
                      </Button>
                      <span className="font-medium">{key.name}</span>
                    </div>
                  </TableCell>
                  <TableCell>
                    {organizationId ? key.owner_email : key.permission}
                  </TableCell>
                  <TableCell>
                    {organizationId
                      ? `${key.permission} · ${memberKeyAccessSummary(key, organizationId, context)}`
                      : keyAccessSummary(key, context)}
                  </TableCell>
                  <TableCell>
                    <span
                      className="inline-flex items-center gap-1.5 text-muted-foreground"
                      title={
                        lastUsed
                          ? new Date(lastUsed).toLocaleString()
                          : undefined
                      }
                    >
                      <span
                        aria-hidden="true"
                        className={`size-1.5 shrink-0 rounded-full ${!lastUsed ? "bg-muted-foreground/40" : now - new Date(lastUsed).getTime() < 86_400_000 ? "bg-success-foreground" : "bg-muted-foreground"}`}
                      />
                      {lastUsed ? formatRelativeTimeLabel(lastUsed) : "Never"}
                    </span>
                  </TableCell>
                  <TableCell>
                    {key.revoked_at
                      ? `Revoked ${new Date(key.revoked_at).toLocaleDateString()}`
                      : key.expires_at
                        ? `${inactive ? "Expired " : ""}${new Date(key.expires_at).toLocaleDateString()}`
                        : "Never"}
                  </TableCell>
                  <TableCell>{renderActions(key, inactive)}</TableCell>
                </TableRow>
                {expanded && (
                  <TableRow>
                    <TableCell colSpan={6} className="whitespace-normal">
                      <div id={reachId} className="px-6 pb-2">
                        <KeyScope apiKey={key} context={context} />
                      </div>
                    </TableCell>
                  </TableRow>
                )}
              </React.Fragment>
            );
          })}
        </TableBody>
      </Table>
    </div>
  );
}
