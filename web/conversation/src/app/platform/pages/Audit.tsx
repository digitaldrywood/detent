import { Link } from "@tanstack/react-router";
import { SearchIcon } from "lucide-react";
import React from "react";

import { Badge } from "../../../components/ui/badge.tsx";
import { Input } from "../../../components/ui/input.tsx";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "../../../components/ui/table.tsx";
import { Toggle, ToggleGroup } from "../../../components/ui/toggle-group.tsx";
import type { AuditEntry, AuditPage } from "../../../contracts/platform.ts";
import { useResource } from "../../account/useResource.ts";
import { SettingsSection } from "../../settings/settingsLayout.tsx";
import { usePlatformApi } from "../PlatformLayout.tsx";
import { Ago, EmptyNote, Loadable, PlatformPage } from "../ui.tsx";

const ACTOR_KINDS = [
  { value: "all", label: "Everyone" },
  { value: "staff", label: "Staff" },
  { value: "support", label: "Support" },
  { value: "member", label: "Members" },
  { value: "system", label: "System" },
] as const;

const KIND_VARIANT: Record<AuditEntry["actor_kind"], "info" | "warning" | "outline" | "secondary"> = {
  staff: "info",
  support: "warning",
  member: "outline",
  system: "secondary",
};

export function AuditTable({
  entries,
  showOrganization = true,
}: {
  readonly entries: readonly AuditEntry[];
  readonly showOrganization?: boolean;
}): React.ReactElement {
  if (entries.length === 0) return <EmptyNote>No audit entries match.</EmptyNote>;
  return (
    <Table aria-label="Audit entries">
      <TableHeader>
        <TableRow>
          <TableHead>When</TableHead>
          <TableHead>Actor</TableHead>
          <TableHead>Event</TableHead>
          {showOrganization ? <TableHead>Organization</TableHead> : null}
          <TableHead>Detail</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {entries.map((entry) => (
          <TableRow key={entry.id}>
            <TableCell className="whitespace-nowrap">
              <Ago at={entry.at} />
            </TableCell>
            <TableCell>
              <div>{entry.actor}</div>
              <Badge variant={KIND_VARIANT[entry.actor_kind]} size="sm">
                {entry.actor_kind}
              </Badge>
            </TableCell>
            <TableCell>
              <span className="font-mono text-xs">{entry.event}</span>
            </TableCell>
            {showOrganization ? (
              <TableCell>
                {entry.organization === null ? (
                  <span className="text-muted-foreground">Platform</span>
                ) : (
                  <Link
                    to={`/platform/organizations/${encodeURIComponent(entry.organization.id)}` as never}
                    className="hover:underline"
                  >
                    {entry.organization.name}
                  </Link>
                )}
              </TableCell>
            ) : null}
            <TableCell className="max-w-80 whitespace-normal text-muted-foreground">{entry.detail}</TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}

export function AuditLogPage(): React.ReactElement {
  const api = usePlatformApi();
  const [query, setQuery] = React.useState("");
  const [actorKind, setActorKind] = React.useState("all");
  const [submitted, setSubmitted] = React.useState("");
  const page = useResource<AuditPage>(
    () => api.audit({ q: submitted, actorKind: actorKind === "all" ? undefined : actorKind }),
    [api, submitted, actorKind],
  );
  return (
    <PlatformPage crumbs={[{ label: "Audit log" }]} proposed>
      <form
        className="flex flex-wrap items-center gap-2"
        onSubmit={(event) => {
          event.preventDefault();
          setSubmitted(query);
        }}
      >
        <div className="relative min-w-56 flex-1">
          <SearchIcon className="pointer-events-none absolute top-1/2 left-2.5 size-4 -translate-y-1/2 text-muted-foreground" />
          <Input
            type="search"
            aria-label="Search the audit log"
            placeholder="Search events, actors or organizations"
            className="ps-8"
            value={query}
            onChange={(event) => setQuery(event.currentTarget.value)}
          />
        </div>
        <div className="max-w-full overflow-x-auto">
          <ToggleGroup
            aria-label="Actor"
            variant="segmented"
            value={[actorKind]}
            onValueChange={(next) => setActorKind(next[0] ?? "all")}
          >
            {ACTOR_KINDS.map((kind) => (
              <Toggle key={kind.value} value={kind.value}>
                {kind.label}
              </Toggle>
            ))}
          </ToggleGroup>
        </div>
      </form>
      <SettingsSection title="Entries">
        <Loadable resource={page} label="the audit log" rows={8}>
          {(value) => (
            <>
              <AuditTable entries={value.entries} />
              {value.next_cursor === null ? null : (
                <p className="px-4 py-3 text-xs text-muted-foreground">Showing the newest {value.entries.length}. Older entries load by cursor.</p>
              )}
            </>
          )}
        </Loadable>
      </SettingsSection>
    </PlatformPage>
  );
}
